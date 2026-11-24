package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PostPayment moves amountCents from one account to another as a
// single balanced double-entry transaction. It is PostPaymentWithNote
// with an empty note; see there for the rules.
func (s *Store) PostPayment(ctx context.Context, fromID, toID uuid.UUID, amountCents int64, kind string, idemKey *string) (p *Payment, created bool, err error) {
	return s.postPayment(ctx, fromID, toID, amountCents, kind, idemKey, "")
}

// PostPaymentWithNote is PostPayment with a free-text note attached
// to the payment (a memo like "Rent split — November"). The note is
// display-only: it plays no part in the ledger math.
//
// The transaction guarantees:
//
//   - both account rows are locked with SELECT ... FOR UPDATE in id
//     order, so concurrent payments touching the same accounts
//     serialise and can never deadlock by locking in opposite orders;
//   - the sender must have the funds (system accounts excepted —
//     EXTERNAL is allowed to go negative, it mirrors user money);
//   - exactly two entries are written: -amount on the sender,
//     +amount on the receiver, and both stored balances are updated
//     in the same transaction. Either everything commits or nothing
//     does.
//
// If idemKey is non-nil and a payment with that key already exists,
// the ORIGINAL payment is returned with created=false and nothing is
// written — a retried request never moves money twice.
func (s *Store) PostPaymentWithNote(ctx context.Context, fromID, toID uuid.UUID, amountCents int64, kind string, idemKey *string, note string) (p *Payment, created bool, err error) {
	return s.postPayment(ctx, fromID, toID, amountCents, kind, idemKey, note)
}

func (s *Store) postPayment(ctx context.Context, fromID, toID uuid.UUID, amountCents int64, kind string, idemKey *string, note string) (p *Payment, created bool, err error) {
	if amountCents <= 0 {
		return nil, false, ErrInvalidAmount
	}
	if fromID == toID {
		return nil, false, ErrSelfPayment
	}
	if kind != KindPayment && kind != KindDeposit {
		return nil, false, fmt.Errorf("unknown payment kind %q", kind)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("begin payment tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Idempotent replay: the key already posted — hand back the
	// original payment untouched.
	if idemKey != nil {
		existing, err := scanPayment(tx.QueryRow(ctx,
			`SELECT `+paymentColumns+` FROM payments WHERE idempotency_key = $1`, *idemKey))
		if err == nil {
			if err := tx.Commit(ctx); err != nil {
				return nil, false, fmt.Errorf("commit replay tx: %w", err)
			}
			return existing, false, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, false, fmt.Errorf("check idempotency key: %w", err)
		}
	}

	// Lock both accounts in a deterministic (id) order.
	rows, err := tx.Query(ctx, `
		SELECT `+accountColumns+` FROM accounts
		WHERE id IN ($1, $2) ORDER BY id FOR UPDATE`, fromID, toID)
	if err != nil {
		return nil, false, fmt.Errorf("lock accounts: %w", err)
	}
	var from, to *Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			rows.Close()
			return nil, false, fmt.Errorf("scan locked account: %w", err)
		}
		switch a.ID {
		case fromID:
			from = a
		case toID:
			to = a
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("iterate locked accounts: %w", err)
	}
	if from == nil || to == nil {
		return nil, false, ErrAccountNotFound
	}
	if from.Currency != to.Currency {
		return nil, false, ErrCurrencyMismatch
	}
	if !from.IsSystem && from.BalanceCents < amountCents {
		return nil, false, ErrInsufficientFunds
	}

	p, err = scanPayment(tx.QueryRow(ctx, `
		INSERT INTO payments (from_account, to_account, amount_cents, currency, kind, idempotency_key, note)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING `+paymentColumns,
		fromID, toID, amountCents, from.Currency, kind, idemKey, note))
	if err != nil {
		// A concurrent request with the same idempotency key won the
		// race between our check and this insert: abandon this
		// transaction and return the winner's payment.
		var pgErr *pgconn.PgError
		if idemKey != nil && errors.As(err, &pgErr) && pgErr.Code == "23505" {
			_ = tx.Rollback(ctx)
			existing, ferr := s.GetPaymentByKey(ctx, *idemKey)
			if ferr != nil {
				return nil, false, fmt.Errorf("fetch payment after idempotency race: %w", ferr)
			}
			return existing, false, nil
		}
		return nil, false, fmt.Errorf("insert payment: %w", err)
	}

	// The balanced pair: debit the sender, credit the receiver.
	if _, err := tx.Exec(ctx, `
		INSERT INTO ledger_entries (payment_id, account_id, amount_cents)
		VALUES ($1, $2, $3), ($1, $4, $5)`,
		p.ID, fromID, -amountCents, toID, amountCents); err != nil {
		return nil, false, fmt.Errorf("insert ledger entries: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE accounts SET balance_cents = balance_cents - $2 WHERE id = $1`,
		fromID, amountCents); err != nil {
		return nil, false, fmt.Errorf("debit sender balance: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE accounts SET balance_cents = balance_cents + $2 WHERE id = $1`,
		toID, amountCents); err != nil {
		return nil, false, fmt.Errorf("credit receiver balance: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("commit payment tx: %w", err)
	}
	return p, true, nil
}

// GetPaymentByKey returns the payment posted under an idempotency
// key, or ErrPaymentNotFound.
func (s *Store) GetPaymentByKey(ctx context.Context, key string) (*Payment, error) {
	p, err := scanPayment(s.pool.QueryRow(ctx,
		`SELECT `+paymentColumns+` FROM payments WHERE idempotency_key = $1`, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPaymentNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get payment by key: %w", err)
	}
	return p, nil
}

// Deposit funds an account from its currency's EXTERNAL system
// account — a payment like any other, so deposits are balanced,
// idempotent, and auditable in the ledger.
func (s *Store) Deposit(ctx context.Context, accountID uuid.UUID, amountCents int64, idemKey *string) (*Payment, bool, error) {
	if amountCents <= 0 {
		return nil, false, ErrInvalidAmount
	}
	acct, err := s.GetAccount(ctx, accountID)
	if err != nil {
		return nil, false, err
	}
	if acct.IsSystem {
		return nil, false, ErrSystemDeposit
	}
	external, err := s.SystemAccount(ctx, acct.Currency)
	if err != nil {
		return nil, false, err
	}
	return s.postPayment(ctx, external.ID, accountID, amountCents, KindDeposit, idemKey, "")
}

// AccountCheck compares one account's stored balance with the sum of
// its raw ledger entries.
type AccountCheck struct {
	AccountID            uuid.UUID `json:"account_id"`
	Name                 string    `json:"name"`
	Currency             string    `json:"currency"`
	StoredBalanceCents   int64     `json:"stored_balance_cents"`
	ComputedBalanceCents int64     `json:"computed_balance_cents"`
	OK                   bool      `json:"ok"`
}

// CurrencyCheck asserts that one currency's ledger entries sum to
// exactly zero — money was neither created nor destroyed.
type CurrencyCheck struct {
	Currency        string `json:"currency"`
	EntriesSumCents int64  `json:"entries_sum_cents"`
	OK              bool   `json:"ok"`
}

// VerifyReport is the result of a full ledger reconciliation.
type VerifyReport struct {
	OK         bool            `json:"ok"`
	Accounts   []AccountCheck  `json:"accounts"`
	Currencies []CurrencyCheck `json:"currencies"`
	CheckedAt  time.Time       `json:"checked_at"`
}

// Verify recomputes every account's balance from the raw ledger
// entries and compares it with the stored balance, and checks that
// each currency's entries sum to zero. It is the reconciliation a
// payments team would run against a ledger: if the fast path
// (stored balances) ever drifts from the source of truth (entries),
// Verify says so, per account, by name.
func (s *Store) Verify(ctx context.Context) (*VerifyReport, error) {
	report := &VerifyReport{OK: true, CheckedAt: time.Now().UTC()}

	rows, err := s.pool.Query(ctx, `
		SELECT a.id, a.name, a.currency, a.balance_cents,
		       COALESCE(SUM(e.amount_cents), 0)
		FROM accounts a
		LEFT JOIN ledger_entries e ON e.account_id = a.id
		GROUP BY a.id
		ORDER BY a.currency, a.is_system DESC, a.created_at`)
	if err != nil {
		return nil, fmt.Errorf("verify accounts: %w", err)
	}
	defer rows.Close()
	report.Accounts = []AccountCheck{}
	for rows.Next() {
		var c AccountCheck
		if err := rows.Scan(&c.AccountID, &c.Name, &c.Currency,
			&c.StoredBalanceCents, &c.ComputedBalanceCents); err != nil {
			return nil, fmt.Errorf("scan verify row: %w", err)
		}
		c.OK = c.StoredBalanceCents == c.ComputedBalanceCents
		if !c.OK {
			report.OK = false
		}
		report.Accounts = append(report.Accounts, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate verify rows: %w", err)
	}
	rows.Close()

	crows, err := s.pool.Query(ctx, `
		SELECT a.currency, SUM(e.amount_cents)
		FROM ledger_entries e
		JOIN accounts a ON a.id = e.account_id
		GROUP BY a.currency
		ORDER BY a.currency`)
	if err != nil {
		return nil, fmt.Errorf("verify currencies: %w", err)
	}
	defer crows.Close()
	report.Currencies = []CurrencyCheck{}
	for crows.Next() {
		var c CurrencyCheck
		if err := crows.Scan(&c.Currency, &c.EntriesSumCents); err != nil {
			return nil, fmt.Errorf("scan currency verify row: %w", err)
		}
		c.OK = c.EntriesSumCents == 0
		if !c.OK {
			report.OK = false
		}
		report.Currencies = append(report.Currencies, c)
	}
	if err := crows.Err(); err != nil {
		return nil, fmt.Errorf("iterate currency verify rows: %w", err)
	}
	return report, nil
}
