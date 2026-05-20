package paygate

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// ErrTenantNotFound is returned when a balance operation targets a tenant id
// that does not exist.
var ErrTenantNotFound = errors.New("paygate: tenant not found")

// TenantBalance returns the tenant's prepaid balance as an exact decimal
// string (e.g. "25.0000"). Money is never represented as a float in Go —
// NUMERIC arithmetic stays in Postgres. Returns ErrTenantNotFound for an
// unknown tenant id.
func (s *Service) TenantBalance(tenantID uuid.UUID) (string, error) {
	var balance string
	err := s.db.QueryRow(`SELECT balance FROM tenants WHERE id = $1`, tenantID).Scan(&balance)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrTenantNotFound
	}
	if err != nil {
		return "", fmt.Errorf("paygate: read balance: %w", err)
	}
	return balance, nil
}

// creditBalanceTx adds amount to the tenant's balance inside an existing
// transaction. The addition is done in Postgres (balance = balance + $1) so it
// is exact, and the single UPDATE locks the row for the statement — concurrent
// credits serialize on that lock with no lost update, so no explicit
// SELECT … FOR UPDATE is needed for a pure increment. amount must already be a
// validated positive decimal string.
func (s *Service) creditBalanceTx(tx *sql.Tx, tenantID uuid.UUID, amount string) error {
	res, err := tx.Exec(`UPDATE tenants SET balance = balance + $1::numeric WHERE id = $2`, amount, tenantID)
	if err != nil {
		return fmt.Errorf("paygate: credit balance: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("paygate: credit balance rows affected: %w", err)
	}
	if n != 1 {
		return ErrTenantNotFound
	}
	return nil
}
