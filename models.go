package paygate

import "github.com/google/uuid"

// Gateway identifiers. These match the CHECK constraint on
// payment_gateway_settings.name (migration 069) and the `gateway` column on
// payment_methods / payment_transactions / payment_webhook_logs.
const (
	GatewayStripe = "stripe"
	GatewayPayPal = "paypal"
)

// GatewaySetting is one payment gateway's stored configuration.
//
// Config holds the decrypted credential key/values. Keys are gateway-specific
// and interpreted by the gateway implementations, not by this core package:
//
//	stripe → publishable_key, secret_key, webhook_secret
//	paypal → client_id, client_secret, webhook_id, return_url, cancel_url
//	         (return_url/cancel_url are required only to save methods / vault)
//
// Config is empty when the gateway row exists but has not been configured yet.
type GatewaySetting struct {
	Name        string
	DisplayName string
	IsEnabled   bool
	IsTestMode  bool
	Config      map[string]string
}

// CardDetails is the card information a gateway returns for a saved method.
type CardDetails struct {
	Brand    string
	Last4    string
	ExpMonth int
	ExpYear  int
}

// PaymentMethod is a saved payment method (a stored card / wallet token) a
// tenant can reuse. The gateway reference ids are kept internal and never
// serialized to clients.
type PaymentMethod struct {
	ID           uuid.UUID `json:"id"`
	Gateway      string    `json:"gateway"`
	PaymentType  string    `json:"payment_type"`
	CardBrand    string    `json:"card_brand"`
	CardLast4    string    `json:"card_last4"`
	CardExpMonth int       `json:"card_exp_month"`
	CardExpYear  int       `json:"card_exp_year"`
	Nickname     string    `json:"nickname"`
	IsDefault    bool      `json:"is_default"`

	GatewayCustomerID      string `json:"-"`
	GatewayPaymentMethodID string `json:"-"`
}
