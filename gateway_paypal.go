package paygate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/plutov/paypal/v4"
)

// paypalGateway implements Gateway (and CapturableGateway) against PayPal
// Orders v2 via plutov/paypal v4. plutov's client auto-manages the OAuth token
// (SendWithAuth fetches/refreshes it), so no explicit token call is needed.
type paypalGateway struct {
	client    *paypal.Client
	webhookID string
}

// newPayPalGateway builds the gateway from decrypted config. client_id and
// client_secret are required; webhook_id is required only to verify webhooks.
// testMode selects the sandbox vs live API base.
func newPayPalGateway(config map[string]string, testMode bool) (*paypalGateway, error) {
	clientID := config["client_id"]
	secret := config["client_secret"]
	if clientID == "" || secret == "" {
		return nil, errors.New("paygate: paypal client_id/client_secret not configured")
	}
	apiBase := paypal.APIBaseLive
	if testMode {
		apiBase = paypal.APIBaseSandBox
	}
	client, err := paypal.NewClient(clientID, secret, apiBase)
	if err != nil {
		return nil, fmt.Errorf("paygate: paypal client: %w", err)
	}
	return &paypalGateway{client: client, webhookID: config["webhook_id"]}, nil
}

func (g *paypalGateway) Name() string { return GatewayPayPal }

// Validate confirms the client_id/client_secret work by fetching an OAuth
// access token. plutov caches the token, so a later API call reuses it.
func (g *paypalGateway) Validate(ctx context.Context) error {
	if _, err := g.client.GetAccessToken(ctx); err != nil {
		return fmt.Errorf("paygate: paypal credential check failed: %w", err)
	}
	return nil
}

// CreateTopupPayment creates a PayPal order (intent CAPTURE). The browser
// approves it with the PayPal Buttons SDK using the returned order id; the
// server then settles it via CaptureTopupPayment. PayPal has no client secret,
// so ClientSecret is empty.
func (g *paypalGateway) CreateTopupPayment(ctx context.Context, in TopupPaymentInput) (TopupPaymentResult, error) {
	units := []paypal.PurchaseUnitRequest{{
		Amount: &paypal.PurchaseUnitAmount{
			Currency: strings.ToUpper(in.Currency),
			Value:    formatPayPalAmount(in.AmountCents),
		},
	}}
	order, err := g.client.CreateOrder(ctx, paypal.OrderIntentCapture, units, nil, nil)
	if err != nil {
		return TopupPaymentResult{}, fmt.Errorf("paygate: paypal create order: %w", err)
	}
	return TopupPaymentResult{GatewayTxnID: order.ID}, nil
}

// CaptureTopupPayment captures the PayPal order identified by gatewayTxnID,
// returning completed=true when PayPal reports the order COMPLETED.
func (g *paypalGateway) CaptureTopupPayment(ctx context.Context, gatewayTxnID string) (bool, error) {
	resp, err := g.client.CaptureOrder(ctx, gatewayTxnID, paypal.CaptureOrderRequest{})
	if err != nil {
		return false, fmt.Errorf("paygate: paypal capture order: %w", err)
	}
	return resp.Status == "COMPLETED", nil
}

// ParseWebhook verifies the PayPal webhook signature via PayPal's server-side
// postback (requires webhook_id) and normalizes the event. PayPal's primary
// completion path for topups is the synchronous CaptureTopupPayment; this
// verified webhook is a backup that the caller records and applies
// idempotently. GatewayTxnID carries the event resource id (capture id),
// best-effort.
func (g *paypalGateway) ParseWebhook(payload []byte, headers http.Header) (WebhookEvent, error) {
	if g.webhookID == "" {
		return WebhookEvent{}, errors.New("paygate: paypal webhook_id not configured")
	}

	// plutov's VerifyWebhookSignature reads the PAYPAL-* headers and the body
	// from an *http.Request; reconstruct a minimal one from our inputs.
	req := &http.Request{Header: headers, Body: io.NopCloser(bytes.NewReader(payload))}
	verify, err := g.client.VerifyWebhookSignature(context.Background(), req, g.webhookID)
	if err != nil {
		return WebhookEvent{}, fmt.Errorf("paygate: paypal webhook verify: %w", err)
	}
	if verify.VerificationStatus != "SUCCESS" {
		return WebhookEvent{}, fmt.Errorf("paygate: paypal webhook signature not verified (%s)", verify.VerificationStatus)
	}

	var event paypal.AnyEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		return WebhookEvent{}, fmt.Errorf("paygate: paypal webhook payload: %w", err)
	}
	out := WebhookEvent{EventID: event.ID, EventType: event.EventType, Kind: WebhookIgnored}
	switch event.EventType {
	case "PAYMENT.CAPTURE.COMPLETED":
		out.Kind = WebhookTopupSucceeded
	case "PAYMENT.CAPTURE.DENIED":
		out.Kind = WebhookTopupFailed
	}
	var res struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(event.Resource, &res)
	out.GatewayTxnID = res.ID
	return out, nil
}

// formatPayPalAmount converts integer minor units (cents) to PayPal's decimal
// string for a 2-decimal currency (USD this phase), e.g. 2500 → "25.00".
func formatPayPalAmount(cents int64) string {
	return fmt.Sprintf("%d.%02d", cents/100, cents%100)
}
