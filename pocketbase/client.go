package pocketbase

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"ai-server/config"
)

var (
	ErrTokenNotFound   = errors.New("token not found")
	ErrUnauthorized    = errors.New("invalid or expired auth token")
	ErrRateLimitMinute = errors.New("rate limit: only 1 request per minute allowed")
	ErrRateLimitDay    = errors.New("daily quota exceeded")
)

// pbTimeLayout — формат дат в PocketBase.
const pbTimeLayout = "2006-01-02 15:04:05.000Z"

// TokenRecord отражает запись из коллекции tokens.
type TokenRecord struct {
	ID              string  `json:"id"`
	Profile         string  `json:"profile"`
	Token           string  `json:"token"`
	TotalRequests   float64 `json:"total_requests"`
	LastRequestDate string  `json:"last_request_date"`
	DayRequests     float64 `json:"day_requests"`
	DayResetDate    string  `json:"day_reset_date"`
}

// QuotaInfo возвращается клиенту вместе с ответом.
type QuotaInfo struct {
	RequestsToday int        `json:"requests_today"`
	LimitDay      int        `json:"limit_day"`
	LimitMinute   int        `json:"limit_minute"`
	NextRequestAt *time.Time `json:"next_request_at,omitempty"`
}

// Client — HTTP-клиент для PocketBase Admin API.
type Client struct {
	cfg        *config.Config
	httpClient *http.Client

	mu         sync.Mutex
	adminToken string
	tokenExp   time.Time
}

func NewClient(cfg *config.Config) *Client {
	return &Client{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// ── Admin auth ────────────────────────────────────────────────────────────────

func (c *Client) getAdminToken() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.adminToken != "" && time.Now().Before(c.tokenExp) {
		return c.adminToken, nil
	}
	return c.refreshAdminToken()
}

func (c *Client) refreshAdminToken() (string, error) {
	body, _ := json.Marshal(map[string]string{
		"identity": c.cfg.PBAdminEmail,
		"password": c.cfg.PBAdminPassword,
	})
	// PocketBase v0.23+: /api/collections/_superusers/auth-with-password
	// PocketBase < v0.23: /api/admins/auth-with-password
	resp, err := c.httpClient.Post(
		c.cfg.PBUrl+"/api/collections/_superusers/auth-with-password",
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		return "", fmt.Errorf("pb admin auth: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("pb admin auth decode: %w", err)
	}
	if result.Token == "" {
		return "", errors.New("pb admin auth: empty token returned")
	}

	c.adminToken = result.Token
	// Берём реальный срок жизни из JWT (PB-настройка authToken.duration),
	// с буфером 60с. Если распарсить не вышло — консервативный фоллбэк.
	if exp, ok := jwtExpiry(result.Token); ok {
		c.tokenExp = exp.Add(-60 * time.Second)
	} else {
		c.tokenExp = time.Now().Add(30 * time.Minute)
	}
	return c.adminToken, nil
}

// jwtExpiry достаёт claim "exp" из JWT без проверки подписи.
func jwtExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}

// ── User auth verification + token lookup ─────────────────────────────────────

// VerifyUser проверяет PocketBase user JWT через auth-refresh и возвращает user ID.
// userAuthToken — голый токен (без "Bearer ").
func (c *Client) VerifyUser(userAuthToken string) (string, error) {
	req, _ := http.NewRequest("POST",
		c.cfg.PBUrl+"/api/collections/users/auth-refresh",
		nil)
	req.Header.Set("Authorization", "Bearer "+userAuthToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("pb auth-refresh: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", ErrUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("pb auth-refresh: status %d", resp.StatusCode)
	}

	var result struct {
		Record struct {
			ID string `json:"id"`
		} `json:"record"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("pb auth-refresh decode: %w", err)
	}
	if result.Record.ID == "" {
		return "", ErrUnauthorized
	}
	return result.Record.ID, nil
}

// FindTokenByUser ищет запись tokens по user ID (поле profile).
func (c *Client) FindTokenByUser(userID string) (*TokenRecord, error) {
	adminToken, err := c.getAdminToken()
	if err != nil {
		return nil, err
	}

	// Строим URL через url.Values, чтобы избежать двойного кодирования.
	endpoint, _ := url.Parse(c.cfg.PBUrl + "/api/collections/tokens/records")
	q := endpoint.Query()
	q.Set("filter", "(profile='"+escapePBFilter(userID)+"')")
	q.Set("perPage", "1")
	endpoint.RawQuery = q.Encode()

	req, _ := http.NewRequest("GET", endpoint.String(), nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pb find token: %w", err)
	}
	defer resp.Body.Close()

	// Отличать «записи нет» от ошибки PocketBase важно: на ErrTokenNotFound
	// вызывающий код заводит новую запись.
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("pb find token: status %d: %s", resp.StatusCode, body)
	}

	var result struct {
		Items []TokenRecord `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("pb find token decode: %w", err)
	}
	if len(result.Items) == 0 {
		return nil, ErrTokenNotFound
	}
	return &result.Items[0], nil
}

// ── Quota ─────────────────────────────────────────────────────────────────────

func parsePBTime(s string) (time.Time, bool) {
	// PB может возвращать дату без миллисекунд.
	for _, layout := range []string{
		"2006-01-02 15:04:05.000Z",
		"2006-01-02 15:04:05Z",
		"2006-01-02 15:04:05.999Z",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func todayUTC() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}

// CheckQuota проверяет ограничения без изменения записи.
// Возвращает QuotaInfo и ошибку ErrRateLimitMinute / ErrRateLimitDay при превышении.
func (c *Client) CheckQuota(rec *TokenRecord, perMinute, perDay int) (QuotaInfo, error) {
	now := time.Now().UTC()
	today := todayUTC()
	cooldown := time.Minute / time.Duration(perMinute)

	info := QuotaInfo{LimitDay: perDay, LimitMinute: perMinute}

	// Минутный лимит.
	if rec.LastRequestDate != "" {
		if last, ok := parsePBTime(rec.LastRequestDate); ok {
			if elapsed := now.Sub(last); elapsed < cooldown {
				next := last.Add(cooldown)
				info.NextRequestAt = &next
				info.RequestsToday = c.effectiveDayRequests(rec, today)
				return info, ErrRateLimitMinute
			}
		}
	}

	// Дневной лимит.
	dayReqs := c.effectiveDayRequests(rec, today)
	info.RequestsToday = dayReqs
	if dayReqs >= perDay {
		return info, ErrRateLimitDay
	}

	return info, nil
}

// ConsumeQuota атомарно обновляет счётчики в PocketBase.
func (c *Client) ConsumeQuota(rec *TokenRecord, perMinute, perDay int) (QuotaInfo, error) {
	adminToken, err := c.getAdminToken()
	if err != nil {
		return QuotaInfo{}, err
	}

	now := time.Now().UTC()
	today := todayUTC()

	dayReqs := c.effectiveDayRequests(rec, today) + 1
	dayResetDate := rec.DayResetDate
	if rec.DayResetDate == "" {
		dayResetDate = today.Format(pbTimeLayout)
	} else if reset, ok := parsePBTime(rec.DayResetDate); ok && reset.Before(today) {
		// Новый день — сбрасываем.
		dayReqs = 1
		dayResetDate = today.Format(pbTimeLayout)
	}

	update := map[string]any{
		"total_requests":    rec.TotalRequests + 1,
		"last_request_date": now.Format(pbTimeLayout),
		"day_requests":      dayReqs,
		"day_reset_date":    dayResetDate,
	}

	body, _ := json.Marshal(update)
	req, _ := http.NewRequest("PATCH",
		c.cfg.PBUrl+"/api/collections/tokens/records/"+rec.ID,
		bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return QuotaInfo{}, fmt.Errorf("pb consume quota: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	return QuotaInfo{
		RequestsToday: dayReqs,
		LimitDay:      perDay,
		LimitMinute:   perMinute,
	}, nil
}

func (c *Client) effectiveDayRequests(rec *TokenRecord, today time.Time) int {
	if rec.DayResetDate == "" {
		return 0
	}
	if reset, ok := parsePBTime(rec.DayResetDate); ok && reset.Before(today) {
		return 0 // новый день, счётчик ещё не сброшен
	}
	return int(rec.DayRequests)
}

// ── Автосоздание записи tokens ────────────────────────────────────────────────

// tokenMu сериализует автосоздание записи tokens: между «не нашли» и
// «создали» есть гонка — два параллельных запроса нового пользователя иначе
// заведут ему две строки с разными счётчиками.
var tokenMu sync.Mutex

// EnsureTokenByUser возвращает запись tokens пользователя, создавая её при
// первом обращении. Раньше строку заводили в PocketBase руками, и до этого
// любой запрос нового пользователя падал с TOKEN_NOT_FOUND.
func (c *Client) EnsureTokenByUser(userID string) (*TokenRecord, error) {
	rec, err := c.FindTokenByUser(userID)
	if err == nil {
		return rec, nil
	}
	if !errors.Is(err, ErrTokenNotFound) {
		return nil, err
	}

	tokenMu.Lock()
	defer tokenMu.Unlock()

	// Пока ждали блокировку, запись мог создать параллельный запрос.
	rec, err = c.FindTokenByUser(userID)
	if err == nil {
		return rec, nil
	}
	if !errors.Is(err, ErrTokenNotFound) {
		return nil, err
	}

	return c.createTokenRecord(userID)
}

// createTokenRecord заводит нулевую запись квот для пользователя.
func (c *Client) createTokenRecord(userID string) (*TokenRecord, error) {
	adminToken, err := c.getAdminToken()
	if err != nil {
		return nil, err
	}

	// last_request_date не заполняем: пустое значение означает «запросов ещё
	// не было», и минутный кулдаун не срабатывает на первом обращении.
	payload := map[string]any{
		"profile":        userID,
		"token":          newTokenValue(),
		"total_requests": 0,
		"day_requests":   0,
		"day_reset_date": todayUTC().Format(pbTimeLayout),
	}

	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost,
		c.cfg.PBUrl+"/api/collections/tokens/records",
		bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pb create token: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		// 400 — в том числе нарушение уникального индекса по profile:
		// запись мог создать другой инстанс сервера. Тогда она уже есть.
		if resp.StatusCode == http.StatusBadRequest {
			if rec, findErr := c.FindTokenByUser(userID); findErr == nil {
				return rec, nil
			}
		}
		return nil, fmt.Errorf("pb create token: status %d: %s", resp.StatusCode, respBody)
	}

	var rec TokenRecord
	if err := json.NewDecoder(resp.Body).Decode(&rec); err != nil {
		return nil, fmt.Errorf("pb create token decode: %w", err)
	}
	if rec.ID == "" {
		return nil, errors.New("pb create token: empty record id returned")
	}
	return &rec, nil
}

// newTokenValue генерирует значение поля token. Логика сервера его не
// использует, но поле может быть обязательным/уникальным в схеме коллекции.
func newTokenValue() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// Источник энтропии недоступен — не повод отказывать пользователю.
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}
