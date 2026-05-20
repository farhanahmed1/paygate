package paygate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// ErrPaymentMethodNotFound is returned when a saved-method operation targets a
// method that does not exist (or is no longer active) for the requesting tenant.
var ErrPaymentMethodNotFound = errors.New("paygate: payment method not found")

// ListPaymentMethods returns a tenant's active saved methods, default first
// then most-recently-used. Gateway reference ids are populated but not
// serialized to clients (json:"-").
func (s *Service) ListPaymentMethods(tenantID uuid.UUID) ([]PaymentMethod, error) {
	rows, err := s.db.Query(
		`SELECT id, gateway, payment_type,
		        COALESCE(card_brand, ''), COALESCE(card_last4, ''),
		        COALESCE(card_exp_month, 0), COALESCE(card_exp_year, 0),
		        COALESCE(nickname, ''), is_default,
		        gateway_customer_id, gateway_payment_method_id
		   FROM payment_methods
		  WHERE tenant_id = $1 AND is_active = true
		  ORDER BY is_default DESC, last_used_at DESC NULLS LAST, created_at`,
		tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("paygate: list payment methods: %w", err)
	}
	defer rows.Close()

	methods := []PaymentMethod{}
	for rows.Next() {
		var m PaymentMethod
		if err := rows.Scan(
			&m.ID, &m.Gateway, &m.PaymentType,
			&m.CardBrand, &m.CardLast4, &m.CardExpMonth, &m.CardExpYear,
			&m.Nickname, &m.IsDefault, &m.GatewayCustomerID, &m.GatewayPaymentMethodID,
		); err != nil {
			return nil, fmt.Errorf("paygate: scan payment method: %w", err)
		}
		methods = append(methods, m)
	}
	return methods, rows.Err()
}

// customerIDForTenant finds an existing gateway customer id for the tenant —
// from saved methods first, then the transaction ledger (the LeanPBX lookup
// order). Returns "" (no error) when none exists yet.
func (s *Service) customerIDForTenant(tenantID uuid.UUID, gateway string) (string, error) {
	var cid string
	err := s.db.QueryRow(
		`SELECT gateway_customer_id FROM payment_methods
		  WHERE tenant_id = $1 AND gateway = $2 AND gateway_customer_id <> ''
		  ORDER BY last_used_at DESC NULLS LAST
		  LIMIT 1`, tenantID, gateway,
	).Scan(&cid)
	if err == nil {
		return cid, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("paygate: lookup customer in methods: %w", err)
	}

	err = s.db.QueryRow(
		`SELECT gateway_customer_id FROM payment_transactions
		  WHERE tenant_id = $1 AND gateway = $2
		    AND gateway_customer_id IS NOT NULL AND gateway_customer_id <> ''
		  ORDER BY created_at DESC
		  LIMIT 1`, tenantID, gateway,
	).Scan(&cid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("paygate: lookup customer in transactions: %w", err)
	}
	return cid, nil
}

// ensureCustomer returns the tenant's gateway customer id, creating one at the
// provider when none exists yet.
func (s *Service) ensureCustomer(ctx context.Context, gw SavedMethodGateway, tenantID uuid.UUID, gateway, email, name string) (string, error) {
	cid, err := s.customerIDForTenant(tenantID, gateway)
	if err != nil {
		return "", err
	}
	if cid != "" {
		return cid, nil
	}
	return gw.CreateCustomer(ctx, email, name, map[string]string{"tenant_id": tenantID.String()})
}

// savePaymentMethod upserts a saved card for the tenant, keyed on
// (tenant, gateway, gateway_payment_method_id). The tenant's first card (when
// no active default exists) becomes the default; re-saving an existing card
// refreshes its details and reactivates it without demoting an existing
// default. Idempotent, so the webhook can call it on every redelivery.
func (s *Service) savePaymentMethod(tenantID uuid.UUID, gateway, customerID, pmID string, card CardDetails) error {
	var hasDefault bool
	if err := s.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM payment_methods
		                WHERE tenant_id = $1 AND is_active = true AND is_default = true)`,
		tenantID,
	).Scan(&hasDefault); err != nil {
		return fmt.Errorf("paygate: check default method: %w", err)
	}

	nickname := titleBrand(card.Brand) + " ending in " + card.Last4
	if _, err := s.db.Exec(
		`INSERT INTO payment_methods
		     (tenant_id, gateway, payment_type, gateway_customer_id, gateway_payment_method_id,
		      card_brand, card_last4, card_exp_month, card_exp_year, nickname,
		      is_default, is_active, last_used_at)
		 VALUES ($1, $2, 'card', $3, $4, $5, $6, $7, $8, $9, $10, true, NOW())
		 ON CONFLICT (tenant_id, gateway, gateway_payment_method_id) DO UPDATE
		    SET gateway_customer_id = EXCLUDED.gateway_customer_id,
		        card_brand          = EXCLUDED.card_brand,
		        card_last4          = EXCLUDED.card_last4,
		        card_exp_month      = EXCLUDED.card_exp_month,
		        card_exp_year       = EXCLUDED.card_exp_year,
		        is_default          = payment_methods.is_default OR EXCLUDED.is_default,
		        is_active           = true,
		        last_used_at        = NOW()`,
		tenantID, gateway, customerID, pmID,
		card.Brand, card.Last4, card.ExpMonth, card.ExpYear, nickname, !hasDefault,
	); err != nil {
		return fmt.Errorf("paygate: save payment method: %w", err)
	}
	return nil
}

// titleBrand upper-cases the first letter of a card brand for display
// ("visa" → "Visa"), matching LeanPBX's saved-method nickname format.
func titleBrand(brand string) string {
	if brand == "" {
		return "Card"
	}
	return strings.ToUpper(brand[:1]) + brand[1:]
}

// SetDefaultPaymentMethod makes one of the tenant's active methods the default
// (default is one per tenant). The flip is a single statement so there is never
// a window with two defaults. Returns ErrPaymentMethodNotFound for an unknown
// or inactive method.
func (s *Service) SetDefaultPaymentMethod(tenantID, methodID uuid.UUID) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("paygate: begin: %w", err)
	}
	defer tx.Rollback()

	var exists bool
	if err := tx.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM payment_methods
		                WHERE id = $1 AND tenant_id = $2 AND is_active = true)`,
		methodID, tenantID,
	).Scan(&exists); err != nil {
		return fmt.Errorf("paygate: check payment method: %w", err)
	}
	if !exists {
		return ErrPaymentMethodNotFound
	}
	if _, err := tx.Exec(
		`UPDATE payment_methods SET is_default = (id = $1)
		  WHERE tenant_id = $2 AND is_active = true`,
		methodID, tenantID,
	); err != nil {
		return fmt.Errorf("paygate: set default method: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("paygate: commit: %w", err)
	}
	return nil
}

// DeletePaymentMethod removes a tenant's saved method: it detaches the method at
// the provider (best-effort — the local row is always removed, as in LeanPBX)
// then soft-deletes it (is_active=false) and promotes another active method to
// default if the deleted one was the default. Returns ErrPaymentMethodNotFound
// for an unknown or already-removed method.
func (s *Service) DeletePaymentMethod(ctx context.Context, tenantID, methodID uuid.UUID) error {
	var gateway, pmID string
	err := s.db.QueryRow(
		`SELECT gateway, gateway_payment_method_id FROM payment_methods
		  WHERE id = $1 AND tenant_id = $2 AND is_active = true`,
		methodID, tenantID,
	).Scan(&gateway, &pmID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrPaymentMethodNotFound
	}
	if err != nil {
		return fmt.Errorf("paygate: load payment method: %w", err)
	}

	// Best-effort provider detach; the local row is removed regardless (a
	// detach failure, e.g. already detached, must not strand the row).
	if gw, gerr := s.Gateway(gateway); gerr == nil {
		if smg, ok := gw.(SavedMethodGateway); ok {
			_ = smg.DetachPaymentMethod(ctx, pmID)
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("paygate: begin: %w", err)
	}
	defer tx.Rollback()

	var wasDefault bool
	err = tx.QueryRow(
		`SELECT is_default FROM payment_methods
		  WHERE id = $1 AND tenant_id = $2 AND is_active = true
		  FOR UPDATE`,
		methodID, tenantID,
	).Scan(&wasDefault)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrPaymentMethodNotFound // concurrently removed
	}
	if err != nil {
		return fmt.Errorf("paygate: lock payment method: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE payment_methods SET is_active = false, is_default = false
		  WHERE id = $1 AND tenant_id = $2`,
		methodID, tenantID,
	); err != nil {
		return fmt.Errorf("paygate: deactivate payment method: %w", err)
	}
	if wasDefault {
		if _, err := tx.Exec(
			`UPDATE payment_methods SET is_default = true
			  WHERE id = (SELECT id FROM payment_methods
			               WHERE tenant_id = $1 AND is_active = true
			               ORDER BY last_used_at DESC NULLS LAST, created_at
			               LIMIT 1)`,
			tenantID,
		); err != nil {
			return fmt.Errorf("paygate: promote default method: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("paygate: commit: %w", err)
	}
	return nil
}

// ChargeSavedMethod tops up a tenant's balance using a stored card, charged
// off-session at the gateway. It records a pending topup transaction and, when
// the charge settles synchronously, credits the balance immediately (the
// webhook is an idempotent backstop for the asynchronous case). A declined card
// surfaces as an error and records no transaction.
func (s *Service) ChargeSavedMethod(ctx context.Context, tenantID, methodID uuid.UUID, amountCents int64) (CreateTopupResult, error) {
	if amountCents < minTopupCents {
		return CreateTopupResult{}, fmt.Errorf("paygate: minimum topup is %d cents", minTopupCents)
	}

	var gateway, customerID, pmID, last4 string
	err := s.db.QueryRow(
		`SELECT gateway, gateway_customer_id, gateway_payment_method_id, COALESCE(card_last4, '')
		   FROM payment_methods
		  WHERE id = $1 AND tenant_id = $2 AND is_active = true`,
		methodID, tenantID,
	).Scan(&gateway, &customerID, &pmID, &last4)
	if errors.Is(err, sql.ErrNoRows) {
		return CreateTopupResult{}, ErrPaymentMethodNotFound
	}
	if err != nil {
		return CreateTopupResult{}, fmt.Errorf("paygate: load payment method: %w", err)
	}

	gw, err := s.enabledGateway(gateway)
	if err != nil {
		return CreateTopupResult{}, err
	}
	smg, ok := gw.(SavedMethodGateway)
	if !ok {
		return CreateTopupResult{}, fmt.Errorf("paygate: gateway %q does not support saved methods", gateway)
	}

	gwTxnID, succeeded, err := smg.ChargeSavedMethod(ctx, SavedChargeInput{
		CustomerID:      customerID,
		PaymentMethodID: pmID,
		AmountCents:     amountCents,
		Currency:        "usd",
		Metadata: map[string]string{
			"tenant_id":       tenantID.String(),
			"purpose":         "topup",
			"saved_method_id": methodID.String(),
		},
	})
	if err != nil {
		return CreateTopupResult{}, err
	}

	txnID, err := s.CreateTopupTransaction(CreateTopupInput{
		TenantID:          tenantID,
		Gateway:           gateway,
		Amount:            CentsToAmount(amountCents),
		GatewayTxnID:      gwTxnID,
		GatewayCustomerID: customerID,
		Description:       "Account topup using saved card ending in " + last4,
	})
	if err != nil {
		return CreateTopupResult{}, err
	}

	// Mark the card used (best-effort; the charge already succeeded).
	_, _ = s.db.Exec(`UPDATE payment_methods SET last_used_at = NOW() WHERE id = $1 AND tenant_id = $2`, methodID, tenantID)

	// Off-session charges normally settle immediately — credit now. Best-effort:
	// if this transient-fails, the payment_intent.succeeded webhook completes the
	// still-pending transaction. Not yet settled → the webhook credits it.
	if succeeded {
		_, _ = s.CompleteTopup(txnID)
	}
	return CreateTopupResult{TransactionID: txnID, GatewayTxnID: gwTxnID}, nil
}
