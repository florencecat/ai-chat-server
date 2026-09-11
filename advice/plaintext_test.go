package advice

import (
	"encoding/json"
	"testing"
)

func TestPlainText(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		// Ровно то, что приехало из продакшена.
		{"приём **Пульмикорта**", "приём Пульмикорта"},
		{"**жирный**", "жирный"},
		{"*курсив*", "курсив"},
		{"`5 мг`", "5 мг"},
		{"__подчёркнутый__", "подчёркнутый"},
		{"~~зачёркнутый~~", "зачёркнутый"},
		{"**доза `5 мг` внутри**", "доза 5 мг внутри"},
		{"## Заголовок", "Заголовок"},
		{"- пункт списка", "пункт списка"},
		{"1. первый шаг", "первый шаг"},
		{"[клиника](https://example.com)", "клиника"},
		// Непарный остаток — это маркер, а не содержание.
		{"доза 5** мг", "доза 5 мг"},
		// Обычный текст не трогаем.
		{"приём Пульмикорта", "приём Пульмикорта"},
		{"вес 4.2 кг", "вес 4.2 кг"},
		{"ключ day_requests", "ключ day_requests"},
		{"  пробелы  ", "пробелы"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := plainText(tc.in); got != tc.want {
			t.Errorf("plainText(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Разметка в name — то, что клиент рисует как есть, поэтому снимается
// на сервере, а не оставляется на совесть модели.
func TestEventNameLosesMarkup(t *testing.T) {
	a, err := Parse(`{"response":"**жирный** текст ответа остаётся","events":[
		{"name":"приём **Пульмикорта**","category":"health",
		 "datetime":"2026-09-11T15:23:00","repeat":"daily"}
	]}`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := a.Events[0].Name; got != "приём Пульмикорта" {
		t.Errorf("name = %q, want без разметки", got)
	}
	// В самом ответе разметка — это фича, её трогать нельзя.
	if a.Response != "**жирный** текст ответа остаётся" {
		t.Errorf("response = %q, разметка в ответе должна сохраняться", a.Response)
	}
}

func TestEntryTextFieldsLoseMarkup(t *testing.T) {
	a, err := Parse("{\"response\":\"ок\",\"entries\":[" +
		"{\"tracker\":\"note\",\"text\":\"дали **Цетрин**\"}," +
		"{\"tracker\":\"symptom\",\"symptom\":\"coughing\",\"note\":\"кашель `сухой`\"}," +
		"{\"tracker\":\"meal\",\"food\":\"**курица**\"}]}")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(a.Entries) != 3 {
		t.Fatalf("len(Entries) = %d, want 3", len(a.Entries))
	}
	if got := a.Entries[0].Text; got != "дали Цетрин" {
		t.Errorf("text = %q", got)
	}
	if got := a.Entries[1].Note; got != "кашель сухой" {
		t.Errorf("note = %q", got)
	}
	if got := a.Entries[2].Food; got != "курица" {
		t.Errorf("food = %q", got)
	}
}

func TestRedFlagsLoseMarkup(t *testing.T) {
	a, err := Parse(`{"response":"ок","red_flags":["**кровь** в мокроте","","- отказ от воды"]}`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []string{"кровь в мокроте", "отказ от воды"}
	if len(a.RedFlags) != len(want) {
		t.Fatalf("red_flags = %v", a.RedFlags)
	}
	for i, w := range want {
		if a.RedFlags[i] != w {
			t.Errorf("red_flags[%d] = %q, want %q", i, a.RedFlags[i], w)
		}
	}
}

// Событие, у которого после снятия разметки не осталось имени, всё равно
// годится: клиент подставит «Напоминание».
func TestEventSurvivesEmptyName(t *testing.T) {
	a, err := Parse(`{"response":"ок","events":[
		{"name":"**","category":"health","datetime":"2026-09-11T15:00:00","repeat":"none"}
	]}`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(a.Events) != 1 {
		t.Fatalf("len(Events) = %d, want 1", len(a.Events))
	}
	got, _ := json.Marshal(a.Events[0])
	want := `{"name":"","category":"health","datetime":"2026-09-11T15:00:00","repeat":"none"}`
	if string(got) != want {
		t.Errorf("event = %s\nwant %s", got, want)
	}
}
