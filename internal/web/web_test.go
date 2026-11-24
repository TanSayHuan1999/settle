package web_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"settle/internal/store"
	"settle/internal/testdb"
	"settle/internal/web"
)

func newHandler(t *testing.T, basePath string) (*store.Store, http.Handler) {
	t.Helper()
	st := testdb.Open(t)
	return st, web.New(st, basePath).Routes()
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func postForm(t *testing.T, h http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func balanceOf(t *testing.T, st *store.Store, name string) int64 {
	t.Helper()
	accounts, err := st.ListAccounts(context.Background())
	if err != nil {
		t.Fatalf("list accounts: %v", err)
	}
	for _, a := range accounts {
		if a.Name == name {
			return a.BalanceCents
		}
	}
	t.Fatalf("account %q not found", name)
	return 0
}

func TestFormatMoney(t *testing.T) {
	cases := []struct {
		currency string
		cents    int64
		want     string
	}{
		{"SGD", 123_456, "SGD 1,234.56"},
		{"SGD", -25_000, "−SGD 250.00"},
		{"USD", 5, "USD 0.05"},
		{"SGD", 0, "SGD 0.00"},
		{"SGD", 100_000_000, "SGD 1,000,000.00"},
		{"USD", -99, "−USD 0.99"},
	}
	for _, tc := range cases {
		if got := web.FormatMoney(tc.currency, tc.cents); got != tc.want {
			t.Errorf("FormatMoney(%s, %d) = %q, want %q", tc.currency, tc.cents, got, tc.want)
		}
	}
	if got := web.FormatSigned("SGD", 2_500); got != "+SGD 25.00" {
		t.Errorf("FormatSigned positive = %q, want +SGD 25.00", got)
	}
	if got := web.FormatSigned("SGD", -2_500); got != "−SGD 25.00" {
		t.Errorf("FormatSigned negative = %q, want −SGD 25.00", got)
	}
}

func TestParseAmountToCents(t *testing.T) {
	valid := map[string]int64{
		"10":    1_000,
		"10.5":  1_050,
		"10.55": 1_055,
		"0.01":  1,
		" 7.5 ": 750,
	}
	for in, want := range valid {
		got, err := web.ParseAmountToCents(in)
		if err != nil || got != want {
			t.Errorf("ParseAmountToCents(%q) = %d, %v; want %d, nil", in, got, err, want)
		}
	}
	invalid := []string{"10.555", "abc", "0", "0.00", "", "-5", "1,000", "10.", ".5", "1.2.3", " "}
	for _, in := range invalid {
		if got, err := web.ParseAmountToCents(in); err == nil {
			t.Errorf("ParseAmountToCents(%q) = %d, nil; want error", in, got)
		}
	}
}

func TestDashboardRendersSeededAccounts(t *testing.T) {
	st, h := newHandler(t, "")
	ctx := context.Background()
	seeded, err := st.SeedIfEmpty(ctx)
	if err != nil || !seeded {
		t.Fatalf("seed = %v, %v; want true, nil", seeded, err)
	}
	// Seeding again must be a no-op.
	if again, err := st.SeedIfEmpty(ctx); err != nil || again {
		t.Fatalf("second seed = %v, %v; want false, nil", again, err)
	}

	rec := get(t, h, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Aisha Rahman", "Wei Jie Tan", "Priya Nair", "Marcus Lee", "Lakeside Mart",
		"SGD 2,314.25", // Aisha's balance after the seed payments
		"Rent split — November",
		"EXTERNAL",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	// 5 deposits + 10 payments, and the ledger still verifies.
	if n, err := st.PaymentCount(ctx); err != nil || n != 15 {
		t.Errorf("payment count = %d, %v; want 15, nil", n, err)
	}
	report, err := st.Verify(ctx)
	if err != nil || !report.OK {
		t.Errorf("verify after seed = %+v, %v; want OK", report, err)
	}
}

func TestBasePathPrefixesLinks(t *testing.T) {
	st, h := newHandler(t, "/settle")
	if _, err := st.SeedIfEmpty(context.Background()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	body := get(t, h, "/").Body.String()
	for _, want := range []string{`href="/settle/pay"`, `href="/settle/integrity"`, `href="/settle/static/style.css"`, `action="/settle/accounts"`} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard with BASE_PATH missing %q", want)
		}
	}
}

func TestAccountPageStatementAndRunningBalance(t *testing.T) {
	st, h := newHandler(t, "")
	ctx := context.Background()
	if _, err := st.SeedIfEmpty(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	accounts, err := st.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var aisha *store.Account
	for _, a := range accounts {
		if a.Name == "Aisha Rahman" {
			aisha = a
		}
	}
	rec := get(t, h, "/accounts/"+aisha.ID.String())
	if rec.Code != http.StatusOK {
		t.Fatalf("GET account = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// Newest row is the Lakeside refund (+50.00) landing on her final
	// balance; the deposit row shows the balance right after funding.
	for _, want := range []string{"SGD 2,314.25", "Refund — returned items", "+SGD 50.00", "Deposit", "SGD 2,500.00"} {
		if !strings.Contains(body, want) {
			t.Errorf("statement missing %q", want)
		}
	}
}

func TestPayFormHappyPath(t *testing.T) {
	st, h := newHandler(t, "")
	ctx := context.Background()
	alice, err := st.CreateAccount(ctx, "Alice", "SGD")
	if err != nil {
		t.Fatalf("create alice: %v", err)
	}
	bob, err := st.CreateAccount(ctx, "Bob", "SGD")
	if err != nil {
		t.Fatalf("create bob: %v", err)
	}
	key := "web-test-dep"
	if _, _, err := st.Deposit(ctx, alice.ID, 10_000, &key); err != nil {
		t.Fatalf("deposit: %v", err)
	}

	form := url.Values{
		"from_id":  {alice.ID.String()},
		"to_id":    {bob.ID.String()},
		"amount":   {"25.00"},
		"note":     {"Invoice 42"},
		"idem_key": {"web-test-pay-1"},
	}
	rec := postForm(t, h, "/pay", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /pay = %d, want 303; body: %s", rec.Code, rec.Body)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/accounts/"+alice.ID.String()) {
		t.Fatalf("redirect = %q, want sender's account page", loc)
	}
	if got := balanceOf(t, st, "Alice"); got != 7_500 {
		t.Errorf("alice balance = %d, want 7500", got)
	}
	if got := balanceOf(t, st, "Bob"); got != 2_500 {
		t.Errorf("bob balance = %d, want 2500", got)
	}

	// Resubmitting the identical form (same idempotency key) must
	// not move the money again.
	rec = postForm(t, h, "/pay", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("replay POST /pay = %d, want 303", rec.Code)
	}
	if got := balanceOf(t, st, "Alice"); got != 7_500 {
		t.Errorf("after replay alice balance = %d, want 7500 (no double-pay)", got)
	}

	// The payment, with its note, shows on the dashboard and the
	// receiver's statement carries the updated balance.
	dash := get(t, h, "/").Body.String()
	if !strings.Contains(dash, "Invoice 42") {
		t.Errorf("dashboard recent payments missing the note")
	}
	stmt := get(t, h, "/accounts/"+bob.ID.String()).Body.String()
	if !strings.Contains(stmt, "SGD 25.00") || !strings.Contains(stmt, "From Alice") {
		t.Errorf("bob statement missing the payment / updated balance")
	}
}

func TestPayFormInsufficientFunds(t *testing.T) {
	st, h := newHandler(t, "")
	ctx := context.Background()
	alice, err := st.CreateAccount(ctx, "Alice", "SGD")
	if err != nil {
		t.Fatalf("create alice: %v", err)
	}
	bob, err := st.CreateAccount(ctx, "Bob", "SGD")
	if err != nil {
		t.Fatalf("create bob: %v", err)
	}
	key := "web-test-dep-2"
	if _, _, err := st.Deposit(ctx, alice.ID, 1_000, &key); err != nil {
		t.Fatalf("deposit: %v", err)
	}

	rec := postForm(t, h, "/pay", url.Values{
		"from_id":  {alice.ID.String()},
		"to_id":    {bob.ID.String()},
		"amount":   {"50.00"},
		"idem_key": {"web-test-pay-2"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /pay over balance = %d, want 400; body: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "Insufficient funds") {
		t.Errorf("error page does not show the plain error; body: %s", rec.Body)
	}
	if got := balanceOf(t, st, "Alice"); got != 1_000 {
		t.Errorf("alice balance = %d, want 1000 (nothing moved)", got)
	}
	if got := balanceOf(t, st, "Bob"); got != 0 {
		t.Errorf("bob balance = %d, want 0 (nothing moved)", got)
	}
	if n, err := st.PaymentCount(ctx); err != nil || n != 1 {
		t.Errorf("payment count = %d, %v; want 1 (only the deposit)", n, err)
	}
}

func TestPayFormSelfPaymentAndBadAmount(t *testing.T) {
	st, h := newHandler(t, "")
	ctx := context.Background()
	alice, err := st.CreateAccount(ctx, "Alice", "SGD")
	if err != nil {
		t.Fatalf("create alice: %v", err)
	}
	key := "web-test-dep-3"
	if _, _, err := st.Deposit(ctx, alice.ID, 1_000, &key); err != nil {
		t.Fatalf("deposit: %v", err)
	}
	rec := postForm(t, h, "/pay", url.Values{
		"from_id":  {alice.ID.String()},
		"to_id":    {alice.ID.String()},
		"amount":   {"5.00"},
		"idem_key": {"web-test-pay-3"},
	})
	if rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "different accounts") {
		t.Errorf("self-payment = %d, body missing error; body: %s", rec.Code, rec.Body)
	}
	rec = postForm(t, h, "/pay", url.Values{
		"from_id":  {alice.ID.String()},
		"to_id":    {alice.ID.String()},
		"amount":   {"10.555"},
		"idem_key": {"web-test-pay-4"},
	})
	if rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "decimal places") {
		t.Errorf("bad amount = %d, body missing error; body: %s", rec.Code, rec.Body)
	}
}

func TestDepositFormAndCreateAccountForm(t *testing.T) {
	st, h := newHandler(t, "")

	rec := postForm(t, h, "/accounts", url.Values{
		"name": {"Carol"}, "currency": {"USD"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /accounts = %d, want 303; body: %s", rec.Code, rec.Body)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/accounts/") {
		t.Fatalf("create redirect = %q, want the new account page", loc)
	}
	accountPath := strings.SplitN(loc, "?", 2)[0]

	rec = postForm(t, h, accountPath+"/deposit", url.Values{
		"amount": {"20.50"}, "idem_key": {"web-test-dep-form"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST deposit = %d, want 303; body: %s", rec.Code, rec.Body)
	}
	if got := balanceOf(t, st, "Carol"); got != 2_050 {
		t.Errorf("carol balance = %d, want 2050", got)
	}
	page := get(t, h, accountPath).Body.String()
	if !strings.Contains(page, "USD 20.50") {
		t.Errorf("account page missing formatted balance USD 20.50")
	}

	// A bad deposit amount re-renders with the error and moves nothing.
	rec = postForm(t, h, accountPath+"/deposit", url.Values{
		"amount": {"abc"}, "idem_key": {"web-test-dep-bad"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad deposit = %d, want 400", rec.Code)
	}
	if got := balanceOf(t, st, "Carol"); got != 2_050 {
		t.Errorf("carol balance after bad deposit = %d, want 2050", got)
	}
}

func TestIntegrityPage(t *testing.T) {
	st, h := newHandler(t, "")
	if _, err := st.SeedIfEmpty(context.Background()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rec := get(t, h, "/integrity")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /integrity = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Ledger balanced", "✓ match", "SGD 0.00", "Aisha Rahman"} {
		if !strings.Contains(body, want) {
			t.Errorf("integrity page missing %q", want)
		}
	}
}
