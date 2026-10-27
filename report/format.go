package report

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

var monthsGenitive = [...]string{
	"января", "февраля", "марта", "апреля", "мая", "июня",
	"июля", "августа", "сентября", "октября", "ноября", "декабря",
}

var weekdayShort = map[string]string{
	"1": "пн", "2": "вт", "3": "ср", "4": "чт", "5": "пт", "6": "сб", "7": "вс",
}

// plural выбирает форму слова для числа: 1 год, 2 года, 5 лет.
func plural(n int, one, few, many string) string {
	n %= 100
	if n >= 11 && n <= 14 {
		return many
	}
	switch n % 10 {
	case 1:
		return one
	case 2, 3, 4:
		return few
	}
	return many
}

// longDate — «12 августа 2026».
func longDate(t time.Time) string {
	return fmt.Sprintf("%d %s %d", t.Day(), monthsGenitive[t.Month()-1], t.Year())
}

// dayMonth — «6 июля».
func dayMonth(t time.Time) string {
	return fmt.Sprintf("%d %s", t.Day(), monthsGenitive[t.Month()-1])
}

func shortDate(t time.Time) string { return t.Format("02.01.2006") }

// age — «1 год и 4 мес.» на дату now.
func age(birth, now time.Time) string {
	y := now.Year() - birth.Year()
	m := int(now.Month()) - int(birth.Month())
	if now.Day() < birth.Day() {
		m--
	}
	if m < 0 {
		y--
		m += 12
	}
	switch {
	case y < 0:
		return "—"
	case y == 0 && m == 0:
		return "меньше месяца"
	case y == 0:
		return fmt.Sprintf("%d мес.", m)
	case m == 0:
		return fmt.Sprintf("%d %s", y, plural(y, "год", "года", "лет"))
	}
	return fmt.Sprintf("%d %s и %d мес.", y, plural(y, "год", "года", "лет"), m)
}

// weightText — «13.6 кг»; мелких животных удобнее читать в граммах.
func weightText(kg float64) string {
	if kg < 1 {
		return fmt.Sprintf("%d г", int(math.Round(kg*1000)))
	}
	return fmt.Sprintf("%.1f кг", kg)
}

// weightTrend сравнивает два последних взвешивания. Порог в 2% отсекает
// разброс напольных весов.
func weightTrend(ws []Weight) string {
	if len(ws) < 2 {
		return ""
	}
	prev, cur := ws[len(ws)-2].Kg, ws[len(ws)-1].Kg
	if prev <= 0 {
		return ""
	}
	d := (cur - prev) / prev
	switch {
	case math.Abs(d) < 0.02:
		return "стабилен"
	case d > 0:
		return "растёт"
	}
	return "снижается"
}

// period — «с 6 июля по 6 октября 2026»; год у начала опускается, если он
// совпадает с годом конца.
func period(start, end *time.Time) string {
	switch {
	case start != nil && end != nil:
		from := dayMonth(*start)
		if start.Year() != end.Year() {
			from = longDate(*start)
		}
		return "с " + from + " по " + longDate(*end)
	case start != nil:
		return "с " + longDate(*start)
	case end != nil:
		return "до " + longDate(*end)
	}
	return ""
}

// joinAnd — «12:00 и 22:00», «8:00, 14:00 и 20:00».
func joinAnd(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " и " + items[len(items)-1]
}

// pillSubtitle — строка под названием препарата:
// «ингаляция · 1 доза · 12:00 и 22:00 · с 6 июля по 6 октября 2026».
func pillSubtitle(p Pill) string {
	var parts []string
	if p.Kind != "" {
		parts = append(parts, strings.ToLower(p.Kind))
	}
	if p.DoseValue > 0 {
		dose := strconv.FormatFloat(p.DoseValue, 'f', -1, 64)
		if p.DoseUnit != "" {
			dose += " " + p.DoseUnit
		}
		parts = append(parts, dose)
	}
	switch p.Frequency {
	case "onDemand":
		parts = append(parts, "по необходимости")
	case "weekdays":
		var days []string
		for _, d := range p.Weekdays {
			if s, ok := weekdayShort[d]; ok {
				days = append(days, s)
			}
		}
		if len(days) > 0 {
			parts = append(parts, strings.Join(days, ", "))
		}
	}
	if t := joinAnd(p.Times); t != "" {
		parts = append(parts, t)
	}
	if s := period(p.Start, p.End); s != "" {
		parts = append(parts, s)
	}
	return strings.Join(parts, " · ")
}

func treatmentTarget(kind string) string {
	switch kind {
	case "ticks":
		return "клещи и блохи"
	case "worms":
		return "гельминты"
	}
	return "—"
}

func optDate(t *time.Time) string {
	if t == nil {
		return "—"
	}
	return shortDate(*t)
}

// startOfDay — полночь дня t в его же поясе.
func startOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}
