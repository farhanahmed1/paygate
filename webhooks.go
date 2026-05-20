package paygate

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/uuid"
)

// ProcessWebhook verifies an inbound gateway webhook, records it for audit, and
// applies its effect to the matching topup. Signature verification is delegated
// to the gateway. Money-safety against reprocessing comes from CompleteTopup's
// pending-guard (and MarkTopupFailed's), not from webhook dedup — so a
// redelivered webhook is safe even though the audit row is inserted only once.
func (s *Service) ProcessWebhook(ctx context.Context, gateway string, payload []byte, headers http.Header) error {
	gw, err := s.Gateway(gateway)
	if err != nil {
		return fmt.Errorf("paygate: webhook gateway: %w", err)
	}
	ev, err := gw.ParseWebhook(payload, headers)
	if err != nil {
		return err // signature/verification failure
	}

	if err := s.recordWebhookEvent(gateway, ev.EventID, ev.EventType, payload); err != nil {
		return err
	}

	var procErr error
	switch ev.Kind {
	case WebhookTopupSucceeded:
		id, tenantID, found, lerr := s.topupTxnByGatewayTxn(gateway, ev.GatewayTxnID)
		switch {
		case lerr != nil:
			procErr = lerr
		case found:
			if _, procErr = s.CompleteTopup(id); procErr == nil && ev.SavesPaymentMethod && ev.PaymentMethodID != "" {
				// Persist the saved card after the credit. Best-effort: the
				// credit is the money-critical step; a save failure must not
				// fail the webhook (which would reverse nothing but would
				// trigger pointless retries). The card can be saved on a later
				// topup if this transiently fails.
				if smg, ok := gw.(SavedMethodGateway); ok {
					_ = s.persistSavedCard(ctx, smg, gateway, tenantID, ev.CustomerID, ev.PaymentMethodID)
				}
			}
		}
	case WebhookTopupFailed:
		if id, _, found, lerr := s.topupTxnByGatewayTxn(gateway, ev.GatewayTxnID); lerr != nil {
			procErr = lerr
		} else if found {
			procErr = s.MarkTopupFailed(id)
		}
	}

	status, errMsg := "processed", ""
	if procErr != nil {
		status, errMsg = "failed", procErr.Error()
	}
	if uerr := s.markWebhookProcessed(gateway, ev.EventID, status, errMsg); uerr != nil && procErr == nil {
		return uerr
	}
	return procErr
}

// persistSavedCard fetches the charged card's details from the gateway and
// stores it for the tenant. Called only after the topup was credited.
func (s *Service) persistSavedCard(ctx context.Context, gw SavedMethodGateway, gateway string, tenantID uuid.UUID, customerID, pmID string) error {
	card, err := gw.RetrieveCardDetails(ctx, pmID)
	if err != nil {
		return err
	}
	return s.savePaymentMethod(tenantID, gateway, customerID, pmID, card)
}

// recordWebhookEvent stores the event for audit and idempotency. A duplicate
// (gateway, event_id) is a no-op (ON CONFLICT DO NOTHING). payload must be
// valid JSON for the jsonb column — it is, since the gateway verified it.
func (s *Service) recordWebhookEvent(gateway, eventID, eventType string, payload []byte) error {
	if _, err := s.db.Exec(
		`INSERT INTO payment_webhook_logs (gateway, event_id, event_type, payload)
		      VALUES ($1, $2, $3, $4)
		 ON CONFLICT (gateway, event_id) DO NOTHING`,
		gateway, eventID, eventType, string(payload),
	); err != nil {
		return fmt.Errorf("paygate: record webhook event: %w", err)
	}
	return nil
}

// markWebhookProcessed sets the event's terminal status ('processed' or
// 'failed') and processed_at.
func (s *Service) markWebhookProcessed(gateway, eventID, status, errMsg string) error {
	if _, err := s.db.Exec(
		`UPDATE payment_webhook_logs
		    SET status = $3, error_message = NULLIF($4, ''), processed_at = NOW()
		  WHERE gateway = $1 AND event_id = $2`,
		gateway, eventID, status, errMsg,
	); err != nil {
		return fmt.Errorf("paygate: mark webhook processed: %w", err)
	}
	return nil
}
