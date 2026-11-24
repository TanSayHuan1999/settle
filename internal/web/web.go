// Package web is Settle's server-rendered dashboard: account cards
// and statements, a payment form, and the ledger integrity page.
// Templates and CSS are embedded in the binary — no JS framework,
// no build pipeline, no external assets.
//
// Every form posts straight into the store engine (Deposit /
// PostPaymentWithNote), never back through the JSON API, so UI
// actions obey exactly the same invariants as API calls. Each money
// form carries a hidden idempotency key minted when the form was
// rendered: resubmitting the same form (double-click, refresh,
// back-button) replays the original payment instead of moving
// money twice.
package web

import (
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"settle/internal/store"
)

//go:embed templates static
var assets embed.FS

// Handler serves the UI. BasePath ("" or "/settle") is prefixed to
// every generated link; the routes themselves stay unprefixed — in
// production the reverse proxy strips the prefix before forwarding.
type Handler struct {
	store     *store.Store
	basePath  string
	templates map[string]*template.Template
}

// New builds a Handler for the store, generating links under
// basePath.
func New(st *store.Store, basePath string) *Handler {
	funcs := template.FuncMap{
		"money": FormatMoney,
		// signed returns template.HTML: html/template escapes a
		// leading "+" to &#43; in text nodes, which would hide the
		// credit sign FormatSigned deliberately adds. The string is
		// built only from a validated currency code and an int64, so
		// marking it safe is sound.
		"signed": func(currency string, cents int64) template.HTML {
			return template.HTML(FormatSigned(currency, cents))
		},
		"day":   func(t time.Time) string { return t.Format("02 Jan 2006") },
		"clock": func(t time.Time) string { return t.Format("02 Jan · 15:04") },
	}
	pages := []string{"dashboard", "account", "pay", "integrity"}
	templates := make(map[string]*template.Template, len(pages))
	for _, page := range pages {
		templates[page] = template.Must(template.New("base").Funcs(funcs).
			ParseFS(assets, "templates/base.html", "templates/"+page+".html"))
	}
	return &Handler{store: st, basePath: basePath, templates: templates}
}

// Routes returns the UI mux. Paths are unprefixed; see BasePath.
func (h *Handler) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.FileServerFS(assets))
	mux.HandleFunc("GET /", h.dashboard)
	mux.HandleFunc("GET /accounts/{id}", h.accountPage)
	mux.HandleFunc("POST /accounts", h.createAccount)
	mux.HandleFunc("POST /accounts/{id}/deposit", h.deposit)
	mux.HandleFunc("GET /pay", h.payForm)
	mux.HandleFunc("POST /pay", h.paySubmit)
	mux.HandleFunc("GET /integrity", h.integrity)
	return mux
}

// chrome is the page furniture every template receives: the base
// path for links, plus the notice/error banners.
type chrome struct {
	BasePath string
	Notice   string
	Error    string
}

func (h *Handler) chrome(r *http.Request) chrome {
	return chrome{BasePath: h.basePath, Notice: r.URL.Query().Get("notice")}
}

func (h *Handler) render(w http.ResponseWriter, status int, page string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := h.templates[page].ExecuteTemplate(w, "base", data); err != nil {
		log.Printf("web: render %s: %v", page, err)
	}
}

func (h *Handler) fail(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintln(w, msg)
}

// --- dashboard -----------------------------------------------------

type currencyTotal struct {
	Currency string
	Cents    int64
}

type dashboardData struct {
	chrome
	Users        []*store.Account
	System       []*store.Account
	Totals       []currencyTotal
	PaymentCount int
	Recent       []*store.PaymentRow
	// Form state for the open-an-account card (echoed on error).
	FormName     string
	FormCurrency string
}

func (h *Handler) dashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		h.fail(w, http.StatusNotFound, "not found")
		return
	}
	data, err := h.loadDashboard(r)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "could not load dashboard")
		return
	}
	h.render(w, http.StatusOK, "dashboard", data)
}

func (h *Handler) loadDashboard(r *http.Request) (*dashboardData, error) {
	ctx := r.Context()
	accounts, err := h.store.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	data := &dashboardData{chrome: h.chrome(r), FormCurrency: "SGD"}
	totals := map[string]int64{}
	var order []string
	for _, a := range accounts {
		if a.IsSystem {
			data.System = append(data.System, a)
			continue
		}
		data.Users = append(data.Users, a)
		if _, seen := totals[a.Currency]; !seen {
			order = append(order, a.Currency)
		}
		totals[a.Currency] += a.BalanceCents
	}
	for _, c := range order {
		data.Totals = append(data.Totals, currencyTotal{Currency: c, Cents: totals[c]})
	}
	if data.PaymentCount, err = h.store.PaymentCount(ctx); err != nil {
		return nil, err
	}
	if data.Recent, err = h.store.ListPayments(ctx, 10); err != nil {
		return nil, err
	}
	return data, nil
}

func (h *Handler) createAccount(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.fail(w, http.StatusBadRequest, "bad form")
		return
	}
	name := r.FormValue("name")
	currency := r.FormValue("currency")
	acct, err := h.store.CreateAccount(r.Context(), name, currency)
	if err != nil {
		data, lerr := h.loadDashboard(r)
		if lerr != nil {
			h.fail(w, http.StatusInternalServerError, "could not load dashboard")
			return
		}
		data.Error = formError(err)
		data.FormName = name
		data.FormCurrency = currency
		h.render(w, http.StatusBadRequest, "dashboard", data)
		return
	}
	http.Redirect(w, r, h.basePath+"/accounts/"+acct.ID.String()+
		"?notice="+url.QueryEscape("Account opened — deposit some funds to get started."), http.StatusSeeOther)
}

// --- account page --------------------------------------------------

type statementView struct {
	*store.StatementRow
	RunningBalance int64
	Label          string
}

type accountData struct {
	chrome
	Account   *store.Account
	Statement []statementView
	FormKey   string // idempotency key for the deposit form
	FormAmt   string // echoed back on a deposit error
}

func (h *Handler) accountPage(w http.ResponseWriter, r *http.Request) {
	acct, ok := h.loadAccount(w, r)
	if !ok {
		return
	}
	h.renderAccount(w, r, acct, http.StatusOK, "", "")
}

func (h *Handler) loadAccount(w http.ResponseWriter, r *http.Request) (*store.Account, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		h.fail(w, http.StatusNotFound, "account not found")
		return nil, false
	}
	acct, err := h.store.GetAccount(r.Context(), id)
	if errors.Is(err, store.ErrAccountNotFound) {
		h.fail(w, http.StatusNotFound, "account not found")
		return nil, false
	}
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "could not load account")
		return nil, false
	}
	return acct, true
}

// renderAccount builds the statement view: rows arrive newest-first,
// so the running balance starts at the account's current balance and
// each older row's balance is the current one minus the amounts of
// every newer entry.
func (h *Handler) renderAccount(w http.ResponseWriter, r *http.Request, acct *store.Account, status int, errMsg, formAmt string) {
	rows, err := h.store.ListStatement(r.Context(), acct.ID, 200)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "could not load statement")
		return
	}
	data := &accountData{
		chrome:  h.chrome(r),
		Account: acct,
		FormKey: "web-dep-" + uuid.NewString(),
		FormAmt: formAmt,
	}
	data.Error = errMsg
	running := acct.BalanceCents
	for _, row := range rows {
		label := "To " + row.CounterpartyName
		if row.Kind == store.KindDeposit {
			label = "Deposit"
		} else if row.Entry.AmountCents > 0 {
			label = "From " + row.CounterpartyName
		}
		data.Statement = append(data.Statement, statementView{
			StatementRow:   row,
			RunningBalance: running,
			Label:          label,
		})
		running -= row.Entry.AmountCents
	}
	h.render(w, status, "account", data)
}

func (h *Handler) deposit(w http.ResponseWriter, r *http.Request) {
	acct, ok := h.loadAccount(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.fail(w, http.StatusBadRequest, "bad form")
		return
	}
	amountStr := r.FormValue("amount")
	cents, err := ParseAmountToCents(amountStr)
	if err != nil {
		h.renderAccount(w, r, acct, http.StatusBadRequest, err.Error(), amountStr)
		return
	}
	key := strings.TrimSpace(r.FormValue("idem_key"))
	if key == "" {
		key = "web-dep-" + uuid.NewString()
	}
	if _, _, err := h.store.Deposit(r.Context(), acct.ID, cents, &key); err != nil {
		h.renderAccount(w, r, acct, http.StatusBadRequest, formError(err), amountStr)
		return
	}
	notice := "Deposited " + FormatMoney(acct.Currency, cents) + " into " + acct.Name + "."
	http.Redirect(w, r, h.basePath+"/accounts/"+acct.ID.String()+
		"?notice="+url.QueryEscape(notice), http.StatusSeeOther)
}

// --- payments ------------------------------------------------------

type payData struct {
	chrome
	Accounts []*store.Account // user accounts only
	FromID   string
	ToID     string
	Amount   string
	Note     string
	FormKey  string
}

func (h *Handler) payForm(w http.ResponseWriter, r *http.Request) {
	data, err := h.loadPay(r)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "could not load payment form")
		return
	}
	h.render(w, http.StatusOK, "pay", data)
}

func (h *Handler) loadPay(r *http.Request) (*payData, error) {
	accounts, err := h.store.ListAccounts(r.Context())
	if err != nil {
		return nil, err
	}
	data := &payData{chrome: h.chrome(r), FormKey: "web-pay-" + uuid.NewString()}
	for _, a := range accounts {
		if !a.IsSystem {
			data.Accounts = append(data.Accounts, a)
		}
	}
	// Friendly defaults: first account pays the second.
	if len(data.Accounts) >= 2 {
		data.FromID = data.Accounts[0].ID.String()
		data.ToID = data.Accounts[1].ID.String()
	}
	return data, nil
}

func (h *Handler) paySubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.fail(w, http.StatusBadRequest, "bad form")
		return
	}
	data, err := h.loadPay(r)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "could not load payment form")
		return
	}
	data.FromID = r.FormValue("from_id")
	data.ToID = r.FormValue("to_id")
	data.Amount = r.FormValue("amount")
	data.Note = r.FormValue("note")
	if key := strings.TrimSpace(r.FormValue("idem_key")); key != "" {
		data.FormKey = key // a resubmitted form replays, never double-pays
	}

	rerender := func(err error) {
		data.Error = formError(err)
		h.render(w, http.StatusBadRequest, "pay", data)
	}

	fromID, err := uuid.Parse(data.FromID)
	if err != nil {
		rerender(errors.New("choose who the payment is from"))
		return
	}
	toID, err := uuid.Parse(data.ToID)
	if err != nil {
		rerender(errors.New("choose who the payment goes to"))
		return
	}
	cents, err := ParseAmountToCents(data.Amount)
	if err != nil {
		rerender(err)
		return
	}
	note := strings.TrimSpace(data.Note)
	if len(note) > 140 {
		note = note[:140]
	}
	p, _, err := h.store.PostPaymentWithNote(r.Context(), fromID, toID, cents,
		store.KindPayment, &data.FormKey, note)
	if err != nil {
		rerender(err)
		return
	}
	to, err := h.store.GetAccount(r.Context(), p.ToAccount)
	notice := "Payment sent — " + FormatMoney(p.Currency, p.AmountCents)
	if err == nil {
		notice += " to " + to.Name + "."
	}
	http.Redirect(w, r, h.basePath+"/accounts/"+p.FromAccount.String()+
		"?notice="+url.QueryEscape(notice), http.StatusSeeOther)
}

// --- integrity -----------------------------------------------------

type integrityData struct {
	chrome
	Report *store.VerifyReport
}

func (h *Handler) integrity(w http.ResponseWriter, r *http.Request) {
	report, err := h.store.Verify(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "could not run verification")
		return
	}
	h.render(w, http.StatusOK, "integrity", &integrityData{chrome: h.chrome(r), Report: report})
}

// formError turns engine errors into the plain sentence the user
// should see. The store's messages are already human ("insufficient
// funds", "sender and receiver must be different accounts"); the
// only work here is capitalising the first letter for display.
func formError(err error) string {
	msg := err.Error()
	if msg == "" {
		return "something went wrong"
	}
	return strings.ToUpper(msg[:1]) + msg[1:] + "."
}
