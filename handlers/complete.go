package handlers

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"ai-server/advice"
	"ai-server/entitlement"
	"ai-server/llm"
	"ai-server/metrics"
	"ai-server/pocketbase"
)

// chatJob — то, что отличается между версиями эндпоинта. Всё остальное
// (кэш, квота, вызов модели, нормализация, логи) общее, поэтому живёт здесь.
type chatJob struct {
	version      string // "v1" | "v2" — попадает в ключ кэша и в логи
	systemPrompt string
	messages     []llm.Message
	cacheKey     string
}

func (h *Handler) complete(c *gin.Context, job chatJob) {
	userID, tokenRec, ok := h.resolveTokenRecord(c)
	if !ok {
		return
	}

	// Права читаются из кэша в PocketBase (без похода в RuStore) — это
	// горячий путь. Тариф решает и модель, и дневной лимит.
	ent := h.ent.Current(userID)
	plan := h.ent.Plans().For(ent.Tier)

	// Namespace кэша: модель + версия промта + версия эндпоинта. Версия
	// промта обязательна — без неё после правки промта пользователи ещё
	// CACHE_TTL получают ответы по старой версии, и сравнить версии не на чем.
	ns := plan.Model + "\x00" + h.cfg.PromptVersion + "\x00" + job.version

	// Кэш — не тратим квоту, модель не вызываем. Разделён по модели, чтобы
	// ответ улучшенной модели не утёк бесплатному пользователю.
	if cached, hit := h.c.Get(ns, job.cacheKey); hit {
		metrics.ChatAnswer(job.version, string(ent.Tier), "cache")
		qi, _ := h.pb.CheckQuota(tokenRec, plan.QuotaPerMinute, plan.QuotaPerDay)
		h.respond(c, cached, true, qi, ent.Tier)
		return
	}

	qi, err := h.pb.CheckQuota(tokenRec, plan.QuotaPerMinute, plan.QuotaPerDay)
	if err != nil {
		code := "QUOTA_EXCEEDED"
		if errors.Is(err, pocketbase.ErrRateLimitMinute) {
			code = "RATE_LIMIT_MINUTE"
		} else if errors.Is(err, pocketbase.ErrRateLimitDay) {
			code = "RATE_LIMIT_DAY"
		}
		metrics.ChatFailure(job.version, code)
		c.JSON(http.StatusTooManyRequests, errResp{Error: err.Error(), Code: code, Quota: &qi})
		return
	}

	answer, retried, err := h.ask(job, plan.Model)
	if err != nil {
		if errors.Is(err, llm.ErrTooManyRequests) {
			metrics.ChatFailure(job.version, "UPSTREAM_RATE_LIMIT")
			c.JSON(http.StatusServiceUnavailable, errResp{
				Error: "upstream rate limit reached, try again later",
				Code:  "UPSTREAM_RATE_LIMIT",
			})
			return
		}
		slog.Error("llm: no usable answer", "endpoint", job.version, "model", plan.Model,
			"prompt", h.cfg.PromptVersion, "err", err)
		metrics.ChatFailure(job.version, "LLM_ERROR")
		c.JSON(http.StatusInternalServerError, errResp{
			Error: "llm error: " + err.Error(),
			Code:  "LLM_ERROR",
		})
		return
	}

	// Красные флаги: уровень только поднимается. Понижать автоматически
	// нельзя ни при каких условиях — цена ошибки несимметрична.
	hits, raised := h.rf.Escalate(answer)
	if raised {
		slog.Info("red flags: raised urgency", "urgency", answer.Urgency, "markers", hits,
			"endpoint", job.version, "user", userID)
		metrics.RedFlagEscalation(job.version, answer.Urgency)
	}

	// Списываем квоту.
	qi, err = h.pb.ConsumeQuota(tokenRec, plan.QuotaPerMinute, plan.QuotaPerDay)
	if err != nil {
		slog.Error("pb: consume quota", "token", tokenRec.ID, "err", err)
	}

	responseJSON, err := json.Marshal(answer)
	if err != nil {
		slog.Error("chat: marshal answer", "err", err)
		metrics.ChatFailure(job.version, "INTERNAL_ERROR")
		c.JSON(http.StatusInternalServerError, errResp{Error: "internal error", Code: "INTERNAL_ERROR"})
		return
	}
	// В кэш кладём уже нормализованный и, если надо, поднятый по красным
	// флагам ответ: urgency не должен расходиться с текстом.
	if err := h.c.Set(ns, job.cacheKey, responseJSON); err != nil {
		slog.Error("cache: set", "err", err)
	}

	slog.Info("chat ok", "endpoint", job.version, "user", userID, "tier", ent.Tier,
		"model", plan.Model, "prompt", h.cfg.PromptVersion,
		"chars", utf8.RuneCountInString(answer.Response), "events", len(answer.Events),
		"entries", len(answer.Entries), "urgency", answer.Urgency, "red_flags", len(hits),
		"escalated", raised, "retried", retried)
	metrics.ChatAnswer(job.version, string(ent.Tier), "llm")

	h.respond(c, responseJSON, false, qi, ent.Tier)
}

func (h *Handler) respond(
	c *gin.Context,
	body json.RawMessage,
	cached bool,
	qi pocketbase.QuotaInfo,
	tier entitlement.Tier,
) {
	c.JSON(http.StatusOK, ChatResponse{
		Response:      body,
		Cached:        cached,
		Quota:         qi,
		Tier:          tier,
		PromptVersion: h.cfg.PromptVersion,
	})
}

// ask спрашивает модель и разбирает ответ. Невалидный JSON — повод повторить
// запрос один раз, а не отдать пользователю мусор: строгая схема уменьшает
// вероятность срыва, но не исключает его.
//
// Отдельно обрабатывается обрыв по лимиту токенов: строгая схема заставляет
// модель выдать все поля разом, и на длинном ответе JSON обрывается на
// полуслове. Повторять такое с тем же потолком бессмысленно — повтор идёт с
// удвоенным.
func (h *Handler) ask(job chatJob, model string) (answer *advice.Answer, retried bool, err error) {
	req := llm.Request{
		Model:        model,
		SystemPrompt: job.systemPrompt,
		Messages:     job.messages,
		SchemaName:   "pet_assistant_answer",
	}
	if h.llm.SupportsSchema() {
		req.JSONSchema = advice.Schema()
	}

	for attempt := 1; attempt <= 2; attempt++ {
		var resp llm.Response
		resp, err = h.llm.Chat(req)
		if err != nil {
			// Rate limit повторять бессмысленно — отдаём сразу.
			if errors.Is(err, llm.ErrTooManyRequests) {
				return nil, attempt > 1, err
			}
			slog.Warn("llm: call failed", "attempt", attempt, "endpoint", job.version, "err", err)
			continue
		}

		answer, err = advice.Parse(resp.Content)
		if err == nil {
			if resp.Truncated() {
				// JSON дочитался, но ответ всё равно оборвали: текст в пузыре
				// закончится на полуслове. Пользователю отдаём — это лучше,
				// чем ошибка, — но в логе это повод поднять лимит.
				slog.Warn("llm: answer hit the token ceiling but parsed",
					"endpoint", job.version, "max_tokens", h.cfg.YandexMaxTokens)
			}
			return answer, attempt > 1, nil
		}

		slog.Warn("llm: invalid answer", "attempt", attempt, "endpoint", job.version,
			"finish_reason", resp.FinishReason, "chars", utf8.RuneCountInString(resp.Content),
			"err", err, "raw", previewJSON(resp.Content))
		metrics.LLMInvalidAnswer(job.version)

		// Обрыв по лимиту — не случайность, а нехватка бюджета: второй
		// одинаковый запрос упрётся в тот же потолок.
		if resp.Truncated() {
			req.MaxTokens = doubleBudget(req.MaxTokens, h.cfg.YandexMaxTokens)
			slog.Warn("llm: retrying with a doubled token budget, raise YANDEX_MAX_TOKENS to avoid the extra call",
				"max_tokens", req.MaxTokens)
		}
	}
	return nil, true, err
}

// doubleBudget удваивает потолок токенов для повтора, отталкиваясь от
// значения из конфигурации, если на этой попытке он ещё не задавался.
func doubleBudget(current, fallback int) int {
	if current <= 0 {
		current = fallback
	}
	if current <= 0 {
		current = 700
	}
	return current * 2
}

// previewJSON показывает начало и конец ответа модели: по обрезанному хвосту
// сразу видно, оборвался JSON или пришёл пустым.
func previewJSON(s string) string {
	const edge = 200
	switch r := []rune(s); {
	case len(r) == 0:
		return "<empty>"
	case len(r) <= 2*edge:
		return strconv.Quote(s)
	default:
		return strconv.Quote(string(r[:edge])) + " … " + strconv.Quote(string(r[len(r)-edge:]))
	}
}
