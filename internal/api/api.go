// Package api exposes the HTTP surface: account creation, deposits
// and payments (idempotent, integer cents), ledger reads, and the
// /verify reconciliation report.
package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"settle/internal/store"
)

// API holds the handlers' dependencies.
type API struct {
	Store *store.Store
}

// New returns an API bound to the store.
func New(st *store.Store) *API { return &API{Store: st} }

// Routes returns the mux with all endpoints registered.
func (a *API) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.healthz)
	mux.HandleFunc("POST /accounts", a.createAccount)
	mux.HandleFunc("GET /accounts", a.listAccounts)
	mux.HandleFunc("GET /accounts/{id}", a.getAccount)
	mux.HandleFunc("POST /deposits", a.createDeposit)
	mux.HandleFunc("POST /payments", a.createPayment)
	mux.HandleFunc("GET /payments/{id}", a.getPayment)
	mux.HandleFunc("GET /verify", a.verify)
	return mux
}

func (a *API) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type createAccountRequest struct {
	Name     string `json:"name"`
	Currency string `json:"currency"`
}

func (a *API) createAccount(w http.ResponseWriter, r *http.Request) {
	var req createAccountRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "body must be a JSON object")
		return
	}
	acct, err := a.Store.CreateAccount(r.Context(), req.Name, req.Currency)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, acct)
}

func (a *API) listAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := a.Store.ListAccounts(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list accounts")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": accounts})
}

type accountResponse struct {
	*store.Account
	Entries []*store.Entry `json:"entries"`
}

func (a *API) getAccount(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "id must be a UUID")
		return
	}
	acct, err := a.Store.GetAccount(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	entries, err := a.Store.ListEntries(r.Context(), id, 50)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load entries")
		return
	}
	writeJSON(w, http.StatusOK, accountResponse{Account: acct, Entries: entries})
}

type depositRequest struct {
	AccountID   string `json:"account_id"`
	AmountCents int64  `json:"amount_cents"`
}

func (a *API) createDeposit(w http.ResponseWriter, r *http.Request) {
	var req depositRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "body must be a JSON object with an integer amount_cents")
		return
	}
	accountID, err := uuid.Parse(req.AccountID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "account_id must be a UUID")
		return
	}
	key := idempotencyKey(r)
	p, created, err := a.Store.Deposit(r.Context(), accountID, req.AmountCents, &key)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writePayment(w, p, created)
}

type paymentRequest struct {
	FromID      string `json:"from_id"`
	ToID        string `json:"to_id"`
	AmountCents int64  `json:"amount_cents"`
}

func (a *API) createPayment(w http.ResponseWriter, r *http.Request) {
	var req paymentRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "body must be a JSON object with an integer amount_cents")
		return
	}
	fromID, err := uuid.Parse(req.FromID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "from_id must be a UUID")
		return
	}
	toID, err := uuid.Parse(req.ToID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "to_id must be a UUID")
		return
	}
	key := idempotencyKey(r)
	p, created, err := a.Store.PostPayment(r.Context(), fromID, toID, req.AmountCents, store.KindPayment, &key)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writePayment(w, p, created)
}

// writePayment returns 201 for a fresh posting and 200 for an
// idempotent replay of the original payment.
func writePayment(w http.ResponseWriter, p *store.Payment, created bool) {
	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	writeJSON(w, status, p)
}

func (a *API) getPayment(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "id must be a UUID")
		return
	}
	p, err := a.Store.GetPayment(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (a *API) verify(w http.ResponseWriter, r *http.Request) {
	report, err := a.Store.Verify(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not run verification")
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// idempotencyKey returns the client's Idempotency-Key header, or a
// generated key when the client did not send one (the generated key
// is echoed back on the payment, so the call can still be replayed
// safely by key later).
func idempotencyKey(r *http.Request) string {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key != "" {
		return key
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return uuid.NewString()
	}
	return "gen_" + hex.EncodeToString(b)
}

func decodeBody(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	// Decoding into int64 fields rejects fractional amounts (10.5)
	// outright — money here is integer cents or it is an error.
	return json.Unmarshal(body, v)
}

// writeStoreError maps ledger errors to HTTP status codes:
// validation 400, missing rows 404, insufficient funds 409.
func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrInvalidAmount),
		errors.Is(err, store.ErrSelfPayment),
		errors.Is(err, store.ErrCurrencyMismatch),
		errors.Is(err, store.ErrInvalidCurrency),
		errors.Is(err, store.ErrInvalidName),
		errors.Is(err, store.ErrSystemDeposit):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, store.ErrAccountNotFound),
		errors.Is(err, store.ErrPaymentNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, store.ErrInsufficientFunds):
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
