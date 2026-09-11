package handlers

import (
	"fmt"
	"strings"
	"time"
)

// Плейсхолдеры в файлах промтов.
const (
	phCurrentTime = "{{CURRENT_TIME}}"
	phFormat      = "{{FORMAT}}"
	phNow         = "{{NOW}}"
	phPetContext  = "{{PET_CONTEXT}}"
)

var weekdaysRU = [...]string{
	time.Sunday:    "воскресенье",
	time.Monday:    "понедельник",
	time.Tuesday:   "вторник",
	time.Wednesday: "среда",
	time.Thursday:  "четверг",
	time.Friday:    "пятница",
	time.Saturday:  "суббота",
}

// formatBlock выбирает блок о разметке. Разметка в v1 выключена по умолчанию:
// установленные сборки без markdown-рендера показали бы звёздочки текстом.
func (h *Handler) formatBlock(markup bool) string {
	if markup {
		return h.cfg.FormatMarkup
	}
	return h.cfg.FormatPlain
}

// systemPromptV1 собирает промт для старого /chat.
func (h *Handler) systemPromptV1() string {
	p := strings.ReplaceAll(h.cfg.SystemPrompt, phCurrentTime, time.Now().Format(time.RFC3339))
	return strings.ReplaceAll(p, phFormat, h.formatBlock(h.cfg.MarkupEnabled))
}

// systemPromptV2 собирает промт для /v2/chat: разметка включена всегда,
// время и пояс — пользовательские, контекст питомца склеивается на сервере.
func (h *Handler) systemPromptV2(now time.Time, tz, petContext string) string {
	p := strings.ReplaceAll(h.cfg.SystemPromptV2, phFormat, h.formatBlock(true))
	p = strings.ReplaceAll(p, phCurrentTime, now.Format(time.RFC3339))
	p = strings.ReplaceAll(p, phNow, nowBlock(now, tz))
	p = strings.ReplaceAll(p, phPetContext, petContextBlock(petContext))
	return strings.TrimSpace(p)
}

// nowBlock сообщает модели текущее местное время пользователя и правило
// записи datetime. Без него модель считает «через неделю» от даты, которую
// предполагает сама, и промахивается тем сильнее, чем дальше обучающий срез.
func nowBlock(now time.Time, tz string) string {
	zone := tz
	if zone == "" {
		zone, _ = now.Zone()
	}
	return fmt.Sprintf(
		"Сейчас у пользователя: %s, %s, %s (%s).\n"+
			"В поле datetime пиши МЕСТНОЕ время пользователя в формате\n"+
			"YYYY-MM-DDTHH:MM:SS, без «Z» и без смещения.",
		now.Format("2006-01-02"), weekdaysRU[now.Weekday()], now.Format("15:04"), zone,
	)
}

func petContextBlock(petContext string) string {
	petContext = strings.TrimSpace(petContext)
	if petContext == "" {
		return ""
	}
	return "Контекст питомца (данные из приложения, считай их достоверными):\n" + petContext
}

// userLocation разбирает client_time/tz от клиента и возвращает время
// пользователя. Старая сборка их не присылает — тогда берём время сервера.
func userLocation(clientTime, tz string) time.Time {
	if clientTime == "" {
		return time.Now()
	}
	t, err := time.Parse(time.RFC3339, clientTime)
	if err != nil {
		return time.Now()
	}
	// Имя пояса важнее смещения: «Europe/Moscow» переживает переход на
	// летнее время, а «+03:00» — нет.
	if tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			return t.In(loc)
		}
	}
	return t
}
