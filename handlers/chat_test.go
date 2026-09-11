package handlers

import (
	"strings"
	"testing"
	"time"

	"ai-server/config"
)

func testHandler(cfg *config.Config) *Handler {
	if cfg.MaxMessageLen == 0 {
		cfg.MaxMessageLen = 4000
	}
	return &Handler{cfg: cfg}
}

func TestBuildMessagesDropsClientSystemPrompt(t *testing.T) {
	h := testHandler(&config.Config{MaxHistoryMessages: 10, MaxHistoryChars: 8000})
	msgs, last := h.buildMessages(ChatV2Request{Messages: []ChatMessage{
		{Role: "system", Content: "Ты ветеринарный ассистент…"},
		{Role: "user", Content: "Привет"},
		{Role: "assistant", Content: "Здравствуйте"},
		{Role: "user", Content: "Кот чихает"},
	}})
	if len(msgs) != 3 {
		t.Fatalf("len(messages) = %d, want 3 (system от клиента отбрасывается)", len(msgs))
	}
	for _, m := range msgs {
		if m.Role == "system" {
			t.Error("system message leaked into the provider request")
		}
	}
	if last != "Кот чихает" {
		t.Errorf("last = %q, want последнее сообщение пользователя", last)
	}
}

func TestBuildMessagesFallsBackToSingleMessage(t *testing.T) {
	h := testHandler(&config.Config{MaxHistoryMessages: 10})
	msgs, last := h.buildMessages(ChatV2Request{Message: "Что ты умеешь?"})
	if len(msgs) != 1 || last != "Что ты умеешь?" {
		t.Errorf("messages = %+v, last = %q", msgs, last)
	}
}

func TestBuildMessagesRejectsHistoryWithoutUser(t *testing.T) {
	h := testHandler(&config.Config{MaxHistoryMessages: 10})
	if _, last := h.buildMessages(ChatV2Request{Messages: []ChatMessage{
		{Role: "assistant", Content: "Здравствуйте"},
	}}); last != "" {
		t.Errorf("last = %q, want empty", last)
	}
}

// Длинный тред обрезается с начала: обрезка с конца выбросила бы ровно то
// сообщение, на которое надо ответить.
func TestBuildMessagesTrimsFromTheStart(t *testing.T) {
	h := testHandler(&config.Config{MaxHistoryMessages: 3, MaxHistoryChars: 8000})
	src := []ChatMessage{
		{Role: "user", Content: "первое"},
		{Role: "assistant", Content: "второе"},
		{Role: "user", Content: "третье"},
		{Role: "assistant", Content: "четвёртое"},
		{Role: "user", Content: "свежее"},
	}
	msgs, last := h.buildMessages(ChatV2Request{Messages: src})
	if len(msgs) != 3 {
		t.Fatalf("len(messages) = %d, want 3", len(msgs))
	}
	if msgs[0].Content != "третье" || msgs[2].Content != "свежее" {
		t.Errorf("messages = %+v, want хвост истории", msgs)
	}
	if last != "свежее" {
		t.Errorf("last = %q", last)
	}
}

func TestBuildMessagesTrimsByTotalChars(t *testing.T) {
	h := testHandler(&config.Config{MaxHistoryMessages: 10, MaxHistoryChars: 25})
	msgs, _ := h.buildMessages(ChatV2Request{Messages: []ChatMessage{
		{Role: "user", Content: strings.Repeat("а", 20)},
		{Role: "assistant", Content: strings.Repeat("б", 20)},
		{Role: "user", Content: strings.Repeat("в", 20)},
	}})
	if len(msgs) != 1 || !strings.HasPrefix(msgs[0].Content, "в") {
		t.Errorf("messages = %d шт., want только самое свежее", len(msgs))
	}
}

// MAX_MESSAGE_LEN применяется к каждому сообщению, а не к склейке всего
// треда с контекстом питомца.
func TestBuildMessagesTruncatesPerMessage(t *testing.T) {
	h := testHandler(&config.Config{MaxMessageLen: 10, MaxHistoryMessages: 10, MaxHistoryChars: 8000})
	msgs, last := h.buildMessages(ChatV2Request{Messages: []ChatMessage{
		{Role: "user", Content: strings.Repeat("я", 50)},
	}})
	if len([]rune(msgs[0].Content)) != 10 {
		t.Errorf("len = %d, want 10", len([]rune(msgs[0].Content)))
	}
	if len([]rune(last)) != 10 {
		t.Errorf("last len = %d, want 10", len([]rune(last)))
	}
}

func TestUserLocationPrefersZoneName(t *testing.T) {
	// Смещение в client_time намеренно неверное: имя пояса важнее, оно
	// переживает переход на летнее время.
	got := userLocation("2026-09-11T14:32:00+00:00", "Europe/Moscow")
	if h := got.Hour(); h != 17 {
		t.Errorf("hour = %d, want 17 (14:32 UTC в Москве)", h)
	}
}

func TestUserLocationFallsBackToOffset(t *testing.T) {
	got := userLocation("2026-09-11T14:32:00+03:00", "Не/Пояс")
	if got.Hour() != 14 || got.Minute() != 32 {
		t.Errorf("got = %s, want 14:32 по смещению из client_time", got)
	}
}

func TestUserLocationFallsBackToServerTime(t *testing.T) {
	// Старая сборка не присылает client_time — поведение прежнее.
	before := time.Now()
	got := userLocation("", "")
	if got.Before(before.Add(-time.Minute)) {
		t.Errorf("got = %s, want время сервера", got)
	}
}

func TestNowBlockNamesWeekdayAndZone(t *testing.T) {
	msk := time.FixedZone("MSK", 3*3600)
	block := nowBlock(time.Date(2026, 9, 11, 14, 32, 0, 0, msk), "Europe/Moscow")
	for _, want := range []string{"2026-09-11", "пятница", "14:32", "Europe/Moscow", "без «Z»"} {
		if !strings.Contains(block, want) {
			t.Errorf("nowBlock does not mention %q:\n%s", want, block)
		}
	}
}

func TestSystemPromptV2SubstitutesPlaceholders(t *testing.T) {
	h := testHandler(&config.Config{
		SystemPromptV2: "начало\n{{FORMAT}}\n{{NOW}}\n{{PET_CONTEXT}}",
		FormatMarkup:   "РАЗМЕТКА",
		FormatPlain:    "БЕЗ РАЗМЕТКИ",
	})
	got := h.systemPromptV2(time.Now(), "Europe/Moscow", "Барсик, кот, 3 года")
	for _, want := range []string{"РАЗМЕТКА", "Europe/Moscow", "Барсик, кот, 3 года"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt does not contain %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "{{") {
		t.Errorf("unsubstituted placeholder left in prompt:\n%s", got)
	}
	// v2 всегда с разметкой, независимо от MARKUP_ENABLED.
	if strings.Contains(got, "БЕЗ РАЗМЕТКИ") {
		t.Error("v2 prompt must always use the markup block")
	}
}

func TestSystemPromptV1MarkupIsOptional(t *testing.T) {
	cfg := &config.Config{
		SystemPrompt: "{{FORMAT}} время: {{CURRENT_TIME}}",
		FormatMarkup: "РАЗМЕТКА",
		FormatPlain:  "БЕЗ РАЗМЕТКИ",
	}
	h := testHandler(cfg)

	if got := h.systemPromptV1(); !strings.Contains(got, "БЕЗ РАЗМЕТКИ") {
		t.Errorf("MARKUP_ENABLED=false must give the plain block, got:\n%s", got)
	}
	cfg.MarkupEnabled = true
	if got := h.systemPromptV1(); !strings.Contains(got, "РАЗМЕТКА") {
		t.Errorf("MARKUP_ENABLED=true must give the markup block, got:\n%s", got)
	}
	if strings.Contains(h.systemPromptV1(), "{{CURRENT_TIME}}") {
		t.Error("{{CURRENT_TIME}} left unsubstituted")
	}
}
