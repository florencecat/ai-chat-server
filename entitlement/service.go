package entitlement

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"ai-server/rustore"
)

var (
	// ErrNotFound — прав для пользователя/покупки в хранилище нет.
	ErrNotFound = errors.New("entitlement: not found")
	// ErrDisabled — верификация недоступна (RuStore-интеграция выключена).
	ErrDisabled = errors.New("entitlement: rustore verification is disabled")
	// ErrNotConfirmed — покупка найдена, но не оплачена.
	ErrNotConfirmed = errors.New("entitlement: purchase is not paid")
	// ErrPurchaseTaken — покупка уже привязана к другому пользователю.
	ErrPurchaseTaken = errors.New("entitlement: purchase belongs to another user")
	// ErrUnknownPurchase — RuStore не знает такой purchaseId.
	ErrUnknownPurchase = errors.New("entitlement: unknown purchase")
)

// Store — хранилище прав (реализовано поверх PocketBase).
// ByUser/ByPurchase возвращают ErrNotFound, если записи нет.
type Store interface {
	ByUser(userID string) (*Entitlement, error)
	ByPurchase(purchaseID string) (*Entitlement, error)
	Upsert(e *Entitlement) error
}

// refreshWindow — за сколько до истечения подписки имеет смысл сходить в
// RuStore за свежими данными на чтении (вебхук о продлении мог опоздать).
const refreshWindow = 5 * time.Minute

// Service — прикладная логика прав: чтение, верификация покупок и обработка
// вебхуков. Единственное место, где решается, что такое «премиум».
type Service struct {
	store Store
	ru    *rustore.Client // nil, если интеграция выключена
	plans Plans
	grace time.Duration
}

func NewService(store Store, ru *rustore.Client, plans Plans, grace time.Duration) *Service {
	return &Service{store: store, ru: ru, plans: plans, grace: grace}
}

// Plans — планы, с которыми собран сервис.
func (s *Service) Plans() Plans { return s.plans }

// Enabled сообщает, доступна ли верификация покупок.
func (s *Service) Enabled() bool { return s.ru != nil }

// Current — права пользователя по кэшу в PocketBase, без похода в RuStore.
// Используется на горячем пути /chat, поэтому не должна ни падать, ни ждать
// внешний API: при любой ошибке отдаём бесплатный тариф.
func (s *Service) Current(userID string) Entitlement {
	return s.decorate(s.load(userID))
}

// Fetch — права для клиентского EntitlementApi.fetch(). В отличие от Current
// подтягивает свежие данные из RuStore, если подписка вот-вот истечёт или уже
// истекла: так пользователь не теряет премиум из-за потерянного вебхука.
func (s *Service) Fetch(userID string) Entitlement {
	e := s.load(userID)
	if s.needsRefresh(e) {
		refreshed, err := s.refresh(e)
		if err != nil {
			log.Printf("entitlement: refresh for user %s failed: %v", userID, err)
		} else {
			s.persist(&refreshed)
			if err := s.store.Upsert(&refreshed); err != nil {
				log.Printf("entitlement: persist refreshed for user %s: %v", userID, err)
			}
			e = refreshed
		}
	}
	return s.decorate(e)
}

// Verify проверяет покупку в RuStore, сохраняет права и подтверждает доставку.
// Соответствует POST /verify клиентского EntitlementApi.
func (s *Service) Verify(userID, purchaseID string) (Entitlement, error) {
	// Одна покупка — один пользователь: иначе чужой purchaseId, подсмотренный
	// в логах или у знакомого, раздавал бы премиум всем подряд. Проверка идёт
	// первой, чтобы не зависеть от доступности public-api.
	switch existing, err := s.store.ByPurchase(purchaseID); {
	case err == nil && existing.UserID != "" && existing.UserID != userID:
		return Entitlement{}, ErrPurchaseTaken
	case err != nil && !errors.Is(err, ErrNotFound):
		return Entitlement{}, err
	}

	if s.ru == nil {
		return Entitlement{}, ErrDisabled
	}

	productID, sub, err := s.ru.FindSubscription(purchaseID)
	if err != nil {
		if errors.Is(err, rustore.ErrUnknownProduct) || errors.Is(err, rustore.ErrNotFound) {
			return Entitlement{}, ErrUnknownPurchase
		}
		return Entitlement{}, err
	}
	if !sub.Paid() {
		return Entitlement{}, ErrNotConfirmed
	}

	e := fromSubscription(userID, productID, purchaseID, sub, s.grace)
	s.persist(&e)
	if err := s.store.Upsert(&e); err != nil {
		return Entitlement{}, err
	}

	// Доставку подтверждаем после записи прав: если RuStore ответит ошибкой,
	// пользователь премиум уже получил, а acknowledge повторится при
	// следующем Fetch/вебхуке.
	if !sub.Acknowledged() {
		if err := s.ru.Acknowledge(productID, purchaseID); err != nil {
			log.Printf("entitlement: acknowledge %s/%s failed: %v", productID, purchaseID, err)
		}
	}

	return s.decorate(e), nil
}

// ── Вебхуки ───────────────────────────────────────────────────────────────────

// HandleNotification применяет уведомление RuStore к правам пользователя.
// Пользователь находится по purchase_id, записанному при верификации: сам
// RuStore про наши userId ничего не знает.
func (s *Service) HandleNotification(n rustore.Notification) error {
	switch n.Kind() {
	case rustore.NotifySubscriptionEvent:
		ev, err := n.SubscriptionEvent()
		if err != nil {
			return err
		}
		return s.applySubscriptionEvent(ev)
	case rustore.NotifyInvoiceStatus:
		ev, err := n.InvoiceEvent()
		if err != nil {
			return err
		}
		return s.applyInvoiceEvent(ev)
	case rustore.NotifyTestEvent:
		log.Printf("entitlement: rustore test notification received")
		return nil
	default:
		log.Printf("entitlement: unhandled rustore notification type %q", n.NotificationType)
		return nil
	}
}

func (s *Service) applySubscriptionEvent(ev rustore.SubscriptionEvent) error {
	rec, err := s.store.ByPurchase(ev.PurchaseID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// Покупка ещё не привязана к пользователю: клиент не успел вызвать
			// /verify. Ничего страшного — /verify запишет актуальное состояние.
			log.Printf("entitlement: notification for unknown purchase %s (%s)", ev.PurchaseID, ev.SubscriptionEventType)
			return nil
		}
		return err
	}

	rec.State = ev.StatusNew
	rec.Period = ev.PeriodNew
	rec.AutoRenewing = ev.AutoRenewing
	if rec.ProductID == "" {
		rec.ProductID = ev.ProductCode
	}
	if ev.OrderID != "" {
		rec.OrderID = ev.OrderID
	}
	if ev.InvoiceID != "" {
		rec.InvoiceID = ev.InvoiceID
	}

	switch ev.SubscriptionEventType {
	case rustore.EventActivated, rustore.EventRenewed, rustore.EventResumed:
		// Дату окончания берём не из события, а из v4 — это источник истины.
		if refreshed, err := s.refresh(*rec); err == nil {
			refreshed.State = ev.StatusNew
			refreshed.Period = ev.PeriodNew
			*rec = refreshed
		} else {
			log.Printf("entitlement: refresh on %s for purchase %s: %v", ev.SubscriptionEventType, ev.PurchaseID, err)
			rec.Active = true
		}
	case rustore.EventCancelled:
		// Отключено автопродление: доступ сохраняется до конца оплаченного
		// периода, поэтому Active не трогаем.
		rec.AutoRenewing = false
	case rustore.EventPaymentFailed:
		// Подписка уходит в GRACE/HOLD. Пока RuStore не закрыл её, доступ
		// оставляем и полагаемся на expires_at.
		if refreshed, err := s.refresh(*rec); err == nil {
			*rec = refreshed
			rec.State = ev.StatusNew
			rec.Period = ev.PeriodNew
		}
	case rustore.EventClosed:
		rec.Active = false
		rec.AutoRenewing = false
		now := time.Now().UTC()
		if rec.ExpiresAt == nil || rec.ExpiresAt.After(now) {
			rec.ExpiresAt = &now
		}
	}

	s.persist(rec)
	return s.store.Upsert(rec)
}

// refundStatuses — статусы платежа, при которых права надо отобрать сразу.
var refundStatuses = map[string]bool{
	"REFUNDED": true, "REFUNDING": true, "REVERSED": true,
	"CANCELLED": true, "REJECTED": true, "EXPIRED": true,
}

func (s *Service) applyInvoiceEvent(ev rustore.InvoiceEvent) error {
	if !refundStatuses[normalizeStatus(ev.StatusNew)] {
		return nil
	}
	rec, err := s.store.ByPurchase(ev.PurchaseID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}

	rec.Active = false
	rec.AutoRenewing = false
	rec.State = normalizeStatus(ev.StatusNew)
	now := time.Now().UTC()
	rec.ExpiresAt = &now
	s.persist(rec)
	return s.store.Upsert(rec)
}

func normalizeStatus(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

// persist приводит запись в консистентный вид перед записью в хранилище:
// колонка tier не должна расходиться с active, иначе выборки в админке
// PocketBase будут врать.
func (s *Service) persist(e *Entitlement) {
	e.UpdatedAt = time.Now().UTC()
	if e.Live(e.UpdatedAt, s.grace) {
		e.Tier = TierPremium
	} else {
		e.Tier = TierFree
	}
}

// ── Внутреннее ────────────────────────────────────────────────────────────────

func (s *Service) load(userID string) Entitlement {
	rec, err := s.store.ByUser(userID)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			log.Printf("entitlement: load for user %s: %v", userID, err)
		}
		return Free()
	}
	return *rec
}

// decorate приводит запись к тому, что видит клиент: пересчитывает активность
// на «сейчас» и подставляет лимиты тарифа.
func (s *Service) decorate(e Entitlement) Entitlement {
	if e.Live(time.Now().UTC(), s.grace) {
		e.Tier = TierPremium
		e.Active = true
	} else {
		e.Tier = TierFree
		e.Active = false
	}
	p := s.plans.For(e.Tier)
	e.Limits = Limits{RequestsPerDay: p.QuotaPerDay, RequestsPerMinute: p.QuotaPerMinute}
	return e
}

// needsRefresh — стоит ли перечитать подписку из RuStore.
func (s *Service) needsRefresh(e Entitlement) bool {
	if s.ru == nil || e.PurchaseID == "" || e.Source == SourceManual {
		return false
	}
	if e.ExpiresAt == nil {
		return false
	}
	return time.Now().UTC().After(e.ExpiresAt.Add(-refreshWindow))
}

// refresh перечитывает подписку из Subscription Data v4 и пересобирает запись.
func (s *Service) refresh(e Entitlement) (Entitlement, error) {
	if s.ru == nil {
		return e, ErrDisabled
	}
	if e.PurchaseID == "" {
		return e, ErrUnknownPurchase
	}

	productID := e.ProductID
	var sub *rustore.SubscriptionPurchase
	var err error
	if productID != "" {
		sub, err = s.ru.Subscription(productID, e.PurchaseID)
		if errors.Is(err, rustore.ErrNotFound) {
			// Продукт мог смениться (апгрейд тарифа) — ищем заново.
			productID, sub, err = s.ru.FindSubscription(e.PurchaseID)
		}
	} else {
		productID, sub, err = s.ru.FindSubscription(e.PurchaseID)
	}
	if err != nil {
		return e, fmt.Errorf("refresh purchase %s: %w", e.PurchaseID, err)
	}

	refreshed := fromSubscription(e.UserID, productID, e.PurchaseID, sub, s.grace)
	refreshed.InvoiceID = e.InvoiceID
	if refreshed.OrderID == "" {
		refreshed.OrderID = e.OrderID
	}
	return refreshed, nil
}

// fromSubscription собирает Entitlement из ответа Subscription Data v4.
func fromSubscription(userID, productID, purchaseID string, sub *rustore.SubscriptionPurchase, grace time.Duration) Entitlement {
	e := Entitlement{
		UserID:       userID,
		ProductID:    productID,
		PurchaseID:   purchaseID,
		OrderID:      sub.OrderID,
		AutoRenewing: sub.AutoRenewing,
		Source:       SourceRuStore,
		UpdatedAt:    time.Now().UTC(),
	}
	if exp, ok := sub.ExpiresAt(); ok {
		e.ExpiresAt = &exp
	}

	// Активна, если оплачена и оплаченный период (плюс grace) ещё не кончился.
	e.Active = sub.Paid()
	if e.Active && e.ExpiresAt != nil && !time.Now().UTC().Before(e.ExpiresAt.Add(grace)) {
		e.Active = false
	}
	if e.Active {
		e.Tier = TierPremium
		e.State = "ACTIVE"
	} else {
		e.Tier = TierFree
		e.State = "CLOSED"
	}
	return e
}
