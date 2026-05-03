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
	"ai-server/quota"
)

type Handler struct {
	gc  *gigachat.Client
	c   *cache.Cache
	q   *quota.Manager
	cfg *config.Config
}

func New(gc *gigachat.Client, c *cache.Cache, q *quota.Manager, cfg *config.Config) *Handler {
	return &Handler{gc: gc, c: c, q: q, cfg: cfg}
}

type ChatRequest struct {
	UserID  string `json:"user_id" binding:"required"`
	Message string `json:"message" binding:"required"`
}

type ChatResponse struct {
	Response json.RawMessage `json:"response"`
	Cached   bool            `json:"cached"`
	Quota    quota.Info      `json:"quota"`
}

type errResp struct {
	Error string     `json:"error"`
	Code  string     `json:"code"`
	Quota *quota.Info `json:"quota,omitempty"`
}

// controlCharsRe matches non-printable ASCII except \t and \n.
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

// ensureJSON returns a valid JSON value. If the content is already JSON, it is
// returned as-is; otherwise it is wrapped in {"text": "..."}.
func ensureJSON(content string) json.RawMessage {
	content = strings.TrimSpace(content)
	if json.Valid([]byte(content)) {
		return json.RawMessage(content)
	}
	wrapped, _ := json.Marshal(map[string]string{"text": content})
	return wrapped
}

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

	// Cached responses bypass quota — we're not hitting GigaChat.
	if cached, ok := h.c.Get(msg); ok {
		c.JSON(http.StatusOK, ChatResponse{
			Response: cached,
			Cached:   true,
			Quota:    h.q.Info(req.UserID),
		})
		return
	}

	if err := h.q.Check(req.UserID); err != nil {
		code := "QUOTA_EXCEEDED"
		if errors.Is(err, quota.ErrRateLimitMinute) {
			code = "RATE_LIMIT_MINUTE"
		} else if errors.Is(err, quota.ErrRateLimitDay) {
			code = "RATE_LIMIT_DAY"
		}
		qi := h.q.Info(req.UserID)
		c.JSON(http.StatusTooManyRequests, errResp{
			Error: err.Error(),
			Code:  code,
			Quota: &qi,
		})
		return
	}

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

	if err := h.q.Consume(req.UserID); err != nil {
		log.Printf("quota consume error for %s: %v", req.UserID, err)
	}

	responseJSON := ensureJSON(gcResp.Choices[0].Message.Content)

	if err := h.c.Set(msg, responseJSON); err != nil {
		log.Printf("cache set error: %v", err)
	}

	c.JSON(http.StatusOK, ChatResponse{
		Response: responseJSON,
		Cached:   false,
		Quota:    h.q.Info(req.UserID),
	})
}

func (h *Handler) GetQuota(c *gin.Context) {
	userID := c.Param("user_id")
	if userID == "" {
		c.JSON(http.StatusBadRequest, errResp{Error: "user_id required", Code: "INVALID_REQUEST"})
		return
	}
	c.JSON(http.StatusOK, h.q.Info(userID))
}

func (h *Handler) Health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
