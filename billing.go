package paygate

import (
	"database/sql"
	"errors"
)

// DB is the minimal database surface the billing service needs. A standard
// *sql.DB satisfies it.
type DB interface {
	QueryRow(query string, args ...interface{}) *sql.Row
	Query(query string, args ...interface{}) (*sql.Rows, error)
	Exec(query string, args ...interface{}) (sql.Result, error)
	// Begin starts a transaction for multi-statement work — the atomic
	// topup-complete + balance-credit path.
	Begin() (*sql.Tx, error)
}

// ErrGatewayNotFound is returned by GatewaySetting when no row exists for the
// requested gateway name.
var ErrGatewayNotFound = errors.New("paygate: gateway not configured")

// Service is the gateway-agnostic billing core.
type Service struct {
	db        DB
	key       []byte // 32-byte AES-256 key for gateway-credential encryption
	namespace string // AAD namespace binding ciphertext to this host/purpose
}

// NewService constructs the billing service. key is a 32-byte AES-256 key used
// to encrypt gateway credentials at rest; namespace is mixed into the AAD so a
// ciphertext is bound to (namespace, gateway) — pass a stable host-specific
// value such as "payment_gateway".
func NewService(db DB, key []byte, namespace string) *Service {
	return &Service{db: db, key: key, namespace: namespace}
}

// validGateway reports whether name is a gateway this build supports. Mirrors
// the DB CHECK on payment_gateway_settings.name.
func validGateway(name string) bool {
	return name == GatewayStripe || name == GatewayPayPal
}
