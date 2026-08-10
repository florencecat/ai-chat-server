package handlers

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"ai-server/entitlement"
	"ai-server/rustore"
)

// maxWebhookBody — потолок на тело вебхука; сам конверт RuStore крошечный.
const maxWebhookBody = 1 << 20

// GetEntitlement — GET /entitlement, серверная сторона EntitlementApi.fetch().
// Возвращает текущие права пользователя по JWT.
func (h *Handler) GetEntitlement(c *gin.Context) {
	userID, ok := h.resolveUser(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, h.ent.Fetch(userID))
}

// VerifyRequest — тело POST /verify. Клиент на Dart шлёт purchaseId; snake_case
// принимаем тоже, чтобы не зависеть от настроек сериализации на клиенте.
type VerifyRequest struct {
	PurchaseID      string `json:"purchaseId"`
	PurchaseIDSnake string `json:"purchase_id"`
}

func (r VerifyRequest) purchaseID() string {
	if r.PurchaseID != "" {
		return strings.TrimSpace(r.PurchaseID)
	}
	return strings.TrimSpace(r.PurchaseIDSnake)
}

// VerifyPurchase — POST /verify, серверная сторона EntitlementApi.verify().
// Ходит в RuStore Subscription Data v4, при оплаченной подписке записывает
// права и подтверждает доставку (Confirm Delivery v2).
func (h *Handler) VerifyPurchase(c *gin.Context) {
	userID, ok := h.resolveUser(c)
	if !ok {
		return
	}

	var req VerifyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, errResp{
			Error: "invalid request: " + err.Error(),
			Code:  "INVALID_REQUEST",
		})
		return
	}

	purchaseID := req.purchaseID()
	if purchaseID == "" {
		c.JSON(http.StatusBadRequest, errResp{
			Error: "purchaseId is required",
			Code:  "INVALID_REQUEST",
		})
		return
	}

	ent, err := h.ent.Verify(userID, purchaseID)
	if err != nil {
		status, code := verifyErrorResponse(err)
		if status == http.StatusInternalServerError || status == http.StatusBadGateway {
			log.Printf("entitlement: verify %s for user %s: %v", purchaseID, userID, err)
		}
		c.JSON(status, errResp{Error: err.Error(), Code: code})
		return
	}

	c.JSON(http.StatusOK, ent)
}

// verifyErrorResponse переводит доменную ошибку в HTTP-статус и код для клиента.
func verifyErrorResponse(err error) (int, string) {
	switch {
	case errors.Is(err, entitlement.ErrDisabled):
		return http.StatusServiceUnavailable, "VERIFICATION_DISABLED"
	case errors.Is(err, entitlement.ErrUnknownPurchase):
		return http.StatusNotFound, "PURCHASE_NOT_FOUND"
	case errors.Is(err, entitlement.ErrNotConfirmed):
		return http.StatusConflict, "PURCHASE_NOT_CONFIRMED"
	case errors.Is(err, entitlement.ErrPurchaseTaken):
		return http.StatusConflict, "PURCHASE_ALREADY_CLAIMED"
	default:
		return http.StatusBadGateway, "VERIFICATION_FAILED"
	}
}

// RuStoreWebhook принимает серверные уведомления RuStore о жизненном цикле
// подписки (продление, отмена, возврат).
//
// RuStore ждёт 200 в течение 3 секунд, иначе шлёт до 16 повторов за 36 часов.
// Поэтому расшифровка синхронная (она же — проверка подлинности), а применение
// прав, которое ходит в public-api, уезжает в фон.
func (h *Handler) RuStoreWebhook(c *gin.Context) {
	if h.nKey == nil {
		c.JSON(http.StatusServiceUnavailable, errResp{
			Error: "rustore notifications are not configured",
			Code:  "WEBHOOK_DISABLED",
		})
		return
	}

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxWebhookBody))
	if err != nil {
		c.JSON(http.StatusBadRequest, errResp{Error: "cannot read body", Code: "INVALID_REQUEST"})
		return
	}

	var envelope rustore.NotificationEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Payload == "" {
		c.JSON(http.StatusBadRequest, errResp{Error: "invalid notification envelope", Code: "INVALID_REQUEST"})
		return
	}

	// Расшифровка AES-GCM одновременно подтверждает подлинность: подделать
	// payload без ключа из консоли RuStore нельзя.
	notification, err := rustore.DecryptNotification(h.nKey, envelope.Payload)
	if err != nil {
		log.Printf("rustore webhook: decrypt notification %s: %v", envelope.ID, err)
		c.JSON(http.StatusBadRequest, errResp{Error: "cannot decrypt payload", Code: "INVALID_PAYLOAD"})
		return
	}

	// Песочные события не должны трогать боевые права (и наоборот).
	if notification.Sandbox() != h.cfg.RuStoreSandbox {
		log.Printf("rustore webhook: ignoring %s (sandbox mismatch)", notification.NotificationType)
		c.Status(http.StatusOK)
		return
	}

	go func() {
		// Горутина живёт вне gin.Recovery — паника здесь уронила бы процесс.
		defer func() {
			if r := recover(); r != nil {
				log.Printf("rustore webhook: panic while applying notification %s: %v", envelope.ID, r)
			}
		}()
		if err := h.ent.HandleNotification(notification); err != nil {
			log.Printf("rustore webhook: apply %s (id=%s): %v", notification.NotificationType, envelope.ID, err)
		}
	}()

	c.Status(http.StatusOK)
}
