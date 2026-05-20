package paygate

import (
	"net/http"
	"testing"

	"github.com/stripe/stripe-go/v85/webhook"
)

const testWebhookSecret = "whsec_test_secret"

// signedEvent builds a Stripe-signed webhook payload for the given event JSON
// using the test secret, returning the body and a header set carrying the
// Stripe-Signature.
func signedEvent(body string) ([]byte, http.Header) {
	sp := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{
		Payload: []byte(body),
		Secret:  testWebhookSecret,
	})
	h := http.Header{}
	h.Set("Stripe-Signature", sp.Header)
	return sp.Payload, h
}

func testStripeGateway(t *testing.T) *stripeGateway {
	t.Helper()
	g, err := newStripeGateway(map[string]string{
		"secret_key":     "sk_test_dummy",
		"webhook_secret": testWebhookSecret,
	})
	if err != nil {
		t.Fatalf("newStripeGateway: %v", err)
	}
	return g
}

func TestStripeParseWebhookSucceeded(t *testing.T) {
	g := testStripeGateway(t)
	body := `{"id":"evt_1","object":"event","type":"payment_intent.succeeded","data":{"object":{"id":"pi_abc","object":"payment_intent"}}}`
	payload, header := signedEvent(body)

	ev, err := g.ParseWebhook(payload, header)
	if err != nil {
		t.Fatalf("ParseWebhook: %v", err)
	}
	if ev.Kind != WebhookTopupSucceeded {
		t.Errorf("Kind = %d, want WebhookTopupSucceeded", ev.Kind)
	}
	if ev.GatewayTxnID != "pi_abc" {
		t.Errorf("GatewayTxnID = %q, want pi_abc", ev.GatewayTxnID)
	}
	if ev.EventID != "evt_1" {
		t.Errorf("EventID = %q, want evt_1", ev.EventID)
	}
}

func TestStripeParseWebhookFailed(t *testing.T) {
	g := testStripeGateway(t)
	body := `{"id":"evt_2","object":"event","type":"payment_intent.payment_failed","data":{"object":{"id":"pi_def","object":"payment_intent"}}}`
	payload, header := signedEvent(body)

	ev, err := g.ParseWebhook(payload, header)
	if err != nil {
		t.Fatalf("ParseWebhook: %v", err)
	}
	if ev.Kind != WebhookTopupFailed {
		t.Errorf("Kind = %d, want WebhookTopupFailed", ev.Kind)
	}
	if ev.GatewayTxnID != "pi_def" {
		t.Errorf("GatewayTxnID = %q, want pi_def", ev.GatewayTxnID)
	}
}

func TestStripeParseWebhookIgnored(t *testing.T) {
	g := testStripeGateway(t)
	body := `{"id":"evt_3","object":"event","type":"customer.created","data":{"object":{"id":"cus_x"}}}`
	payload, header := signedEvent(body)

	ev, err := g.ParseWebhook(payload, header)
	if err != nil {
		t.Fatalf("ParseWebhook: %v", err)
	}
	if ev.Kind != WebhookIgnored {
		t.Errorf("Kind = %d, want WebhookIgnored", ev.Kind)
	}
}

func TestStripeParseWebhookBadSignature(t *testing.T) {
	g := testStripeGateway(t)
	body := `{"id":"evt_4","type":"payment_intent.succeeded","data":{"object":{"id":"pi_ghi"}}}`
	// Sign with a different secret → signature must not verify.
	sp := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{
		Payload: []byte(body),
		Secret:  "whsec_wrong_secret",
	})
	h := http.Header{}
	h.Set("Stripe-Signature", sp.Header)
	if _, err := g.ParseWebhook(sp.Payload, h); err == nil {
		t.Fatal("expected signature-verification error, got nil")
	}
}

func TestNewStripeGatewayRequiresSecretKey(t *testing.T) {
	if _, err := newStripeGateway(map[string]string{"webhook_secret": "whsec_x"}); err == nil {
		t.Fatal("expected error when secret_key is missing")
	}
}
