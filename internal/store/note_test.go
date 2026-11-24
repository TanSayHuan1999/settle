package store_test

import (
	"context"
	"testing"

	"settle/internal/store"
	"settle/internal/testdb"
)

// Stage 2: the payments.note column round-trips through the engine,
// and payments posted without a note read back with an empty one.
func TestPaymentNoteRoundTrips(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()

	alice, err := st.CreateAccount(ctx, "Alice", "SGD")
	if err != nil {
		t.Fatalf("create alice: %v", err)
	}
	bob, err := st.CreateAccount(ctx, "Bob", "SGD")
	if err != nil {
		t.Fatalf("create bob: %v", err)
	}
	depKey := "note-dep"
	if _, _, err := st.Deposit(ctx, alice.ID, 10_000, &depKey); err != nil {
		t.Fatalf("deposit: %v", err)
	}

	payKey := "note-pay"
	p, _, err := st.PostPaymentWithNote(ctx, alice.ID, bob.ID, 2_500,
		store.KindPayment, &payKey, "Invoice 42 — design work")
	if err != nil {
		t.Fatalf("post payment with note: %v", err)
	}
	if p.Note != "Invoice 42 — design work" {
		t.Fatalf("returned payment note = %q, want %q", p.Note, "Invoice 42 — design work")
	}
	got, err := st.GetPayment(ctx, p.ID)
	if err != nil {
		t.Fatalf("get payment: %v", err)
	}
	if got.Note != "Invoice 42 — design work" {
		t.Fatalf("stored note = %q, want %q", got.Note, "Invoice 42 — design work")
	}

	plainKey := "note-plain"
	plain, _, err := st.PostPayment(ctx, alice.ID, bob.ID, 100, store.KindPayment, &plainKey)
	if err != nil {
		t.Fatalf("post payment without note: %v", err)
	}
	if plain.Note != "" {
		t.Fatalf("note-less payment note = %q, want empty", plain.Note)
	}

	// The note also surfaces on the display read models.
	rows, err := st.ListPayments(ctx, 10)
	if err != nil {
		t.Fatalf("list payments: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.ID == p.ID {
			found = true
			if r.Note != "Invoice 42 — design work" || r.FromName != "Alice" || r.ToName != "Bob" {
				t.Fatalf("payment row = %+v, want note + Alice → Bob", r)
			}
		}
	}
	if !found {
		t.Fatalf("seeded payment %s missing from ListPayments", p.ID)
	}

	stmt, err := st.ListStatement(ctx, bob.ID, 10)
	if err != nil {
		t.Fatalf("list statement: %v", err)
	}
	if len(stmt) != 2 {
		t.Fatalf("bob statement has %d rows, want 2 (both payments in)", len(stmt))
	}
	found = false
	for _, row := range stmt {
		if row.Note == "Invoice 42 — design work" {
			found = true
			if row.CounterpartyName != "Alice" || row.Kind != store.KindPayment ||
				row.Entry.AmountCents != 2_500 {
				t.Fatalf("noted statement row = %+v, want +2500 payment from Alice", row)
			}
		}
	}
	if !found {
		t.Fatalf("noted payment missing from bob's statement: %+v", stmt)
	}
}
