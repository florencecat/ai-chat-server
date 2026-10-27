package report

import (
	"bytes"
	"os"
	"testing"
	"time"
)

func day(s string) *time.Time {
	t, _ := time.ParseInLocation("02.01.2006", s, time.UTC)
	return &t
}

// sample — данные из макета «Карточка для ветеринара (PDF)».
func sample() Data {
	return Data{
		Pet: Pet{
			Name:      "Барни",
			Species:   "Собака",
			Breed:     "Вельш-корги кардиган",
			Gender:    "male",
			Birthdate: day("06.04.2025"),
			Allergies: "пыльца злаковых — сезонный кожный зуд (июнь–август).",
		},
		Weights: []Weight{
			{*day("05.02.2026"), 12.0},
			{*day("14.05.2026"), 13.1},
			{*day("06.07.2026"), 13.5},
			{*day("10.08.2026"), 13.6},
		},
		Pills: []Pill{
			{Name: "Фликсотид", Kind: "Ингаляция", Times: []string{"16:00"}, Start: day("06.07.2026"), End: day("06.10.2026")},
			{Name: "Пульмикорт", Kind: "Ингаляция", Times: []string{"12:00", "22:00"}, Start: day("06.07.2026"), End: day("06.10.2026")},
		},
		Treatments: []Treatment{
			{Name: "Нобивак DHPPi + Lepto", Kind: "vaccine", Date: day("14.05.2026"), Next: day("14.05.2027")},
			{Name: "Бешенство (Нобивак Rabies)", Kind: "rabies", Date: day("14.05.2026"), Next: day("14.05.2027")},
			{Name: "Нобивак DHPPi + Lepto", Kind: "vaccine", Date: day("10.05.2025"), Next: day("10.05.2026")},
			{Name: "Бравекто", Kind: "ticks", Date: day("12.07.2026"), Next: day("12.10.2026")},
			{Name: "Мильбемакс", Kind: "worms", Date: day("03.06.2026"), Next: day("03.09.2026")},
		},
		Symptoms: []Symptom{
			{Title: "Жидкий стул", Note: "очень очень жидкий и воняло", Severity: "moderate", Date: *day("25.07.2026")},
			{Title: "Кашель", Note: "сухой, по утрам", Severity: "moderate", Date: *day("25.07.2026")},
		},
	}
}

func TestRenderSample(t *testing.T) {
	now := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	pdf, err := Render(sample(), now, Options{WeightHistory: true, VetNotes: true})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(pdf, []byte("%PDF-")) {
		t.Fatalf("not a PDF: %q", pdf[:min(len(pdf), 16)])
	}
	// REPORT_SAMPLE=path сохраняет PDF, чтобы посмотреть вёрстку глазами.
	if path := os.Getenv("REPORT_SAMPLE"); path != "" {
		if err := os.WriteFile(path, pdf, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// Длинные списки должны переноситься на новые страницы, а не уезжать за край.
func TestRenderManyPages(t *testing.T) {
	d := sample()
	for i := 0; i < 40; i++ {
		d.Symptoms = append(d.Symptoms, Symptom{
			Title:    "Рвота",
			Note:     "после еды, несколько раз за день, с примесью травы; очень длинное описание, которое не влезает в одну строку и должно переноситься",
			Severity: "severe",
			Date:     *day("01.08.2026"),
		})
		d.Treatments = append(d.Treatments, Treatment{Name: "Вакцина " + string(rune('А'+i)), Kind: "vaccine", Date: day("01.01.2026"), Next: day("01.01.2026")})
	}
	pdf, err := Render(d, time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC), Options{WeightHistory: true})
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(pdf, []byte("/Type /Page\n")); n < 3 {
		t.Fatalf("pages = %d, want >= 3", n)
	}
	if path := os.Getenv("REPORT_SAMPLE_LONG"); path != "" {
		_ = os.WriteFile(path, pdf, 0o644)
	}
}

func TestAge(t *testing.T) {
	now := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	cases := map[string]string{
		"06.04.2025": "1 год и 4 мес.",
		"12.08.2024": "2 года",
		"20.07.2026": "меньше месяца",
		"01.03.2026": "5 мес.",
		"01.01.2015": "11 лет и 7 мес.",
	}
	for birth, want := range cases {
		if got := age(*day(birth), now); got != want {
			t.Errorf("age(%s) = %q, want %q", birth, got, want)
		}
	}
}

func TestPillSubtitle(t *testing.T) {
	p := Pill{Kind: "Ингаляция", Times: []string{"12:00", "22:00"}, Start: day("06.07.2026"), End: day("06.10.2026")}
	want := "ингаляция · 12:00 и 22:00 · с 6 июля по 6 октября 2026"
	if got := pillSubtitle(p); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
