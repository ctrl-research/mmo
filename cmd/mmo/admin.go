package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"text/tabwriter"
	"time"

	"github.com/ctrl-research/mmo/internal/auth"
	"github.com/ctrl-research/mmo/internal/store"
)

// Administration commands.
//
// The allowlist admits nobody by default, so a fresh server needs a way to let
// the first person in. Telling an operator to write SQL by hand is a bad
// answer: it is easy to get the match kind wrong, and getting it wrong on an
// allowlist fails open in the worst case.
//
//	mmo allow jonathan              add a local username
//	mmo allow --provider=google --kind=email you@example.com
//	mmo allowlist                   list every rule
//	mmo revoke jonathan             remove a rule
//	mmo passwd jonathan             set a local account's password
const adminUsage = `Usage:
  mmo allow [flags] VALUE      allow someone to sign in
  mmo revoke [flags] VALUE     remove an allowlist rule
  mmo allowlist                list allowlist rules
  mmo passwd USERNAME          set a local account's password

Flags for allow and revoke:
  --provider   identity provider, or empty for any (default: local)
  --kind       subject, email, or email_domain (default: subject)
  --note       a reminder of why the rule exists

Every command takes --database-url, or reads DATABASE_URL.
`

// runAdmin dispatches an administration subcommand, reporting whether one was
// recognised.
func runAdmin(args []string) (handled bool, err error) {
	if len(args) == 0 {
		return false, nil
	}

	switch args[0] {
	case "allow", "revoke", "allowlist", "passwd":
	default:
		return false, nil
	}

	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	dbURL := fs.String("database-url",
		envOr("DATABASE_URL", "postgres://mmo:devpassword@localhost:5432/mmo?sslmode=disable"),
		"Postgres connection string")
	provider := fs.String("provider", "local", "identity provider, or empty for any")
	kind := fs.String("kind", store.MatchSubject, "subject, email, or email_domain")
	note := fs.String("note", "", "a reminder of why the rule exists")
	fs.Usage = func() { fmt.Fprint(os.Stderr, adminUsage) }

	if err := fs.Parse(args[1:]); err != nil {
		return true, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Quiet, so command output is the output rather than being buried under
	// migration logging.
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	db, err := store.Open(ctx, store.Config{URL: *dbURL, Logger: log, MaxConns: 2})
	if err != nil {
		return true, err
	}
	defer db.Close()

	switch args[0] {
	case "allow":
		return true, adminAllow(ctx, db, fs.Arg(0), *provider, *kind, *note)
	case "revoke":
		return true, adminRevoke(ctx, db, fs.Arg(0), *provider, *kind)
	case "allowlist":
		return true, adminList(ctx, db)
	case "passwd":
		return true, adminPasswd(ctx, db, fs.Arg(0))
	}
	return true, nil
}

func adminAllow(ctx context.Context, db *store.Store, value, provider, kind, note string) error {
	if value == "" {
		return errors.New("a value is required: mmo allow USERNAME")
	}

	// Usernames are matched case-insensitively and stored normalised, so an
	// entry added as "Jonathan" must still match a sign-in as "jonathan".
	if provider == "local" && kind == store.MatchSubject {
		value = auth.NormaliseUsername(value)
	}

	if err := db.AddAllowlistEntry(ctx, provider, kind, value, note); err != nil {
		return err
	}

	scope := provider
	if scope == "" {
		scope = "any provider"
	}
	fmt.Printf("allowed %s %q on %s\n", kind, value, scope)

	if provider == "local" && kind == store.MatchSubject {
		fmt.Printf("They can now register at /auth/local/register with the username %q.\n", value)
	}
	return nil
}

func adminRevoke(ctx context.Context, db *store.Store, value, provider, kind string) error {
	if value == "" {
		return errors.New("a value is required: mmo revoke USERNAME")
	}
	if provider == "local" && kind == store.MatchSubject {
		value = auth.NormaliseUsername(value)
	}

	if err := db.RemoveAllowlistEntry(ctx, provider, kind, value); err != nil {
		return err
	}

	// Access ends at their next sign-in, because the allowlist is re-checked
	// every time rather than only at registration. An open session survives
	// until its access token expires.
	fmt.Printf("revoked %s %q\n", kind, value)
	fmt.Println("This takes effect at their next sign-in; any open session lasts until its token expires.")
	return nil
}

func adminList(ctx context.Context, db *store.Store) error {
	entries, err := db.ListAllowlist(ctx)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Println("The allowlist is empty, so nobody can sign in.")
		fmt.Println("Add someone with:  mmo allow USERNAME")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PROVIDER\tKIND\tVALUE\tADDED\tNOTE")
	for _, e := range entries {
		provider := e.Provider
		if provider == "" {
			provider = "(any)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			provider, e.MatchKind, e.MatchValue, e.AddedAt.Format("2006-01-02"), e.Note)
	}
	return w.Flush()
}

func adminPasswd(ctx context.Context, db *store.Store, username string) error {
	if username == "" {
		return errors.New("a username is required: mmo passwd USERNAME")
	}
	normalised := auth.NormaliseUsername(username)

	cred, err := db.LocalCredentialByUsername(ctx, normalised)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no local account named %q", normalised)
	}
	if err != nil {
		return err
	}

	password, err := readPassword("New password: ")
	if err != nil {
		return err
	}
	again, err := readPassword("Again: ")
	if err != nil {
		return err
	}
	if password != again {
		return errors.New("passwords did not match")
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	if err := db.UpdatePasswordHash(ctx, cred.AccountID, hash); err != nil {
		return err
	}

	// Clearing the lockout as well, since an operator resetting a password is
	// usually doing it because somebody is locked out.
	if err := db.ClearLoginFailures(ctx, cred.AccountID); err != nil {
		return err
	}

	fmt.Printf("password set for %q\n", cred.Username)
	return nil
}
