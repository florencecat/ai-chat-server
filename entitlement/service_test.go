package entitlement

import (
	"errors"
	"testing"
	"time"

	"ai-server/rustore"
)

// memStore — хранилище прав в памяти для тестов.
type memStore struct {
	byUser map[string]*Entitlement
}

func newMemStore(records ...*Entitlement) *memStore {
	s := &memStore{byUser: map[string]*Entitlement{}}
	for _, r := range records {
		copied := *r
		s.byUser[r.UserID] = &copied
	}
	return s
}

func (s *memStore) ByUser(userID string) (*Entitlement, error) {
	e, ok := s.byUser[userID]
	if !ok {
		return nil, ErrNotFound
	}
	copied := *e
	return &copied, nil
}

func (s *memStore) ByPurchase(purchaseID string) (*Entitlement, error) {
	for _, e := range s.byUser {
		if e.PurchaseID == purchaseID {
			copied := *e
			return &copied, nil
		}
	}
	return nil, ErrNotFound
}

func (s *memStore) Upsert(e *Entitlement) error {
	copied := *e
	s.byUser[e.UserID] = &copied
	return nil
}

func testPlans() Plans {
	return Plans{
		Free:    Plan{Tier: TierFree, Model: "base", QuotaPerDay: 15, QuotaPerMinute: 1},
		Premium: Plan{Tier: TierPremium, Model: "pro", QuotaPerDay: 200, QuotaPerMinute: 5},
	}
}

func ptr(t time.Time) *time.Time { return &t }

func TestCurrentFallsBackToFreeWithoutRecord(t *testing.T) {
	s := NewService(newMemStore(), nil, testPlans(), 0)

	e := s.Current("user-1")
	if e.Tier != TierFree || e.Active {
		t.Fatalf("got %+v, want inactive free", e)
	}
	if e.Limits.RequestsPerDay != 15 || e.Limits.RequestsPerMinute != 1 {
		t.Errorf("limits = %+v, want free plan limits", e.Limits)
	}
}

func TestCurrentReturnsPremiumForLiveSubscription(t *testing.T) {
	now := time.Now().UTC()
	s := NewService(newMemStore(&Entitlement{
		UserID:     "user-1",
		Active:     true,
		Tier:       TierPremium,
		PurchaseID: "p-1",
		ExpiresAt:  ptr(now.Add(24 * time.Hour)),
	}), nil, testPlans(), 0)

	e := s.Current("user-1")
	if e.Tier != TierPremium || !e.Active {
		t.Fatalf("got %+v, want active premium", e)
	}
	if e.Limits.RequestsPerDay != 200 {
		t.Errorf("limits = %+v, want premium plan limits", e.Limits)
	}
}

func TestCurrentDowngradesExpiredSubscription(t *testing.T) {
	now := time.Now().UTC()
	rec := &Entitlement{
		UserID:     "user-1",
		Active:     true,
		Tier:       TierPremium,
		PurchaseID: "p-1",
		ExpiresAt:  ptr(now.Add(-2 * time.Hour)),
	}

	// Без grace-периода истёкшая подписка — уже бесплатный тариф.
	if e := NewService(newMemStore(rec), nil, testPlans(), 0).Current("user-1"); e.Tier != TierFree || e.Active {
		t.Errorf("without grace: got %+v, want free", e)
	}

	// С grace доступ ещё живёт: вебхук о продлении мог опоздать.
	if e := NewService(newMemStore(rec), nil, testPlans(), 24*time.Hour).Current("user-1"); e.Tier != TierPremium {
		t.Errorf("within grace: got %+v, want premium", e)
	}
}

func TestVerifyRejectsPurchaseClaimedByAnotherUser(t *testing.T) {
	store := newMemStore(&Entitlement{UserID: "owner", PurchaseID: "p-1", Active: true})
	// ru == nil, но проверка владельца идёт раньше похода в RuStore — так
	// чужой purchaseId не пройдёт даже при доступном public-api.
	s := NewService(store, nil, testPlans(), 0)

	if _, err := s.Verify("intruder", "p-1"); !errors.Is(err, ErrPurchaseTaken) {
		t.Fatalf("err = %v, want ErrPurchaseTaken", err)
	}
}

func TestVerifyWithoutRuStoreIsDisabled(t *testing.T) {
	s := NewService(newMemStore(), nil, testPlans(), 0)
	if _, err := s.Verify("user-1", "p-new"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("err = %v, want ErrDisabled", err)
	}
}

func TestHandleNotificationClosedRevokesAccess(t *testing.T) {
	store := newMemStore(&Entitlement{
		UserID:       "user-1",
		PurchaseID:   "p-1",
		ProductID:    "premium_month",
		Active:       true,
		AutoRenewing: true,
		ExpiresAt:    ptr(time.Now().UTC().Add(20 * 24 * time.Hour)),
	})
	s := NewService(store, nil, testPlans(), 0)

	err := s.HandleNotification(notificationFor(t, `{
		"product_code":"premium_month",
		"subscription_event_type":"CLOSED",
		"status_new":"CLOSED",
		"autorenewing":false,
		"purchase_id":"p-1"
	}`))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}

	got, err := store.ByUser("user-1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Active || got.AutoRenewing {
		t.Errorf("got %+v, want revoked", got)
	}
	if got.ExpiresAt == nil || got.ExpiresAt.After(time.Now().UTC().Add(time.Minute)) {
		t.Errorf("expires_at = %v, want moved to now", got.ExpiresAt)
	}
	if e := s.Current("user-1"); e.Tier != TierFree {
		t.Errorf("tier after CLOSED = %q, want free", e.Tier)
	}
}

func TestHandleNotificationCancelledKeepsAccessUntilExpiry(t *testing.T) {
	expiry := time.Now().UTC().Add(20 * 24 * time.Hour)
	store := newMemStore(&Entitlement{
		UserID: "user-1", PurchaseID: "p-1", ProductID: "premium_month",
		Active: true, AutoRenewing: true, ExpiresAt: ptr(expiry),
	})
	s := NewService(store, nil, testPlans(), 0)

	// Отмена автопродления не должна отбирать оплаченный период.
	if err := s.HandleNotification(notificationFor(t, `{
		"subscription_event_type":"CANCELLED","status_new":"ACTIVE",
		"autorenewing":false,"purchase_id":"p-1"
	}`)); err != nil {
		t.Fatalf("handle: %v", err)
	}

	got, _ := store.ByUser("user-1")
	if !got.Active || got.AutoRenewing {
		t.Errorf("got %+v, want active without autorenew", got)
	}
	if e := s.Current("user-1"); e.Tier != TierPremium {
		t.Errorf("tier after CANCELLED = %q, want premium", e.Tier)
	}
}

func TestHandleNotificationRefundRevokesAccess(t *testing.T) {
	store := newMemStore(&Entitlement{
		UserID: "user-1", PurchaseID: "p-1", Active: true,
		ExpiresAt: ptr(time.Now().UTC().Add(20 * 24 * time.Hour)),
	})
	s := NewService(store, nil, testPlans(), 0)

	n := rustore.Notification{
		NotificationType: rustore.NotifyInvoiceStatus,
		Data:             `{"status_new":"refunded","status_old":"paid","purchase_id":"p-1","invoice_id":"123"}`,
	}
	if err := s.HandleNotification(n); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if got, _ := store.ByUser("user-1"); got.Active {
		t.Errorf("got %+v, want revoked after refund", got)
	}
}

func TestHandleNotificationForUnknownPurchaseIsNoop(t *testing.T) {
	s := NewService(newMemStore(), nil, testPlans(), 0)
	if err := s.HandleNotification(notificationFor(t, `{
		"subscription_event_type":"RENEWED","status_new":"ACTIVE","purchase_id":"p-unknown"
	}`)); err != nil {
		t.Fatalf("handle: %v", err)
	}
}

func notificationFor(t *testing.T, data string) rustore.Notification {
	t.Helper()
	return rustore.Notification{
		NotificationType: rustore.NotifySubscriptionEvent,
		Data:             data,
	}
}
