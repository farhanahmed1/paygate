package paygate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/stripe/stripe-go/v85"
	"github.com/stripe/stripe-go/v85/webhook"
)

// stripeGateway implements Gateway against Stripe (stripe-go v85). The client is
// bound to the configured secret key — no global stripe.Key state — so each
// gateway config produces its own client.
type stripeGateway struct {
	client        *stripe.Client
	webhookSecret string
}

// Compile-time checks that the Stripe gateway satisfies the optional contracts.
// It saves cards via its webhook (cardRetriever), not via capture.
var (
	_ SavedMethodGateway = (*stripeGateway)(nil)
	_ cardRetriever      = (*stripeGateway)(nil)
)

// newStripeGateway builds the gateway from decrypted gateway config. secret_key
// is required; webhook_secret is required only to verify webhooks.
func newStripeGateway(config map[string]string) (*stripeGateway, error) {
	secretKey := config["secret_key"]
	if secretKey == "" {
		return nil, errors.New("paygate: stripe secret_key not configured")
	}
	return &stripeGateway{
		client:        stripe.NewClient(secretKey),
		webhookSecret: config["webhook_secret"],
	}, nil
}

func (g *stripeGateway) Name() string { return GatewayStripe }

// Validate confirms the secret key works via a cheap authenticated call
// (retrieve the account balance, GET /v1/balance).
func (g *stripeGateway) Validate(ctx context.Context) error {
	if _, err := g.client.V1Balance.Retrieve(ctx, nil); err != nil {
		return fmt.Errorf("paygate: stripe credential check failed: %w", err)
	}
	return nil
}

// CreateTopupPayment creates a PaymentIntent with automatic payment methods
// enabled and returns its id and client secret for Stripe.js to confirm in the
// browser. When in.SaveCard is set, the intent is attached to in.CustomerID
// with setup_future_usage='off_session' so the confirmed card is stored for
// later off-session topups; otherwise it is a one-off payment.
func (g *stripeGateway) CreateTopupPayment(ctx context.Context, in TopupPaymentInput) (TopupPaymentResult, error) {
	params := &stripe.PaymentIntentCreateParams{
		Amount:   stripe.Int64(in.AmountCents),
		Currency: &in.Currency,
		AutomaticPaymentMethods: &stripe.PaymentIntentCreateAutomaticPaymentMethodsParams{
			Enabled: stripe.Bool(true),
		},
		Metadata: in.Metadata,
	}
	if in.SaveCard && in.CustomerID != "" {
		offSession := "off_session"
		params.Customer = &in.CustomerID
		params.SetupFutureUsage = &offSession
	}
	pi, err := g.client.V1PaymentIntents.Create(ctx, params)
	if err != nil {
		return TopupPaymentResult{}, fmt.Errorf("paygate: stripe create payment intent: %w", err)
	}
	return TopupPaymentResult{GatewayTxnID: pi.ID, ClientSecret: pi.ClientSecret}, nil
}

// ParseWebhook verifies the Stripe signature and normalizes the event. Events
// other than the topup-relevant ones are returned with Kind=WebhookIgnored so
// the caller still records them.
func (g *stripeGateway) ParseWebhook(payload []byte, headers http.Header) (WebhookEvent, error) {
	if g.webhookSecret == "" {
		return WebhookEvent{}, errors.New("paygate: stripe webhook_secret not configured")
	}
	// Signature + timestamp tolerance are enforced; API-version matching is
	// not. We read only version-stable fields (event id, type, and the
	// object id), so an account whose API version differs from stripe-go's
	// pinned stripe.APIVersion must not break webhook processing.
	event, err := webhook.ConstructEventWithOptions(payload, headers.Get("Stripe-Signature"), g.webhookSecret,
		webhook.ConstructEventOptions{IgnoreAPIVersionMismatch: true})
	if err != nil {
		return WebhookEvent{}, fmt.Errorf("paygate: stripe webhook verify: %w", err)
	}

	out := WebhookEvent{EventID: event.ID, EventType: string(event.Type), Kind: WebhookIgnored}
	switch event.Type {
	case stripe.EventTypePaymentIntentSucceeded:
		out.Kind = WebhookTopupSucceeded
	case stripe.EventTypePaymentIntentPaymentFailed:
		out.Kind = WebhookTopupFailed
	default:
		return out, nil
	}

	var pi stripe.PaymentIntent
	if err := json.Unmarshal(event.Data.Raw, &pi); err != nil {
		return WebhookEvent{}, fmt.Errorf("paygate: stripe webhook payload: %w", err)
	}
	out.GatewayTxnID = pi.ID
	// Saved-method hints: setup_future_usage is set only when the payer chose
	// to save the card; customer/payment_method arrive as bare ids in the
	// webhook (stripe-go unmarshals each into its .ID).
	out.SavesPaymentMethod = pi.SetupFutureUsage != ""
	if pi.PaymentMethod != nil {
		out.PaymentMethodID = pi.PaymentMethod.ID
	}
	if pi.Customer != nil {
		out.CustomerID = pi.Customer.ID
	}
	return out, nil
}

// CreateCustomer creates a Stripe customer that saved cards attach to.
func (g *stripeGateway) CreateCustomer(ctx context.Context, email, name string, metadata map[string]string) (string, error) {
	params := &stripe.CustomerCreateParams{Metadata: metadata}
	if email != "" {
		params.Email = &email
	}
	if name != "" {
		params.Name = &name
	}
	c, err := g.client.V1Customers.Create(ctx, params)
	if err != nil {
		return "", fmt.Errorf("paygate: stripe create customer: %w", err)
	}
	return c.ID, nil
}

// RetrieveCardDetails fetches the card brand/last4/expiry for a stored Stripe
// payment method (pm_…).
func (g *stripeGateway) RetrieveCardDetails(ctx context.Context, paymentMethodID string) (CardDetails, error) {
	pm, err := g.client.V1PaymentMethods.Retrieve(ctx, paymentMethodID, nil)
	if err != nil {
		return CardDetails{}, fmt.Errorf("paygate: stripe retrieve payment method: %w", err)
	}
	if pm.Card == nil {
		return CardDetails{}, errors.New("paygate: stripe payment method has no card")
	}
	return CardDetails{
		Brand:    string(pm.Card.Brand),
		Last4:    pm.Card.Last4,
		ExpMonth: int(pm.Card.ExpMonth),
		ExpYear:  int(pm.Card.ExpYear),
	}, nil
}

// ChargeSavedMethod creates an off-session PaymentIntent against a stored card
// (customer + payment_method + off_session + confirm) and reports whether it
// settled synchronously. A declined card surfaces as an error here, so no
// transaction is recorded for it.
func (g *stripeGateway) ChargeSavedMethod(ctx context.Context, in SavedChargeInput) (string, bool, error) {
	pi, err := g.client.V1PaymentIntents.Create(ctx, &stripe.PaymentIntentCreateParams{
		Amount:        stripe.Int64(in.AmountCents),
		Currency:      &in.Currency,
		Customer:      &in.CustomerID,
		PaymentMethod: &in.PaymentMethodID,
		OffSession:    stripe.Bool(true),
		Confirm:       stripe.Bool(true),
		Metadata:      in.Metadata,
	})
	if err != nil {
		return "", false, fmt.Errorf("paygate: stripe off-session charge: %w", err)
	}
	return pi.ID, pi.Status == stripe.PaymentIntentStatusSucceeded, nil
}

// DetachPaymentMethod detaches a stored payment method from its Stripe customer.
func (g *stripeGateway) DetachPaymentMethod(ctx context.Context, paymentMethodID string) error {
	if _, err := g.client.V1PaymentMethods.Detach(ctx, paymentMethodID, nil); err != nil {
		return fmt.Errorf("paygate: stripe detach payment method: %w", err)
	}
	return nil
}
