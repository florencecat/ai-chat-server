package pocketbase

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"time"

	"ai-server/report"
)

// ErrPetNotFound — питомца нет или он принадлежит другому пользователю.
// Сервер ходит в PocketBase под админским токеном, правила коллекций его не
// ограничивают, поэтому владельца проверяем сами в фильтре.
var ErrPetNotFound = errors.New("pet not found")

// petQuery — один запрос за данными карточки. Отпечаток и загрузка идут по
// одним и тем же запросам, отличается только набор полей: так отпечаток
// гарантированно описывает ровно те записи, которые попадут в PDF.
type petQuery struct {
	collection string
	filter     string
	sort       string
	expand     string
	limit      int
}

// symptomWindow — за сколько дней симптомы попадают в карточку.
const symptomWindow = 30 * 24 * time.Hour

func petQueries(userID, petID string, now time.Time) []petQuery {
	pet := escapePBFilter(petID)
	today := startOfDayUTC(now)
	return []petQuery{
		{collection: "pets", filter: fmt.Sprintf("(id='%s' && user='%s')", pet, escapePBFilter(userID)), expand: "species,breed", limit: 1},
		{collection: "weights", filter: fmt.Sprintf("(pet='%s')", pet), sort: "-date", limit: report.ChartPoints},
		// Завершённые курсы врачу не нужны: только текущие и запланированные.
		{collection: "pills", filter: fmt.Sprintf("(pet='%s' && (end='' || end>='%s'))", pet, today), sort: "start", expand: "kind", limit: 100},
		{collection: "treatments", filter: fmt.Sprintf("(pet='%s')", pet), sort: "-date", limit: 200},
		{collection: "symptoms", filter: fmt.Sprintf("(pet='%s' && date>='%s')", pet, startOfDayUTC(now.Add(-symptomWindow))), sort: "-date", expand: "symptom", limit: 100},
	}
}

// startOfDayUTC — полночь дня now в поясе пользователя, в формате фильтра PB.
func startOfDayUTC(now time.Time) string {
	d := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	return d.UTC().Format(pbTimeLayout)
}

func (c *Client) listRecords(q petQuery, fields string) ([]json.RawMessage, error) {
	adminToken, err := c.getAdminToken()
	if err != nil {
		return nil, err
	}

	endpoint, _ := url.Parse(c.cfg.PBUrl + "/api/collections/" + q.collection + "/records")
	v := endpoint.Query()
	v.Set("filter", q.filter)
	v.Set("perPage", strconv.Itoa(q.limit))
	v.Set("skipTotal", "1")
	if q.sort != "" {
		v.Set("sort", q.sort)
	}
	if fields != "" {
		v.Set("fields", fields)
	} else if q.expand != "" {
		v.Set("expand", q.expand)
	}
	endpoint.RawQuery = v.Encode()

	req, _ := http.NewRequest(http.MethodGet, endpoint.String(), nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pb list %s: %w", q.collection, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("pb list %s: status %d: %s", q.collection, resp.StatusCode, body)
	}

	var result struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("pb list %s decode: %w", q.collection, err)
	}
	return result.Items, nil
}

// PetReportFingerprint — отпечаток данных карточки по парам (id, updated).
// Запросы лёгкие (только два поля), поэтому повторный запрос отчёта по
// неизменным данным не грузит ни PocketBase, ни рендер. Заодно возвращается
// кличка — для имени файла, когда отчёт отдаётся готовым.
//
// Хэшируются именно пары, а не max(updated): удаление записи updated не
// меняет, а набор id — меняет.
func (c *Client) PetReportFingerprint(userID, petID string, now time.Time) (fingerprint, petName string, err error) {
	h := sha256.New()
	for i, q := range petQueries(userID, petID, now) {
		fields := "id,updated"
		if i == 0 {
			fields += ",name"
		}
		items, err := c.listRecords(q, fields)
		if err != nil {
			return "", "", err
		}
		if i == 0 && len(items) == 0 {
			return "", "", ErrPetNotFound
		}
		fmt.Fprintf(h, "%s:%d\n", q.collection, len(items))
		for _, raw := range items {
			var r struct {
				ID      string `json:"id"`
				Updated string `json:"updated"`
				Name    string `json:"name"`
			}
			if err := json.Unmarshal(raw, &r); err != nil {
				return "", "", fmt.Errorf("pb fingerprint %s: %w", q.collection, err)
			}
			if i == 0 {
				petName = r.Name
			}
			fmt.Fprintf(h, "%s@%s\n", r.ID, r.Updated)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), petName, nil
}

// ── Загрузка данных карточки ──────────────────────────────────────────────────

type petRecord struct {
	Name              string `json:"name"`
	Birthdate         string `json:"birthdate"`
	Gender            string `json:"gender"`
	Castrated         bool   `json:"castrated"`
	CastrationDate    string `json:"castration_date"`
	Allergies         string `json:"allergies"`
	ChronicConditions string `json:"chronic_conditions"`
	SpeciesCustomName string `json:"species_custom_name"`
	BreedCustomName   string `json:"breed_custom_name"`
	Expand            struct {
		Species *struct {
			Name string `json:"name"`
		} `json:"species"`
		Breed *struct {
			Name string `json:"name"`
		} `json:"breed"`
	} `json:"expand"`
}

type weightRecord struct {
	Date   string  `json:"date"`
	Weight float64 `json:"weight"`
}

type pillRecord struct {
	Name      string          `json:"name"`
	DoseValue float64         `json:"dose_value"`
	DoseUnit  string          `json:"dose_unit"`
	Frequency string          `json:"frequency"`
	Weekdays  []string        `json:"weekdays"`
	Start     string          `json:"start"`
	End       string          `json:"end"`
	Schedules json.RawMessage `json:"schedules"`
	Expand    struct {
		Kind *struct {
			Name string `json:"name"`
		} `json:"kind"`
	} `json:"expand"`
}

type treatmentRecord struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	Date string `json:"date"`
	Next string `json:"next"`
}

type symptomRecord struct {
	Date     string `json:"date"`
	Note     string `json:"note"`
	Severity string `json:"severity"`
	Expand   struct {
		Symptom *struct {
			Caption string `json:"caption"`
		} `json:"symptom"`
	} `json:"expand"`
}

// LoadPetReport собирает данные карточки. Даты переводятся в пояс now: клиент
// пишет «день» как локальную полночь, и в UTC он съехал бы на вчера.
func (c *Client) LoadPetReport(userID, petID string, now time.Time) (report.Data, error) {
	var d report.Data
	loc := now.Location()
	date := func(s string) *time.Time {
		t, ok := parsePBTime(s)
		if !ok {
			return nil
		}
		t = t.In(loc)
		return &t
	}

	for i, q := range petQueries(userID, petID, now) {
		items, err := c.listRecords(q, "")
		if err != nil {
			return d, err
		}
		if i == 0 && len(items) == 0 {
			return d, ErrPetNotFound
		}
		for _, raw := range items {
			switch q.collection {
			case "pets":
				var r petRecord
				if err := json.Unmarshal(raw, &r); err != nil {
					return d, fmt.Errorf("pb pet decode: %w", err)
				}
				d.Pet = report.Pet{
					Name:           r.Name,
					Species:        r.SpeciesCustomName,
					Breed:          r.BreedCustomName,
					Gender:         r.Gender,
					Birthdate:      date(r.Birthdate),
					Castrated:      r.Castrated,
					CastrationDate: date(r.CastrationDate),
					Allergies:      r.Allergies,
					Chronic:        r.ChronicConditions,
				}
				if s := r.Expand.Species; s != nil && s.Name != "" {
					d.Pet.Species = s.Name
				}
				if b := r.Expand.Breed; b != nil && b.Name != "" {
					d.Pet.Breed = b.Name
				}

			case "weights":
				var r weightRecord
				if err := json.Unmarshal(raw, &r); err != nil {
					return d, fmt.Errorf("pb weight decode: %w", err)
				}
				if t := date(r.Date); t != nil && r.Weight > 0 {
					d.Weights = append(d.Weights, report.Weight{Date: *t, Kg: r.Weight})
				}

			case "pills":
				var r pillRecord
				if err := json.Unmarshal(raw, &r); err != nil {
					return d, fmt.Errorf("pb pill decode: %w", err)
				}
				p := report.Pill{
					Name:      r.Name,
					DoseValue: r.DoseValue,
					DoseUnit:  r.DoseUnit,
					Frequency: r.Frequency,
					Weekdays:  r.Weekdays,
					Times:     scheduleTimes(r.Schedules),
					Start:     date(r.Start),
					End:       date(r.End),
				}
				if k := r.Expand.Kind; k != nil {
					p.Kind = k.Name
				}
				d.Pills = append(d.Pills, p)

			case "treatments":
				var r treatmentRecord
				if err := json.Unmarshal(raw, &r); err != nil {
					return d, fmt.Errorf("pb treatment decode: %w", err)
				}
				d.Treatments = append(d.Treatments, report.Treatment{
					Name: r.Name, Kind: r.Kind, Date: date(r.Date), Next: date(r.Next),
				})

			case "symptoms":
				var r symptomRecord
				if err := json.Unmarshal(raw, &r); err != nil {
					return d, fmt.Errorf("pb symptom decode: %w", err)
				}
				t := date(r.Date)
				if t == nil {
					continue
				}
				s := report.Symptom{Note: r.Note, Severity: r.Severity, Date: *t}
				if st := r.Expand.Symptom; st != nil {
					s.Title = st.Caption
				}
				d.Symptoms = append(d.Symptoms, s)
			}
		}
	}

	// Вес запрашивается от новых к старым (нужны последние N), а график
	// рисуется слева направо.
	sort.Slice(d.Weights, func(i, j int) bool { return d.Weights[i].Date.Before(d.Weights[j].Date) })
	return d, nil
}

// Перед временем не должно быть цифры, двоеточия или знака: иначе из
// «T16:00:00» и «+03:00» достались бы секунды и смещение пояса.
var timeRe = regexp.MustCompile(`(?:^|[^\d:+\-])([01]?\d|2[0-3]):([0-5]\d)`)

// scheduleTimes вытаскивает время приёма из pills.schedules. Формат поля
// задаёт клиент и он может меняться, поэтому ищем «ЧЧ:ММ» в любом месте JSON,
// а не завязываемся на структуру.
func scheduleTimes(raw json.RawMessage) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range timeRe.FindAllStringSubmatch(string(raw), -1) {
		h, _ := strconv.Atoi(m[1])
		t := fmt.Sprintf("%02d:%s", h, m[2])
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}
