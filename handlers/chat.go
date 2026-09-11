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

	"ai-server/advice"
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
	rf   *advice.RedFlags
	nKey []byte // ключ расшифровки вебхуков RuStore; nil — вебхук выключен
}

func New(
	provider llm.Provider,
	c *cache.Cache,
	pb *pocketbase.Client,
	cfg *config.Config,
	ent *entitlement.Service,
	redFlags *advice.RedFlags,
	notificationKey []byte,
) *Handler {
	return &Handler{llm: provider, c: c, pb: pb, cfg: cfg, ent: ent, rf: redFlags, nKey: notificationKey}
}

// ── Request / Response types ──────────────────────────────────────────────────

// ChatRequest — тело старого POST /chat. Не меняется: установленные сборки
// шлют весь диалог одной строкой в message.
type ChatRequest struct {
	Message string `json:"message" binding:"required"`
}

// ChatV2Request — тело POST /v2/chat.
//
// Системную инструкцию сервер собирает сам: пока формат ответа задаётся из
// APK, поменять поведение ассистента можно только релизом приложения.
// Клиент присылает историю, контекст питомца и своё время.
type ChatV2Request struct {
	Messages []ChatMessage `json:"messages"`
	// Message — запасной путь для одного сообщения без истории.
	Message string `json:"message"`

	PetID      string `json:"pet_id"`
	PetContext string `json:"pet_context"`

	// ClientTime — RFC 3339 со смещением пользователя, TZ — имя пояса
	// («Europe/Moscow»). Оба необязательны: без них берётся время сервера.
	ClientTime string `json:"client_time"`
	TZ         string `json:"tz"`
}

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatResponse struct {
	Response json.RawMessage      `json:"response"`
	Cached   bool                 `json:"cached"`
	Quota    pocketbase.QuotaInfo `json:"quota"`
	// Tier — по какому тарифу обслужен запрос: клиенту это нужно, чтобы
	// показать, что премиум уже применился.
	Tier entitlement.Tier `json:"tier"`
	// PromptVersion — версия промта, которой получен ответ. Нужна, чтобы
	// сравнивать версии промта на живом трафике; клиент её игнорирует.
	PromptVersion string `json:"prompt_version,omitempty"`
}

type errResp struct {
	Error string                `json:"error"`
	Code  string                `json:"code"`
	Quota *pocketbase.QuotaInfo `json:"quota,omitempty"`
}

// ── Helpers ───────────────────────────────────────────────────────────────────

var controlCharsRe = regexp.MustCompile("[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]+")

func (h *Handler) sanitize(msg string) string {
	msg = controlCharsRe.ReplaceAllString(strings.TrimSpace(msg), "")
	return truncateRunes(msg, h.cfg.MaxMessageLen)
}

func truncateRunes(s string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(s) <= limit {
		return s
	}
	return string([]rune(s)[:limit])
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

// Chat — POST /chat. Контракт запроса не меняется; в ответе появились
// prompt_version и поля Фазы 4 внутри response. Клиент их уже разбирает, а
// более старые сборки игнорируют неизвестные ключи.
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

	h.complete(c, chatJob{
		version:      "v1",
		systemPrompt: h.systemPromptV1(),
		messages:     []llm.Message{{Role: "user", Content: msg}},
		cacheKey:     msg,
	})
}

// ChatV2 — POST /v2/chat.
func (h *Handler) ChatV2(c *gin.Context) {
	var req ChatV2Request
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, errResp{
			Error: "invalid request: " + err.Error(),
			Code:  "INVALID_REQUEST",
		})
		return
	}

	messages, last := h.buildMessages(req)
	if last == "" {
		c.JSON(http.StatusBadRequest, errResp{
			Error: "no user message in request",
			Code:  "EMPTY_MESSAGE",
		})
		return
	}

	now := userLocation(req.ClientTime, req.TZ)
	h.complete(c, chatJob{
		version:      "v2",
		systemPrompt: h.systemPromptV2(now, req.TZ, h.sanitize(req.PetContext)),
		messages:     messages,
		// Ключ кэша — последнее сообщение пользователя плюс питомец, а не всё
		// тело запроса: иначе любое новое сообщение в треде или изменившийся
		// вес в профиле делают запрос уникальным и кэш не работает вовсе.
		cacheKey: req.PetID + "\x00" + last,
	})
}

// buildMessages приводит историю к виду для провайдера: системные сообщения
// от клиента отбрасываются (промт серверный), лимит MaxMessageLen применяется
// к каждому сообщению по отдельности — то есть длинный тред больше не режет
// самое свежее сообщение, — а история ограничивается числом сообщений и
// суммарной длиной.
//
// Возвращает историю и текст последнего сообщения пользователя; пустой
// last означает, что отвечать не на что.
func (h *Handler) buildMessages(req ChatV2Request) (out []llm.Message, last string) {
	src := req.Messages
	if len(src) == 0 && strings.TrimSpace(req.Message) != "" {
		src = []ChatMessage{{Role: "user", Content: req.Message}}
	}

	kept := make([]llm.Message, 0, len(src))
	for _, m := range src {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role != "user" && role != "assistant" {
			continue // system от клиента игнорируем: промт собирает сервер
		}
		content := h.sanitize(m.Content)
		if content == "" {
			continue
		}
		kept = append(kept, llm.Message{Role: role, Content: content})
	}
	for i := len(kept) - 1; i >= 0; i-- {
		if kept[i].Role == "user" {
			last = kept[i].Content
			break
		}
	}
	if last == "" {
		return nil, ""
	}

	// Историю режем с начала, а не с конца: обрезка с конца выбросила бы
	// ровно то сообщение, на которое надо ответить.
	if n := h.cfg.MaxHistoryMessages; n > 0 && len(kept) > n {
		kept = kept[len(kept)-n:]
	}
	// Суммарная длина считается с конца; сообщение, которое переполнило
	// лимит, само в историю уже не входит. Самое свежее сообщение остаётся
	// всегда — даже если оно одно длиннее лимита.
	if limit := h.cfg.MaxHistoryChars; limit > 0 {
		total, cut := 0, 0
		for i := len(kept) - 1; i >= 0; i-- {
			total += utf8.RuneCountInString(kept[i].Content)
			if total > limit && i < len(kept)-1 {
				cut = i + 1
				break
			}
		}
		kept = kept[cut:]
	}
	return kept, last
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
