// Package rustore — клиент RuStore Public API (public-api.rustore.ru).
//
// Используются методы, актуальные для Pay SDK:
//   - POST /public/auth                                                   — JWE-токен
//   - GET  /public/v4/subscription/{pkg}/{subId}/{purchaseId}             — Subscription Data v4
//   - POST /public/v2/subscription/{pkg}/{subId}/{purchaseId}:acknowledge — Confirm Delivery v2
//   - PUT  /public/applications/{appId}/purchases/{purchaseId}:confirm    — подтверждение разовой покупки
//
// В песочнице (RUSTORE_SANDBOX=true) те же пути живут под /public/sandbox/.
package rustore

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"ai-server/config"
)

var (
	// ErrNotFound — покупки с таким purchaseId у этого продукта нет.
	ErrNotFound = errors.New("rustore: purchase not found")
	// ErrUnknownProduct — purchaseId не сошёлся ни с одним продуктом из
	// RUSTORE_SUBSCRIPTION_IDS.
	ErrUnknownProduct = errors.New("rustore: purchase does not match any configured subscription")
)

// tokenTTLBuffer — насколько раньше протухания обновляем JWE (он живёт 900с).
const tokenTTLBuffer = 60 * time.Second

// signatureTimeLayout — формат timestamp, который RuStore ждёт в /public/auth.
const signatureTimeLayout = "2006-01-02T15:04:05.000-07:00"

// Client — потокобезопасный клиент public-api с кэшем JWE-токена.
type Client struct {
	baseURL         string
	keyID           string
	privateKey      *rsa.PrivateKey
	packageName     string
	appID           string
	subscriptionIDs []string
	sandbox         bool

	httpClient *http.Client

	mu       sync.Mutex
	token    string
	tokenExp time.Time
}

// NewClient собирает клиент из конфигурации. Возвращает (nil, nil), если
// интеграция выключена (RUSTORE_ENABLED=false) — вызывающий трактует nil как
// «верификация недоступна».
func NewClient(cfg *config.Config) (*Client, error) {
	if !cfg.RuStoreEnabled {
		return nil, nil
	}
	if cfg.RuStoreKeyID == "" {
		return nil, errors.New("RUSTORE_KEY_ID must be set")
	}
	if cfg.RuStorePackageName == "" {
		return nil, errors.New("RUSTORE_PACKAGE_NAME must be set")
	}
	key, err := parsePrivateKey(cfg.RuStorePrivateKey)
	if err != nil {
		return nil, err
	}
	if len(cfg.RuStoreSubscriptionIDs) == 0 {
		return nil, errors.New("RUSTORE_SUBSCRIPTION_IDS must list at least one subscription product code")
	}
	return &Client{
		baseURL:         cfg.RuStoreAPIURL,
		keyID:           cfg.RuStoreKeyID,
		privateKey:      key,
		packageName:     cfg.RuStorePackageName,
		appID:           cfg.RuStoreAppID,
		subscriptionIDs: cfg.RuStoreSubscriptionIDs,
		sandbox:         cfg.RuStoreSandbox,
		// Подтверждение покупки, по документации, отвечает до 30с.
		httpClient: &http.Client{Timeout: 40 * time.Second},
	}, nil
}

// SubscriptionIDs — коды подписок, с которыми работает приложение.
func (c *Client) SubscriptionIDs() []string { return c.subscriptionIDs }

// parsePrivateKey читает RSA-ключ в PKCS#1 или PKCS#8. Принимается PEM,
// «голый» DER в base64 (в таком виде ключ отдаёт консоль RuStore) и base64
// от целого PEM — так его удобнее хранить одной строкой в секретах CI.
func parsePrivateKey(raw string) (*rsa.PrivateKey, error) {
	if raw == "" {
		return nil, errors.New("RUSTORE_PRIVATE_KEY (or RUSTORE_PRIVATE_KEY_FILE) must be set")
	}

	var der []byte
	if block, _ := pem.Decode([]byte(raw)); block != nil {
		der = block.Bytes
	} else {
		// Переносы строк и пробелы в base64 не значимы, но мешают декодеру.
		compact := strings.Map(func(r rune) rune {
			if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
				return -1
			}
			return r
		}, raw)
		decoded, err := base64.StdEncoding.DecodeString(compact)
		if err != nil {
			return nil, errors.New("rustore private key: expected PEM or base64-encoded key")
		}
		// Ключ мог быть закодирован целиком вместе с PEM-обёрткой.
		if block, _ := pem.Decode(decoded); block != nil {
			der = block.Bytes
		} else {
			der = decoded
		}
	}

	if key, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("rustore private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("rustore private key: not an RSA key")
	}
	return key, nil
}

// ── Авторизация ───────────────────────────────────────────────────────────────

// envelope — общая обёртка всех ответов public-api.
type envelope struct {
	Code      string          `json:"code"`
	Message   string          `json:"message"`
	Body      json.RawMessage `json:"body"`
	Timestamp string          `json:"timestamp"`
}

type authBody struct {
	JWE string `json:"jwe"`
	TTL int    `json:"ttl"`
}

// jwe возвращает актуальный токен, обновляя его при необходимости.
func (c *Client) jwe() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.tokenExp) {
		return c.token, nil
	}

	timestamp := time.Now().Format(signatureTimeLayout)
	// Подпись — SHA512withRSA по конкатенации keyId + timestamp, base64.
	digest := sha512.Sum512([]byte(c.keyID + timestamp))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.privateKey, crypto.SHA512, digest[:])
	if err != nil {
		return "", fmt.Errorf("rustore auth sign: %w", err)
	}

	reqBody, _ := json.Marshal(map[string]string{
		"keyId":     c.keyID,
		"timestamp": timestamp,
		"signature": base64.StdEncoding.EncodeToString(sig),
	})

	resp, err := c.httpClient.Post(c.baseURL+"/public/auth", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("rustore auth: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("rustore auth read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("rustore auth: status %d: %s", resp.StatusCode, raw)
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", fmt.Errorf("rustore auth decode: %w", err)
	}
	if env.Code != "OK" {
		return "", fmt.Errorf("rustore auth: %s: %s", env.Code, env.Message)
	}
	var body authBody
	if err := json.Unmarshal(env.Body, &body); err != nil || body.JWE == "" {
		return "", errors.New("rustore auth: empty jwe in response")
	}

	ttl := time.Duration(body.TTL) * time.Second
	if ttl <= tokenTTLBuffer {
		ttl = 15 * time.Minute
	}
	c.token = body.JWE
	c.tokenExp = time.Now().Add(ttl - tokenTTLBuffer)
	return c.token, nil
}

// invalidateToken сбрасывает кэш JWE — вызывается при 401/403 от API.
func (c *Client) invalidateToken() {
	c.mu.Lock()
	c.token = ""
	c.mu.Unlock()
}

// ── Транспорт ─────────────────────────────────────────────────────────────────

// path строит полный URL, подставляя префикс песочницы при необходимости.
// Сегменты экранируются: purchaseId и коды продуктов приходят снаружи.
func (c *Client) path(format string, segments ...string) string {
	escaped := make([]any, len(segments))
	for i, s := range segments {
		escaped[i] = url.PathEscape(s)
	}
	prefix := "/public"
	if c.sandbox {
		prefix = "/public/sandbox"
	}
	return c.baseURL + prefix + fmt.Sprintf(format, escaped...)
}

// do выполняет запрос с JWE и одним ретраем при 401/403 (протухший токен).
func (c *Client) do(method, endpoint string) (*envelope, int, error) {
	env, status, err := c.doOnce(method, endpoint)
	if err == nil && (status == http.StatusUnauthorized || status == http.StatusForbidden) {
		c.invalidateToken()
		env, status, err = c.doOnce(method, endpoint)
	}
	return env, status, err
}

func (c *Client) doOnce(method, endpoint string) (*envelope, int, error) {
	token, err := c.jwe()
	if err != nil {
		return nil, 0, err
	}

	req, err := http.NewRequest(method, endpoint, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("rustore request: %w", err)
	}
	req.Header.Set("Public-Token", token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("rustore %s %s: %w", method, endpoint, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("rustore read: %w", err)
	}

	var env envelope
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &env); err != nil {
			return nil, resp.StatusCode, fmt.Errorf("rustore decode (%d): %s", resp.StatusCode, raw)
		}
	}
	return &env, resp.StatusCode, nil
}

// ── Subscription Data v4 ──────────────────────────────────────────────────────

// SubscriptionPurchase — тело ответа Subscription Data v4. Времена приходят
// строками с миллисекундами эпохи, поэтому разбираются отдельными методами.
type SubscriptionPurchase struct {
	StartTimeMillis      string `json:"startTimeMillis"`
	ExpiryTimeMillis     string `json:"expiryTimeMillis"`
	AutoRenewing         bool   `json:"autoRenewing"`
	DeveloperPayload     string `json:"developerPayload"`
	PriceCurrencyCode    string `json:"priceCurrencyCode"`
	PriceAmountMicros    string `json:"priceAmountMicros"`
	CountryCode          string `json:"countryCode"`
	PaymentState         *int   `json:"paymentState"`
	CancelReason         *int   `json:"cancelReason"`
	OrderID              string `json:"orderId"`
	AcknowledgementState *int   `json:"acknowledgementState"`
	ExternalAccountID    string `json:"externalAccountId"`
	PurchaseType         *int   `json:"purchaseType"`
}

// Paid — платёж прошёл: 1 = оплачено, 2 = бесплатный пробный период.
// Это и есть серверный аналог «статуса CONFIRMED» для подписок.
func (s *SubscriptionPurchase) Paid() bool {
	return s.PaymentState != nil && (*s.PaymentState == 1 || *s.PaymentState == 2)
}

// Acknowledged — доставка уже подтверждена (acknowledgementState == 1).
func (s *SubscriptionPurchase) Acknowledged() bool {
	return s.AcknowledgementState != nil && *s.AcknowledgementState == 1
}

// ExpiresAt — конец оплаченного периода.
func (s *SubscriptionPurchase) ExpiresAt() (time.Time, bool) {
	return millisToTime(s.ExpiryTimeMillis)
}

func millisToTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	ms, err := strconv.ParseInt(s, 10, 64)
	if err != nil || ms <= 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(ms).UTC(), true
}

// Subscription запрашивает данные подписки по конкретному продукту.
func (c *Client) Subscription(subscriptionID, purchaseID string) (*SubscriptionPurchase, error) {
	endpoint := c.path("/v4/subscription/%s/%s/%s", c.packageName, subscriptionID, purchaseID)
	env, status, err := c.do(http.MethodGet, endpoint)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("rustore subscription v4: status %d: %s", status, env.Message)
	}
	if env.Code != "OK" {
		return nil, fmt.Errorf("rustore subscription v4: %s: %s", env.Code, env.Message)
	}
	if len(env.Body) == 0 || string(env.Body) == "null" {
		return nil, ErrNotFound
	}

	var sub SubscriptionPurchase
	if err := json.Unmarshal(env.Body, &sub); err != nil {
		return nil, fmt.Errorf("rustore subscription v4 decode: %w", err)
	}
	return &sub, nil
}

// FindSubscription подбирает продукт для purchaseId перебором кодов из
// RUSTORE_SUBSCRIPTION_IDS: клиентский EntitlementApi.verify присылает только
// purchaseId, а v4 требует ещё и subscriptionId. Продуктов у приложения
// единицы, так что перебор дешевле, чем тащить productId через клиент.
func (c *Client) FindSubscription(purchaseID string) (string, *SubscriptionPurchase, error) {
	var lastErr error
	for _, id := range c.subscriptionIDs {
		sub, err := c.Subscription(id, purchaseID)
		if err == nil {
			return id, sub, nil
		}
		if !errors.Is(err, ErrNotFound) {
			lastErr = err
		}
	}
	if lastErr != nil {
		return "", nil, lastErr
	}
	return "", nil, ErrUnknownProduct
}

// ── Confirm Delivery v2 ───────────────────────────────────────────────────────

// Acknowledge подтверждает доставку подписки (Confirm Delivery v2).
// Метод идемпотентен по смыслу: повторный вызов для уже подтверждённой
// покупки безопасен.
func (c *Client) Acknowledge(subscriptionID, purchaseID string) error {
	endpoint := c.path("/v2/subscription/%s/%s/%s:acknowledge", c.packageName, subscriptionID, purchaseID)
	env, status, err := c.do(http.MethodPost, endpoint)
	if err != nil {
		return err
	}
	if status != http.StatusOK || (env.Code != "" && env.Code != "OK") {
		return fmt.Errorf("rustore acknowledge: status %d: %s", status, env.Message)
	}
	return nil
}

// ConfirmPurchase подтверждает разовую покупку. Для подписок RuStore ждёт
// Acknowledge, поэтому метод нужен только если появятся неподписочные товары.
func (c *Client) ConfirmPurchase(purchaseID string) error {
	if c.appID == "" {
		return errors.New("rustore confirm: RUSTORE_APP_ID is not set")
	}
	endpoint := c.path("/applications/%s/purchases/%s:confirm", c.appID, purchaseID)
	env, status, err := c.do(http.MethodPut, endpoint)
	if err != nil {
		return err
	}
	if status != http.StatusOK || (env.Code != "" && env.Code != "OK") {
		return fmt.Errorf("rustore confirm purchase: status %d: %s", status, env.Message)
	}
	return nil
}
