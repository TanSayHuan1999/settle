// settle is a small payment platform built on a double-entry ledger:
// every payment is one Postgres transaction that writes a balanced
// debit and credit, so money is never created, destroyed, or moved
// twice — and a /verify endpoint can prove it from the raw entries.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"settle/internal/api"
	"settle/internal/config"
	"settle/internal/store"
	"settle/internal/web"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("settle: %v", err)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return err
	}
	log.Printf("settle: schema ready")
	if seeded, err := st.SeedIfEmpty(ctx); err != nil {
		return err
	} else if seeded {
		log.Printf("settle: seeded demo dataset")
	}

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           dispatch(api.New(st).Routes(), web.New(st, cfg.BasePath).Routes()),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("settle: listening on :%d", cfg.Port)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// dispatch routes each request to the dashboard UI or the JSON API.
// The two share three paths — GET /, GET /accounts/{id} and
// POST /accounts — so browsers are told apart by the headers they
// send: form posts are url-encoded, page loads for an account ask
// for text/html, and /, /pay and /integrity exist only in the UI.
// Everything else (JSON bodies, API clients) goes to the API, whose
// paths and responses are unchanged.
func dispatch(apiH, uiH http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isUIRequest(r) {
			uiH.ServeHTTP(w, r)
			return
		}
		apiH.ServeHTTP(w, r)
	})
}

func isUIRequest(r *http.Request) bool {
	if r.Method == http.MethodPost {
		return strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded")
	}
	if r.Method != http.MethodGet {
		return false
	}
	switch r.URL.Path {
	case "/", "/pay", "/integrity":
		return true
	}
	if strings.HasPrefix(r.URL.Path, "/static/") {
		return true
	}
	if strings.HasPrefix(r.URL.Path, "/accounts/") {
		return strings.Contains(r.Header.Get("Accept"), "text/html")
	}
	return false
}
