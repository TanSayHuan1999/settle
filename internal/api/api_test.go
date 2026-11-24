package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"settle/internal/api"
	"settle/internal/store"
	"settle/internal/testdb"
)

type client struct {
	t   *testing.T
	srv *httptest.Server
}

func newClient(t *testing.T) *client {
	t.Helper()
	st := testdb.Open(t)
	srv := httptest.NewServer(api.New(st).Routes())
	t.Cleanup(srv.Close)
	return &client{t: t, srv: srv}
}

func (c *client) do(method, path, body string, headers map[string]string) (int, map[string]any) {
	c.t.Helper()
	var rdr *bytes.Reader
	if body == "" {
		rdr = bytes.NewReader(nil)
	} else {
		rdr = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequest(method, c.srv.URL+path, rdr)
	if err != nil {
		c.t.Fatalf("new request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		c.t.Fatalf("%s %s: decode response: %v", method, path, err)
	}
	return resp.StatusCode, out
}

func (c *client) createAccount(name, currency string) map[string]any {
	c.t.Helper()
	status, body := c.do("POST", "/accounts",
		fmt.Sprintf(`{"name":%q,"currency":%q}`, name, currency), nil)
	if status != http.StatusCreated {
		c.t.Fatalf("create account: status %d, body %v", status, body)
	}
	return body
}

func TestHealthz(t *testing.T) {
	c := newClient(t)
	status, body := c.do("GET", "/healthz", "", nil)
	if status != http.StatusOK || body["ok"] != true {
		t.Fatalf("healthz: status %d body %v", status, body)
	}
}

func TestAccountDepositPaymentFlow(t *testing.T) {
	c := newClient(t)
	alice := c.createAccount("Alice", "SGD")
	bob := c.createAccount("Bob", "sgd") // currency is normalised
	if bob["currency"] != "SGD" {
		t.Fatalf("currency not normalised: %v", bob["currency"])
	}
	aliceID := alice["id"].(string)
	bobID := bob["id"].(string)

	// Deposit with an idempotency key; replay returns 200 + same id.
	status, dep := c.do("POST", "/deposits",
		fmt.Sprintf(`{"account_id":%q,"amount_cents":10000}`, aliceID),
		map[string]string{"Idempotency-Key": "dep-1"})
	if status != http.StatusCreated {
		t.Fatalf("deposit: status %d body %v", status, dep)
	}
	status, dep2 := c.do("POST", "/deposits",
		fmt.Sprintf(`{"account_id":%q,"amount_cents":10000}`, aliceID),
		map[string]string{"Idempotency-Key": "dep-1"})
	if status != http.StatusOK || dep2["id"] != dep["id"] {
		t.Fatalf("deposit replay: status %d body %v", status, dep2)
	}

	// Pay Alice -> Bob 2500.
	status, pay := c.do("POST", "/payments",
		fmt.Sprintf(`{"from_id":%q,"to_id":%q,"amount_cents":2500}`, aliceID, bobID),
		map[string]string{"Idempotency-Key": "pay-1"})
	if status != http.StatusCreated || pay["kind"] != store.KindPayment {
		t.Fatalf("payment: status %d body %v", status, pay)
	}

	// Balances + statement via GET /accounts/{id}.
	_, got := c.do("GET", "/accounts/"+aliceID, "", nil)
	if got["balance_cents"] != float64(7500) {
		t.Fatalf("alice balance = %v, want 7500", got["balance_cents"])
	}
	entries, ok := got["entries"].([]any)
	if !ok || len(entries) != 2 {
		t.Fatalf("alice must have 2 entries (deposit + payment), got %v", got["entries"])
	}
	_, got = c.do("GET", "/accounts/"+bobID, "", nil)
	if got["balance_cents"] != float64(2500) {
		t.Fatalf("bob balance = %v, want 2500", got["balance_cents"])
	}

	// The payment is fetchable by id.
	status, fetched := c.do("GET", "/payments/"+pay["id"].(string), "", nil)
	if status != http.StatusOK || fetched["amount_cents"] != float64(2500) {
		t.Fatalf("get payment: status %d body %v", status, fetched)
	}

	// /verify proves the books balance.
	status, report := c.do("GET", "/verify", "", nil)
	if status != http.StatusOK || report["ok"] != true {
		t.Fatalf("verify: status %d body %v", status, report)
	}
}

func TestErrorMapping(t *testing.T) {
	c := newClient(t)
	alice := c.createAccount("Alice", "SGD")
	bob := c.createAccount("Bob", "SGD")
	aliceID := alice["id"].(string)
	bobID := bob["id"].(string)

	if status, _ := c.do("POST", "/deposits",
		fmt.Sprintf(`{"account_id":%q,"amount_cents":100}`, aliceID), nil); status != http.StatusCreated {
		t.Fatalf("seed deposit: status %d", status)
	}

	cases := []struct {
		name   string
		path   string
		body   string
		status int
	}{
		{"insufficient funds", "/payments",
			fmt.Sprintf(`{"from_id":%q,"to_id":%q,"amount_cents":101}`, aliceID, bobID), http.StatusConflict},
		{"zero amount", "/payments",
			fmt.Sprintf(`{"from_id":%q,"to_id":%q,"amount_cents":0}`, aliceID, bobID), http.StatusBadRequest},
		{"fractional cents", "/payments",
			fmt.Sprintf(`{"from_id":%q,"to_id":%q,"amount_cents":10.5}`, aliceID, bobID), http.StatusBadRequest},
		{"self payment", "/payments",
			fmt.Sprintf(`{"from_id":%q,"to_id":%q,"amount_cents":10}`, aliceID, aliceID), http.StatusBadRequest},
		{"missing account", "/payments",
			fmt.Sprintf(`{"from_id":%q,"to_id":%q,"amount_cents":10}`, aliceID, "00000000-0000-0000-0000-000000000000"), http.StatusNotFound},
		{"bad uuid", "/payments",
			`{"from_id":"nope","to_id":"alsono","amount_cents":10}`, http.StatusBadRequest},
		{"bad json", "/payments", `{`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		status, body := c.do("POST", tc.path, tc.body, nil)
		if status != tc.status {
			t.Errorf("%s: status %d, want %d (body %v)", tc.name, status, tc.status, body)
		}
		if _, ok := body["error"]; !ok {
			t.Errorf("%s: error body must carry an error field, got %v", tc.name, body)
		}
	}

	if status, _ := c.do("GET", "/accounts/00000000-0000-0000-0000-000000000000", "", nil); status != http.StatusNotFound {
		t.Errorf("get missing account: status %d, want 404", status)
	}
	if status, _ := c.do("POST", "/accounts", `{"name":"X","currency":"SG"}`, nil); status != http.StatusBadRequest {
		t.Errorf("bad currency: status %d, want 400", status)
	}
}
