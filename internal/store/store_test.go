package store_test

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"testing"

	"github.com/google/uuid"

	"settle/internal/store"
	"settle/internal/testdb"
)

func key(s string) *string { return &s }

func mustAccount(t *testing.T, st *store.Store, name, currency string) *store.Account {
	t.Helper()
	a, err := st.CreateAccount(context.Background(), name, currency)
	if err != nil {
		t.Fatalf("create account %s: %v", name, err)
	}
	return a
}

func balance(t *testing.T, st *store.Store, id uuid.UUID) int64 {
	t.Helper()
	a, err := st.GetAccount(context.Background(), id)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	return a.BalanceCents
}

func entryCount(t *testing.T, st *store.Store, accountID uuid.UUID) int {
	t.Helper()
	entries, err := st.ListEntries(context.Background(), accountID, 100000)
	if err != nil {
		t.Fatalf("list entries: %v", err)
	}
	return len(entries)
}

// A payment writes exactly two entries — a debit and a credit that
// cancel out — and moves exactly that much between the balances.
func TestPaymentWritesBalancedPairAndMovesBalances(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()

	alice := mustAccount(t, st, "Alice", "SGD")
	bob := mustAccount(t, st, "Bob", "SGD")

	if _, _, err := st.Deposit(ctx, alice.ID, 10_000, key("dep-1")); err != nil {
		t.Fatalf("deposit: %v", err)
	}
	p, created, err := st.PostPayment(ctx, alice.ID, bob.ID, 2_500, store.KindPayment, key("pay-1"))
	if err != nil {
		t.Fatalf("payment: %v", err)
	}
	if !created {
		t.Fatal("first posting must report created=true")
	}

	entries, err := st.ListPaymentEntries(ctx, p.ID)
	if err != nil {
		t.Fatalf("list payment entries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("payment must write exactly 2 entries, got %d", len(entries))
	}
	var sum int64
	byAccount := map[uuid.UUID]int64{}
	for _, e := range entries {
		sum += e.AmountCents
		byAccount[e.AccountID] = e.AmountCents
	}
	if sum != 0 {
		t.Fatalf("entries must sum to zero, got %d", sum)
	}
	if byAccount[alice.ID] != -2_500 || byAccount[bob.ID] != 2_500 {
		t.Fatalf("debit/credit on wrong accounts: %v", byAccount)
	}
	if got := balance(t, st, alice.ID); got != 7_500 {
		t.Fatalf("alice balance = %d, want 7500", got)
	}
	if got := balance(t, st, bob.ID); got != 2_500 {
		t.Fatalf("bob balance = %d, want 2500", got)
	}
}

// Deposits are balanced too: EXTERNAL goes exactly as negative as
// users go positive, per currency, so the whole ledger sums to zero
// no matter what sequence of deposits and payments ran.
func TestLedgerSumsToZeroPerCurrency(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()

	sgA := mustAccount(t, st, "SG A", "SGD")
	sgB := mustAccount(t, st, "SG B", "SGD")
	usA := mustAccount(t, st, "US A", "USD")

	for i, d := range []struct {
		to     uuid.UUID
		amount int64
	}{
		{sgA.ID, 50_000}, {sgB.ID, 5_000}, {usA.ID, 77_700},
	} {
		if _, _, err := st.Deposit(ctx, d.to, d.amount, key(uuid.NewString())); err != nil {
			t.Fatalf("deposit %d: %v", i, err)
		}
	}
	if _, _, err := st.PostPayment(ctx, sgA.ID, sgB.ID, 12_345, store.KindPayment, key("x1")); err != nil {
		t.Fatalf("payment: %v", err)
	}

	ext, err := st.SystemAccount(ctx, "SGD")
	if err != nil {
		t.Fatalf("system account: %v", err)
	}
	if got := balance(t, st, ext.ID); got != -55_000 {
		t.Fatalf("EXTERNAL SGD balance = %d, want -55000 (mirror of user funds)", got)
	}

	report, err := st.Verify(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !report.OK {
		t.Fatalf("verify must pass: %+v", report)
	}
	if len(report.Currencies) != 2 {
		t.Fatalf("want currency checks for SGD and USD, got %+v", report.Currencies)
	}
	for _, c := range report.Currencies {
		if c.EntriesSumCents != 0 || !c.OK {
			t.Fatalf("currency %s entries sum = %d, want 0", c.Currency, c.EntriesSumCents)
		}
	}
}

// An overdraw attempt is rejected and leaves no trace: no payment,
// no entries, balances untouched — so a later retry after funding
// can still post under the same idempotency key.
func TestInsufficientFundsWritesNothing(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()

	alice := mustAccount(t, st, "Alice", "SGD")
	bob := mustAccount(t, st, "Bob", "SGD")
	if _, _, err := st.Deposit(ctx, alice.ID, 100, key("dep")); err != nil {
		t.Fatalf("deposit: %v", err)
	}
	aliceEntries := entryCount(t, st, alice.ID)
	bobEntries := entryCount(t, st, bob.ID)

	_, _, err := st.PostPayment(ctx, alice.ID, bob.ID, 101, store.KindPayment, key("too-big"))
	if !errors.Is(err, store.ErrInsufficientFunds) {
		t.Fatalf("want ErrInsufficientFunds, got %v", err)
	}
	if got := balance(t, st, alice.ID); got != 100 {
		t.Fatalf("alice balance changed: %d", got)
	}
	if got := balance(t, st, bob.ID); got != 0 {
		t.Fatalf("bob balance changed: %d", got)
	}
	if entryCount(t, st, alice.ID) != aliceEntries || entryCount(t, st, bob.ID) != bobEntries {
		t.Fatal("failed payment must not write entries")
	}
	if _, err := st.GetPaymentByKey(ctx, "too-big"); !errors.Is(err, store.ErrPaymentNotFound) {
		t.Fatalf("failed payment must not be recorded, got %v", err)
	}

	// Fund the account, then the same key posts successfully.
	if _, _, err := st.Deposit(ctx, alice.ID, 100, key("dep-2")); err != nil {
		t.Fatalf("deposit 2: %v", err)
	}
	if _, created, err := st.PostPayment(ctx, alice.ID, bob.ID, 101, store.KindPayment, key("too-big")); err != nil || !created {
		t.Fatalf("retry after funding: created=%v err=%v", created, err)
	}
}

// The same Idempotency-Key posted twice returns the original payment
// and moves money exactly once — for payments and deposits alike.
func TestIdempotentReplay(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()

	alice := mustAccount(t, st, "Alice", "SGD")
	bob := mustAccount(t, st, "Bob", "SGD")

	d1, c1, err := st.Deposit(ctx, alice.ID, 1_000, key("dep-key"))
	if err != nil || !c1 {
		t.Fatalf("first deposit: created=%v err=%v", c1, err)
	}
	d2, c2, err := st.Deposit(ctx, alice.ID, 1_000, key("dep-key"))
	if err != nil {
		t.Fatalf("replay deposit: %v", err)
	}
	if c2 || d2.ID != d1.ID {
		t.Fatalf("replay must return the original deposit: c2=%v same=%v", c2, d2.ID == d1.ID)
	}
	if got := balance(t, st, alice.ID); got != 1_000 {
		t.Fatalf("deposit replay moved money twice: balance %d", got)
	}

	p1, _, err := st.PostPayment(ctx, alice.ID, bob.ID, 400, store.KindPayment, key("pay-key"))
	if err != nil {
		t.Fatalf("payment: %v", err)
	}
	p2, created, err := st.PostPayment(ctx, alice.ID, bob.ID, 400, store.KindPayment, key("pay-key"))
	if err != nil {
		t.Fatalf("replay payment: %v", err)
	}
	if created || p2.ID != p1.ID {
		t.Fatalf("replay must return the original payment: created=%v same=%v", created, p2.ID == p1.ID)
	}
	if got := balance(t, st, alice.ID); got != 600 {
		t.Fatalf("alice balance = %d, want 600 (one posting only)", got)
	}
	if got := balance(t, st, bob.ID); got != 400 {
		t.Fatalf("bob balance = %d, want 400", got)
	}
	if n := entryCount(t, st, alice.ID); n != 2 { // 1 deposit credit + 1 payment debit
		t.Fatalf("alice must have exactly 2 entries, got %d", n)
	}
}

// Ten concurrent payments of 200 against a 1000 balance: exactly
// five succeed, the account lands on exactly zero, and the books
// still verify. Row locks — not luck — decide who gets paid.
func TestConcurrentPaymentsCannotOverdraw(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()

	alice := mustAccount(t, st, "Alice", "SGD")
	bob := mustAccount(t, st, "Bob", "SGD")
	if _, _, err := st.Deposit(ctx, alice.ID, 1_000, key("seed")); err != nil {
		t.Fatalf("deposit: %v", err)
	}

	const workers = 10
	var wg sync.WaitGroup
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, errs[i] = st.PostPayment(ctx, alice.ID, bob.ID, 200,
				store.KindPayment, key(uuid.NewString()))
		}(i)
	}
	wg.Wait()

	var succeeded, rejected int
	for _, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, store.ErrInsufficientFunds):
			rejected++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if succeeded != 5 || rejected != 5 {
		t.Fatalf("want 5 succeeded / 5 rejected, got %d / %d", succeeded, rejected)
	}
	if got := balance(t, st, alice.ID); got != 0 {
		t.Fatalf("alice balance = %d, want exactly 0 (no overdraft)", got)
	}
	if got := balance(t, st, bob.ID); got != 1_000 {
		t.Fatalf("bob balance = %d, want 1000", got)
	}
	report, err := st.Verify(ctx)
	if err != nil || !report.OK {
		t.Fatalf("verify after concurrency: ok=%v err=%v", report != nil && report.OK, err)
	}
}

// After a long randomised sequence of valid payments, Verify still
// passes and no money appeared or vanished: user balances total
// exactly what was deposited.
func TestVerifyAfterRandomisedPayments(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	rng := rand.New(rand.NewSource(42))

	const nAccounts = 5
	accounts := make([]*store.Account, nAccounts)
	var totalDeposited int64
	for i := range accounts {
		accounts[i] = mustAccount(t, st, uuid.NewString()[:8], "SGD")
		amount := int64(50_000 + rng.Intn(50_000))
		if _, _, err := st.Deposit(ctx, accounts[i].ID, amount, key(uuid.NewString())); err != nil {
			t.Fatalf("deposit: %v", err)
		}
		totalDeposited += amount
	}

	for i := 0; i < 200; i++ {
		from := accounts[rng.Intn(nAccounts)]
		to := accounts[rng.Intn(nAccounts)]
		if from.ID == to.ID {
			continue
		}
		amount := int64(1 + rng.Intn(500))
		_, _, err := st.PostPayment(ctx, from.ID, to.ID, amount, store.KindPayment, key(uuid.NewString()))
		if errors.Is(err, store.ErrInsufficientFunds) {
			continue
		}
		if err != nil {
			t.Fatalf("random payment %d: %v", i, err)
		}
	}

	report, err := st.Verify(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !report.OK {
		t.Fatalf("verify failed after randomised payments: %+v", report)
	}
	var userTotal int64
	for _, a := range accounts {
		userTotal += balance(t, st, a.ID)
	}
	if userTotal != totalDeposited {
		t.Fatalf("money created or destroyed: users hold %d, deposited %d", userTotal, totalDeposited)
	}
	ext, err := st.SystemAccount(ctx, "SGD")
	if err != nil {
		t.Fatalf("system account: %v", err)
	}
	if got := balance(t, st, ext.ID); got != -totalDeposited {
		t.Fatalf("EXTERNAL balance = %d, want %d", got, -totalDeposited)
	}
}

// Every validation rule rejects before anything is written.
func TestValidationRejections(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()

	sgd := mustAccount(t, st, "SGD User", "SGD")
	usd := mustAccount(t, st, "USD User", "USD")
	if _, _, err := st.Deposit(ctx, sgd.ID, 1_000, key("dep")); err != nil {
		t.Fatalf("deposit: %v", err)
	}

	cases := []struct {
		name    string
		from    uuid.UUID
		to      uuid.UUID
		amount  int64
		wantErr error
	}{
		{"zero amount", sgd.ID, usd.ID, 0, store.ErrInvalidAmount},
		{"negative amount", sgd.ID, usd.ID, -50, store.ErrInvalidAmount},
		{"self payment", sgd.ID, sgd.ID, 10, store.ErrSelfPayment},
		{"currency mismatch", sgd.ID, usd.ID, 10, store.ErrCurrencyMismatch},
		{"missing sender", uuid.New(), usd.ID, 10, store.ErrAccountNotFound},
		{"missing receiver", sgd.ID, uuid.New(), 10, store.ErrAccountNotFound},
	}
	for _, tc := range cases {
		_, _, err := st.PostPayment(ctx, tc.from, tc.to, tc.amount, store.KindPayment, key(uuid.NewString()))
		if !errors.Is(err, tc.wantErr) {
			t.Errorf("%s: want %v, got %v", tc.name, tc.wantErr, err)
		}
	}

	if _, _, err := st.Deposit(ctx, sgd.ID, 0, key("bad-dep")); !errors.Is(err, store.ErrInvalidAmount) {
		t.Errorf("zero deposit: want ErrInvalidAmount, got %v", err)
	}
	if _, _, err := st.Deposit(ctx, uuid.New(), 100, key("ghost-dep")); !errors.Is(err, store.ErrAccountNotFound) {
		t.Errorf("deposit to missing account: want ErrAccountNotFound, got %v", err)
	}
	if _, err := st.CreateAccount(ctx, "Bad", "SG"); !errors.Is(err, store.ErrInvalidCurrency) {
		t.Errorf("short currency: want ErrInvalidCurrency, got %v", err)
	}
	if _, err := st.CreateAccount(ctx, "  ", "SGD"); !errors.Is(err, store.ErrInvalidName) {
		t.Errorf("blank name: want ErrInvalidName, got %v", err)
	}

	// Nothing above may have moved money.
	if got := balance(t, st, sgd.ID); got != 1_000 {
		t.Fatalf("sgd balance = %d after rejected operations, want 1000", got)
	}
	if got := balance(t, st, usd.ID); got != 0 {
		t.Fatalf("usd balance = %d after rejected operations, want 0", got)
	}
	report, err := st.Verify(ctx)
	if err != nil || !report.OK {
		t.Fatalf("verify after rejections: err=%v report=%+v", err, report)
	}
}
