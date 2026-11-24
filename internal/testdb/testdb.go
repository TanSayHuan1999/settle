// Package testdb gives DB-backed tests an isolated throwaway
// database on the dev Postgres server, so test packages running in
// parallel never see each other's rows.
package testdb

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"settle/internal/store"
)

// Open creates a fresh database, migrates it, and returns a Store
// bound to it. The database is dropped when the test finishes.
// It skips the test when no DATABASE_URL is configured.
func Open(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = os.Getenv("TEST_DATABASE_URL")
	}
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping DB-backed test")
	}
	ctx := context.Background()

	name := fmt.Sprintf("settle_test_%d_%d", os.Getpid(), time.Now().UnixNano()%1_000_000)

	adminCfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	adminCfg.Database = "postgres"
	admin, err := pgx.ConnectConfig(ctx, adminCfg)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create test database: %v", err)
	}

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DATABASE_URL as URL: %v", err)
	}
	u.Path = "/" + name
	st, err := store.New(ctx, u.String())
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	t.Cleanup(func() {
		st.Close()
		admin2, err := pgx.ConnectConfig(ctx, adminCfg)
		if err != nil {
			return
		}
		defer admin2.Close(ctx)
		_, _ = admin2.Exec(ctx, `DROP DATABASE IF EXISTS `+pgx.Identifier{name}.Sanitize()+` WITH (FORCE)`)
	})
	return st
}
