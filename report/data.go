// Package report — PDF «Карточка для ветеринара» для премиум-пользователей.
//
// Пакет ничего не знает о PocketBase: на вход приходит готовый Data, на выход
// уходят байты PDF. Данные собирает pocketbase.Client, кэш и лимиты живут в
// handlers — так рендер можно гонять в тестах без сети.
package report

import "time"

// TemplateVersion попадает в ключ кэша: после правки вёрстки пользователи не
// должны получать PDF, собранный старым шаблоном.
const TemplateVersion = "vet-card-1"

// Data — всё, что нужно для одной карточки.
type Data struct {
	Pet        Pet
	Weights    []Weight // по возрастанию даты
	Pills      []Pill
	Treatments []Treatment
	Symptoms   []Symptom
}

type Pet struct {
	Name           string
	Species        string
	Breed          string
	Gender         string // male | female | none
	Birthdate      *time.Time
	Castrated      bool
	CastrationDate *time.Time
	Allergies      string
	Chronic        string
}

type Weight struct {
	Date time.Time
	Kg   float64
}

// Pill — курс лечения из коллекции pills.
type Pill struct {
	Name      string
	Kind      string // «ингаляция», «таблетка» — из справочника pill_kind
	DoseValue float64
	DoseUnit  string
	Frequency string   // daily | weekdays | onDemand
	Weekdays  []string // "1".."7", понедельник = 1
	Times     []string // "08:00", "20:00"
	Start     *time.Time
	End       *time.Time
}

// Treatment — запись из treatments: прививка (rabies, vaccine) или обработка
// от паразитов (ticks, worms).
type Treatment struct {
	Name string
	Kind string
	Date *time.Time
	Next *time.Time
}

type Symptom struct {
	Title    string
	Note     string
	Severity string // mild | moderate | severe
	Date     time.Time
}

// Options — включаемые блоки, как в макете (showWeightHistory, showVetNotes).
type Options struct {
	WeightHistory bool
	VetNotes      bool
}

// IsVaccine отделяет прививки от обработок: в макете это две разные таблицы.
func (t Treatment) IsVaccine() bool {
	return t.Kind == "rabies" || t.Kind == "vaccine"
}
