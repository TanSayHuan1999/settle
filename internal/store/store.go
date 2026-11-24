// Package store is the Postgres persistence layer and the home of
// the ledger engine. The rules that keep the books correct:
//
//   - Double-entry, always: every payment writes exactly two ledger
//     entries in one transaction — a debit on the sender and a credit
//     on the receiver — so per currency the entries always sum to
//     zero and money is never created or destroyed inside the system.
//   - Sign convention: a credit (money in) is POSITIVE, a debit
//     (money out) is NEGATIVE. An account's balance is the sum of its
//     entries; accounts.balance_cents is that sum, maintained inside
//     the same transaction that writes the entries.
//   - Money is integer cents (int64) end to end. No floats.
//   - Money enters the system only as a payment from the per-currency
//     EXTERNAL system account, whose balance goes equally negative —
//     even deposits balance.
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Payment kinds.
const (
	KindPayment = "payment"
	KindDeposit = "deposit"
)

// SystemAccountName is the name of the per-currency system account
// that funds deposits.
const SystemAccountName = "EXTERNAL"

// Errors returned by the ledger engine. The API layer maps them to
// HTTP status codes; tests assert on them directly.
var (
	ErrInvalidAmount     = errors.New("amount must be a positive number of cents")
	ErrSelfPayment       = errors.New("sender and receiver must be different accounts")
	ErrCurrencyMismatch  = errors.New("accounts are in different currencies")
	ErrInsufficientFunds = errors.New("insufficient funds")
	ErrAccountNotFound   = errors.New("account not found")
	ErrPaymentNotFound   = errors.New("payment not found")
	ErrInvalidCurrency   = errors.New("currency must be a 3-letter code")
	ErrInvalidName       = errors.New("account name must not be empty")
	ErrSystemDeposit     = errors.New("cannot deposit into a system account")
)

// Account is a ledger account. BalanceCents is the sum of the
// account's ledger entries, maintained transactionally.
type Account struct {
	ID           uuid.UUID `json:"id"`
	Name         string    `json:"name"`
	Currency     string    `json:"currency"`
	BalanceCents int64     `json:"balance_cents"`
	IsSystem     bool      `json:"is_system"`
	CreatedAt    time.Time `json:"created_at"`
}

// Payment is one money movement: a balanced pair of ledger entries
// from FromAccount to ToAccount.
type Payment struct {
	ID             uuid.UUID `json:"id"`
	FromAccount    uuid.UUID `json:"from_account"`
	ToAccount      uuid.UUID `json:"to_account"`
	AmountCents    int64     `json:"amount_cents"`
	Currency       string    `json:"currency"`
	Kind           string    `json:"kind"`
	IdempotencyKey *string   `json:"idempotency_key,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// Entry is one side of a payment: negative on the sender (debit),
// positive on the receiver (credit).
type Entry struct {
	ID          uuid.UUID `json:"id"`
	PaymentID   uuid.UUID `json:"payment_id"`
	AccountID   uuid.UUID `json:"account_id"`
	AmountCents int64     `json:"amount_cents"`
	CreatedAt   time.Time `json:"created_at"`
}

// Store wraps the connection pool.
type Store struct {
	pool *pgxpool.Pool
}

// New creates a Store from a Postgres connection string.
func New(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close releases the connection pool.
func (s *Store) Close() { s.pool.Close() }

const schema = `
CREATE TABLE IF NOT EXISTS accounts (
	id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	name          text NOT NULL,
	currency      text NOT NULL,
	balance_cents bigint NOT NULL DEFAULT 0,
	is_system     boolean NOT NULL DEFAULT false,
	created_at    timestamptz NOT NULL DEFAULT now()
);
-- At most one system (EXTERNAL) account per currency.
CREATE UNIQUE INDEX IF NOT EXISTS idx_accounts_one_system_per_currency
	ON accounts (currency) WHERE is_system;
CREATE TABLE IF NOT EXISTS payments (
	id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	from_account    uuid NOT NULL REFERENCES accounts(id),
	to_account      uuid NOT NULL REFERENCES accounts(id),
	amount_cents    bigint NOT NULL CHECK (amount_cents > 0),
	currency        text NOT NULL,
	kind            text NOT NULL CHECK (kind IN ('payment', 'deposit')),
	idempotency_key text UNIQUE,
	created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS ledger_entries (
	id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	payment_id   uuid NOT NULL REFERENCES payments(id) ON DELETE CASCADE,
	account_id   uuid NOT NULL REFERENCES accounts(id),
	amount_cents bigint NOT NULL,
	created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_entries_account
	ON ledger_entries (account_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_entries_payment
	ON ledger_entries (payment_id);
CREATE INDEX IF NOT EXISTS idx_payments_created
	ON payments (created_at DESC);
`

// migrateLockKey serialises concurrent migrations (e.g. several
// instances booting at once) so CREATE TABLE IF NOT EXISTS cannot
// race itself.
const migrateLockKey = 535353

// Migrate creates the schema if it does not exist yet.
func (s *Store) Migrate(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("migrate: acquire conn: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrateLockKey); err != nil {
		return fmt.Errorf("migrate: lock: %w", err)
	}
	defer func() { _, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, migrateLockKey) }()
	if _, err := conn.Exec(ctx, schema); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

const accountColumns = `id, name, currency, balance_cents, is_system, created_at`

func scanAccount(row pgx.Row) (*Account, error) {
	var a Account
	if err := row.Scan(&a.ID, &a.Name, &a.Currency, &a.BalanceCents, &a.IsSystem, &a.CreatedAt); err != nil {
		return nil, err
	}
	return &a, nil
}

const paymentColumns = `id, from_account, to_account, amount_cents, currency, kind, idempotency_key, created_at`

func scanPayment(row pgx.Row) (*Payment, error) {
	var p Payment
	if err := row.Scan(&p.ID, &p.FromAccount, &p.ToAccount, &p.AmountCents,
		&p.Currency, &p.Kind, &p.IdempotencyKey, &p.CreatedAt); err != nil {
		return nil, err
	}
	return &p, nil
}

// NormalizeCurrency upper-cases and validates a currency code.
func NormalizeCurrency(c string) (string, error) {
	c = strings.ToUpper(strings.TrimSpace(c))
	if len(c) != 3 {
		return "", ErrInvalidCurrency
	}
	for _, r := range c {
		if r < 'A' || r > 'Z' {
			return "", ErrInvalidCurrency
		}
	}
	return c, nil
}

// CreateAccount opens a user account with a zero balance.
func (s *Store) CreateAccount(ctx context.Context, name, currency string) (*Account, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, ErrInvalidName
	}
	currency, err := NormalizeCurrency(currency)
	if err != nil {
		return nil, err
	}
	a, err := scanAccount(s.pool.QueryRow(ctx, `
		INSERT INTO accounts (name, currency) VALUES ($1, $2)
		RETURNING `+accountColumns, name, currency))
	if err != nil {
		return nil, fmt.Errorf("insert account: %w", err)
	}
	return a, nil
}

// GetAccount returns one account by id, or ErrAccountNotFound.
func (s *Store) GetAccount(ctx context.Context, id uuid.UUID) (*Account, error) {
	a, err := scanAccount(s.pool.QueryRow(ctx,
		`SELECT `+accountColumns+` FROM accounts WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAccountNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get account: %w", err)
	}
	return a, nil
}

// ListAccounts returns all accounts, system accounts first, then by
// creation time.
func (s *Store) ListAccounts(ctx context.Context) ([]*Account, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+accountColumns+` FROM accounts ORDER BY is_system DESC, created_at ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	defer rows.Close()
	out := []*Account{}
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("scan account: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetPayment returns one payment by id, or ErrPaymentNotFound.
func (s *Store) GetPayment(ctx context.Context, id uuid.UUID) (*Payment, error) {
	p, err := scanPayment(s.pool.QueryRow(ctx,
		`SELECT `+paymentColumns+` FROM payments WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPaymentNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get payment: %w", err)
	}
	return p, nil
}

// ListEntries returns an account's most recent ledger entries,
// newest first.
func (s *Store) ListEntries(ctx context.Context, accountID uuid.UUID, limit int) ([]*Entry, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, payment_id, account_id, amount_cents, created_at
		FROM ledger_entries WHERE account_id = $1
		ORDER BY created_at DESC, id DESC LIMIT $2`, accountID, limit)
	if err != nil {
		return nil, fmt.Errorf("list entries: %w", err)
	}
	defer rows.Close()
	out := []*Entry{}
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.PaymentID, &e.AccountID, &e.AmountCents, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan entry: %w", err)
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

// ListPaymentEntries returns the entries belonging to one payment —
// always exactly two for a correctly posted payment.
func (s *Store) ListPaymentEntries(ctx context.Context, paymentID uuid.UUID) ([]*Entry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, payment_id, account_id, amount_cents, created_at
		FROM ledger_entries WHERE payment_id = $1 ORDER BY amount_cents ASC`, paymentID)
	if err != nil {
		return nil, fmt.Errorf("list payment entries: %w", err)
	}
	defer rows.Close()
	out := []*Entry{}
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.PaymentID, &e.AccountID, &e.AmountCents, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan entry: %w", err)
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

// SystemAccount returns the EXTERNAL system account for a currency,
// creating it on first use. Its balance is allowed to go negative:
// it is the mirror image of all money users hold in that currency.
func (s *Store) SystemAccount(ctx context.Context, currency string) (*Account, error) {
	currency, err := NormalizeCurrency(currency)
	if err != nil {
		return nil, err
	}
	a, err := scanAccount(s.pool.QueryRow(ctx, `
		INSERT INTO accounts (name, currency, is_system) VALUES ($1, $2, true)
		ON CONFLICT (currency) WHERE is_system DO NOTHING
		RETURNING `+accountColumns, SystemAccountName, currency))
	if err == nil {
		return a, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("insert system account: %w", err)
	}
	a, err = scanAccount(s.pool.QueryRow(ctx,
		`SELECT `+accountColumns+` FROM accounts WHERE is_system AND currency = $1`, currency))
	if err != nil {
		return nil, fmt.Errorf("fetch system account: %w", err)
	}
	return a, nil
}
