package rustore

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Типы уведомлений RuStore (поле notification_type).
const (
	NotifyInvoiceStatus     = "INVOICE_STATUS"
	NotifySubscriptionEvent = "SUBSCRIPTION_EVENT"
	NotifyTestEvent         = "TEST_EVENT"
)

// Типы событий подписки (subscription_event_type).
const (
	EventActivated     = "ACTIVATED"
	EventRenewed       = "RENEWED"
	EventCancelled     = "CANCELLED"
	EventResumed       = "RESUMED"
	EventPaymentFailed = "PAYMENT_FAILED"
	EventClosed        = "CLOSED"
)

// NotificationEnvelope — то, что RuStore шлёт на вебхук: полезная нагрузка
// зашифрована AES-256-GCM ключом из консоли разработчика.
type NotificationEnvelope struct {
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	Payload   string `json:"payload"`
}

// Notification — расшифрованный конверт. Data — это JSON, вложенный строкой.
type Notification struct {
	AppID            int64  `json:"app_id"`
	NotificationType string `json:"notification_type"`
	Data             string `json:"data"`
}

// Sandbox сообщает, что уведомление пришло из песочницы (тип с суффиксом
// _SANDBOX). Такие события не должны менять боевые права, если сервер работает
// не в песочном режиме.
func (n Notification) Sandbox() bool {
	return strings.HasSuffix(n.NotificationType, "_SANDBOX")
}

// Kind возвращает тип события без суффикса песочницы.
func (n Notification) Kind() string {
	return strings.TrimSuffix(n.NotificationType, "_SANDBOX")
}

// SubscriptionEvent — тело уведомления SUBSCRIPTION_EVENT.
type SubscriptionEvent struct {
	ProductCode           string `json:"product_code"`
	EventTime             string `json:"event_time"`
	SubscriptionEventType string `json:"subscription_event_type"`
	StatusNew             string `json:"status_new"`
	StatusOld             string `json:"status_old"`
	PeriodOld             string `json:"period_old"`
	PeriodNew             string `json:"period_new"`
	AutoRenewing          bool   `json:"autorenewing"`
	InvoiceID             string `json:"invoice_id"`
	OrderID               string `json:"order_id"`
	PurchaseID            string `json:"purchase_id"`
	DeveloperPayload      string `json:"developer_payload"`
}

// InvoiceEvent — тело уведомления INVOICE_STATUS.
type InvoiceEvent struct {
	ChangeStatusTime string `json:"change_status_time"`
	ProductCode      string `json:"product_code"`
	StatusNew        string `json:"status_new"`
	StatusOld        string `json:"status_old"`
	PurchaseToken    string `json:"purchase_token"`
	InvoiceID        string `json:"invoice_id"`
	OrderID          string `json:"order_id"`
	PurchaseID       string `json:"purchase_id"`
	DeveloperPayload string `json:"developer_payload"`
}

// SubscriptionEvent разбирает вложенный JSON события подписки.
func (n Notification) SubscriptionEvent() (SubscriptionEvent, error) {
	var ev SubscriptionEvent
	if err := json.Unmarshal([]byte(n.Data), &ev); err != nil {
		return ev, fmt.Errorf("rustore notification: parse subscription data: %w", err)
	}
	return ev, nil
}

// InvoiceEvent разбирает вложенный JSON события платежа.
func (n Notification) InvoiceEvent() (InvoiceEvent, error) {
	var ev InvoiceEvent
	if err := json.Unmarshal([]byte(n.Data), &ev); err != nil {
		return ev, fmt.Errorf("rustore notification: parse invoice data: %w", err)
	}
	return ev, nil
}

// ParseNotificationKey читает ключ расшифровки из конфигурации. Из консоли он
// приезжает строкой base64; поддерживаем также hex и сырые 32 байта.
func ParseNotificationKey(raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("rustore notification key is empty")
	}
	if key, err := base64.StdEncoding.DecodeString(raw); err == nil && len(key) == 32 {
		return key, nil
	}
	if key, err := hex.DecodeString(raw); err == nil && len(key) == 32 {
		return key, nil
	}
	if len(raw) == 32 {
		return []byte(raw), nil
	}
	return nil, errors.New("rustore notification key must decode to 32 bytes (AES-256)")
}

// DecryptNotification расшифровывает payload вебхука.
//
// Формат по документации RuStore: base64( IV(12 байт) || ciphertext || tag(16 байт) ),
// шифр AES/GCM/NoPadding. Успешная расшифровка одновременно служит проверкой
// подлинности — отдельной подписи в уведомлениях нет, а GCM не даст подделать
// содержимое без знания ключа.
func DecryptNotification(key []byte, payload string) (Notification, error) {
	var n Notification

	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(payload))
	if err != nil {
		return n, fmt.Errorf("rustore notification: base64: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return n, fmt.Errorf("rustore notification: cipher: %w", err)
	}
	gcm, err := cipher.NewGCMWithTagSize(block, 16)
	if err != nil {
		return n, fmt.Errorf("rustore notification: gcm: %w", err)
	}

	const ivLen = 12
	if len(raw) <= ivLen+gcm.Overhead() {
		return n, errors.New("rustore notification: payload too short")
	}
	plain, err := gcm.Open(nil, raw[:ivLen], raw[ivLen:], nil)
	if err != nil {
		return n, fmt.Errorf("rustore notification: decrypt: %w", err)
	}

	if err := json.Unmarshal(plain, &n); err != nil {
		return n, fmt.Errorf("rustore notification: parse: %w", err)
	}
	return n, nil
}
