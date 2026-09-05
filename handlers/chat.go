package handlers

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"ai-server/cache"
	"ai-server/config"
	"ai-server/entitlement"
	"ai-server/llm"
	"ai-server/pocketbase"
)

type Handler struct {
	llm  llm.Provider
	c    *cache.Cache
	pb   *pocketbase.Client
	cfg  *config.Config
	ent  *entitlement.Service
	nKey []byte // ключ расшифровки вебхуков RuStore; nil — вебхук выключен
}

func New(
	provider llm.Provider,
	c *cache.Cache,
	pb *pocketbase.Client,
	cfg *config.Config,
	ent *entitlement.Service,
	notificationKey []byte,
) *Handler {
	return &Handler{llm: provider, c: c, pb: pb, cfg: cfg, ent: ent, nKey: notificationKey}
}

// ── Request / Response types ──────────────────────────────────────────────────

type ChatRequest struct {
	Message string `json:"message" binding:"required"`
}

type ChatResponse struct {
	Response json.RawMessage      `json:"response"`
	Cached   bool                 `json:"cached"`
	Quota    pocketbase.QuotaInfo `json:"quota"`
	// Tier — по какому тарифу обслужен запрос: клиенту это нужно, чтобы
	// показать, что премиум уже применился.
	Tier entitlement.Tier `json:"tier"`
}

type errResp struct {
	Error string                `json:"error"`
	Code  string                `json:"code"`
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

func ensureJSON(content string) json.RawMessage {
	content = stripCodeFence(strings.TrimSpace(content))
	if json.Valid([]byte(content)) {
		return json.RawMessage(content)
	}
	wrapped, _ := json.Marshal(map[string]string{"text": content})
	return wrapped
}

// stripCodeFence срезает markdown-обёртку ```json ... ``` вокруг ответа модели.
func stripCodeFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}

// bearerToken извлекает токен из заголовка Authorization: Bearer <token>.
func bearerToken(c *gin.Context) string {
	h := c.GetHeader("Authorization")
	const prefix = "Bearer "
	if strings.HasPrefix(h, prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

// resolveUser верифицирует PB user JWT и возвращает id пользователя.
// При ошибке сам пишет ответ и возвращает false.
func (h *Handler) resolveUser(c *gin.Context) (string, bool) {
	authToken := bearerToken(c)
	if authToken == "" {
		c.JSON(http.StatusUnauthorized, errResp{
			Error: "Authorization header required",
			Code:  "MISSING_AUTH",
		})
		return "", false
	}

	userID, err := h.pb.VerifyUser(authToken)
	if err != nil {
		if errors.Is(err, pocketbase.ErrUnauthorized) {
			c.JSON(http.StatusUnauthorized, errResp{
				Error: "invalid or expired token",
				Code:  "UNAUTHORIZED",
			})
			return "", false
		}
		log.Printf("pb verify user error: %v", err)
		c.JSON(http.StatusInternalServerError, errResp{
			Error: "internal error",
			Code:  "INTERNAL_ERROR",
		})
		return "", false
	}
	return userID, true
}

// resolveTokenRecord верифицирует PB user JWT и находит связанную запись tokens,
// создавая её при первом обращении пользователя.
func (h *Handler) resolveTokenRecord(c *gin.Context) (string, *pocketbase.TokenRecord, bool) {
	userID, ok := h.resolveUser(c)
	if !ok {
		return "", nil, false
	}

	tokenRec, err := h.pb.EnsureTokenByUser(userID)
	if err != nil {
		if errors.Is(err, pocketbase.ErrTokenNotFound) {
			c.JSON(http.StatusForbidden, errResp{
				Error: "no token record found for this user",
				Code:  "TOKEN_NOT_FOUND",
			})
			return "", nil, false
		}
		log.Printf("pb ensure token error for user %s: %v", userID, err)
		c.JSON(http.StatusInternalServerError, errResp{
			Error: "internal error",
			Code:  "INTERNAL_ERROR",
		})
		return "", nil, false
	}

	return userID, tokenRec, true
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

	userID, tokenRec, ok := h.resolveTokenRecord(c)
	if !ok {
		return
	}

	// Права читаются из кэша в PocketBase (без похода в RuStore) — это
	// горячий путь. Тариф решает и модель, и дневной лимит.
	ent := h.ent.Current(userID)
	plan := h.ent.Plans().For(ent.Tier)

	// Кэш — не тратим квоту, модель не вызываем. Разделён по модели, чтобы
	// ответ улучшенной модели не утёк бесплатному пользователю.
	if cached, ok := h.c.Get(plan.Model, msg); ok {
		qi, _ := h.pb.CheckQuota(tokenRec, plan.QuotaPerMinute, plan.QuotaPerDay)
		c.JSON(http.StatusOK, ChatResponse{Response: cached, Cached: true, Quota: qi, Tier: ent.Tier})
		return
	}

	// Проверяем квоту.
	qi, err := h.pb.CheckQuota(tokenRec, plan.QuotaPerMinute, plan.QuotaPerDay)
	if err != nil {
		code := "QUOTA_EXCEEDED"
		if errors.Is(err, pocketbase.ErrRateLimitMinute) {
			code = "RATE_LIMIT_MINUTE"
		} else if errors.Is(err, pocketbase.ErrRateLimitDay) {
			code = "RATE_LIMIT_DAY"
		}
		c.JSON(http.StatusTooManyRequests, errResp{Error: err.Error(), Code: code, Quota: &qi})
		return
	}

	// Запрос к модели. Подставляем текущее время, чтобы модель могла
	// переводить относительные даты («завтра») в абсолютные.
	systemPrompt := strings.ReplaceAll(
		h.cfg.SystemPrompt,
		"{{CURRENT_TIME}}",
		time.Now().Format(time.RFC3339),
	)
	content, err := h.llm.Chat(plan.Model, systemPrompt, msg)
	if err != nil {
		if errors.Is(err, llm.ErrTooManyRequests) {
			c.JSON(http.StatusServiceUnavailable, errResp{
				Error: "upstream rate limit reached, try again later",
				Code:  "UPSTREAM_RATE_LIMIT",
			})
			return
		}
		log.Printf("llm error: %v", err)
		c.JSON(http.StatusInternalServerError, errResp{
			Error: "llm error: " + err.Error(),
			Code:  "LLM_ERROR",
		})
		return
	}

	if strings.TrimSpace(content) == "" {
		c.JSON(http.StatusInternalServerError, errResp{
			Error: "empty response from model",
			Code:  "EMPTY_RESPONSE",
		})
		return
	}

	// Списываем квоту.
	qi, err = h.pb.ConsumeQuota(tokenRec, plan.QuotaPerMinute, plan.QuotaPerDay)
	if err != nil {
		log.Printf("pb consume quota error for token %s: %v", tokenRec.ID, err)
	}

	responseJSON := ensureJSON(content)
	if err := h.c.Set(plan.Model, msg, responseJSON); err != nil {
		log.Printf("cache set error: %v", err)
	}

	c.JSON(http.StatusOK, ChatResponse{Response: responseJSON, Cached: false, Quota: qi, Tier: ent.Tier})
}

func (h *Handler) GetQuota(c *gin.Context) {
	userID, tokenRec, ok := h.resolveTokenRecord(c)
	if !ok {
		return
	}
	plan := h.ent.Plans().For(h.ent.Current(userID).Tier)
	qi, _ := h.pb.CheckQuota(tokenRec, plan.QuotaPerMinute, plan.QuotaPerDay)
	c.JSON(http.StatusOK, qi)
}

func (h *Handler) Health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
