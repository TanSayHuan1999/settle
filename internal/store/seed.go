package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// SeedIfEmpty loads the demo dataset the first time the service
// boots against an empty database: five named accounts, a deposit
// funding each one, and ten believable payments between them. It
// runs only when there are no user accounts at all, and every money
// movement carries a fixed idempotency key ("seed-dep-…",
// "seed-pay-…"), so even a crash mid-seed followed by a restart can
// never double-post — the replayed calls return the originals.
//
// All movements go through Deposit / PostPaymentWithNote, so the
// seed obeys exactly the invariants the rest of the system enforces
// (in particular, the payment order below never overdraws anyone).
// It reports whether it seeded.
func (s *Store) SeedIfEmpty(ctx context.Context) (bool, error) {
	var userAccounts int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM accounts WHERE NOT is_system`).Scan(&userAccounts); err != nil {
		return false, fmt.Errorf("seed: count accounts: %w", err)
	}
	if userAccounts > 0 {
		return false, nil
	}

	type seedAccount struct {
		slug     string
		name     string
		currency string
		deposit  int64 // cents
	}
	specs := []seedAccount{
		{"aisha", "Aisha Rahman", "SGD", 250_000},    // SGD 2,500.00
		{"weijie", "Wei Jie Tan", "SGD", 180_000},    // SGD 1,800.00
		{"priya", "Priya Nair", "SGD", 95_000},       // SGD 950.00
		{"marcus", "Marcus Lee", "USD", 120_000},     // USD 1,200.00
		{"lakeside", "Lakeside Mart", "SGD", 30_000}, // SGD 300.00
	}
	ids := make(map[string]uuid.UUID, len(specs))
	for _, spec := range specs {
		acct, err := s.CreateAccount(ctx, spec.name, spec.currency)
		if err != nil {
			return false, fmt.Errorf("seed: create %s: %w", spec.name, err)
		}
		ids[spec.slug] = acct.ID
		key := "seed-dep-" + spec.slug
		if _, _, err := s.Deposit(ctx, acct.ID, spec.deposit, &key); err != nil {
			return false, fmt.Errorf("seed: deposit %s: %w", spec.name, err)
		}
	}

	type seedPayment struct {
		from, to string // slugs
		amount   int64  // cents
		note     string
	}
	payments := []seedPayment{
		{"aisha", "weijie", 60_000, "Rent split — November"},
		{"weijie", "lakeside", 8_450, "Groceries for the week"},
		{"priya", "lakeside", 3_260, "Groceries and household supplies"},
		{"aisha", "priya", 12_000, "Dinner — hawker centre, my treat"},
		{"weijie", "priya", 4_500, "Taxi share to the airport"},
		{"priya", "aisha", 20_000, "Concert ticket — paying you back"},
		{"aisha", "lakeside", 1_575, "Coffee beans and milk"},
		{"weijie", "aisha", 30_000, "Group trip deposit"},
		{"priya", "weijie", 999, "Bubble tea IOU"},
		{"lakeside", "aisha", 5_000, "Refund — returned items"},
	}
	for i, p := range payments {
		key := fmt.Sprintf("seed-pay-%d", i+1)
		if _, _, err := s.PostPaymentWithNote(ctx, ids[p.from], ids[p.to], p.amount,
			KindPayment, &key, p.note); err != nil {
			return false, fmt.Errorf("seed: payment %d (%s): %w", i+1, p.note, err)
		}
	}
	return true, nil
}
