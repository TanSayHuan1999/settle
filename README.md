# settle

A small payment platform in Go + PostgreSQL, built on a proper
**double-entry ledger**. It opens accounts, takes deposits, and moves
money between accounts — every movement an atomic, balanced, idempotent
ledger transaction, with a server-rendered dashboard on top (no JS
framework, no build pipeline).

Moving money between two accounts sounds trivial until it isn't:
clients retry and double-post, two payments race and overdraw, a crash
lands between the debit and the credit, and three months later nobody
can prove the books balance. Settle is the small, careful version of
that system: the ledger is the source of truth, and every rule that
protects it is enforced inside the transaction that moves the money.

**Live demo:** the app is running at http://5.222.0.70/settle/ — open
the dashboard, post a payment from the Pay page, then check the
Integrity page to watch the ledger prove it still balances.

## How it works

A payment is one database transaction:

```
POST /payments ──▶ begin transaction
                     lock both account rows (FOR UPDATE, in id order)
                     validate: positive amount, different accounts,
                               same currency, sender has the funds
                     insert the payment row
                     insert 2 ledger entries: −amount (sender), +amount (receiver)
                     update both stored balances
                   commit — or roll everything back
```

**Double-entry, in plain terms:** money never appears or disappears.
Every payment writes two entries that sum to zero — a debit on the
sender, an equal credit on the receiver — so the sum of *all* entries
in a currency is always zero. An account's balance is the sum of its
entries; if the entries are right, the balances cannot be wrong.

Deposits follow the same rule. Each currency has a system account,
`EXTERNAL`, and a deposit is a payment from EXTERNAL to the user.
EXTERNAL therefore sits at exactly −(total user balances): the mirror
image of all the money in the system, and the reason "the ledger sums
to zero" covers money entering, not just money moving.

**Idempotency:** deposits and payments accept an `Idempotency-Key`
header (the UI's forms mint one per render). Replaying a key returns
the original payment and writes nothing — a retried request, a
double-clicked form, a resent webhook can never move money twice.
Leave the header out and one is generated for you.

## Design decisions worth talking about

- **Double-entry, always.** Every payment writes exactly two ledger
  entries inside one transaction. Per currency, entries always sum to
  zero. There is no code path that moves money any other way — the
  UI's forms call the same engine as the API.
- **Money enters through the EXTERNAL account.** Deposits are
  payments from a per-currency system account, so even funding the
  system is balanced double-entry, and the zero-sum invariant holds
  across *all* accounts, not just user-to-user payments.
- **Integer minor units only.** Amounts are `amount_cents` — int64
  cents, end to end. JSON decoding into an int64 rejects `10.5` at the
  door, and the UI parses dollar strings digit by digit. Floats never
  touch money.
- **An Idempotency-Key on every money-moving call.** A successful
  posting replayed under the same key returns the original payment
  (200, not 201). A *failed* payment rolls back completely — including
  its payment row — so the same key can be retried after funding and
  will post fresh. If two requests race on one key, the unique index
  settles it: the loser re-reads and returns the winner's payment.
- **Rules enforced inside the transaction.** Both account rows are
  locked `SELECT … FOR UPDATE` in id order, so opposite-direction
  payments serialise instead of deadlocking, and ten concurrent
  payments against one balance cannot overdraw it. Sufficient funds,
  no self-payments, positive amounts, same currency — all checked
  under the lock, before anything is written.
- **Balances are accountable.** Each account's stored balance is
  maintained in the same transaction as its entries, and `GET /verify`
  recomputes every balance from the raw entries and asserts each
  currency sums to zero. Reconciliation as a built-in endpoint — and a
  page in the UI — not a script you run when something smells.

## API

Amounts are always integer `amount_cents`. Money-moving calls take an
optional `Idempotency-Key` header.

| Endpoint | What it does |
|---|---|
| `POST /accounts` | Open an account: `{name, currency}` |
| `GET /accounts` | List accounts with balances |
| `GET /accounts/{id}` | One account, its balance, and recent entries |
| `POST /deposits` | Fund an account from EXTERNAL: `{account_id, amount_cents}` |
| `POST /payments` | Move money: `{from_id, to_id, amount_cents, note?}` |
| `GET /payments/{id}` | One payment and its two ledger entries |
| `GET /verify` | Integrity report: stored vs recomputed balances, per-currency sums |
| `GET /healthz` | Liveness check |

```bash
curl -X POST localhost:8080/payments \
  -H "Idempotency-Key: rent-split-nov" \
  -H "Content-Type: application/json" \
  -d '{"from_id": "…", "to_id": "…", "amount_cents": 18575, "note": "rent split"}'
# 201 — send it again with the same key and you get the same payment back (200)
```

Errors map to status codes: 400 for validation, 404 for unknown ids,
409 for insufficient funds — and a failed payment writes nothing.

## The UI

Server-rendered HTML with embedded templates and one stylesheet.

- **Dashboard** (`/`) — account cards with balances, per-currency
  totals held for users, recent payments, and the EXTERNAL system
  accounts in their own labelled section.
- **Account statement** (`/accounts/{id}`) — balance, a deposit form,
  and the full statement: every entry with its counterparty, note,
  and running balance.
- **Pay** (`/pay`) — the payment form: from/to accounts, amount,
  optional note. Errors come back in plain language on the form;
  resubmitting a form replays instead of double-paying.
- **Integrity** (`/integrity`) — the `/verify` report rendered:
  balanced banner, per-currency sums, and a stored-vs-recomputed
  row for every account.

## Run it

With Docker (Postgres on host port 5434, app on 8080):

```bash
docker compose up
# → http://localhost:8080
```

Locally (needs Go and a Postgres to point at):

```bash
DATABASE_URL=postgres://settle:settle@localhost:5434/settle go run ./cmd/settle
```

The schema is created automatically at startup. Configuration is
environment-only: `DATABASE_URL` (required), `PORT` (8080), and
`BASE_PATH` — set it (e.g. `/settle`) when a reverse proxy serves the
UI under a path prefix and strips it; routes themselves stay
unprefixed and the JSON API ignores it.

**Seed data:** on first boot, if there are no user accounts yet, the
app seeds a small demo dataset — Aisha Rahman, Wei Jie Tan, and Priya
Nair (SGD), Marcus Lee (USD), and Lakeside Mart (an SGD merchant) —
with opening deposits and ten payments with human notes (rent split,
groceries, a merchant refund…). Seed keys are fixed, so a restart
never re-seeds or double-posts.

## Tests

```bash
TEST_DATABASE_URL=postgres://settle:settle@localhost:5434/postgres go test ./...
```

21 tests. The DB-backed ones create a throwaway database per package
(and skip if no database URL is set). They test the invariants, not
just the endpoints:

- a payment's entries sum to zero, and the per-currency total —
  including the EXTERNAL mirror — stays zero after any flow;
- insufficient funds rejects with nothing written, and the
  idempotency key stays reusable afterwards;
- replaying a payment or deposit under the same key returns the
  original and moves nothing;
- ten concurrent 200¢ payments against a 1000¢ balance: exactly five
  succeed, the balance lands on exactly 0 — also under `-race`;
- a randomised 200-payment run still passes `/verify` with no money
  created or destroyed;
- validation (self-payment, zero/negative amounts, mixed currencies)
  rejects before writing;
- amount parsing rejects `10.555`, `abc`, and `0` without ever
  going through a float, and resubmitting the pay form does not
  double-pay.

The deployed instance's `/verify` agrees: every account's stored
balance matches its balance recomputed from raw entries, and each
currency sums to zero.

## Out of scope

Deliberately, for v1: no auth or logins, no real money, no FX
(accounts are multi-currency, but payments are same-currency only),
no cards or bank integrations. Outbound webhook notifications aren't
in either — that's [relayhook](https://github.com/TanSayHuan1999/relayhook)'s
job; the two are designed to pair.

## License

MIT
