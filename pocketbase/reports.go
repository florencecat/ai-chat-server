package pocketbase

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"time"
)

// reportsCollection — последние PDF-карточки, по одной записи на питомца.
// Схема описана в README (раздел «Коллекция pdf_reports»). Файл protected,
// правила пустые: отдаёт отчёты только сервер.
const reportsCollection = "pdf_reports"

// maxReportSize — потолок на скачивание: карточка весит ~100 КБ.
const maxReportSize = 10 << 20

// StoredReport — метаданные сохранённого отчёта, без самого файла: чтобы
// понять, годится ли он, файл качать не нужно.
type StoredReport struct {
	ID      string
	Key     string
	File    string
	Updated time.Time
}

type reportRecord struct {
	ID      string `json:"id"`
	Key     string `json:"key"`
	Report  string `json:"report"`
	Updated string `json:"updated"`
}

func (r reportRecord) toStored() *StoredReport {
	s := &StoredReport{ID: r.ID, Key: r.Key, File: r.Report}
	if t, ok := parsePBTime(r.Updated); ok {
		s.Updated = t
	}
	return s
}

// FindReport возвращает сохранённый отчёт питомца или nil, если его нет.
// Владельца здесь не проверяем: вызывающий код уже проверил его отпечатком.
func (c *Client) FindReport(petID string) (*StoredReport, error) {
	adminToken, err := c.getAdminToken()
	if err != nil {
		return nil, err
	}

	endpoint, _ := url.Parse(c.cfg.PBUrl + "/api/collections/" + reportsCollection + "/records")
	q := endpoint.Query()
	q.Set("filter", fmt.Sprintf("(pet='%s')", escapePBFilter(petID)))
	q.Set("perPage", "1")
	q.Set("skipTotal", "1")
	q.Set("sort", "-updated")
	endpoint.RawQuery = q.Encode()

	req, _ := http.NewRequest(http.MethodGet, endpoint.String(), nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pb find report: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("pb find report: status %d: %s", resp.StatusCode, body)
	}

	var result struct {
		Items []reportRecord `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("pb find report decode: %w", err)
	}
	if len(result.Items) == 0 || result.Items[0].Report == "" {
		return nil, nil
	}
	return result.Items[0].toStored(), nil
}

// DownloadReport скачивает PDF. Поле protected, поэтому нужен файловый токен:
// без него PocketBase отдал бы файл любому, кто знает URL.
func (c *Client) DownloadReport(r *StoredReport) ([]byte, error) {
	adminToken, err := c.getAdminToken()
	if err != nil {
		return nil, err
	}

	req, _ := http.NewRequest(http.MethodPost, c.cfg.PBUrl+"/api/files/token", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pb file token: %w", err)
	}
	var tok struct {
		Token string `json:"token"`
	}
	err = json.NewDecoder(resp.Body).Decode(&tok)
	resp.Body.Close()
	if err != nil || tok.Token == "" {
		return nil, fmt.Errorf("pb file token: status %d: %v", resp.StatusCode, err)
	}

	fileURL := fmt.Sprintf("%s/api/files/%s/%s/%s?token=%s", c.cfg.PBUrl, reportsCollection,
		url.PathEscape(r.ID), url.PathEscape(r.File), url.QueryEscape(tok.Token))
	resp, err = c.httpClient.Get(fileURL)
	if err != nil {
		return nil, fmt.Errorf("pb download report: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pb download report: status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxReportSize))
}

// SaveReport создаёт запись отчёта или заменяет файл в существующей. Старый
// файл PocketBase удаляет сам: поле однофайловое.
func (c *Client) SaveReport(existing *StoredReport, petID, key string, pdf []byte) (*StoredReport, error) {
	adminToken, err := c.getAdminToken()
	if err != nil {
		return nil, err
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("pet", petID)
	_ = mw.WriteField("key", key)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="report"; filename="vet-card.pdf"`)
	h.Set("Content-Type", "application/pdf")
	part, _ := mw.CreatePart(h)
	_, _ = part.Write(pdf)
	_ = mw.Close()

	method, endpoint := http.MethodPost, c.cfg.PBUrl+"/api/collections/"+reportsCollection+"/records"
	if existing != nil {
		method, endpoint = http.MethodPatch, endpoint+"/"+url.PathEscape(existing.ID)
	}
	req, _ := http.NewRequest(method, endpoint, &body)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pb save report: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("pb save report: status %d: %s", resp.StatusCode, b)
	}

	var rec reportRecord
	if err := json.NewDecoder(resp.Body).Decode(&rec); err != nil {
		return nil, fmt.Errorf("pb save report decode: %w", err)
	}
	return rec.toStored(), nil
}
