package pocketbase

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"ai-server/config"
)

var (
	ErrTokenNotFound   = errors.New("token not found")
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
	// Токены суперадмина живут 7 дней, обновляем за сутки до истечения.
	c.tokenExp = time.Now().Add(6 * 24 * time.Hour)
	return c.adminToken, nil
}

// ── Token lookup ──────────────────────────────────────────────────────────────

// FindToken ищет запись tokens по значению поля token.
func (c *Client) FindToken(tokenValue string) (*TokenRecord, error) {
	adminToken, err := c.getAdminToken()
	if err != nil {
		return nil, err
	}

	filter := url.QueryEscape("(token='" + tokenValue + "')")
	req, _ := http.NewRequest("GET",
		c.cfg.PBUrl+"/api/collections/tokens/records?filter="+filter+"&perPage=1",
		nil)
	req.Header.Set("Authorization", adminToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pb find token: %w", err)
	}
	defer resp.Body.Close()

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
	req.Header.Set("Authorization", adminToken)
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
