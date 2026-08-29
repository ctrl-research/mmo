// Package storetest gives each test its own isolated database schema.
//
// Several packages test against Postgres, and `go test ./...` runs packages in
// parallel. Sharing one schema and truncating between tests means they delete
// each other's rows, which produces failures that pass in isolation and fail
// in the suite -- the worst kind to chase.
//
// Each Store here gets a freshly created schema with its own migrations,
// dropped on cleanup. Tests are then genuinely independent, and no test has to
// remember to clean up after itself.
package storetest

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/ctrl-research/mmo/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DatabaseURL returns the test database URL, or skips the test.
func DatabaseURL(t *testing.T) string {
	t.Helper()

	url := os.Getenv("MMO_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MMO_TEST_DATABASE_URL is not set; skipping database tests")
	}
	return url
}

// New returns a Store backed by a schema private to this test.
func New(t *testing.T) *store.Store {
	t.Helper()

	url := DatabaseURL(t)
	ctx := context.Background()

	// A valid identifier that cannot collide: schema names may not start with
	// a digit, and a UUID's hyphens are not allowed unquoted.
	schema := "t" + sanitise(uuid.NewString())

	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connecting to create a test schema: %v", err)
	}
	defer admin.Close()

	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE SCHEMA %q`, schema)); err != nil {
		t.Fatalf("creating schema %s: %v", schema, err)
	}

	t.Cleanup(func() {
		dropCtx, cancel := context.WithCancel(context.Background())
		defer cancel()

		pool, err := pgxpool.New(dropCtx, url)
		if err != nil {
			return
		}
		defer pool.Close()
		pool.Exec(dropCtx, fmt.Sprintf(`DROP SCHEMA %q CASCADE`, schema))
	})

	// search_path scopes every unqualified name in this connection to the
	// private schema, so the migrations and all subsequent queries land there
	// without a single query needing to know about it.
	scoped := appendSearchPath(url, schema)

	s, err := store.Open(ctx, store.Config{
		URL:      scoped,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxConns: 4,
	})
	if err != nil {
		t.Fatalf("opening store on schema %s: %v", schema, err)
	}
	t.Cleanup(s.Close)

	return s
}

// appendSearchPath adds a search_path option to a connection string.
func appendSearchPath(url, schema string) string {
	sep := "?"
	for i := 0; i < len(url); i++ {
		if url[i] == '?' {
			sep = "&"
			break
		}
	}
	return url + sep + "search_path=" + schema
}

// sanitise strips characters that would need quoting in an identifier.
func sanitise(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			out = append(out, c)
		}
	}
	return string(out)
}
