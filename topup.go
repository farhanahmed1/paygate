package paygate

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// minTopupCents is the smallest topup we accept ($1.00). Keeps amounts above
// gateway minimums and avoids dust transactions.
const minTopupCents = 100

// ErrGatewayDisabled is returned when a topup targets a configured-but-disabled
// gateway. (Configuration/testing build the gateway regardless; only the
// payment path enforces the enabled flag.)
var ErrGatewayDisabled = errors.New("paygate: gateway disabled")

// ErrTopupNotFound is returned when a capture targets a topup that does not
// exist for the requesting tenant.
var ErrTopupNotFound = errors.New("paygate: topup not found")

// CentsToAmount converts integer cents to a NUMERIC(12,4) decimal string for
// the ledger, e.g. 2500 → "25.0000". Exact for 2-decimal (USD) amounts.
func CentsToAmount(cents int64) string {
	return fmt.Sprintf("%d.%02d00", cents/100, cents%100)
}

// CreateTopupResult is what the browser needs to complete a topup.
type CreateTopupResult struct {
	TransactionID uuid.UUID
	GatewayTxnID  string
	ClientSecret  string // Stripe.js client secret; empty for PayPal
}

// TopupRequest is the input to CreateTopup.
type TopupRequest struct {
	TenantID    uuid.UUID
	Gateway     string // GatewayStripe | GatewayPayPal
	AmountCents int64

	// SaveCard stores the card for future off-session topups (Stripe only).
	// CustomerEmail/CustomerName label the gateway customer if one is created.
	SaveCard      bool
	CustomerEmail string
	CustomerName  string
}

// CreateTopup starts a topup: enforces the enabled flag, creates the payment at
// the gateway, and records a pending transaction. The browser then completes it
// (Stripe.js confirm → webhook; PayPal approve → CaptureTopup). When
// in.SaveCard is set (Stripe), the tenant's gateway customer is ensured and the
// confirmed card is saved for reuse by the webhook once the payment succeeds.
func (s *Service) CreateTopup(ctx context.Context, in TopupRequest) (CreateTopupResult, error) {
	if in.AmountCents < minTopupCents {
		return CreateTopupResult{}, fmt.Errorf("paygate: minimum topup is %d cents", minTopupCents)
	}

	gw, err := s.enabledGateway(in.Gateway)
	if err != nil {
		return CreateTopupResult{}, err // ErrGatewayNotFound / ErrGatewayDisabled
	}

	pmInput := TopupPaymentInput{
		AmountCents: in.AmountCents,
		Currency:    "usd",
		Metadata:    map[string]string{"tenant_id": in.TenantID.String(), "purpose": "topup"},
	}
	var customerID string
	if in.SaveCard {
		smg, ok := gw.(SavedMethodGateway)
		if !ok {
			return CreateTopupResult{}, fmt.Errorf("paygate: gateway %q does not support saving cards", in.Gateway)
		}
		if customerID, err = s.ensureCustomer(ctx, smg, in.TenantID, in.Gateway, in.CustomerEmail, in.CustomerName); err != nil {
			return CreateTopupResult{}, err
		}
		pmInput.CustomerID = customerID
		pmInput.SaveCard = true
	}

	pay, err := gw.CreateTopupPayment(ctx, pmInput)
	if err != nil {
		return CreateTopupResult{}, err
	}

	txnID, err := s.CreateTopupTransaction(CreateTopupInput{
		TenantID:          in.TenantID,
		Gateway:           in.Gateway,
		Amount:            CentsToAmount(in.AmountCents),
		GatewayTxnID:      pay.GatewayTxnID,
		GatewayCustomerID: customerID,
		Description:       "Account topup",
	})
	if err != nil {
		return CreateTopupResult{}, err
	}
	return CreateTopupResult{TransactionID: txnID, GatewayTxnID: pay.GatewayTxnID, ClientSecret: pay.ClientSecret}, nil
}

// enabledGateway resolves a configured-and-enabled gateway implementation.
// Returns ErrGatewayNotFound when unconfigured and ErrGatewayDisabled when
// configured but disabled. (Building the gateway only requires it to be
// configured; the enabled flag is enforced here, on the payment path.)
func (s *Service) enabledGateway(name string) (Gateway, error) {
	gs, err := s.GatewaySetting(name)
	if err != nil {
		return nil, err
	}
	if !gs.IsEnabled {
		return nil, ErrGatewayDisabled
	}
	return s.Gateway(name)
}

// CaptureTopup captures a gateway payment the tenant created (PayPal order) and
// completes the topup. Returns credited=true when the balance was credited.
// Ownership is verified: the transaction must belong to the requesting tenant.
func (s *Service) CaptureTopup(ctx context.Context, tenantID uuid.UUID, gateway, gatewayTxnID string) (bool, error) {
	id, owner, found, err := s.topupTxnByGatewayTxn(gateway, gatewayTxnID)
	if err != nil {
		return false, err
	}
	if !found || owner != tenantID {
		return false, ErrTopupNotFound
	}

	gw, err := s.Gateway(gateway)
	if err != nil {
		return false, err
	}
	cg, ok := gw.(CapturableGateway)
	if !ok {
		return false, fmt.Errorf("paygate: gateway %q does not support capture", gateway)
	}

	captured, err := cg.CaptureTopupPayment(ctx, gatewayTxnID)
	if err != nil {
		return false, err
	}
	if !captured {
		_ = s.MarkTopupFailed(id)
		return false, nil
	}
	return s.CompleteTopup(id)
}

// TopupGatewayOption tells the browser how to render a gateway payment option.
type TopupGatewayOption struct {
	Name      string `json:"name"`
	TestMode  bool   `json:"test_mode"`
	PublicKey string `json:"public_key"` // Stripe publishable_key / PayPal client_id (browser-safe)
}

// TopupOptions is the data the topup page needs to initialize.
type TopupOptions struct {
	Balance  string               `json:"balance"`
	Gateways []TopupGatewayOption `json:"gateways"`
}

// publicConfigKey maps a gateway to its public (browser-safe) credential key.
func publicConfigKey(gateway string) string {
	switch gateway {
	case GatewayStripe:
		return "publishable_key"
	case GatewayPayPal:
		return "client_id"
	}
	return ""
}

// TopupOptions returns the tenant balance plus each *enabled* gateway's public
// key, for initializing the topup page. Secret values are never included.
func (s *Service) TopupOptions(tenantID uuid.UUID) (TopupOptions, error) {
	balance, err := s.TenantBalance(tenantID)
	if err != nil {
		return TopupOptions{}, err
	}
	out := TopupOptions{Balance: balance, Gateways: []TopupGatewayOption{}}
	for _, name := range []string{GatewayStripe, GatewayPayPal} {
		gs, err := s.GatewaySetting(name)
		if errors.Is(err, ErrGatewayNotFound) {
			continue
		}
		if err != nil {
			return TopupOptions{}, err
		}
		if !gs.IsEnabled {
			continue
		}
		out.Gateways = append(out.Gateways, TopupGatewayOption{
			Name:      name,
			TestMode:  gs.IsTestMode,
			PublicKey: gs.Config[publicConfigKey(name)],
		})
	}
	return out, nil
}
