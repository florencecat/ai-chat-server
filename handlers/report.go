package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"ai-server/entitlement"
	"ai-server/metrics"
	"ai-server/pocketbase"
	"ai-server/report"
)

// defaultReportTZ — пояс, если клиент не прислал tz. Аудитория российская, а
// время сервера в контейнере — UTC: без этого «сегодня» в отчёте с 00:00 до
// 03:00 по Москве было бы вчерашним.
const defaultReportTZ = "Europe/Moscow"

// VetCard — GET /report/vet-card/:pet_id, PDF «Карточка для ветеринара».
// Только для премиума.
//
// Query: tz (IANA, «Europe/Moscow»), weight_history (по умолчанию 1),
// vet_notes (по умолчанию 0).
//
// Повторная сборка по тем же данным не происходит: ключ отчёта — отпечаток
// пар (id, updated) всех записей карточки плюс день. Последняя версия лежит в
// pdf_reports; если данные изменились, а лимит на генерацию исчерпан,
// отдаётся она с X-Report-Stale.
func (h *Handler) VetCard(c *gin.Context) {
	userID, ok := h.resolveUser(c)
	if !ok {
		return
	}
	if h.ent.Current(userID).Tier != entitlement.TierPremium {
		c.JSON(http.StatusForbidden, errResp{Error: "premium required", Code: "PREMIUM_REQUIRED"})
		return
	}

	start := time.Now()
	if ok, wait := h.reportLimit.allowRequest(userID, start); !ok {
		metrics.Report("limited")
		c.Header("Retry-After", retryAfter(wait))
		c.JSON(http.StatusTooManyRequests, errResp{Error: "too many report requests", Code: "RATE_LIMIT_MINUTE"})
		return
	}

	petID := c.Param("pet_id")
	loc, err := time.LoadLocation(c.DefaultQuery("tz", defaultReportTZ))
	if err != nil {
		loc, _ = time.LoadLocation(defaultReportTZ)
	}
	now := start.In(loc)
	opt := report.Options{
		WeightHistory: queryBool(c, "weight_history", true),
		VetNotes:      queryBool(c, "vet_notes", false),
	}

	unlock := h.reportLocks.lock(petID)
	defer unlock()

	// Отпечаток идёт первым: он же проверяет, что питомец принадлежит
	// пользователю. pdf_reports владельца не знает, и без этой проверки
	// готовый отчёт ушёл бы по чужому pet_id.
	fp, petName, err := h.pb.PetReportFingerprint(userID, petID, now)
	if err != nil {
		h.reportUpstreamError(c, "fingerprint", userID, petID, err)
		return
	}
	key := reportKey(fp, now, opt)

	stored, err := h.pb.FindReport(petID)
	if err != nil {
		// Без старой версии ещё можно собрать новую — не повод отказывать.
		slog.Warn("report: find stored", "pet", petID, "err", err)
		stored = nil
	}

	if stored != nil && stored.Key == key {
		pdf, err := h.pb.DownloadReport(stored)
		if err == nil {
			metrics.Report("cached")
			sendReport(c, pdf, stored.Updated, petName, true, false, 0)
			return
		}
		slog.Warn("report: download stored", "pet", petID, "err", err)
	}

	if ok, wait := h.reportLimit.allowGenerate(userID, start); !ok {
		if stored != nil {
			if pdf, err := h.pb.DownloadReport(stored); err == nil {
				metrics.Report("stale")
				sendReport(c, pdf, stored.Updated, petName, true, stored.Key != key, wait)
				return
			}
		}
		metrics.Report("limited")
		c.Header("Retry-After", retryAfter(wait))
		c.JSON(http.StatusTooManyRequests, errResp{Error: "report generation limit", Code: "RATE_LIMIT_REPORT"})
		return
	}

	data, err := h.pb.LoadPetReport(userID, petID, now)
	if err != nil {
		h.reportUpstreamError(c, "load", userID, petID, err)
		return
	}
	renderStart := time.Now()
	pdf, err := report.Render(data, now, opt)
	if err != nil {
		slog.Error("report: render", "user", userID, "pet", petID, "err", err)
		metrics.Report("error")
		c.JSON(http.StatusInternalServerError, errResp{Error: "internal error", Code: "INTERNAL_ERROR"})
		return
	}
	metrics.ReportRender(time.Since(renderStart))
	h.reportLimit.recordGenerate(userID, start)

	// Не сохранилось — пользователь всё равно получает свежий PDF; следующий
	// запрос просто соберёт его заново.
	generatedAt := start
	if saved, err := h.pb.SaveReport(stored, petID, key, pdf); err != nil {
		slog.Error("report: save", "pet", petID, "err", err)
	} else if !saved.Updated.IsZero() {
		generatedAt = saved.Updated
	}
	slog.Info("report: generated", "user", userID, "pet", petID,
		"duration_ms", time.Since(start).Milliseconds(), "bytes", len(pdf))
	metrics.Report("generated")
	sendReport(c, pdf, generatedAt, data.Pet.Name, false, false, 0)
}

func (h *Handler) reportUpstreamError(c *gin.Context, stage, userID, petID string, err error) {
	if errors.Is(err, pocketbase.ErrPetNotFound) {
		c.JSON(http.StatusNotFound, errResp{Error: "pet not found", Code: "PET_NOT_FOUND"})
		return
	}
	slog.Error("report: upstream", "stage", stage, "user", userID, "pet", petID, "err", err)
	metrics.Report("error")
	c.JSON(http.StatusBadGateway, errResp{Error: "data source unavailable", Code: "UPSTREAM_ERROR"})
}

// reportKey — версия шаблона + отпечаток данных + день пользователя + блоки.
// День нужен, потому что от него зависят возраст, статусы прививок и окно
// симптомов, даже если сами записи не менялись.
func reportKey(fingerprint string, now time.Time, opt report.Options) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%t\x00%t",
		report.TemplateVersion, fingerprint, now.Format("2006-01-02"), now.Location(),
		opt.WeightHistory, opt.VetNotes)))
	return hex.EncodeToString(sum[:])
}

func sendReport(c *gin.Context, pdf []byte, generatedAt time.Time, petName string, cached, stale bool, wait time.Duration) {
	c.Header("X-Report-Cached", strconv.FormatBool(cached))
	c.Header("X-Report-Stale", strconv.FormatBool(stale))
	c.Header("X-Report-Generated-At", generatedAt.UTC().Format(time.RFC3339))
	if stale && wait > 0 {
		c.Header("Retry-After", retryAfter(wait))
	}
	// Медицинские данные: не кэшировать ни в прокси, ни на устройстве.
	c.Header("Cache-Control", "private, no-store")
	name := "Карточка для ветеринара"
	if petName != "" {
		name += " — " + petName
	}
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="vet-card.pdf"; filename*=UTF-8''%s`,
		url.PathEscape(name+".pdf")))
	c.Data(http.StatusOK, "application/pdf", pdf)
}

func retryAfter(d time.Duration) string {
	return strconv.Itoa(int(math.Ceil(d.Seconds())))
}

func queryBool(c *gin.Context, key string, def bool) bool {
	v, ok := c.GetQuery(key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}
