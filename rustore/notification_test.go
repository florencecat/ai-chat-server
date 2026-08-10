package rustore

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"testing"
)

// encryptForTest шифрует полезную нагрузку так же, как это делает RuStore:
// base64( IV(12) || ciphertext || tag(16) ), AES-256-GCM.
func encryptForTest(t *testing.T, key, plain []byte) string {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	gcm, err := cipher.NewGCMWithTagSize(block, 16)
	if err != nil {
		t.Fatalf("gcm: %v", err)
	}
	iv := make([]byte, 12)
	if _, err := rand.Read(iv); err != nil {
		t.Fatalf("iv: %v", err)
	}
	return base64.StdEncoding.EncodeToString(gcm.Seal(iv, iv, plain, nil))
}

func testKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("key: %v", err)
	}
	return key
}

func TestDecryptNotificationSubscriptionEvent(t *testing.T) {
	key := testKey(t)
	plain := `{"app_id":12345,"notification_type":"SUBSCRIPTION_EVENT","data":"{\"product_code\":\"premium_month\",\"subscription_event_type\":\"RENEWED\",\"status_new\":\"ACTIVE\",\"period_new\":\"MAIN\",\"autorenewing\":true,\"invoice_id\":\"123\",\"purchase_id\":\"3aa0c7bd-964e-4562-b218-fe365adb4ae3\"}"}`

	n, err := DecryptNotification(key, encryptForTest(t, key, []byte(plain)))
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if n.AppID != 12345 || n.Kind() != NotifySubscriptionEvent || n.Sandbox() {
		t.Fatalf("unexpected envelope: %+v", n)
	}

	ev, err := n.SubscriptionEvent()
	if err != nil {
		t.Fatalf("parse event: %v", err)
	}
	if ev.SubscriptionEventType != EventRenewed {
		t.Errorf("event type = %q, want %q", ev.SubscriptionEventType, EventRenewed)
	}
	if ev.PurchaseID != "3aa0c7bd-964e-4562-b218-fe365adb4ae3" {
		t.Errorf("purchase id = %q", ev.PurchaseID)
	}
	if !ev.AutoRenewing {
		t.Error("autorenewing = false, want true")
	}
}

func TestDecryptNotificationRejectsWrongKey(t *testing.T) {
	payload := encryptForTest(t, testKey(t), []byte(`{"app_id":1,"notification_type":"TEST_EVENT","data":"{}"}`))

	// Подделать уведомление без ключа из консоли нельзя — GCM не даст.
	if _, err := DecryptNotification(testKey(t), payload); err == nil {
		t.Fatal("expected decryption to fail with a different key")
	}
}

func TestNotificationSandboxSuffix(t *testing.T) {
	n := Notification{NotificationType: "SUBSCRIPTION_EVENT_SANDBOX"}
	if !n.Sandbox() {
		t.Error("Sandbox() = false, want true")
	}
	if n.Kind() != NotifySubscriptionEvent {
		t.Errorf("Kind() = %q, want %q", n.Kind(), NotifySubscriptionEvent)
	}
}

func TestParseNotificationKey(t *testing.T) {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i)
	}

	for name, encoded := range map[string]string{
		"base64": base64.StdEncoding.EncodeToString(raw),
		"hex":    "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f",
	} {
		key, err := ParseNotificationKey(encoded)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(key) != 32 {
			t.Errorf("%s: key length = %d, want 32", name, len(key))
		}
	}

	if _, err := ParseNotificationKey("too-short"); err == nil {
		t.Error("expected error for a key that is not 32 bytes")
	}
}
