# paygate

[![Go Reference](https://pkg.go.dev/badge/github.com/farhanahmed1/paygate.svg)](https://pkg.go.dev/github.com/farhanahmed1/paygate)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

`paygate` is a small, gateway-agnostic billing core for Go: it manages encrypted
gateway credentials, a money-movement ledger, prepaid tenant balances, top-ups,
saved cards (off-session charging), and webhook handling — across **Stripe** and
**PayPal** behind one interface.

It is self-contained (only the Stripe/PayPal SDKs, `google/uuid`, and the
standard library) so independent applications can share it instead of
re-implementing payment plumbing. The host supplies a database, a 32-byte
encryption key, HTTP handlers, and UI; `paygate` owns the gateways, ledger,
balance, idempotency, and webhooks.

## Features

- **Two gateways, one interface** — Stripe (PaymentIntents → webhook) and PayPal
  (Orders → capture), selectable at runtime.
- **Prepaid balances** — exact `NUMERIC(12,4)` decimal math in Postgres, never
  floats.
- **Saved cards (Stripe)** — save during a top-up, list, set-default, delete,
  and off-session charging with full parity to a battle-tested flow.
- **Encrypted credentials at rest** — AES-256-GCM, with each ciphertext bound to
  `(namespace, gateway)`.
- **Idempotent & money-safe** — DB-level uniqueness on transactions/webhooks and
  a pending-guarded balance credit, so redelivered webhooks never double-credit.

## Install

```sh
go get github.com/farhanahmed1/paygate
```

Requires Go 1.25+ and PostgreSQL.

## Quick start

```go
import "github.com/farhanahmed1/paygate"

// db satisfies paygate.DB (a *sql.DB does); key is 32 bytes (AES-256);
// namespace binds ciphertexts to this host (e.g. "payment_gateway").
svc := paygate.NewService(db, key, "payment_gateway")

// Configure a gateway (admin) — credentials are encrypted at rest:
_ = svc.UpsertGatewaySetting(paygate.GatewayStripe, "Credit Card", true, true,
    map[string]string{
        "publishable_key": "pk_test_…",
        "secret_key":      "sk_test_…",
        "webhook_secret":  "whsec_…",
    })

// Start a top-up (tenant), optionally saving the card for later:
res, err := svc.CreateTopup(ctx, paygate.TopupRequest{
    TenantID:      tenantID,
    Gateway:       paygate.GatewayStripe,
    AmountCents:   2500,
    SaveCard:      true,
    CustomerEmail: "user@example.com",
})
// res.ClientSecret → confirm with Stripe.js in the browser; the
// payment_intent.succeeded webhook credits the balance (and saves the card).

// Later: top up off-session with a saved card:
res, err = svc.ChargeSavedMethod(ctx, tenantID, methodID, 2500)

// Inbound webhook (mount on a public, unauthenticated route):
err = svc.ProcessWebhook(ctx, paygate.GatewayStripe, body, req.Header)
```

## Gateways

| Gateway | Completion model | Saved cards |
|---------|------------------|-------------|
| `stripe` | PaymentIntent confirmed client-side → credited by the `payment_intent.succeeded` webhook | Yes (`SavedMethodGateway`) |
| `paypal` | Order approved in-browser → captured server-side by `CaptureTopup` | No |

Gateways implement the [`Gateway`](gateway.go) interface; PayPal additionally
implements [`CapturableGateway`](gateway.go), and Stripe implements
[`SavedMethodGateway`](gateway.go). Building a gateway only requires it to be
configured; the *enabled* flag is enforced on the payment path.

## What the host provides

`paygate` is the core only. The host application provides:

1. **A database** implementing the [`DB`](billing.go) interface (`*sql.DB`
   satisfies it) and the schema below.
2. **HTTP handlers & routes** — authenticated tenant endpoints (overview,
   top-up, saved-method management) and public, signature-verified webhook
   endpoints.
3. **UI** — e.g. Stripe Elements / PayPal Buttons for the browser side.

### Required schema

`paygate` does not run migrations; create these tables (Postgres). Reference
DDL — adapt the `tenant_id` type and UUID default to your app:

```text
tenants
  id           UUID PRIMARY KEY
  balance      NUMERIC(12,4) NOT NULL DEFAULT 0

payment_gateway_settings
  name             VARCHAR(50) PRIMARY KEY   -- 'stripe' | 'paypal'
  display_name     VARCHAR
  is_enabled       BOOLEAN NOT NULL DEFAULT false
  is_test_mode     BOOLEAN NOT NULL DEFAULT true
  encrypted_config TEXT NOT NULL DEFAULT ''  -- AES-256-GCM blob

payment_methods
  id, tenant_id (FK→tenants), gateway, payment_type,
  gateway_customer_id NOT NULL, gateway_payment_method_id NOT NULL,
  card_brand, card_last4, card_exp_month, card_exp_year,
  nickname, is_default, is_active, last_used_at,
  UNIQUE (tenant_id, gateway, gateway_payment_method_id)

payment_transactions
  id, tenant_id (FK→tenants), gateway, transaction_type ('topup'),
  amount NUMERIC(12,4), currency, status ('pending'|'completed'|'failed'),
  gateway_transaction_id, gateway_customer_id, description,
  UNIQUE (gateway, gateway_transaction_id)        -- DB-level idempotency

payment_webhook_logs
  id, gateway, event_id, event_type, payload JSONB, status, ...,
  UNIQUE (gateway, event_id)
```

## Saved cards (Stripe)

- **Save during top-up**: `CreateTopup` with `SaveCard: true` ensures a gateway
  customer (looked up in `payment_methods`, then the ledger, else created) and
  sets `setup_future_usage=off_session`. The success webhook persists the card.
- **Manage**: `ListPaymentMethods`, `SetDefaultPaymentMethod`,
  `DeletePaymentMethod` (soft-delete + best-effort provider detach + default
  promotion). The default is one per tenant.
- **Pay off-session**: `ChargeSavedMethod` charges a stored card and credits the
  balance synchronously when it settles immediately (the webhook is an
  idempotent backstop).

## Webhooks & money-safety

`ProcessWebhook` verifies the provider signature, records the event for audit
(`UNIQUE(gateway, event_id)`), and applies it. Balance credits run inside a
transaction guarded by `WHERE status='pending'`, so a redelivered or duplicate
webhook credits nothing the second time.

## Security

- Gateway credentials are encrypted with AES-256-GCM ([`internal/crypto`](internal/crypto)),
  the ciphertext bound to `(namespace, gateway)` as additional authenticated
  data — a Stripe blob cannot be decrypted as PayPal, nor reused across hosts.
- Public keys (`publishable_key` / `client_id`) are the only credential values
  surfaced to clients; secret keys never leave the server.

## Versioning

Released as Go module versions; see [CHANGELOG.md](CHANGELOG.md).

## License

[Apache-2.0](LICENSE).
