// Package entitlement — доменная модель прав доступа (премиум-статуса).
//
// Источник истины по правам — этот бэкенд: покупка верифицируется в RuStore
// public-api, результат кладётся в PocketBase и живёт независимо от клиента.
// Клиент (Flutter, EntitlementApi) только читает готовый Entitlement.
package entitlement

import "time"

// Tier — тариф пользователя. Значения совпадают с теми, что ждёт клиент.
type Tier string

const (
	TierFree    Tier = "free"
	TierPremium Tier = "premium"
)

// Source — откуда взялись права.
const (
	SourceRuStore = "rustore"
	SourceManual  = "manual" // выдано руками через админку PocketBase
)

// Entitlement — права пользователя в том виде, в котором они уезжают клиенту
// и хранятся в PocketBase. Поля покупки нужны вебхуку: по purchase_id он
// находит, чьи права обновлять.
type Entitlement struct {
	UserID string `json:"-"`

	Tier   Tier `json:"tier"`
	Active bool `json:"active"`

	ProductID  string `json:"product_id,omitempty"`
	PurchaseID string `json:"purchase_id,omitempty"`
	OrderID    string `json:"order_id,omitempty"`
	InvoiceID  string `json:"invoice_id,omitempty"`

	// State / Period — как их видит RuStore: ACTIVE|PAUSED|CLOSED|TERMINATED
	// и TRIAL|PROMO|MAIN|GRACE|HOLD. Для диагностики и для UI («на паузе»).
	State  string `json:"state,omitempty"`
	Period string `json:"period,omitempty"`

	// ExpiresAt == nil означает бессрочные права (выданные вручную).
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	AutoRenewing bool       `json:"auto_renewing"`

	Source    string    `json:"source,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`

	// Limits заполняется на чтении из плана — клиенту полезно знать, сколько
	// запросов ему осталось по тарифу, не зашивая числа в приложение.
	Limits Limits `json:"limits"`
}

// Limits — лимиты тарифа, отдаваемые клиенту.
type Limits struct {
	RequestsPerDay    int `json:"requests_per_day"`
	RequestsPerMinute int `json:"requests_per_minute"`
}

// Free — бесплатный тариф. Ответ по умолчанию, если прав нет либо хранилище
// недоступно: деградируем в сторону «без премиума», но не в сторону 500.
func Free() Entitlement {
	return Entitlement{Tier: TierFree, Active: false, UpdatedAt: time.Now().UTC()}
}

// Live сообщает, действуют ли права на момент now с учётом grace-периода
// (льготные сутки после expiry — страховка от опоздавшего вебхука о продлении).
func (e Entitlement) Live(now time.Time, grace time.Duration) bool {
	if !e.Active {
		return false
	}
	if e.ExpiresAt == nil {
		return true
	}
	return now.Before(e.ExpiresAt.Add(grace))
}

// ── Планы ─────────────────────────────────────────────────────────────────────

// Plan — что даёт тариф: какая модель и какие квоты. Именно здесь бэкенд
// «сам решает базовая/улучшенная модель и дневной лимит».
type Plan struct {
	Tier           Tier
	Model          string // пустая строка — модель провайдера по умолчанию
	QuotaPerDay    int
	QuotaPerMinute int
}

// Plans — набор планов, собирается из конфигурации на старте.
type Plans struct {
	Free    Plan
	Premium Plan
}

// For возвращает план по тарифу; неизвестный тариф трактуется как бесплатный.
func (p Plans) For(t Tier) Plan {
	if t == TierPremium {
		return p.Premium
	}
	return p.Free
}
