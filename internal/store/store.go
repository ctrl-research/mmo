// Package store is the durable state layer.
//
// Postgres holds accounts, identities, the allowlist, and characters. It is a
// *checkpoint store*, not the simulation's working memory: live character
// state lives in memory on the world node that owns it, and is written here on
// an interval, on logout, and on room handoff. Writing position at 20 Hz would
// melt the database and buy nothing, since position is worthless a second
// later (docs/data-model.md).
//
// The rule for what may sit in that checkpoint window: if losing it would make
// a player file a ticket, write it through immediately rather than waiting.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Common errors.
var (
	ErrNotFound = errors.New("store: not found")

	// ErrNameTaken means another living character already has the name.
	ErrNameTaken = errors.New("store: character name is taken")

	// ErrStaleWrite means a write carried a fencing token lower than the one
	// currently held. It is never routine: it means this process lost
	// ownership of the character and must discard its in-memory copy rather
	// than retry. See docs/data-model.md.
	ErrStaleWrite = errors.New("store: write rejected, ownership lost")
)

// Store is a connection pool plus the queries the game needs.
type Store struct {
	pool *pgxpool.Pool
	log  *slog.Logger
}

// Config configures a Store.
type Config struct {
	// URL is a Postgres connection string.
	URL string

	// MaxConns bounds the pool. The game is not query-heavy -- checkpoints are
	// periodic, not per-tick -- so a small pool is plenty and keeps Postgres
	// from being the thing that falls over first.
	MaxConns int32

	Logger *slog.Logger
}

// Open connects, verifies the connection, and applies any pending migrations.
func Open(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.URL == "" {
		return nil, errors.New("store: a database URL is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.MaxConns <= 0 {
		cfg.MaxConns = 10
	}

	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("store: parsing database URL: %w", err)
	}
	poolCfg.MaxConns = cfg.MaxConns
	poolCfg.MaxConnLifetime = time.Hour
	poolCfg.HealthCheckPeriod = 30 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("store: creating pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: connecting to Postgres: %w", err)
	}

	s := &Store{pool: pool, log: cfg.Logger}
	if err := s.Migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Pool exposes the underlying pool for tests and for callers that need a
// transaction spanning several repositories.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// migrationLockID is an arbitrary but fixed key for the advisory lock that
// serialises migrations. Several nodes starting at once must not race to
// apply the same file.
const migrationLockID int64 = 0x6D6D6F5F6D696772 // "mmo_migr"

// Migrate applies every pending migration in order.
//
// Forward-only, one transaction per file, under an advisory lock so that
// concurrent starts serialise rather than collide. A partially applied
// migration rolls back with its own transaction, so the schema is never left
// halfway.
func (s *Store) Migrate(ctx context.Context) error {
	files, err := migrationFiles()
	if err != nil {
		return err
	}

	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("store: acquiring connection for migration: %w", err)
	}
	defer conn.Release()

	// Blocks until any other node finishes migrating, rather than failing.
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockID); err != nil {
		return fmt.Errorf("store: taking migration lock: %w", err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, migrationLockID)
	}()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    text PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("store: creating migration table: %w", err)
	}

	applied := make(map[string]bool)
	rows, err := conn.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("store: reading applied migrations: %w", err)
	}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		applied[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, name := range files {
		version := strings.TrimSuffix(name, ".sql")
		if applied[version] {
			continue
		}

		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("store: reading migration %s: %w", name, err)
		}

		// One transaction per migration, so a failure leaves the schema
		// exactly as it was rather than halfway through a file.
		err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(body)); err != nil {
				return fmt.Errorf("applying %s: %w", name, err)
			}
			_, err := tx.Exec(ctx,
				`INSERT INTO schema_migrations (version) VALUES ($1)`, version)
			return err
		})
		if err != nil {
			return fmt.Errorf("store: %w", err)
		}

		s.log.Info("migration applied", "version", version)
	}
	return nil
}

// migrationFiles lists the embedded migrations in lexical order.
//
// Sorted explicitly rather than relying on directory order: migrations must
// apply in the same sequence everywhere, and filesystem iteration order is not
// a guarantee worth betting a schema on.
func migrationFiles() ([]string, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("store: reading migrations: %w", err)
	}

	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)

	if len(out) == 0 {
		return nil, errors.New("store: no migrations found")
	}
	return out, nil
}

// AppliedMigrations returns the versions already applied, for diagnostics.
func (s *Store) AppliedMigrations(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
