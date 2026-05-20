package paygate

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestFormatPayPalAmount(t *testing.T) {
	cases := map[int64]string{
		1:       "0.01",
		5:       "0.05",
		100:     "1.00",
		2500:    "25.00",
		2550:    "25.50",
		2505:    "25.05",
		1234567: "12345.67",
	}
	for cents, want := range cases {
		if got := formatPayPalAmount(cents); got != want {
			t.Errorf("formatPayPalAmount(%d) = %q, want %q", cents, got, want)
		}
	}
}

func TestNewPayPalGatewayRequiresCredentials(t *testing.T) {
	if _, err := newPayPalGateway(map[string]string{"client_id": "abc"}, true); err == nil {
		t.Fatal("expected error when client_secret is missing")
	}
	if _, err := newPayPalGateway(map[string]string{"client_secret": "xyz"}, true); err == nil {
		t.Fatal("expected error when client_id is missing")
	}
}

// TestPayPalCreateCustomerSynthetic checks the synthetic customer id is the
// 22-char base64url of the tenant UUID — stable, length-safe, no API call.
func TestPayPalCreateCustomerSynthetic(t *testing.T) {
	g := &paypalGateway{} // CreateCustomer does not touch the client
	tid := uuid.New()
	got, err := g.CreateCustomer(context.Background(), "", "", map[string]string{"tenant_id": tid.String()})
	if err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	want := base64.RawURLEncoding.EncodeToString(tid[:])
	if got != want {
		t.Fatalf("customer id = %q, want %q", got, want)
	}
	if len(got) != 22 {
		t.Fatalf("customer id length = %d, want 22", len(got))
	}
	again, _ := g.CreateCustomer(context.Background(), "", "", map[string]string{"tenant_id": tid.String()})
	if again != got {
		t.Fatalf("customer id not deterministic: %q vs %q", got, again)
	}
	if _, err := g.CreateCustomer(context.Background(), "", "", map[string]string{"tenant_id": "not-a-uuid"}); err == nil {
		t.Fatal("expected error for an invalid tenant_id")
	}
}

// TestPayPalVaultRequestJSON locks the exact PayPal field names for the two
// vault requests: vault-on-purchase (attributes.vault + customer) and the
// off-session vault_id charge.
func TestPayPalVaultRequestJSON(t *testing.T) {
	vaultOnPurchase := ppOrderRequest{
		Intent:        "CAPTURE",
		PurchaseUnits: []ppPurchaseUnit{{Amount: ppAmount{CurrencyCode: "USD", Value: "25.00"}, CustomID: "t1"}},
		PaymentSource: &ppPaymentSource{Paypal: ppPaypal{
			ExperienceContext: &ppExperienceContext{ShippingPreference: "NO_SHIPPING", UserAction: "PAY_NOW", ReturnURL: "https://h/r", CancelURL: "https://h/c"},
			Attributes: &ppAttributes{
				Vault:    ppVault{StoreInVault: "ON_SUCCESS", UsageType: "MERCHANT", CustomerType: "CONSUMER", PermitMultiplePaymentTokens: true},
				Customer: ppCustomer{ID: "cust22"},
			},
		}},
	}
	b, _ := json.Marshal(vaultOnPurchase)
	for _, want := range []string{
		`"store_in_vault":"ON_SUCCESS"`, `"usage_type":"MERCHANT"`,
		`"customer_type":"CONSUMER"`, `"permit_multiple_payment_tokens":true`,
		`"customer":{"id":"cust22"}`, `"currency_code":"USD"`, `"custom_id":"t1"`,
		`"return_url":"https://h/r"`, `"cancel_url":"https://h/c"`,
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("vault-on-purchase JSON missing %s\n got: %s", want, b)
		}
	}

	charge := ppOrderRequest{
		Intent:        "CAPTURE",
		PurchaseUnits: []ppPurchaseUnit{{Amount: ppAmount{CurrencyCode: "USD", Value: "10.00"}}},
		PaymentSource: &ppPaymentSource{Paypal: ppPaypal{VaultID: "tok_123"}},
	}
	cb, _ := json.Marshal(charge)
	if !strings.Contains(string(cb), `"vault_id":"tok_123"`) {
		t.Errorf("vault charge JSON missing vault_id\n got: %s", cb)
	}
	if strings.Contains(string(cb), "attributes") {
		t.Errorf("vault charge JSON should not contain attributes\n got: %s", cb)
	}
}
