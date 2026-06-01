package handlers

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"ai-server/cache"
	"ai-server/config"
	"ai-server/gigachat"
	"ai-server/pocketbase"
)

type Handler struct {
	gc  *gigachat.Client
	c   *cache.Cache
	pb  *pocketbase.Client
	cfg *config.Config
}

func New(gc *gigachat.Client, c *cache.Cache, pb *pocketbase.Client, cfg *config.Config) *Handler {
	return &Handler{gc: gc, c: c, pb: pb, cfg: cfg}
}

// ── Request / Response types ──────────────────────────────────────────────────

type ChatRequest struct {
	Token   string `json:"token"   binding:"required"`
	Message string `json:"message" binding:"required"`
}

type ChatResponse struct {
	Response json.RawMessage      `json:"response"`
	Cached   bool                 `json:"cached"`
	Quota    pocketbase.QuotaInfo `json:"quota"`
}

type errResp struct {
	Error string               `json:"error"`
	Code  string               `json:"code"`
	Quota *pocketbase.QuotaInfo `json:"quota,omitempty"`
}

// ── Helpers ───────────────────────────────────────────────────────────────────

var controlCharsRe = regexp.MustCompile(`[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]+`)

func (h *Handler) sanitize(msg string) string {
	msg = controlCharsRe.ReplaceAllString(msg, "")
	msg = strings.TrimSpace(msg)
	if utf8.RuneCountInString(msg) > h.cfg.MaxMessageLen {
		runes := []rune(msg)
		msg = string(runes[:h.cfg.MaxMessageLen])
	}
	return msg
}

// ensureJSON возвращает валидный JSON: если ответ уже JSON — возвращает as-is,
// иначе оборачивает в {"text": "..."}.
func ensureJSON(content string) json.RawMessage {
	content = strings.TrimSpace(content)
	if json.Valid([]byte(content)) {
		return json.RawMessage(content)
	}
	wrapped, _ := json.Marshal(map[string]string{"text": content})
	return wrapped
}

// ── Handlers ──────────────────────────────────────────────────────────────────

func (h *Handler) Chat(c *gin.Context) {
	var req ChatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, errResp{
			Error: "invalid request: " + err.Error(),
			Code:  "INVALID_REQUEST",
		})
		return
	}

	msg := h.sanitize(req.Message)
	if msg == "" {
		c.JSON(http.StatusBadRequest, errResp{
			Error: "message is empty after sanitization",
			Code:  "EMPTY_MESSAGE",
		})
		return
	}

	// Находим токен в PocketBase.
	tokenRec, err := h.pb.FindToken(req.Token)
	if err != nil {
		if errors.Is(err, pocketbase.ErrTokenNotFound) {
			c.JSON(http.StatusUnauthorized, errResp{
				Error: "invalid token",
				Code:  "INVALID_TOKEN",
			})
			return
		}
		log.Printf("pb find token error: %v", err)
		c.JSON(http.StatusInternalServerError, errResp{
			Error: "internal error",
			Code:  "INTERNAL_ERROR",
		})
		return
	}

	// Кэшированные ответы не считаются за запрос к GigaChat — квоту не тратим.
	if cached, ok := h.c.Get(msg); ok {
		qi, _ := h.pb.CheckQuota(tokenRec, h.cfg.QuotaPerMinute, h.cfg.QuotaPerDay)
		c.JSON(http.StatusOK, ChatResponse{
			Response: cached,
			Cached:   true,
			Quota:    qi,
		})
		return
	}

	// Проверяем квоту.
	qi, err := h.pb.CheckQuota(tokenRec, h.cfg.QuotaPerMinute, h.cfg.QuotaPerDay)
	if err != nil {
		code := "QUOTA_EXCEEDED"
		if errors.Is(err, pocketbase.ErrRateLimitMinute) {
			code = "RATE_LIMIT_MINUTE"
		} else if errors.Is(err, pocketbase.ErrRateLimitDay) {
			code = "RATE_LIMIT_DAY"
		}
		c.JSON(http.StatusTooManyRequests, errResp{
			Error: err.Error(),
			Code:  code,
			Quota: &qi,
		})
		return
	}

	// Запрос к GigaChat.
	messages := []gigachat.Message{
		{Role: "system", Content: h.cfg.SystemPrompt},
		{Role: "user", Content: msg},
	}
	gcResp, err := h.gc.Chat(messages)
	if err != nil {
		if errors.Is(err, gigachat.ErrTooManyRequests) {
			c.JSON(http.StatusServiceUnavailable, errResp{
				Error: "upstream rate limit reached, try again later",
				Code:  "UPSTREAM_RATE_LIMIT",
			})
			return
		}
		log.Printf("gigachat error: %v", err)
		c.JSON(http.StatusInternalServerError, errResp{
			Error: "gigachat error: " + err.Error(),
			Code:  "GIGACHAT_ERROR",
		})
		return
	}

	if len(gcResp.Choices) == 0 {
		c.JSON(http.StatusInternalServerError, errResp{
			Error: "no choices in gigachat response",
			Code:  "EMPTY_RESPONSE",
		})
		return
	}

	// Списываем квоту в PocketBase.
	qi, err = h.pb.ConsumeQuota(tokenRec, h.cfg.QuotaPerMinute, h.cfg.QuotaPerDay)
	if err != nil {
		log.Printf("pb consume quota error for token %s: %v", tokenRec.ID, err)
	}

	responseJSON := ensureJSON(gcResp.Choices[0].Message.Content)

	if err := h.c.Set(msg, responseJSON); err != nil {
		log.Printf("cache set error: %v", err)
	}

	c.JSON(http.StatusOK, ChatResponse{
		Response: responseJSON,
		Cached:   false,
		Quota:    qi,
	})
}

func (h *Handler) GetQuota(c *gin.Context) {
	token := c.Param("token")
	if token == "" {
		c.JSON(http.StatusBadRequest, errResp{Error: "token required", Code: "INVALID_REQUEST"})
		return
	}
	tokenRec, err := h.pb.FindToken(token)
	if err != nil {
		if errors.Is(err, pocketbase.ErrTokenNotFound) {
			c.JSON(http.StatusNotFound, errResp{Error: "token not found", Code: "INVALID_TOKEN"})
			return
		}
		c.JSON(http.StatusInternalServerError, errResp{Error: "internal error", Code: "INTERNAL_ERROR"})
		return
	}
	qi, _ := h.pb.CheckQuota(tokenRec, h.cfg.QuotaPerMinute, h.cfg.QuotaPerDay)
	c.JSON(http.StatusOK, qi)
}

func (h *Handler) Health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
