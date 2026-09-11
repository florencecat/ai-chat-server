package handlers

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"ai-server/advice"
	"ai-server/entitlement"
	"ai-server/llm"
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
		c.JSON(http.StatusTooManyRequests, errResp{Error: err.Error(), Code: code, Quota: &qi})
		return
	}

	answer, retried, err := h.ask(job, plan.Model)
	if err != nil {
		if errors.Is(err, llm.ErrTooManyRequests) {
			c.JSON(http.StatusServiceUnavailable, errResp{
				Error: "upstream rate limit reached, try again later",
				Code:  "UPSTREAM_RATE_LIMIT",
			})
			return
		}
		log.Printf("llm error (endpoint=%s model=%s prompt=%s): %v",
			job.version, plan.Model, h.cfg.PromptVersion, err)
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
		log.Printf("red flags: raised urgency to %s, markers=%v (endpoint=%s user=%s)",
			answer.Urgency, hits, job.version, userID)
	}

	// Списываем квоту.
	qi, err = h.pb.ConsumeQuota(tokenRec, plan.QuotaPerMinute, plan.QuotaPerDay)
	if err != nil {
		log.Printf("pb consume quota error for token %s: %v", tokenRec.ID, err)
	}

	responseJSON, err := json.Marshal(answer)
	if err != nil {
		log.Printf("marshal answer: %v", err)
		c.JSON(http.StatusInternalServerError, errResp{Error: "internal error", Code: "INTERNAL_ERROR"})
		return
	}
	// В кэш кладём уже нормализованный и, если надо, поднятый по красным
	// флагам ответ: urgency не должен расходиться с текстом.
	if err := h.c.Set(ns, job.cacheKey, responseJSON); err != nil {
		log.Printf("cache set error: %v", err)
	}

	log.Printf("chat ok endpoint=%s user=%s tier=%s model=%s prompt=%s chars=%d events=%d entries=%d urgency=%q red_flags=%d escalated=%v retried=%v",
		job.version, userID, ent.Tier, plan.Model, h.cfg.PromptVersion,
		utf8.RuneCountInString(answer.Response), len(answer.Events), len(answer.Entries),
		answer.Urgency, len(hits), raised, retried)

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

	for attempt := 0; attempt < 2; attempt++ {
		var content string
		content, err = h.llm.Chat(req)
		if err != nil {
			// Rate limit повторять бессмысленно — отдаём сразу.
			if errors.Is(err, llm.ErrTooManyRequests) {
				return nil, attempt > 0, err
			}
			log.Printf("llm call failed (attempt %d, endpoint=%s): %v", attempt+1, job.version, err)
			continue
		}
		answer, err = advice.Parse(content)
		if err == nil {
			return answer, attempt > 0, nil
		}
		log.Printf("llm invalid answer (attempt %d, endpoint=%s): %v", attempt+1, job.version, err)
	}
	return nil, true, err
}
