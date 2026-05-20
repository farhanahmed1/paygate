package paygate

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/plutov/paypal/v4"
)

// paypalGateway implements Gateway, CapturableGateway, and SavedMethodGateway
// against PayPal Orders v2 + Vault v3 via plutov/paypal v4. plutov's client
// auto-manages the OAuth token (SendWithAuth fetches/refreshes it). plutov does
// not model PayPal's vault fields, so the vault-specific calls (vault-on-order,
// vault_id charge, token delete) are sent as raw requests through plutov's
// authed transport (NewRequest + SendWithAuth) — mirroring the LeanPBX flow.
type paypalGateway struct {
	client    *paypal.Client
	webhookID string
	returnURL string // experience_context.return_url — PayPal requires it to vault
	cancelURL string // experience_context.cancel_url — PayPal requires it to vault
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
	return &paypalGateway{
		client:    client,
		webhookID: config["webhook_id"],
		returnURL: config["return_url"],
		cancelURL: config["cancel_url"],
	}, nil
}

// Compile-time checks that the PayPal gateway satisfies the optional contracts.
var (
	_ CapturableGateway  = (*paypalGateway)(nil)
	_ SavedMethodGateway = (*paypalGateway)(nil)
)

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
// so ClientSecret is empty. When in.SaveCard is set, the order carries a vault
// instruction (store_in_vault=ON_SUCCESS) so the capture returns a reusable
// vault token (mirrors LeanPBX vault-on-purchase).
func (g *paypalGateway) CreateTopupPayment(ctx context.Context, in TopupPaymentInput) (TopupPaymentResult, error) {
	if !in.SaveCard {
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

	// PayPal requires return/cancel URLs to vault during purchase.
	if g.returnURL == "" || g.cancelURL == "" {
		return TopupPaymentResult{}, errors.New("paygate: paypal return_url and cancel_url must be configured to save payment methods")
	}
	body := ppOrderRequest{
		Intent: "CAPTURE",
		PurchaseUnits: []ppPurchaseUnit{{
			Amount:   ppAmount{CurrencyCode: strings.ToUpper(in.Currency), Value: formatPayPalAmount(in.AmountCents)},
			CustomID: in.Metadata["tenant_id"],
		}},
		PaymentSource: &ppPaymentSource{Paypal: ppPaypal{
			ExperienceContext: &ppExperienceContext{
				ShippingPreference: "NO_SHIPPING",
				UserAction:         "PAY_NOW",
				ReturnURL:          g.returnURL,
				CancelURL:          g.cancelURL,
			},
			Attributes: &ppAttributes{
				Vault: ppVault{
					StoreInVault:                "ON_SUCCESS",
					UsageType:                   "MERCHANT",
					CustomerType:                "CONSUMER",
					PermitMultiplePaymentTokens: true,
				},
				Customer: ppCustomer{ID: in.CustomerID},
			},
		}},
	}
	out, err := g.postOrder(ctx, body, "")
	if err != nil {
		return TopupPaymentResult{}, err
	}
	if out.ID == "" {
		return TopupPaymentResult{}, errors.New("paygate: paypal create order returned no id")
	}
	return TopupPaymentResult{GatewayTxnID: out.ID}, nil
}

// CaptureTopupPayment captures the PayPal order identified by gatewayTxnID. When
// the order was created with a vault instruction, the capture response carries
// the vaulted token (payment_source.paypal.attributes.vault.id) plus the payer
// email/id, returned as CaptureResult.SavedMethod for the Service to persist.
func (g *paypalGateway) CaptureTopupPayment(ctx context.Context, gatewayTxnID string) (CaptureResult, error) {
	out, err := g.captureOrderRaw(ctx, gatewayTxnID)
	if err != nil {
		return CaptureResult{}, err
	}
	res := CaptureResult{Completed: out.Status == "COMPLETED"}
	if vaultID := out.PaymentSource.Paypal.Attributes.Vault.ID; vaultID != "" {
		res.SavedMethod = &SavedMethodDetails{
			CustomerID:      out.PaymentSource.Paypal.Attributes.Vault.Customer.ID,
			PaymentMethodID: vaultID,
			PaymentType:     "paypal_wallet",
			PayPalEmail:     out.PaymentSource.Paypal.EmailAddress,
			PayPalPayerID:   out.PaymentSource.Paypal.AccountID,
		}
	}
	return res, nil
}

// CreateCustomer returns a stable, length-safe synthetic customer id for the
// tenant. PayPal has no customer object; the id (base64url of the 16-byte UUID,
// 22 chars, within PayPal's id pattern) groups a tenant's vault tokens. No API
// call is made.
func (g *paypalGateway) CreateCustomer(ctx context.Context, email, name string, metadata map[string]string) (string, error) {
	id, err := uuid.Parse(metadata["tenant_id"])
	if err != nil {
		return "", fmt.Errorf("paygate: paypal customer id: invalid tenant_id %q: %w", metadata["tenant_id"], err)
	}
	return base64.RawURLEncoding.EncodeToString(id[:]), nil
}

// ChargeSavedMethod charges a vaulted PayPal token off-session by creating a
// CAPTURE order whose payment_source.paypal.vault_id is the stored token. Such
// orders are single-step — createOrder auto-captures — and require a
// PayPal-Request-Id for idempotency. succeeded is true when PayPal reports the
// order COMPLETED (mirrors LeanPBX); otherwise it settles later via webhook.
func (g *paypalGateway) ChargeSavedMethod(ctx context.Context, in SavedChargeInput) (string, bool, error) {
	body := ppOrderRequest{
		Intent: "CAPTURE",
		PurchaseUnits: []ppPurchaseUnit{{
			Amount:   ppAmount{CurrencyCode: strings.ToUpper(in.Currency), Value: formatPayPalAmount(in.AmountCents)},
			CustomID: in.Metadata["tenant_id"],
		}},
		PaymentSource: &ppPaymentSource{Paypal: ppPaypal{VaultID: in.PaymentMethodID}},
	}
	out, err := g.postOrder(ctx, body, uuid.NewString())
	if err != nil {
		return "", false, err
	}
	if out.ID == "" {
		return "", false, errors.New("paygate: paypal vault charge returned no order id")
	}
	return out.ID, out.Status == "COMPLETED", nil
}

// DetachPaymentMethod deletes a vaulted PayPal token (Vault v3
// DELETE /v3/vault/payment-tokens/{id}). A 204 has no body, so no result is
// decoded.
func (g *paypalGateway) DetachPaymentMethod(ctx context.Context, paymentMethodID string) error {
	req, err := g.client.NewRequest(ctx, http.MethodDelete, g.client.APIBase+"/v3/vault/payment-tokens/"+paymentMethodID, nil)
	if err != nil {
		return fmt.Errorf("paygate: paypal new delete request: %w", err)
	}
	if err := g.client.SendWithAuth(req, nil); err != nil {
		return fmt.Errorf("paygate: paypal delete vault token: %w", err)
	}
	return nil
}

// ParseWebhook verifies the PayPal webhook signature via PayPal's server-side
// postback (requires webhook_id) and normalizes the event. PayPal's primary
// completion path for topups is the synchronous CaptureTopupPayment; this
// verified webhook is a backup that the caller records and applies idempotently.
// For PAYMENT.CAPTURE.* the resource is a capture, so GatewayTxnID is taken from
// supplementary_data.related_ids.order_id (the order id our transaction is keyed
// on), falling back to the resource id.
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
		ID                string `json:"id"`
		SupplementaryData struct {
			RelatedIDs struct {
				OrderID string `json:"order_id"`
			} `json:"related_ids"`
		} `json:"supplementary_data"`
	}
	_ = json.Unmarshal(event.Resource, &res)
	out.GatewayTxnID = res.SupplementaryData.RelatedIDs.OrderID
	if out.GatewayTxnID == "" {
		out.GatewayTxnID = res.ID
	}
	return out, nil
}

// formatPayPalAmount converts integer minor units (cents) to PayPal's decimal
// string for a 2-decimal currency (USD this phase), e.g. 2500 → "25.00".
func formatPayPalAmount(cents int64) string {
	return fmt.Sprintf("%d.%02d", cents/100, cents%100)
}

// ---- raw vault requests (plutov does not model PayPal's vault fields) ----

// postOrder POSTs an Orders v2 create request through plutov's authed transport
// and decodes the (return=representation) response. requestID, when non-empty,
// sets the mandatory PayPal-Request-Id header for single-step vault_id orders.
func (g *paypalGateway) postOrder(ctx context.Context, body ppOrderRequest, requestID string) (ppOrderResponse, error) {
	req, err := g.client.NewRequest(ctx, http.MethodPost, g.client.APIBase+"/v2/checkout/orders", body)
	if err != nil {
		return ppOrderResponse{}, fmt.Errorf("paygate: paypal new order request: %w", err)
	}
	req.Header.Set("Prefer", "return=representation")
	if requestID != "" {
		req.Header.Set("PayPal-Request-Id", requestID)
	}
	var out ppOrderResponse
	if err := g.client.SendWithAuth(req, &out); err != nil {
		return ppOrderResponse{}, fmt.Errorf("paygate: paypal create order: %w", err)
	}
	return out, nil
}

// captureOrderRaw POSTs an Orders v2 capture (return=representation) so the
// response includes the vaulted token + payer details.
func (g *paypalGateway) captureOrderRaw(ctx context.Context, orderID string) (ppOrderResponse, error) {
	req, err := g.client.NewRequest(ctx, http.MethodPost, g.client.APIBase+"/v2/checkout/orders/"+orderID+"/capture", struct{}{})
	if err != nil {
		return ppOrderResponse{}, fmt.Errorf("paygate: paypal new capture request: %w", err)
	}
	req.Header.Set("Prefer", "return=representation")
	var out ppOrderResponse
	if err := g.client.SendWithAuth(req, &out); err != nil {
		return ppOrderResponse{}, fmt.Errorf("paygate: paypal capture order: %w", err)
	}
	return out, nil
}

type ppOrderRequest struct {
	Intent        string           `json:"intent"`
	PurchaseUnits []ppPurchaseUnit `json:"purchase_units"`
	PaymentSource *ppPaymentSource `json:"payment_source,omitempty"`
}

type ppPurchaseUnit struct {
	Amount   ppAmount `json:"amount"`
	CustomID string   `json:"custom_id,omitempty"`
}

type ppAmount struct {
	CurrencyCode string `json:"currency_code"`
	Value        string `json:"value"`
}

type ppPaymentSource struct {
	Paypal ppPaypal `json:"paypal"`
}

type ppPaypal struct {
	VaultID           string               `json:"vault_id,omitempty"`           // off-session charge
	ExperienceContext *ppExperienceContext `json:"experience_context,omitempty"` // vault-on-purchase
	Attributes        *ppAttributes        `json:"attributes,omitempty"`         // vault-on-purchase
}

type ppExperienceContext struct {
	ShippingPreference string `json:"shipping_preference,omitempty"`
	UserAction         string `json:"user_action,omitempty"`
	ReturnURL          string `json:"return_url,omitempty"`
	CancelURL          string `json:"cancel_url,omitempty"`
}

type ppAttributes struct {
	Vault    ppVault    `json:"vault"`
	Customer ppCustomer `json:"customer"`
}

type ppVault struct {
	StoreInVault                string `json:"store_in_vault"`
	UsageType                   string `json:"usage_type"`
	CustomerType                string `json:"customer_type"`
	PermitMultiplePaymentTokens bool   `json:"permit_multiple_payment_tokens"`
}

type ppCustomer struct {
	ID string `json:"id"`
}

// ppOrderResponse is the subset of the Orders v2 create/capture response paygate
// reads: the order id/status, the payer email/account, and the vaulted token.
type ppOrderResponse struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	PaymentSource struct {
		Paypal struct {
			EmailAddress string `json:"email_address"`
			AccountID    string `json:"account_id"`
			Attributes   struct {
				Vault struct {
					ID       string `json:"id"`
					Customer struct {
						ID string `json:"id"`
					} `json:"customer"`
				} `json:"vault"`
			} `json:"attributes"`
		} `json:"paypal"`
	} `json:"payment_source"`
}
