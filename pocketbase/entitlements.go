package pocketbase

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"ai-server/entitlement"
)

// entitlementsCollection — коллекция PocketBase, где живут права доступа.
// Схема описана в README (раздел «Коллекция entitlements»).
const entitlementsCollection = "entitlements"

// entitlementMu сериализует upsert: между «найти запись» и «создать» есть
// гонка, а уникальный индекс по user в PocketBase превратил бы её в 400.
var entitlementMu sync.Mutex

// entitlementRecord — строка коллекции entitlements.
type entitlementRecord struct {
	ID           string `json:"id,omitempty"`
	User         string `json:"user"`
	Tier         string `json:"tier"`
	Active       bool   `json:"active"`
	ProductID    string `json:"product_id"`
	PurchaseID   string `json:"purchase_id"`
	OrderID      string `json:"order_id"`
	InvoiceID    string `json:"invoice_id"`
	State        string `json:"state"`
	Period       string `json:"period"`
	ExpiresAt    string `json:"expires_at"`
	AutoRenewing bool   `json:"auto_renewing"`
	Source       string `json:"source"`
}

func (r entitlementRecord) toDomain() *entitlement.Entitlement {
	e := &entitlement.Entitlement{
		UserID:       r.User,
		Tier:         entitlement.Tier(r.Tier),
		Active:       r.Active,
		ProductID:    r.ProductID,
		PurchaseID:   r.PurchaseID,
		OrderID:      r.OrderID,
		InvoiceID:    r.InvoiceID,
		State:        r.State,
		Period:       r.Period,
		AutoRenewing: r.AutoRenewing,
		Source:       r.Source,
	}
	if t, ok := parsePBTime(r.ExpiresAt); ok {
		e.ExpiresAt = &t
	}
	return e
}

func fromDomain(e *entitlement.Entitlement) entitlementRecord {
	r := entitlementRecord{
		User:         e.UserID,
		Tier:         string(e.Tier),
		Active:       e.Active,
		ProductID:    e.ProductID,
		PurchaseID:   e.PurchaseID,
		OrderID:      e.OrderID,
		InvoiceID:    e.InvoiceID,
		State:        e.State,
		Period:       e.Period,
		AutoRenewing: e.AutoRenewing,
		Source:       e.Source,
	}
	if e.ExpiresAt != nil {
		r.ExpiresAt = e.ExpiresAt.UTC().Format(pbTimeLayout)
	}
	return r
}

// ── Store (реализация entitlement.Store) ──────────────────────────────────────

// ByUser возвращает права пользователя. entitlement.ErrNotFound, если записи нет.
func (c *Client) ByUser(userID string) (*entitlement.Entitlement, error) {
	return c.findEntitlement("user", userID)
}

// ByPurchase ищет права по идентификатору покупки — так вебхук RuStore
// понимает, чьи права обновлять.
func (c *Client) ByPurchase(purchaseID string) (*entitlement.Entitlement, error) {
	return c.findEntitlement("purchase_id", purchaseID)
}

func (c *Client) findEntitlement(field, value string) (*entitlement.Entitlement, error) {
	if value == "" {
		return nil, entitlement.ErrNotFound
	}
	adminToken, err := c.getAdminToken()
	if err != nil {
		return nil, err
	}

	endpoint, _ := url.Parse(c.cfg.PBUrl + "/api/collections/" + entitlementsCollection + "/records")
	q := endpoint.Query()
	q.Set("filter", fmt.Sprintf("(%s='%s')", field, escapePBFilter(value)))
	q.Set("perPage", "1")
	q.Set("sort", "-updated")
	endpoint.RawQuery = q.Encode()

	req, _ := http.NewRequest(http.MethodGet, endpoint.String(), nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pb find entitlement: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("pb find entitlement: status %d: %s", resp.StatusCode, body)
	}

	var result struct {
		Items []entitlementRecord `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("pb find entitlement decode: %w", err)
	}
	if len(result.Items) == 0 {
		return nil, entitlement.ErrNotFound
	}
	return result.Items[0].toDomain(), nil
}

// Upsert создаёт или обновляет запись прав. Ключ — user: у пользователя всегда
// ровно одна строка прав, покупки её перезаписывают.
func (c *Client) Upsert(e *entitlement.Entitlement) error {
	if e.UserID == "" {
		return fmt.Errorf("pb upsert entitlement: empty user id")
	}

	entitlementMu.Lock()
	defer entitlementMu.Unlock()

	adminToken, err := c.getAdminToken()
	if err != nil {
		return err
	}

	existingID, err := c.entitlementRecordID(adminToken, e.UserID)
	if err != nil {
		return err
	}

	body, err := json.Marshal(fromDomain(e))
	if err != nil {
		return fmt.Errorf("pb upsert entitlement marshal: %w", err)
	}

	method := http.MethodPost
	endpoint := c.cfg.PBUrl + "/api/collections/" + entitlementsCollection + "/records"
	if existingID != "" {
		method = http.MethodPatch
		endpoint += "/" + existingID
	}

	req, _ := http.NewRequest(method, endpoint, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("pb upsert entitlement: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("pb upsert entitlement: status %d: %s", resp.StatusCode, respBody)
	}
	io.Copy(io.Discard, resp.Body)

	e.UpdatedAt = time.Now().UTC()
	return nil
}

// entitlementRecordID возвращает id существующей записи прав пользователя
// либо пустую строку.
func (c *Client) entitlementRecordID(adminToken, userID string) (string, error) {
	endpoint, _ := url.Parse(c.cfg.PBUrl + "/api/collections/" + entitlementsCollection + "/records")
	q := endpoint.Query()
	q.Set("filter", fmt.Sprintf("(user='%s')", escapePBFilter(userID)))
	q.Set("perPage", "1")
	endpoint.RawQuery = q.Encode()

	req, _ := http.NewRequest(http.MethodGet, endpoint.String(), nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("pb lookup entitlement: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("pb lookup entitlement: status %d: %s", resp.StatusCode, body)
	}

	var result struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("pb lookup entitlement decode: %w", err)
	}
	if len(result.Items) == 0 {
		return "", nil
	}
	return result.Items[0].ID, nil
}

// escapePBFilter экранирует значение, подставляемое в filter-выражение
// PocketBase. purchaseId приходит от клиента, поэтому кавычку и слеш надо
// нейтрализовать, иначе фильтр можно подменить.
func escapePBFilter(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	return strings.ReplaceAll(v, `'`, `\'`)
}
