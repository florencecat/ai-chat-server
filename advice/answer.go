// Package advice описывает контракт ответа ассистента: строгую JSON-схему для
// модели и серверную нормализацию её ответа.
//
// Схема — это просьба к модели, а не гарантия. Нормализация — гарантия:
// всё, что клиент не умеет разобрать (чужая категория события, незнакомый
// трекер, запись без ключевого значения), отбрасывается здесь, а не молча
// исчезает на клиенте.
package advice

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrNoText — модель вернула валидный JSON без текста ответа. Показывать
// такое пользователю нечем, поэтому это ошибка, а не ответ.
var ErrNoText = errors.New("advice: response text is empty")

// Лимиты карточек под ответом. Три карточки — уже полэкрана.
const (
	MaxEvents            = 3
	MaxEntries           = 5
	MaxRedFlags          = 5
	MaxFollowUpQuestions = 3
)

// Answer — ответ ассистента в том виде, в котором он уезжает клиенту.
//
// Поля Фазы 4 (Urgency / RedFlags / FollowUpQuestions) опущены при отсутствии:
// клиент различает «метаданных нет» и «уровень неизвестен», и подменять
// первое вторым нельзя.
type Answer struct {
	Response string  `json:"response"`
	Events   []Event `json:"events"`
	Entries  []Entry `json:"entries,omitempty"`

	Urgency           string   `json:"urgency,omitempty"`
	RedFlags          []string `json:"red_flags,omitempty"`
	FollowUpQuestions []string `json:"follow_up_questions,omitempty"`
}

// Event — предложенное напоминание.
type Event struct {
	Name     string `json:"name"`
	Category string `json:"category"`
	DateTime string `json:"datetime"`
	Repeat   string `json:"repeat"`
}

// Entry — предложенная запись в трекер. Поля разных трекеров не пересекаются,
// поэтому держим их в одной структуре с omitempty.
type Entry struct {
	Tracker  string `json:"tracker"`
	DateTime string `json:"datetime,omitempty"`

	Mood string `json:"mood,omitempty"`

	WeightKG float64 `json:"weight_kg,omitempty"`

	Symptom  string `json:"symptom,omitempty"`
	Severity string `json:"severity,omitempty"`
	Note     string `json:"note,omitempty"`

	Minutes    int      `json:"minutes,omitempty"`
	Activities []string `json:"activities,omitempty"`

	Grams    float64 `json:"grams,omitempty"`
	Food     string  `json:"food,omitempty"`
	Appetite int     `json:"appetite,omitempty"`
	Kind     string  `json:"kind,omitempty"`

	Text string `json:"text,omitempty"`
}

// ── Допустимые значения ───────────────────────────────────────────────────────

// EventCategories — то, что умеет разобрать клиент (EventCategories.fromAiId).
// В приложении категорий восемь, но через ИИ доступны эти три; остальные
// молча схлопываются в «Другое». Расширять список здесь можно только
// синхронно с клиентским маппингом.
var EventCategories = []string{"health", "grooming", "other"}

// EventRepeats — yearly модель охотно предлагает для прививок, но клиент его
// не знает и превратит в разовое событие, поэтому его в списке нет.
var EventRepeats = []string{"none", "daily", "weekly", "monthly"}

// UrgencyLevels перечислены по возрастанию: индекс — это и есть уровень.
var UrgencyLevels = []string{"self_care", "monitor", "vet_24h", "emergency"}

const (
	UrgencyMonitor = "monitor"
	UrgencyVet24h  = "vet_24h"
)

var (
	moods      = []string{"happy", "calm", "sick", "playful"}
	severities = []string{"mild", "moderate", "severe"}
	activities = []string{"active", "calm", "dogGames", "training"}
	mealKinds  = []string{"natural", "dry", "wet", "treat"}
	symptoms   = []string{
		"vomiting", "diarrhea", "refused_food", "lethargy",
		"sneezing", "coughing", "scratching", "limping",
	}
	trackers = []string{"mood", "weight", "symptom", "walk", "meal", "note"}
)

func oneOf(value string, allowed []string) bool {
	for _, a := range allowed {
		if value == a {
			return true
		}
	}
	return false
}

// urgencyRank возвращает порядковый номер уровня; -1 для неизвестного.
func urgencyRank(level string) int {
	for i, l := range UrgencyLevels {
		if l == level {
			return i
		}
	}
	return -1
}

// ── Разбор и нормализация ─────────────────────────────────────────────────────

// Parse разбирает ответ модели и приводит его к контракту клиента.
// Возвращает ErrNoText или ошибку разбора — оба случая вызывающий трактует
// как повод повторить запрос к модели, а не как ответ.
func Parse(raw string) (*Answer, error) {
	raw = stripCodeFence(strings.TrimSpace(raw))
	var a Answer
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return nil, fmt.Errorf("advice: parse: %w", err)
	}
	a.normalize()
	if a.Response == "" {
		return nil, ErrNoText
	}
	return &a, nil
}

// stripCodeFence срезает markdown-обёртку ```json … ``` вокруг ответа модели.
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

func (a *Answer) normalize() {
	a.Response = strings.TrimSpace(a.Response)

	events := make([]Event, 0, len(a.Events))
	for _, e := range a.Events {
		if e.normalize() {
			events = append(events, e)
		}
		if len(events) == MaxEvents {
			break
		}
	}
	a.Events = events

	entries := make([]Entry, 0, len(a.Entries))
	for _, e := range a.Entries {
		if e.normalize() {
			entries = append(entries, e)
		}
		if len(entries) == MaxEntries {
			break
		}
	}
	a.Entries = entries

	// Неизвестный уровень трактуем как monitor — это решение клиента, но
	// повторяем его на сервере, чтобы логи и ответ совпадали.
	if a.Urgency != "" && urgencyRank(a.Urgency) < 0 {
		a.Urgency = UrgencyMonitor
	}
	a.RedFlags = cleanStrings(a.RedFlags, MaxRedFlags)
	a.FollowUpQuestions = cleanStrings(a.FollowUpQuestions, MaxFollowUpQuestions)
}

// normalize чинит то, что можно починить, и сообщает, годится ли событие.
func (e *Event) normalize() bool {
	e.Name = strings.TrimSpace(e.Name)
	if !oneOf(e.Category, EventCategories) {
		e.Category = "other"
	}
	if !oneOf(e.Repeat, EventRepeats) {
		e.Repeat = "none"
	}
	e.DateTime = localDateTime(e.DateTime)
	// Событие без времени клиенту бесполезно: поставить его некуда.
	return e.DateTime != ""
}

// normalize сообщает, можно ли по записи что-то записать в трекер.
// Правила совпадают с SuggestedEntry.fromAi на клиенте: незнакомый трекер или
// нехватка ключевого значения — предложение отбрасывается целиком.
func (e *Entry) normalize() bool {
	e.Tracker = strings.TrimSpace(e.Tracker)
	if !oneOf(e.Tracker, trackers) {
		return false
	}
	e.DateTime = localDateTime(e.DateTime)

	switch e.Tracker {
	case "mood":
		return oneOf(e.Mood, moods)
	case "weight":
		return e.WeightKG > 0
	case "symptom":
		if !oneOf(e.Symptom, symptoms) {
			return false
		}
		if !oneOf(e.Severity, severities) {
			e.Severity = "mild"
		}
		e.Note = strings.TrimSpace(e.Note)
		return true
	case "walk":
		if e.Minutes <= 0 {
			return false
		}
		kept := e.Activities[:0]
		for _, act := range e.Activities {
			if oneOf(act, activities) {
				kept = append(kept, act)
			}
		}
		e.Activities = kept
		return true
	case "meal":
		e.Food = strings.TrimSpace(e.Food)
		if e.Grams <= 0 && e.Food == "" {
			return false
		}
		if e.Appetite < 1 || e.Appetite > 5 {
			e.Appetite = 0
		}
		if !oneOf(e.Kind, mealKinds) {
			e.Kind = ""
		}
		return true
	case "note":
		e.Text = strings.TrimSpace(e.Text)
		return e.Text != ""
	}
	return false
}

// cleanStrings отбрасывает пустые элементы и обрезает список до limit.
func cleanStrings(in []string, limit int) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
		if len(out) == limit {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
