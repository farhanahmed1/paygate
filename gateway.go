package paygate

import (
	"context"
	"fmt"
	"net/http"
)

// WebhookEventKind is the normalized meaning of a gateway webhook, mapped from
// each provider's native event types so the webhook handler stays
// gateway-agnostic.
type WebhookEventKind int

const (
	WebhookIgnored        WebhookEventKind = iota // acknowledged, no action
	WebhookTopupSucceeded                         // a topup payment completed
	WebhookTopupFailed                            // a topup payment failed
)

// WebhookEvent is a gateway-agnostic view of a verified webhook. The HTTP
// webhook handler uses EventID for idempotency logging and (Kind, GatewayTxnID)
// to act on the payment.
type WebhookEvent struct {
	EventID      string           // gateway event id (e.g. Stripe evt_…, PayPal WH-…)
	EventType    string           // raw gateway event type, for the log
	Kind         WebhookEventKind // normalized action
	GatewayTxnID string           // gateway transaction/resource id this event concerns

	// Saved-method hints, populated for a successful topup that set up a method
	// for future use (Stripe). The webhook handler uses them to persist the
	// card after crediting the balance. Empty/false when no method was saved.
	PaymentMethodID    string // gateway payment-method id (Stripe pm_…)
	CustomerID         string // gateway customer id the method is attached to
	SavesPaymentMethod bool   // the payment was set up for future off-session use
}

// TopupPaymentInput asks a gateway to start a topup payment.
type TopupPaymentInput struct {
	AmountCents int64             // gateway minor units (e.g. cents)
	Currency    string            // ISO 4217, lower-case (e.g. "usd")
	Metadata    map[string]string // optional, attached at the gateway

	// CustomerID + SaveCard request that the card be saved for future
	// off-session topups: the payment is attached to CustomerID with
	// setup_future_usage. Honoured only by gateways implementing
	// SavedMethodGateway; ignored otherwise. When SaveCard is set, CustomerID
	// must be non-empty.
	CustomerID string
	SaveCard   bool
}

// SavedChargeInput asks a gateway to charge an already-stored payment method
// off-session (no buyer present), to top up from a saved card.
type SavedChargeInput struct {
	CustomerID      string            // gateway customer id owning the method
	PaymentMethodID string            // stored payment-method id to charge
	AmountCents     int64             // gateway minor units (e.g. cents)
	Currency        string            // ISO 4217, lower-case (e.g. "usd")
	Metadata        map[string]string // optional, attached at the gateway
}

// TopupPaymentResult carries the gateway transaction id plus the client data
// the browser needs to complete the payment.
type TopupPaymentResult struct {
	GatewayTxnID string // gateway transaction id (Stripe PaymentIntent id / PayPal order id)
	ClientSecret string // Stripe.js client secret; empty for gateways without one (PayPal)
}

// Gateway is the payment-gateway contract. An implementation wraps one
// provider's API and webhook verification; the gateway-agnostic billing.Service
// owns persistence, balance, and idempotency.
type Gateway interface {
	Name() string
	// Validate confirms the configured credentials work via a cheap
	// authenticated call (used by the admin "test connection" action).
	Validate(ctx context.Context) error
	CreateTopupPayment(ctx context.Context, in TopupPaymentInput) (TopupPaymentResult, error)
	ParseWebhook(payload []byte, headers http.Header) (WebhookEvent, error)
}

// CapturableGateway is a Gateway whose payments need an explicit server-side
// capture after the buyer approves (PayPal Orders). Stripe does not implement
// it — its PaymentIntents are confirmed client-side and settle via webhook.
type CapturableGateway interface {
	Gateway
	// CaptureTopupPayment captures a previously-created payment identified by
	// its gateway transaction id (PayPal order id). completed is true when the
	// gateway reports the payment captured/completed.
	CaptureTopupPayment(ctx context.Context, gatewayTxnID string) (completed bool, err error)
}

// SavedMethodGateway is a Gateway that can store a payment method against a
// provider customer and reuse it for off-session charges (Stripe customers +
// cards). PayPal does not implement it in this cut. The gateway-agnostic
// Service owns the saved-method ledger (payment_methods); this interface is
// only the provider-side calls.
type SavedMethodGateway interface {
	Gateway
	// CreateCustomer creates a provider customer that stored methods attach to.
	CreateCustomer(ctx context.Context, email, name string, metadata map[string]string) (customerID string, err error)
	// RetrieveCardDetails fetches a stored method's card brand/last4/expiry.
	RetrieveCardDetails(ctx context.Context, paymentMethodID string) (CardDetails, error)
	// ChargeSavedMethod charges a stored method off-session. It returns the
	// gateway transaction id and whether the charge settled synchronously
	// (succeeded); when false the payment settles later via webhook.
	ChargeSavedMethod(ctx context.Context, in SavedChargeInput) (gatewayTxnID string, succeeded bool, err error)
	// DetachPaymentMethod removes a stored method at the provider.
	DetachPaymentMethod(ctx context.Context, paymentMethodID string) error
}

// Gateway returns a configured gateway implementation, built from its stored
// (decrypted) settings — regardless of the enabled flag, so credentials can be
// tested before the gateway is enabled. Returns ErrGatewayNotFound if the
// gateway has not been configured. The enabled flag is enforced separately by
// the payment path.
func (s *Service) Gateway(name string) (Gateway, error) {
	gs, err := s.GatewaySetting(name)
	if err != nil {
		return nil, err
	}
	switch name {
	case GatewayStripe:
		return newStripeGateway(gs.Config)
	case GatewayPayPal:
		return newPayPalGateway(gs.Config, gs.IsTestMode)
	default:
		return nil, fmt.Errorf("paygate: gateway %q has no implementation", name)
	}
}
