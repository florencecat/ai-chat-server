package gigachat

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
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

var ErrTooManyRequests = errors.New("too many requests to GigaChat")

type Client struct {
	cfg        *config.Config
	httpClient *http.Client

	mu          sync.RWMutex
	accessToken string
	tokenExpiry time.Time
}

func NewClient(cfg *config.Config) *Client {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: cfg.GigaChatSkipTLS}, //nolint:gosec
	}
	return &Client{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout:   30 * time.Second,
			Transport: transport,
		},
	}
}

func (c *Client) getToken() (string, error) {
	c.mu.RLock()
	if c.accessToken != "" && time.Now().Before(c.tokenExpiry) {
		token := c.accessToken
		c.mu.RUnlock()
		return token, nil
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()

	// Double-check after acquiring write lock.
	if c.accessToken != "" && time.Now().Before(c.tokenExpiry) {
		return c.accessToken, nil
	}
	return c.refreshToken()
}

func (c *Client) invalidateToken() {
	c.mu.Lock()
	c.accessToken = ""
	c.mu.Unlock()
}

func (c *Client) refreshToken() (string, error) {
	data := url.Values{}
	data.Set("scope", c.cfg.GigaChatScope)

	req, err := http.NewRequest("POST", c.cfg.GigaChatAuthURL, strings.NewReader(data.Encode()))
	if err != nil {
		return "", fmt.Errorf("create auth request: %w", err)
	}

	authStr := base64.StdEncoding.EncodeToString(
		[]byte(c.cfg.GigaChatClientID + ":" + c.cfg.GigaChatClientSecret),
	)
	req.Header.Set("Authorization", "Basic "+authStr)
	req.Header.Set("RqUID", newUUID())
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("auth request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read auth response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("auth failed %d: %s", resp.StatusCode, body)
	}

	var tok TokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", fmt.Errorf("parse auth response: %w", err)
	}

	c.accessToken = tok.AccessToken
	// expires_at is milliseconds epoch; subtract 60s buffer.
	c.tokenExpiry = time.UnixMilli(tok.ExpiresAt).Add(-60 * time.Second)

	return c.accessToken, nil
}

// Chat sends messages to GigaChat. Retries once on 401 (stale token).
func (c *Client) Chat(messages []Message) (*ChatResponse, error) {
	token, err := c.getToken()
	if err != nil {
		return nil, fmt.Errorf("get token: %w", err)
	}

	resp, status, body, err := c.doChat(token, messages)
	if err != nil {
		return nil, err
	}

	// On 401 refresh token and retry once.
	if status == http.StatusUnauthorized {
		c.invalidateToken()
		token, err = c.getToken()
		if err != nil {
			return nil, fmt.Errorf("refresh token: %w", err)
		}
		resp, status, body, err = c.doChat(token, messages)
		if err != nil {
			return nil, err
		}
	}

	if status == http.StatusTooManyRequests {
		return nil, ErrTooManyRequests
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("chat failed %d: %s", status, body)
	}

	return resp, nil
}

func (c *Client) doChat(token string, messages []Message) (*ChatResponse, int, []byte, error) {
	chatReq := ChatRequest{
		Model:    c.cfg.GigaChatModel,
		Messages: messages,
		Stream:   false,
	}
	reqBody, err := json.Marshal(chatReq)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", c.cfg.GigaChatBaseURL+"/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		return nil, 0, nil, fmt.Errorf("create chat request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("chat request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, nil, fmt.Errorf("read chat response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, respBody, nil
	}

	var chatResp ChatResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return nil, resp.StatusCode, respBody, fmt.Errorf("parse chat response: %w", err)
	}
	return &chatResp, resp.StatusCode, respBody, nil
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
