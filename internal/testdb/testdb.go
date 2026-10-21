// Package testdb gives integration tests a fresh, migrated database.
//
// Tests using it are skipped unless AOE_TEST_DATABASE_URL points at a
// Postgres server (with pgvector) where the user may create databases, e.g.
// postgres://postgres@localhost:5432/postgres?sslmode=disable.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/kwant-dbg/auto/internal/store"
)

// New creates a uniquely named database, applies migrations and returns a
// Store whose blobs live in a temp dir. Everything is removed on cleanup.
func New(t *testing.T) *store.Store {
	t.Helper()
	admin := os.Getenv("AOE_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("AOE_TEST_DATABASE_URL not set; skipping database test")
	}
	ctx := context.Background()

	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	name := "aoe_test_" + hex.EncodeToString(b)

	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		conn.Close(ctx)
		t.Fatalf("create database: %v", err)
	}

	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	st, err := store.Open(ctx, u.String(), t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		st.Close()
		conn.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)") //nolint:errcheck
		conn.Close(ctx)
	})
	if _, err := store.Migrate(ctx, st.DB); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}
