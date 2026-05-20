// Package paygate is the gateway-agnostic core of a payment system: gateway
// credential storage (encrypted at rest), saved payment methods, the
// transaction ledger, tenant balance crediting, topup orchestration, and
// webhook handling. Gateway-specific behaviour (Stripe, PayPal) is layered on
// top via the Gateway and SavedMethodGateway interfaces.
//
// It is self-contained — depending only on a minimal DB surface, the Stripe and
// PayPal SDKs, and the standard library — so multiple hosts can import it. Each
// host supplies a *sql.DB-compatible [DB], a 32-byte credential-encryption key,
// and a namespace string to [NewService], plus the HTTP handlers, routes, and
// UI; paygate owns the gateways, ledger, balance, idempotency, and webhooks.
//
// The host is responsible for the database schema. paygate expects:
//
//   - tenants(id, balance NUMERIC(12,4))
//   - payment_gateway_settings(name, display_name, is_enabled, is_test_mode, encrypted_config)
//   - payment_methods(...)        — saved cards / wallet tokens
//   - payment_transactions(...)   — the money-movement ledger
//   - payment_webhook_logs(gateway, event_id, ...) with UNIQUE(gateway, event_id)
//
// See the README for the full column list and the reference migrations.
package paygate
