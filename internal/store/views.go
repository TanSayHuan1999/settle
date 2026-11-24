package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// This file holds read models shaped for display (the dashboard UI):
// payments with both account names resolved, and account statements
// with the counterparty on every row. The write path and the plain
// entity reads stay in store.go / ledger.go.

// PaymentRow is a payment with the names of both accounts resolved,
// ready to render in a list.
type PaymentRow struct {
	Payment
	FromName string
	ToName   string
}

// paymentColumnsP is paymentColumns with a p. table prefix, for
// queries that join payments against accounts.
const paymentColumnsP = `p.id, p.from_account, p.to_account, p.amount_cents, p.currency, p.kind, p.idempotency_key, p.note, p.created_at`

func scanPaymentRow(row pgx.Row) (*PaymentRow, error) {
	var r PaymentRow
	if err := row.Scan(&r.ID, &r.FromAccount, &r.ToAccount, &r.AmountCents,
		&r.Currency, &r.Kind, &r.IdempotencyKey, &r.Note, &r.CreatedAt,
		&r.FromName, &r.ToName); err != nil {
		return nil, err
	}
	return &r, nil
}

// ListPayments returns the most recent payments across all accounts,
// newest first, with sender and receiver names attached.
func (s *Store) ListPayments(ctx context.Context, limit int) ([]*PaymentRow, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+paymentColumnsP+`, fa.name, ta.name
		FROM payments p
		JOIN accounts fa ON fa.id = p.from_account
		JOIN accounts ta ON ta.id = p.to_account
		ORDER BY p.created_at DESC, p.id DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list payments: %w", err)
	}
	defer rows.Close()
	out := []*PaymentRow{}
	for rows.Next() {
		r, err := scanPaymentRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scan payment row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PaymentCount returns how many payments have ever been posted.
func (s *Store) PaymentCount(ctx context.Context) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM payments`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count payments: %w", err)
	}
	return n, nil
}

// StatementRow is one ledger entry on an account's statement, with
// the payment's kind and note and the OTHER account's identity —
// the counterparty whose side of the payment this entry mirrors.
type StatementRow struct {
	Entry            *Entry
	Kind             string
	Note             string
	CounterpartyID   uuid.UUID
	CounterpartyName string
}

// ListStatement returns an account's entries, newest first, each
// annotated with its payment's kind/note and the counterparty.
// Callers derive running balances by walking the rows from the
// account's current balance (see the web package), which stays
// correct even when the statement is truncated by limit.
func (s *Store) ListStatement(ctx context.Context, accountID uuid.UUID, limit int) ([]*StatementRow, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx, `
		SELECT e.id, e.payment_id, e.account_id, e.amount_cents, e.created_at,
		       p.kind, p.note,
		       CASE WHEN p.from_account = e.account_id THEN p.to_account ELSE p.from_account END,
		       cp.name
		FROM ledger_entries e
		JOIN payments p ON p.id = e.payment_id
		JOIN accounts cp ON cp.id = CASE WHEN p.from_account = e.account_id
		                              THEN p.to_account ELSE p.from_account END
		WHERE e.account_id = $1
		ORDER BY e.created_at DESC, e.id DESC
		LIMIT $2`, accountID, limit)
	if err != nil {
		return nil, fmt.Errorf("list statement: %w", err)
	}
	defer rows.Close()
	out := []*StatementRow{}
	for rows.Next() {
		var r StatementRow
		r.Entry = &Entry{}
		if err := rows.Scan(&r.Entry.ID, &r.Entry.PaymentID, &r.Entry.AccountID,
			&r.Entry.AmountCents, &r.Entry.CreatedAt, &r.Kind, &r.Note,
			&r.CounterpartyID, &r.CounterpartyName); err != nil {
			return nil, fmt.Errorf("scan statement row: %w", err)
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}
