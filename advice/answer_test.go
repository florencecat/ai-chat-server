package advice

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestParseMinimalAnswer(t *testing.T) {
	a, err := Parse(`{"response":"Всё хорошо."}`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.Response != "Всё хорошо." {
		t.Errorf("Response = %q", a.Response)
	}
	if len(a.Events) != 0 || len(a.Entries) != 0 {
		t.Errorf("events/entries must be empty, got %+v / %+v", a.Events, a.Entries)
	}
	// Метаданных нет — поля должны отсутствовать, а не подменяться дефолтом:
	// клиент различает «метаданных нет» и «уровень неизвестен».
	if a.Urgency != "" {
		t.Errorf("Urgency = %q, want empty", a.Urgency)
	}
}

func TestParseStripsCodeFence(t *testing.T) {
	a, err := Parse("```json\n{\"response\":\"текст\"}\n```")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.Response != "текст" {
		t.Errorf("Response = %q", a.Response)
	}
}

func TestParseRejectsAnswerWithoutText(t *testing.T) {
	for _, raw := range []string{
		`{"events":[],"urgency":"monitor"}`,
		`{"response":"   "}`,
	} {
		if _, err := Parse(raw); !errors.Is(err, ErrNoText) {
			t.Errorf("Parse(%s) error = %v, want ErrNoText", raw, err)
		}
	}
}

func TestParseRejectsInvalidJSON(t *testing.T) {
	if _, err := Parse("это не json"); err == nil {
		t.Error("Parse: want error for non-JSON input")
	}
}

func TestEventNormalization(t *testing.T) {
	raw := `{"response":"ок","events":[
		{"name":"Прививка","category":"vaccination","datetime":"2026-07-19T10:00:00Z","repeat":"yearly"},
		{"name":"Прогулка","category":"other","datetime":"без времени","repeat":"daily"},
		{"name":"Таблетка","category":"health","datetime":"2026-07-19T08:30:00+03:00","repeat":"daily"}
	]}`
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(a.Events) != 2 {
		t.Fatalf("len(Events) = %d, want 2 (событие без времени отбрасывается)", len(a.Events))
	}

	// Чужая категория и незнакомый repeat приводятся к тому, что понимает клиент.
	if got := a.Events[0].Category; got != "other" {
		t.Errorf("category = %q, want other", got)
	}
	if got := a.Events[0].Repeat; got != "none" {
		t.Errorf("repeat = %q, want none", got)
	}
	// Обозначение пояса отбрасывается, стенные часы сохраняются.
	if got := a.Events[0].DateTime; got != "2026-07-19T10:00:00" {
		t.Errorf("datetime = %q, want 2026-07-19T10:00:00", got)
	}
	if got := a.Events[1].DateTime; got != "2026-07-19T08:30:00" {
		t.Errorf("datetime = %q, want 2026-07-19T08:30:00", got)
	}
}

func TestEventsClampedToMax(t *testing.T) {
	raw := `{"response":"ок","events":[
		{"name":"1","category":"health","datetime":"2026-07-19T10:00:00","repeat":"none"},
		{"name":"2","category":"health","datetime":"2026-07-19T11:00:00","repeat":"none"},
		{"name":"3","category":"health","datetime":"2026-07-19T12:00:00","repeat":"none"},
		{"name":"4","category":"health","datetime":"2026-07-19T13:00:00","repeat":"none"}
	]}`
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(a.Events) != MaxEvents {
		t.Errorf("len(Events) = %d, want %d", len(a.Events), MaxEvents)
	}
}

func TestEntryCompleteness(t *testing.T) {
	tests := []struct {
		name string
		json string
		keep bool
	}{
		{"mood ок", `{"tracker":"mood","mood":"happy"}`, true},
		{"mood с чужим значением", `{"tracker":"mood","mood":"sad"}`, false},
		{"weight ок", `{"tracker":"weight","weight_kg":4.2}`, true},
		{"weight нулевой", `{"tracker":"weight","weight_kg":0}`, false},
		{"symptom ок", `{"tracker":"symptom","symptom":"vomiting","severity":"severe"}`, true},
		{"symptom чужой", `{"tracker":"symptom","symptom":"hiccups"}`, false},
		{"walk ок", `{"tracker":"walk","minutes":30}`, true},
		{"walk без минут", `{"tracker":"walk","activities":["active"]}`, false},
		{"meal по граммам", `{"tracker":"meal","grams":150}`, true},
		{"meal по корму", `{"tracker":"meal","food":"курица"}`, true},
		{"meal пустой", `{"tracker":"meal","appetite":3}`, false},
		{"note ок", `{"tracker":"note","text":"погуляли"}`, true},
		{"note пустой", `{"tracker":"note","text":"  "}`, false},
		{"незнакомый трекер", `{"tracker":"sleep","minutes":60}`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, err := Parse(`{"response":"ок","entries":[` + tc.json + `]}`)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := len(a.Entries) == 1; got != tc.keep {
				t.Errorf("kept = %v, want %v (entries=%+v)", got, tc.keep, a.Entries)
			}
		})
	}
}

func TestEntryDefaultsAndCleanup(t *testing.T) {
	a, err := Parse(`{"response":"ок","entries":[
		{"tracker":"symptom","symptom":"limping","severity":"critical"},
		{"tracker":"walk","minutes":20,"activities":["active","sleeping"]},
		{"tracker":"meal","grams":100,"appetite":9,"kind":"raw"}
	]}`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := a.Entries[0].Severity; got != "mild" {
		t.Errorf("severity = %q, want mild", got)
	}
	if got := a.Entries[1].Activities; len(got) != 1 || got[0] != "active" {
		t.Errorf("activities = %v, want [active]", got)
	}
	if got := a.Entries[2].Appetite; got != 0 {
		t.Errorf("appetite = %d, want 0 (вне диапазона 1–5)", got)
	}
	if got := a.Entries[2].Kind; got != "" {
		t.Errorf("kind = %q, want empty", got)
	}
}

func TestUnknownUrgencyBecomesMonitor(t *testing.T) {
	a, err := Parse(`{"response":"ок","urgency":"urgent"}`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.Urgency != UrgencyMonitor {
		t.Errorf("Urgency = %q, want %q", a.Urgency, UrgencyMonitor)
	}
}

func TestListsAreCleanedAndClamped(t *testing.T) {
	a, err := Parse(`{"response":"ок","red_flags":["a","","b","c","d","e","f"]}`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(a.RedFlags) != MaxRedFlags {
		t.Errorf("len(RedFlags) = %d, want %d", len(a.RedFlags), MaxRedFlags)
	}
}

// От follow_up_questions отказались: поле не в схеме, и присланное моделью
// значение не должно доехать до клиента.
func TestFollowUpQuestionsAreDropped(t *testing.T) {
	a, err := Parse(`{"response":"ок","follow_up_questions":["Кашель сухой?"]}`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(got), "follow_up") {
		t.Errorf("Marshal = %s, want без follow_up_questions", got)
	}
}

// Ответ без метаданных должен сериализоваться в то же, что отдавалось раньше:
// response + events. Лишние ключи в теле — риск для старых сборок.
func TestMarshalOmitsAbsentMetadata(t *testing.T) {
	a, err := Parse(`{"response":"ок"}`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"response":"ок","events":[]}`
	if string(got) != want {
		t.Errorf("Marshal = %s, want %s", got, want)
	}
}

func TestSchemaIsValidJSON(t *testing.T) {
	var v map[string]any
	if err := json.Unmarshal(Schema(), &v); err != nil {
		t.Fatalf("Schema is not valid JSON: %v", err)
	}
	if v["type"] != "object" {
		t.Errorf("schema type = %v, want object", v["type"])
	}
}

// Строгая схема Yandex требует, чтобы модель прислала все свойства сразу,
// поэтому у записи о прогулке приходят и mood, и symptom. Клиенту они уехать
// не должны: запись пересобирается из полей своего трекера.
func TestEntryDropsForeignFields(t *testing.T) {
	raw := `{"response":"ок","entries":[{
		"tracker":"walk","datetime":"2026-09-11T10:00:00","minutes":30,
		"activities":["active"],
		"mood":"happy","weight_kg":4.2,"symptom":"limping","severity":"severe",
		"note":"лишнее","grams":100,"food":"курица","appetite":3,"kind":"dry",
		"text":"тоже лишнее"
	}]}`
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(a.Entries) != 1 {
		t.Fatalf("len(Entries) = %d, want 1", len(a.Entries))
	}

	got, err := json.Marshal(a.Entries[0])
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"tracker":"walk","datetime":"2026-09-11T10:00:00","minutes":30,"activities":["active"]}`
	if string(got) != want {
		t.Errorf("entry = %s\nwant %s", got, want)
	}
}

// Пустые значения-заглушки, которыми модель заполняет чужие поля, не должны
// превращать запись в неполную.
func TestEntryAcceptsEmptyPlaceholders(t *testing.T) {
	raw := `{"response":"ок","entries":[{
		"tracker":"note","datetime":"2026-09-11T10:00:00","text":"погуляли",
		"mood":"","weight_kg":0,"symptom":"","severity":"","note":"",
		"minutes":0,"activities":[],"grams":0,"food":"","appetite":0,"kind":""
	}]}`
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(a.Entries) != 1 || a.Entries[0].Text != "погуляли" {
		t.Fatalf("entries = %+v, want одну заметку", a.Entries)
	}
	got, _ := json.Marshal(a.Entries[0])
	want := `{"tracker":"note","datetime":"2026-09-11T10:00:00","text":"погуляли"}`
	if string(got) != want {
		t.Errorf("entry = %s\nwant %s", got, want)
	}
}

// Пустая строка — легальное значение enum в схеме, но для своего трекера она
// означает «ключевого поля нет», и предложение отбрасывается.
func TestEntryRejectsEmptyOwnField(t *testing.T) {
	for _, raw := range []string{
		`{"tracker":"mood","mood":""}`,
		`{"tracker":"symptom","symptom":""}`,
		`{"tracker":"meal","grams":0,"food":""}`,
	} {
		a, err := Parse(`{"response":"ок","entries":[` + raw + `]}`)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if len(a.Entries) != 0 {
			t.Errorf("%s kept, want dropped", raw)
		}
	}
}

// Каждое свойство каждого объекта схемы должно быть перечислено в required:
// Yandex отвергает схему целиком, если хоть одно поле необязательное.
func TestSchemaIsStrict(t *testing.T) {
	var root map[string]any
	if err := json.Unmarshal(Schema(), &root); err != nil {
		t.Fatalf("Schema: %v", err)
	}
	var walk func(node any, path string)
	walk = func(node any, path string) {
		switch v := node.(type) {
		case map[string]any:
			if v["type"] == "object" {
				props, _ := v["properties"].(map[string]any)
				req, _ := v["required"].([]any)
				if len(props) != len(req) {
					t.Errorf("%s: %d properties but %d required", path, len(props), len(req))
				}
				for _, r := range req {
					if _, ok := props[r.(string)]; !ok {
						t.Errorf("%s: required %q is not a property", path, r)
					}
				}
				for name := range props {
					if !containsString(req, name) {
						t.Errorf("%s: property %q is missing from required", path, name)
					}
				}
			}
			for k, child := range v {
				walk(child, path+"."+k)
			}
		case []any:
			for i, child := range v {
				walk(child, fmt.Sprintf("%s[%d]", path, i))
			}
		}
	}
	walk(root, "root")
}

func containsString(list []any, want string) bool {
	for _, v := range list {
		if s, ok := v.(string); ok && s == want {
			return true
		}
	}
	return false
}

// Пустой ответ модели и оборванный JSON дают у encoding/json одну и ту же
// ошибку, но лечатся по-разному, поэтому пустой выделен в отдельный случай.
func TestParseDistinguishesEmptyFromTruncated(t *testing.T) {
	for _, raw := range []string{"", "   ", "```json\n```"} {
		if _, err := Parse(raw); !errors.Is(err, ErrEmpty) {
			t.Errorf("Parse(%q) error = %v, want ErrEmpty", raw, err)
		}
	}
	// Оборванный JSON — это уже ошибка разбора, а не пустой ответ.
	if _, err := Parse(`{"response":"начало отве`); err == nil || errors.Is(err, ErrEmpty) {
		t.Errorf("truncated JSON error = %v, want a parse error", err)
	}
}
