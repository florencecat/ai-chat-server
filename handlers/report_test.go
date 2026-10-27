package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"ai-server/config"
	"ai-server/entitlement"
	"ai-server/pocketbase"
)

// fakePB — минимальный PocketBase: авторизация, права, коллекции карточки и
// pdf_reports с protected-файлом. Владелец pets/p1 — u1 (премиум), pets/p2 —
// u2 (бесплатный).
type fakePB struct {
	mu        sync.Mutex
	records   map[string][]map[string]any
	fullLoads int // запросы без fields — то есть реальная сборка отчёта

	reports map[string]*fakeReport // по pet
	files   map[string][]byte      // имя файла → содержимое
	saves   int
}

type fakeReport struct {
	ID, Pet, Key, File, Updated string
}

func (r *fakeReport) json() map[string]any {
	return map[string]any{"id": r.ID, "pet": r.Pet, "key": r.Key, "report": r.File, "updated": r.Updated}
}

func newFakePB() *fakePB {
	return &fakePB{records: map[string][]map[string]any{
		"pets": {
			{"id": "p1", "user": "u1", "updated": "2026-08-01 10:00:00.000Z", "name": "Барни", "gender": "male",
				"birthdate": "2025-04-05 21:00:00.000Z", "allergies": "пыльца злаковых",
				"expand": map[string]any{"breed": map[string]any{"name": "Вельш-корги кардиган"}, "species": map[string]any{"name": "Собака"}}},
			{"id": "p2", "user": "u2", "updated": "2026-08-01 10:00:00.000Z", "name": "Мурка"},
		},
		"weights": {
			{"id": "w1", "pet": "p1", "updated": "2026-08-01 10:00:00.000Z", "date": "2026-02-04 21:00:00.000Z", "weight": 12.0},
			{"id": "w2", "pet": "p1", "updated": "2026-08-01 10:00:00.000Z", "date": "2026-08-09 21:00:00.000Z", "weight": 13.6},
		},
		"pills": {
			{"id": "pl1", "pet": "p1", "updated": "2026-08-01 10:00:00.000Z", "name": "Фликсотид",
				"start": "2026-07-05 21:00:00.000Z", "end": "2026-10-05 21:00:00.000Z", "schedules": []string{"16:00"},
				"expand": map[string]any{"kind": map[string]any{"name": "Ингаляция"}}},
		},
		"treatments": {
			{"id": "t1", "pet": "p1", "updated": "2026-08-01 10:00:00.000Z", "name": "Нобивак Rabies", "kind": "rabies",
				"date": "2026-05-13 21:00:00.000Z", "next": "2027-05-13 21:00:00.000Z"},
		},
		"symptoms": {},
	},
		reports: map[string]*fakeReport{},
		files:   map[string][]byte{},
	}
}

// servePDFReports — pdf_reports: список, create/patch multipart и скачивание
// protected-файла только с файловым токеном.
func (f *fakePB) servePDFReports(w http.ResponseWriter, r *http.Request) bool {
	write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	const records = "/api/collections/pdf_reports/records"
	switch {
	case r.URL.Path == "/api/files/token" && r.Method == http.MethodPost:
		write(map[string]string{"token": "ft"})
	case strings.HasPrefix(r.URL.Path, "/api/files/pdf_reports/"):
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		data, ok := f.files[name]
		if !ok || r.URL.Query().Get("token") != "ft" {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		_, _ = w.Write(data)
	case r.URL.Path == records && r.Method == http.MethodGet:
		items := []map[string]any{}
		for pet, rep := range f.reports {
			if strings.Contains(r.URL.Query().Get("filter"), "pet='"+pet+"'") {
				items = append(items, rep.json())
			}
		}
		write(map[string]any{"items": items})
	case strings.HasPrefix(r.URL.Path, records) && (r.Method == http.MethodPost || r.Method == http.MethodPatch):
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return true
		}
		file, _, err := r.FormFile("report")
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return true
		}
		data, _ := io.ReadAll(file)
		f.saves++
		pet := r.FormValue("pet")
		rep := f.reports[pet]
		if r.Method == http.MethodPost {
			if rep != nil { // уникальный индекс по pet
				w.WriteHeader(http.StatusBadRequest)
				return true
			}
			rep = &fakeReport{ID: "r_" + pet, Pet: pet}
			f.reports[pet] = rep
		} else if rep == nil || !strings.HasSuffix(r.URL.Path, "/"+rep.ID) {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		delete(f.files, rep.File)
		rep.Key = r.FormValue("key")
		rep.File = fmt.Sprintf("vet_card_%d.pdf", f.saves)
		rep.Updated = time.Now().UTC().Format("2006-01-02 15:04:05.000Z")
		f.files[rep.File] = data
		write(rep.json())
	default:
		return false
	}
	return true
}

func (f *fakePB) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	write := func(v any) { _ = json.NewEncoder(w).Encode(v) }

	if f.servePDFReports(w, r) {
		return
	}

	switch {
	case r.URL.Path == "/api/collections/_superusers/auth-with-password":
		write(map[string]string{"token": "admin"})
		return
	case r.URL.Path == "/api/collections/users/auth-refresh":
		switch r.Header.Get("Authorization") {
		case "Bearer u1":
			write(map[string]any{"record": map[string]string{"id": "u1"}})
		case "Bearer u2":
			write(map[string]any{"record": map[string]string{"id": "u2"}})
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
		return
	case r.URL.Path == "/api/collections/entitlements/records":
		var items []map[string]any
		if strings.Contains(r.URL.Query().Get("filter"), "'u1'") {
			items = append(items, map[string]any{"user": "u1", "tier": "premium", "active": true, "source": "manual"})
		}
		write(map[string]any{"items": items})
		return
	}

	coll := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/collections/"), "/records")
	q := r.URL.Query()
	filter := q.Get("filter")
	fields := q.Get("fields")
	if fields == "" {
		f.fullLoads++
	}
	items := []map[string]any{}
	for _, rec := range f.records[coll] {
		if coll == "pets" {
			// Фильтр по владельцу — ровно то, что защищает от чужого pet_id.
			if !strings.Contains(filter, "id='"+rec["id"].(string)+"'") || !strings.Contains(filter, "user='"+rec["user"].(string)+"'") {
				continue
			}
		} else if !strings.Contains(filter, "pet='"+rec["pet"].(string)+"'") {
			continue
		}
		if fields == "id,updated" {
			items = append(items, map[string]any{"id": rec["id"], "updated": rec["updated"]})
		} else {
			items = append(items, rec)
		}
	}
	write(map[string]any{"items": items})
}

func (f *fakePB) touch(coll, id, updated string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, rec := range f.records[coll] {
		if rec["id"] == id {
			rec["updated"] = updated
		}
	}
}

func (f *fakePB) loads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fullLoads
}

func reportTestServer(t *testing.T, cooldown time.Duration) (*gin.Engine, *fakePB, *Handler) {
	t.Helper()
	fake := newFakePB()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	cfg := &config.Config{
		PBUrl: srv.URL, PBAdminEmail: "a", PBAdminPassword: "b",
		ReportRequestsPerMinute: 100, ReportCooldown: cooldown, ReportPerDay: 20,
	}
	pb := pocketbase.NewClient(cfg)
	ent := entitlement.NewService(pb, nil, entitlement.Plans{}, time.Hour)
	h := New(nil, nil, pb, cfg, ent, nil, nil)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/report/vet-card/:pet_id", h.VetCard)
	return r, fake, h
}

func getReport(r *gin.Engine, token, pet string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/report/vet-card/"+pet+"?tz=Europe/Moscow", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestVetCardRequiresPremium(t *testing.T) {
	r, _, _ := reportTestServer(t, time.Minute)
	if w := getReport(r, "u2", "p2"); w.Code != http.StatusForbidden {
		t.Fatalf("free user: status %d, want 403", w.Code)
	}
}

func TestVetCardForeignPet(t *testing.T) {
	r, _, _ := reportTestServer(t, time.Minute)
	if w := getReport(r, "u1", "p2"); w.Code != http.StatusNotFound {
		t.Fatalf("foreign pet: status %d, want 404 (%s)", w.Code, w.Body.String())
	}
}

func TestVetCardCachesByData(t *testing.T) {
	r, fake, _ := reportTestServer(t, time.Hour)

	w := getReport(r, "u1", "p1")
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Body.String(), "%PDF-") {
		t.Fatalf("first: status %d, body %.40q", w.Code, w.Body.String())
	}
	if w.Header().Get("X-Report-Cached") != "false" {
		t.Errorf("first: X-Report-Cached = %q, want false", w.Header().Get("X-Report-Cached"))
	}
	first := w.Body.String()
	loads := fake.loads()

	// Те же данные — отдаём готовый PDF, полную выборку не делаем.
	w = getReport(r, "u1", "p1")
	if w.Header().Get("X-Report-Cached") != "true" || w.Header().Get("X-Report-Stale") != "false" {
		t.Errorf("repeat: cached=%q stale=%q", w.Header().Get("X-Report-Cached"), w.Header().Get("X-Report-Stale"))
	}
	if fake.loads() != loads {
		t.Errorf("repeat triggered a full load")
	}
	if fake.saves != 1 {
		t.Errorf("pdf_reports saves = %d, want 1", fake.saves)
	}

	// Данные изменились, но пауза между генерациями ещё не прошла — старая
	// версия с пометкой и Retry-After.
	fake.touch("weights", "w2", "2026-08-12 09:00:00.000Z")
	w = getReport(r, "u1", "p1")
	if w.Code != http.StatusOK || w.Header().Get("X-Report-Stale") != "true" || w.Header().Get("Retry-After") == "" {
		t.Errorf("changed within cooldown: status %d stale=%q retry=%q",
			w.Code, w.Header().Get("X-Report-Stale"), w.Header().Get("Retry-After"))
	}
	if w.Body.String() != first {
		t.Errorf("stale response is not the stored PDF")
	}
}

func TestVetCardRegeneratesAfterChange(t *testing.T) {
	r, fake, _ := reportTestServer(t, 0)
	getReport(r, "u1", "p1")
	loads := fake.loads()

	fake.touch("pills", "pl1", "2026-08-12 09:00:00.000Z")
	w := getReport(r, "u1", "p1")
	if w.Header().Get("X-Report-Cached") != "false" || fake.loads() == loads {
		t.Errorf("changed data was not regenerated: cached=%q", w.Header().Get("X-Report-Cached"))
	}
	// Новая версия заменяет файл в той же записи, а не плодит записи.
	if len(fake.reports) != 1 || len(fake.files) != 1 || fake.saves != 2 {
		t.Errorf("reports=%d files=%d saves=%d, want 1/1/2", len(fake.reports), len(fake.files), fake.saves)
	}
}

func TestReportLimiterDaily(t *testing.T) {
	l := newReportLimiter(0, 0, 2)
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		if ok, _ := l.allowGenerate("u", now); !ok {
			t.Fatalf("generation %d denied", i+1)
		}
		l.recordGenerate("u", now)
	}
	if ok, wait := l.allowGenerate("u", now); ok || wait != 12*time.Hour {
		t.Errorf("third generation: ok=%v wait=%s, want denied until midnight UTC", ok, wait)
	}
	if ok, _ := l.allowGenerate("u", now.Add(13*time.Hour)); !ok {
		t.Errorf("next day still denied")
	}
}

func TestReportLimiterPerMinute(t *testing.T) {
	l := newReportLimiter(2, 0, 0)
	now := time.Now()
	l.allowRequest("u", now)
	l.allowRequest("u", now)
	if ok, _ := l.allowRequest("u", now.Add(10*time.Second)); ok {
		t.Error("third request within a minute allowed")
	}
	if ok, _ := l.allowRequest("u", now.Add(61*time.Second)); !ok {
		t.Error("request after the window denied")
	}
}
