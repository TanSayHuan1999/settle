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

---

# Stage 2 notes — dashboard UI, seed data, payment form (24 Nov 2026)

Stage 2 is complete: the server-rendered UI from the plan, the seed
dataset, and the `payments.note` field. No GitHub repo yet and no
deploy (stage 3).

## What was added

- `internal/web` — the UI package. `html/template` templates and one
  stylesheet are embedded via `embed.FS`; no JS framework, no build
  pipeline, no external assets. Dark-first fintech theme (deep slate
  background, emerald accent, system font stack), responsive grid.
- `internal/store/views.go` — display read models: `ListPayments`
  (payments with both account names), `ListStatement` (entries with
  kind, note and counterparty), `PaymentCount`.
- `internal/store/seed.go` — `SeedIfEmpty`, called from `main` after
  migrate.
- `internal/config` — `BASE_PATH` env var (normalised to "" or
  "/settle"-style); templates prefix every href/form action with it.
  Routes themselves stay unprefixed — in production Caddy strips the
  prefix — and the JSON API is untouched by it.
- `cmd/settle/main.go` — a small `dispatch` in front of the two muxes:
  the UI and API share `GET /`, `GET /accounts/{id}` and
  `POST /accounts`, so requests are split by what browsers send
  (url-encoded form posts → UI; `Accept: text/html` on the account
  page → UI; `/`, `/pay`, `/integrity`, `/static/` are UI-only).
  JSON clients see exactly the stage-1 API.

## Routes

UI: `GET /` dashboard (user account cards, per-currency "held for
users" totals, payments count, recent payments with notes, EXTERNAL
accounts in a separate labelled system section, open-an-account
form) · `GET /accounts/{id}` (balance header, deposit form, full
statement with running balances) · `POST /accounts` (form create) ·
`POST /accounts/{id}/deposit` · `GET|POST /pay` · `GET /integrity`
(the `Verify()` report rendered: OK banner, per-currency sums,
stored-vs-recomputed table) · `GET /static/style.css`.

Forms call the store engine directly — never HTTP self-calls — so
UI money movement holds every invariant API money movement does.
Each money form carries a hidden idempotency key minted at render
time: resubmitting a form replays the original payment instead of
paying twice (verified in tests and in the smoke run).

Amounts in forms are dollars strings parsed by
`web.ParseAmountToCents` — digit-by-digit, never via float; it
rejects >2 decimal places, signs, separators, zero and non-numeric
input. Display uses `web.FormatMoney` ("SGD 1,234.56", negatives
lead with −) and `FormatSigned` (+/−) for statement rows. Engine
errors re-render the form with the error in plain language
("Insufficient funds."); successes redirect (303) to the sender's
account page with a notice banner.

Running balances on the statement are computed by walking the
newest-first rows down from the account's current balance, so they
stay correct even if the statement is ever truncated by its limit.

## The note field

`payments.note text NOT NULL DEFAULT ''` — in the CREATE TABLE for
fresh databases and as an `ALTER TABLE … ADD COLUMN IF NOT EXISTS`
in the same migration for stage-1 databases. `PostPayment` became a
wrapper over an unexported core; the new `PostPaymentWithNote`
carries the note (stage-1 signatures and tests unchanged). The JSON
API accepts an optional `"note"` on `POST /payments`. Seed payments
and the pay form's optional note field use it; deposits stay
note-less. Covered by `TestPaymentNoteRoundTrips` (engine round-trip
plus the ListPayments/ListStatement read models).

## Seed design

Only when zero user accounts exist (checked again on every boot —
a restart never re-seeds; the fixed keys `seed-dep-<slug>` /
`seed-pay-N` would make even a partial re-run replay-safe).
Accounts: Aisha Rahman (SGD), Wei Jie Tan (SGD), Priya Nair (SGD),
Marcus Lee (USD), Lakeside Mart (SGD merchant). Deposits: 2,500 /
1,800 / 950 SGD, 1,200 USD, 300 SGD. Then 10 SGD payments with
human notes (rent split, groceries, concert-ticket payback, bubble
tea IOU, a merchant refund…), ordered so no sender is ever
overdrawn. Marcus (the only USD account) receives a deposit only —
a USD payment would have no same-currency counterparty in v1.

## Results

- `go build`, `go vet`, `gofmt` clean.
- **21/21 tests pass**: stage 1's 10 (3 API + 7 store) all green,
  plus `TestPaymentNoteRoundTrips`, plus 10 web tests — money
  formatting, amount parsing (incl. "10.555"/"abc"/"0" rejected),
  seeded dashboard content + seed idempotence, BASE_PATH link
  prefixing, statement running balances, pay-form happy path
  (balances move; same-form resubmit does NOT double-pay),
  insufficient-funds (error shown, nothing moves, no payment row),
  self-payment and bad-amount rejections, deposit + create-account
  forms, integrity page.
- Store package also passes under `-race`.
- Manual smoke against a scratch DB on the 5434 instance, server on
  :18099: all four pages 200 with seeded content (Aisha at
  SGD 2,314.25, "Ledger balanced", 7 × ✓ match), stylesheet 200,
  API responses unchanged for non-browser clients, a real form
  payment of SGD 10.00 posted (303 → sender page with notice),
  resubmission replayed without moving money again, a 99,999.00
  attempt refused with "Insufficient funds.", and a restart against
  the same DB did not re-seed (7 accounts, balances intact,
  `/verify` ok). Scratch DB dropped afterwards.

## Gotcha worth remembering

`html/template` escapes a leading `+` in text nodes to `&#43;`, so
the `signed` template func returns `template.HTML` (its input is
only a validated currency code and an int64) — otherwise statement
credits would lose their + sign in the page source.
