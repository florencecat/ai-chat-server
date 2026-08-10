package rustore

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"testing"
	"time"
)

func TestParsePrivateKeyAcceptsPKCS1PKCS8AndBase64(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	pkcs1 := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	pkcs8Bytes, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	pkcs8 := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8Bytes})

	cases := map[string]string{
		"pkcs1":         string(pkcs1),
		"pkcs8":         string(pkcs8),
		"base64(pkcs8)": base64.StdEncoding.EncodeToString(pkcs8),
	}
	for name, raw := range cases {
		parsed, err := parsePrivateKey(raw)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if parsed.N.Cmp(key.N) != 0 {
			t.Errorf("%s: parsed a different key", name)
		}
	}

	if _, err := parsePrivateKey(""); err == nil {
		t.Error("expected error for an empty key")
	}
	if _, err := parsePrivateKey("not a key"); err == nil {
		t.Error("expected error for garbage input")
	}
}

func TestSignatureTimestampLayout(t *testing.T) {
	// RuStore ждёт timestamp вида 2023-08-11T13:31:17.580+03:00.
	ts := time.Date(2023, 8, 11, 13, 31, 17, 580_000_000,
		time.FixedZone("MSK", 3*60*60)).Format(signatureTimeLayout)
	if want := "2023-08-11T13:31:17.580+03:00"; ts != want {
		t.Errorf("timestamp = %q, want %q", ts, want)
	}
}

func TestClientPathSandboxPrefixAndEscaping(t *testing.T) {
	c := &Client{baseURL: "https://public-api.rustore.ru", packageName: "com.example.app"}

	got := c.path("/v4/subscription/%s/%s/%s", c.packageName, "premium_month", "3aa0c7bd")
	want := "https://public-api.rustore.ru/public/v4/subscription/com.example.app/premium_month/3aa0c7bd"
	if got != want {
		t.Errorf("path = %q, want %q", got, want)
	}

	c.sandbox = true
	got = c.path("/v2/subscription/%s/%s/%s:acknowledge", c.packageName, "premium_month", "3aa0c7bd")
	want = "https://public-api.rustore.ru/public/sandbox/v2/subscription/com.example.app/premium_month/3aa0c7bd:acknowledge"
	if got != want {
		t.Errorf("sandbox path = %q, want %q", got, want)
	}

	// purchaseId приходит от клиента — сегмент должен экранироваться.
	if got := c.path("/v4/subscription/%s/%s/%s", "pkg", "sub", "../../admin"); got != "https://public-api.rustore.ru/public/sandbox/v4/subscription/pkg/sub/..%2F..%2Fadmin" {
		t.Errorf("path traversal not escaped: %q", got)
	}
}

func TestSubscriptionPurchaseState(t *testing.T) {
	paid, trial, pending := 1, 2, 0
	ack := 1

	if !(&SubscriptionPurchase{PaymentState: &paid}).Paid() {
		t.Error("paymentState=1 must count as paid")
	}
	if !(&SubscriptionPurchase{PaymentState: &trial}).Paid() {
		t.Error("paymentState=2 (trial) must count as paid")
	}
	if (&SubscriptionPurchase{PaymentState: &pending}).Paid() {
		t.Error("paymentState=0 must not count as paid")
	}
	if (&SubscriptionPurchase{}).Paid() {
		t.Error("missing paymentState must not count as paid")
	}
	if !(&SubscriptionPurchase{AcknowledgementState: &ack}).Acknowledged() {
		t.Error("acknowledgementState=1 must count as acknowledged")
	}

	sub := &SubscriptionPurchase{ExpiryTimeMillis: "1767225600000"}
	exp, ok := sub.ExpiresAt()
	if !ok || exp.UTC() != time.UnixMilli(1767225600000).UTC() {
		t.Errorf("ExpiresAt = %v, %v", exp, ok)
	}
	if _, ok := (&SubscriptionPurchase{}).ExpiresAt(); ok {
		t.Error("empty expiryTimeMillis must not parse")
	}
}
