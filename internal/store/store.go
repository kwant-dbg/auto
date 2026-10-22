// Package store is the Postgres persistence layer: connection, migrations,
// the content-addressed snapshot store, and queries used across services.
package store

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kwant-dbg/auto/migrations"
)

// Store wraps the connection pool and the snapshot blob store.
type Store struct {
	DB    *pgxpool.Pool
	Blobs *BlobStore
}

// DefaultMaxConns sizes the pool when the URL doesn't set pool_max_conns.
// pgx's own default (4, or the CPU count) is too small: collection,
// clustering and consolidation each hold a connection for an advisory lock
// while doing their work on others, so a handful of concurrent tasks would
// otherwise exhaust the pool and deadlock.
const DefaultMaxConns = 32

// Open connects to Postgres and verifies the connection.
func Open(ctx context.Context, databaseURL, blobRoot string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("database url: %w", err)
	}
	if !strings.Contains(databaseURL, "pool_max_conns") {
		cfg.MaxConns = DefaultMaxConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Store{DB: pool, Blobs: &BlobStore{Root: blobRoot}}, nil
}

// Close releases the pool.
func (s *Store) Close() { s.DB.Close() }

// Migrate applies every embedded migration not yet recorded in
// schema_migrations, each in its own transaction. It is safe to run on every
// start; an advisory lock serialises concurrent callers.
func Migrate(ctx context.Context, db *pgxpool.Pool) ([]string, error) {
	conn, err := db.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()

	const lockKey = 7_004_133 // arbitrary, stable
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, lockKey); err != nil {
		return nil, fmt.Errorf("migration lock: %w", err)
	}
	defer conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, lockKey) //nolint:errcheck

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		name text PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return nil, err
	}

	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)

	var applied []string
	for _, name := range names {
		var exists bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name = $1)`, name).Scan(&exists); err != nil {
			return applied, err
		}
		if exists {
			continue
		}
		sql, err := migrations.FS.ReadFile(name)
		if err != nil {
			return applied, err
		}
		err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (name) VALUES ($1)`, name)
			return err
		})
		if err != nil {
			return applied, fmt.Errorf("migration %s: %w", name, err)
		}
		applied = append(applied, strings.TrimSuffix(name, ".sql"))
	}
	return applied, nil
}
