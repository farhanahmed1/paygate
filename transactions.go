package paygate

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// CreateTopupInput is the data needed to open a pending topup transaction.
type CreateTopupInput struct {
	TenantID          uuid.UUID
	Gateway           string // GatewayStripe | GatewayPayPal
	Amount            string // positive decimal, ≤4 fractional digits, e.g. "25.0000"
	GatewayTxnID      string // gateway id (Stripe PaymentIntent / PayPal order); "" if not yet known
	GatewayCustomerID string // optional
	Description       string // optional
}

// topupAmountRe matches a non-negative decimal with up to four fractional
// digits — the scale of payment_transactions.amount / tenants.balance
// (NUMERIC(12,4)). The integer-part magnitude is enforced by the column.
var topupAmountRe = regexp.MustCompile(`^\d+(\.\d{1,4})?$`)

// validateTopupAmount ensures s is a positive decimal with ≤4 fractional
// digits. Postgres enforces the NUMERIC(12,4) upper bound; this gives a clean
// early error for the common bad inputs (negative, zero, too many decimals,
// non-numeric).
func validateTopupAmount(s string) error {
	if !topupAmountRe.MatchString(s) {
		return fmt.Errorf("paygate: invalid amount %q (want a decimal with up to 4 fractional digits)", s)
	}
	if strings.Trim(strings.ReplaceAll(s, ".", ""), "0") == "" {
		return fmt.Errorf("paygate: topup amount must be greater than zero, got %q", s)
	}
	return nil
}

// CreateTopupTransaction inserts a pending topup row and returns its id. The
// amount is credited to the tenant's balance only later, by CompleteTopup,
// once the gateway confirms payment. currency and status use their column
// defaults ('USD' / 'pending'); transaction_type is 'topup'.
func (s *Service) CreateTopupTransaction(in CreateTopupInput) (uuid.UUID, error) {
	if !validGateway(in.Gateway) {
		return uuid.Nil, fmt.Errorf("paygate: unknown gateway %q", in.Gateway)
	}
	if err := validateTopupAmount(in.Amount); err != nil {
		return uuid.Nil, err
	}

	var id uuid.UUID
	err := s.db.QueryRow(
		`INSERT INTO payment_transactions
		     (tenant_id, gateway, transaction_type, amount, gateway_transaction_id, gateway_customer_id, description)
		 VALUES ($1, $2, 'topup', $3::numeric, NULLIF($4, ''), NULLIF($5, ''), NULLIF($6, ''))
		 RETURNING id`,
		in.TenantID, in.Gateway, in.Amount, in.GatewayTxnID, in.GatewayCustomerID, in.Description,
	).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("paygate: create topup transaction: %w", err)
	}
	return id, nil
}

// CompleteTopup marks a pending topup completed and credits the tenant's
// balance — atomically and idempotently. The status flip and the balance
// credit run in one transaction; the flip's WHERE status='pending' guard means
// a redelivered/duplicate call matches zero rows and credits nothing. Returns
// credited=true only when this call performed the transition.
func (s *Service) CompleteTopup(transactionID uuid.UUID) (credited bool, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("paygate: begin: %w", err)
	}
	defer tx.Rollback()

	var (
		tenantID uuid.UUID
		amount   string
	)
	err = tx.QueryRow(
		`UPDATE payment_transactions
		    SET status = 'completed'
		  WHERE id = $1 AND status = 'pending'
		 RETURNING tenant_id, amount`,
		transactionID,
	).Scan(&tenantID, &amount)
	if errors.Is(err, sql.ErrNoRows) {
		// Not pending: already completed/failed, or unknown id. Idempotent no-op.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("paygate: complete topup: %w", err)
	}

	if err := s.creditBalanceTx(tx, tenantID, amount); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("paygate: commit: %w", err)
	}
	return true, nil
}

// MarkTopupFailed marks a pending topup failed. Idempotent — it only flips a
// row still in 'pending', so a duplicate failure event is a no-op and it never
// overrides an already-completed topup.
func (s *Service) MarkTopupFailed(transactionID uuid.UUID) error {
	if _, err := s.db.Exec(
		`UPDATE payment_transactions SET status = 'failed' WHERE id = $1 AND status = 'pending'`,
		transactionID,
	); err != nil {
		return fmt.Errorf("paygate: mark topup failed: %w", err)
	}
	return nil
}

// topupTxnByGatewayTxn resolves our topup transaction id and its owning tenant
// from a gateway transaction id (e.g. a Stripe PaymentIntent / PayPal order
// id). found is false when no matching topup exists. The
// (gateway, gateway_transaction_id) UNIQUE index guarantees at most one row.
// The webhook path ignores the tenant; the capture path uses it to verify
// ownership.
func (s *Service) topupTxnByGatewayTxn(gateway, gatewayTxnID string) (id, tenantID uuid.UUID, found bool, err error) {
	if gatewayTxnID == "" {
		return uuid.Nil, uuid.Nil, false, nil
	}
	err = s.db.QueryRow(
		`SELECT id, tenant_id FROM payment_transactions
		  WHERE gateway = $1 AND gateway_transaction_id = $2 AND transaction_type = 'topup'`,
		gateway, gatewayTxnID,
	).Scan(&id, &tenantID)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, uuid.Nil, false, fmt.Errorf("paygate: lookup topup by gateway txn: %w", err)
	}
	return id, tenantID, true, nil
}
