# Stage 1 notes — ledger core + API (24 Nov 2026)

Stage 1 is complete: schema + migration at startup, the double-entry
money engine, the full JSON API from the plan, Docker/compose/CI
scaffolding, and the invariant test suite. No UI yet (stage 2), no
GitHub repo yet (publishing is a later stage).

## Sign convention (the one choice the plan left open)

**Credit positive, debit negative.** A payment writes −amount on the
sender and +amount on the receiver. An account's balance is the sum
of its entries; `accounts.balance_cents` is that sum, maintained in
the same transaction. The per-currency EXTERNAL system account
therefore sits at exactly −(total user balances) — the mirror image
of all money in the system — which makes "the ledger sums to zero"
a single, easy-to-explain invariant.

## Layout (mirrors relayhook)

- `cmd/settle/main.go` — boot: config → store → migrate → serve.
- `internal/config` — `DATABASE_URL` (required), `PORT` (default 8080).
- `internal/store/store.go` — pool, schema/migrate (advisory-locked,
  like relayhook), accounts, reads, EXTERNAL account get-or-create.
- `internal/store/ledger.go` — `PostPayment`, `Deposit`, `Verify`.
- `internal/api` — stdlib `net/http` mux, JSON in/out, error mapping
  (400 validation / 404 missing / 409 insufficient funds).
- `internal/testdb` — throwaway database per test package (copied
  from relayhook's pattern).

## Engine details worth knowing

- Both account rows are locked `SELECT … FOR UPDATE ORDER BY id` —
  deterministic order, so opposite-direction concurrent payments
  serialise instead of deadlocking.
- A failed payment (insufficient funds, validation) rolls back
  completely — including its payment row — so the same
  Idempotency-Key can be retried after funding and will post fresh.
  A *successful* posting replayed under the same key returns the
  original payment (200, not 201) and writes nothing.
- The concurrent-same-key race is handled: if the unique index fires
  between the in-tx check and the insert, the loser re-reads and
  returns the winner's payment.
- Amounts decode into `int64` JSON fields, so a fractional
  `amount_cents` (10.5) is rejected at decode time — floats never
  touch money.

## How the tests were run

Docker is not available on this VM and no Postgres was running, so
the tests ran against a throwaway **PostgreSQL 16.15** instance
(the official Ubuntu noble .debs, extracted under /tmp — no system
install) listening on 127.0.0.1:5434, superuser `settle`, trust
auth. Each test creates and drops its own database via
`internal/testdb`. The project's canonical dev path stays the
docker-compose db (host port 5434) in `docker-compose.yml`.

```
DATABASE_URL=postgres://settle:settle@127.0.0.1:5434/postgres go test ./... -count=1
```

## Results

- `go build ./...` and `go vet ./...` clean.
- **10/10 tests pass** (7 store invariant tests, 3 API tests):
  balanced pair + balance movement; per-currency zero-sum incl.
  EXTERNAL mirror; insufficient funds writes nothing (and the key
  stays reusable); idempotent replay for payments and deposits;
  10-way concurrency — exactly 5 of 10 × 200¢ succeed against 1000¢,
  balance lands on exactly 0; randomised 200-payment run still
  verifies with no money created/destroyed; all validation rules
  reject before writing.
- Concurrency test also passes under `-race -count=3`.
- Manual smoke test of the built binary against a scratch database:
  account creation, deposit 201 → replay 200 (same payment id),
  payment 201, overdraw 409 `{"error":"insufficient funds"}`,
  `/verify` → `ok: true`, SGD entries sum 0.

## Deviations from the plan

None in scope or API shape. Two small calls made along the way:

1. Deposits/payments **without** an `Idempotency-Key` header get a
   generated key (echoed on the payment), matching relayhook's
   behaviour, instead of rejecting the call — the plan lists the
   header as part of the call but never says it is mandatory.
2. CI is included already (the plan puts CI in stage 1's build
   order line); it mirrors relayhook's workflow with a Postgres 16
   service.
