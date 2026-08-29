package store

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// These tests run against a real Postgres rather than a fake.
//
// The behaviour that matters most here -- the fencing predicate, the unique
// index on names, transaction semantics -- lives in the database. A mock would
// assert that the Go code calls the queries it already calls, and would pass
// happily while the actual constraint was wrong.
//
// Start one with:
//
//	POSTGRES_PORT=5433 docker compose -f deploy/docker-compose.yml up -d postgres
//	export MMO_TEST_DATABASE_URL=postgres://mmo:devpassword@localhost:5433/mmo?sslmode=disable
func testStore(t *testing.T) *Store {
	t.Helper()

	url := os.Getenv("MMO_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MMO_TEST_DATABASE_URL is not set; skipping database tests")
	}

	ctx := context.Background()
	s, err := Open(ctx, Config{
		URL:    url,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(s.Close)

	// Each test starts from a clean slate. Cascades handle the rest.
	if _, err := s.pool.Exec(ctx, `TRUNCATE accounts, allowlist CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return s
}

func newAccount(t *testing.T, s *Store, subject string) uuid.UUID {
	t.Helper()
	acct, _, err := s.UpsertIdentity(context.Background(), "test", subject, subject+"@example.com")
	if err != nil {
		t.Fatalf("upsert identity: %v", err)
	}
	return acct.ID
}

// --- migrations -------------------------------------------------------------

func TestMigrationsApply(t *testing.T) {
	s := testStore(t)

	versions, err := s.AppliedMigrations(context.Background())
	if err != nil {
		t.Fatalf("applied migrations: %v", err)
	}
	if len(versions) < 2 {
		t.Errorf("applied %d migrations, want at least 2: %v", len(versions), versions)
	}
}

// Running migrations again must be a no-op, since every node runs them at
// startup and several may start at once.
func TestMigrationsAreIdempotent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	before, _ := s.AppliedMigrations(ctx)
	for i := 0; i < 3; i++ {
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("re-running migrations: %v", err)
		}
	}
	after, _ := s.AppliedMigrations(ctx)

	if len(before) != len(after) {
		t.Errorf("migration count changed from %d to %d on re-run", len(before), len(after))
	}
}

// Several nodes starting together must serialise on the advisory lock rather
// than racing to apply the same file.
func TestConcurrentMigrationsSerialise(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Migrate(ctx); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent migration failed: %v", err)
	}
}

// --- identity ---------------------------------------------------------------

func TestUpsertIdentityCreatesThenReuses(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	acct1, ident1, err := s.UpsertIdentity(ctx, "google", "sub-123", "a@example.com")
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if acct1.ID == uuid.Nil {
		t.Fatal("no account was created")
	}

	acct2, ident2, err := s.UpsertIdentity(ctx, "google", "sub-123", "a@example.com")
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if acct2.ID != acct1.ID {
		t.Errorf("second login created a new account: %s then %s", acct1.ID, acct2.ID)
	}
	if ident2.ID != ident1.ID {
		t.Error("second login created a duplicate identity")
	}
}

// The subject is the key, not the email. Providers allow email changes, and an
// account whose address changes upstream must stay the same account.
func TestIdentityFollowsSubjectNotEmail(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	first, _, err := s.UpsertIdentity(ctx, "google", "sub-123", "old@example.com")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	second, ident, err := s.UpsertIdentity(ctx, "google", "sub-123", "new@example.com")
	if err != nil {
		t.Fatalf("upsert after email change: %v", err)
	}

	if second.ID != first.ID {
		t.Error("an upstream email change created a new account")
	}
	if ident.Email != "new@example.com" {
		t.Errorf("stored email is %q, want the updated address", ident.Email)
	}
}

func TestSameSubjectOnDifferentProvidersAreDistinct(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	google, _, _ := s.UpsertIdentity(ctx, "google", "12345", "x@example.com")
	github, _, _ := s.UpsertIdentity(ctx, "github", "12345", "x@example.com")

	if google.ID == github.ID {
		t.Error("identical subjects on different providers collapsed into one account")
	}
}

func TestUpsertIdentityRejectsEmptyKeys(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if _, _, err := s.UpsertIdentity(ctx, "", "sub", "e@example.com"); err == nil {
		t.Error("an empty provider was accepted")
	}
	if _, _, err := s.UpsertIdentity(ctx, "google", "", "e@example.com"); err == nil {
		t.Error("an empty subject was accepted")
	}
}

// --- allowlist --------------------------------------------------------------

// An empty allowlist admits nobody. "Empty means open" fails toward a server
// anyone can join, discovered after the fact.
func TestEmptyAllowlistAdmitsNobody(t *testing.T) {
	s := testStore(t)

	ok, err := s.Allowed(context.Background(), "google", "sub-1", "a@example.com")
	if err != nil {
		t.Fatalf("allowed: %v", err)
	}
	if ok {
		t.Error("an empty allowlist admitted a player")
	}
}

func TestAllowlistMatchKinds(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if err := s.AddAllowlistEntry(ctx, "google", MatchSubject, "sub-allowed", ""); err != nil {
		t.Fatalf("add subject rule: %v", err)
	}
	if err := s.AddAllowlistEntry(ctx, "", MatchEmail, "friend@example.com", ""); err != nil {
		t.Fatalf("add email rule: %v", err)
	}
	if err := s.AddAllowlistEntry(ctx, "", MatchEmailDomain, "trusted.org", ""); err != nil {
		t.Fatalf("add domain rule: %v", err)
	}

	tests := []struct {
		name                     string
		provider, subject, email string
		want                     bool
	}{
		{"subject rule", "google", "sub-allowed", "any@nowhere.com", true},
		{"subject rule is provider-scoped", "github", "sub-allowed", "any@nowhere.com", false},
		{"email rule, any provider", "github", "other", "friend@example.com", true},
		{"email rule is case-insensitive", "github", "other", "FRIEND@Example.COM", true},
		{"domain rule", "google", "other", "someone@trusted.org", true},
		{"domain rule is case-insensitive", "google", "other", "someone@TRUSTED.ORG", true},
		{"domain rule does not match a substring", "google", "other", "someone@untrusted.org", false},
		{"no rule matches", "google", "nobody", "stranger@elsewhere.com", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.Allowed(ctx, tt.provider, tt.subject, tt.email)
			if err != nil {
				t.Fatalf("allowed: %v", err)
			}
			if got != tt.want {
				t.Errorf("Allowed(%q, %q, %q) = %v, want %v",
					tt.provider, tt.subject, tt.email, got, tt.want)
			}
		})
	}
}

// Revocation must take effect, or removing someone from the allowlist does
// nothing for anyone who has already signed in once.
func TestRemovingAnAllowlistEntryRevokesAccess(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	s.AddAllowlistEntry(ctx, "", MatchEmail, "gone@example.com", "")
	if ok, _ := s.Allowed(ctx, "google", "sub", "gone@example.com"); !ok {
		t.Fatal("setup: entry did not grant access")
	}

	if err := s.RemoveAllowlistEntry(ctx, "", MatchEmail, "gone@example.com"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if ok, _ := s.Allowed(ctx, "google", "sub", "gone@example.com"); ok {
		t.Error("access survived removal from the allowlist")
	}
}

func TestAddingAnAllowlistEntryTwiceIsFine(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := s.AddAllowlistEntry(ctx, "", MatchEmail, "dup@example.com", ""); err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
	}
	list, _ := s.ListAllowlist(ctx)
	if len(list) != 1 {
		t.Errorf("allowlist holds %d entries after three identical adds, want 1", len(list))
	}
}

func TestAllowlistRejectsBadInput(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if err := s.AddAllowlistEntry(ctx, "", "nonsense", "x", ""); err == nil {
		t.Error("an unknown match kind was accepted")
	}
	if err := s.AddAllowlistEntry(ctx, "", MatchEmail, "", ""); err == nil {
		t.Error("an empty value was accepted")
	}
}

// --- characters -------------------------------------------------------------

func TestCreateAndLoadCharacter(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	acct := newAccount(t, s, "player1")

	created, err := s.CreateCharacter(ctx, acct, "Alice", "warrior", "tutorial")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Level != 1 || created.Exp != 0 {
		t.Errorf("new character starts at level %d exp %d, want 1 and 0", created.Level, created.Exp)
	}

	loaded, err := s.LoadCharacter(ctx, acct, created.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Name != "Alice" || loaded.MapID != "tutorial" {
		t.Errorf("loaded %+v, want name Alice on tutorial", loaded)
	}
}

// Loading is scoped to the owning account, so a stolen or guessed ID is not
// enough to reach someone else's character.
func TestCannotLoadAnotherAccountsCharacter(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	owner := newAccount(t, s, "owner")
	intruder := newAccount(t, s, "intruder")

	c, err := s.CreateCharacter(ctx, owner, "Victim", "warrior", "tutorial")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := s.LoadCharacter(ctx, intruder, c.ID); err != ErrNotFound {
		t.Errorf("loading another account's character returned %v, want ErrNotFound", err)
	}
}

func TestCharacterNamesAreUniqueCaseInsensitively(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := newAccount(t, s, "a")
	b := newAccount(t, s, "b")

	if _, err := s.CreateCharacter(ctx, a, "Hero", "warrior", "tutorial"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.CreateCharacter(ctx, b, "hero", "warrior", "tutorial"); err != ErrNameTaken {
		t.Errorf("creating 'hero' next to 'Hero' returned %v, want ErrNameTaken", err)
	}
}

// The unique index is the authority, not a prior availability check: two
// simultaneous creates would both see the name free.
func TestConcurrentCreatesCannotDuplicateAName(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	accounts := make([]uuid.UUID, 8)
	for i := range accounts {
		accounts[i] = newAccount(t, s, fmt.Sprintf("racer%d", i))
	}

	var wg sync.WaitGroup
	created := make(chan uuid.UUID, len(accounts))
	for _, acct := range accounts {
		wg.Add(1)
		go func(id uuid.UUID) {
			defer wg.Done()
			if c, err := s.CreateCharacter(ctx, id, "Contested", "warrior", "tutorial"); err == nil {
				created <- c.ID
			}
		}(acct)
	}
	wg.Wait()
	close(created)

	if n := len(created); n != 1 {
		t.Errorf("%d characters were created with the same name, want exactly 1", n)
	}
}

func TestDeleteFreesTheName(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	acct := newAccount(t, s, "player")

	c, _ := s.CreateCharacter(ctx, acct, "Recycled", "warrior", "tutorial")
	if err := s.DeleteCharacter(ctx, acct, c.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if _, err := s.LoadCharacter(ctx, acct, c.ID); err != ErrNotFound {
		t.Errorf("a deleted character still loads: %v", err)
	}
	if _, err := s.CreateCharacter(ctx, acct, "Recycled", "warrior", "tutorial"); err != nil {
		t.Errorf("the name was not freed by deletion: %v", err)
	}
}

func TestDeleteIsScopedToTheOwner(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner := newAccount(t, s, "owner")
	intruder := newAccount(t, s, "intruder")

	c, _ := s.CreateCharacter(ctx, owner, "Safe", "warrior", "tutorial")
	if err := s.DeleteCharacter(ctx, intruder, c.ID); err != ErrNotFound {
		t.Errorf("deleting another account's character returned %v, want ErrNotFound", err)
	}
	if _, err := s.LoadCharacter(ctx, owner, c.ID); err != nil {
		t.Error("the character was deleted by someone who did not own it")
	}
}

func TestValidateName(t *testing.T) {
	valid := []string{"Abc", "Alice", "Player1", "aaa", "ABCDEFGHIJKLMNOP"}
	for _, n := range valid {
		if err := ValidateName(n); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", n, err)
		}
	}

	invalid := map[string]string{
		"ab":                "too short",
		"":                  "empty",
		"ABCDEFGHIJKLMNOPQ": "too long",
		"1abc":              "starts with a digit",
		"has space":         "contains a space",
		"semi;colon":        "contains punctuation",
		"under_score":       "contains an underscore",
		"emoji\U0001F600":   "contains an emoji",
	}
	for n, why := range invalid {
		if err := ValidateName(n); err == nil {
			t.Errorf("ValidateName(%q) accepted a name that is %s", n, why)
		}
	}
}

// --- fencing ----------------------------------------------------------------

func TestCheckpointPersistsProgress(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	acct := newAccount(t, s, "player")

	c, _ := s.CreateCharacter(ctx, acct, "Saver", "warrior", "tutorial")

	c.Level = 7
	c.Exp = 1234
	c.Gold = 999
	c.MapID = "henesys"
	c.State = json.RawMessage(`{"x":512,"y":256,"hp":88}`)

	if err := s.Checkpoint(ctx, c, 1); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	loaded, err := s.LoadCharacter(ctx, acct, c.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Level != 7 || loaded.Exp != 1234 || loaded.Gold != 999 {
		t.Errorf("progress not persisted: %+v", loaded)
	}
	if loaded.MapID != "henesys" {
		t.Errorf("map is %q, want henesys -- logging back in would resume in the wrong place", loaded.MapID)
	}

	var state map[string]any
	if err := json.Unmarshal(loaded.State, &state); err != nil {
		t.Fatalf("state did not round-trip: %v", err)
	}
	if state["hp"] != float64(88) {
		t.Errorf("state lost data: %v", state)
	}
}

// The single-writer invariant, which is what stops two nodes duplicating a
// character's items. A stale writer's token is lower, so its write is rejected.
func TestStaleWriterIsFenced(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	acct := newAccount(t, s, "player")

	c, _ := s.CreateCharacter(ctx, acct, "Contested", "warrior", "tutorial")

	// Node A holds token 5 and writes.
	c.Gold = 100
	if err := s.Checkpoint(ctx, c, 5); err != nil {
		t.Fatalf("first checkpoint: %v", err)
	}

	// Ownership moves to node B, which takes token 6 and writes.
	c.Gold = 200
	if err := s.Checkpoint(ctx, c, 6); err != nil {
		t.Fatalf("second checkpoint: %v", err)
	}

	// Node A wakes from a long pause still believing it owns the character.
	c.Gold = 999999
	if err := s.Checkpoint(ctx, c, 5); err != ErrStaleWrite {
		t.Fatalf("stale write returned %v, want ErrStaleWrite", err)
	}

	loaded, _ := s.LoadCharacter(ctx, acct, c.ID)
	if loaded.Gold != 200 {
		t.Errorf("gold is %d after a fenced write, want 200 -- the stale writer won", loaded.Gold)
	}
	if loaded.LeaseToken != 6 {
		t.Errorf("lease token is %d, want 6", loaded.LeaseToken)
	}
}

// The same token must keep working, since one owner checkpoints repeatedly
// throughout a session without reacquiring its lease.
func TestSameTokenCanCheckpointRepeatedly(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	acct := newAccount(t, s, "player")

	c, _ := s.CreateCharacter(ctx, acct, "Repeat", "warrior", "tutorial")
	for i := 1; i <= 5; i++ {
		c.Exp = int64(i * 100)
		if err := s.Checkpoint(ctx, c, 3); err != nil {
			t.Fatalf("checkpoint %d with the held token: %v", i, err)
		}
	}

	loaded, _ := s.LoadCharacter(ctx, acct, c.ID)
	if loaded.Exp != 500 {
		t.Errorf("exp is %d, want 500", loaded.Exp)
	}
}

// A checkpoint for something that no longer exists is a caller bug, and must
// be distinguishable from losing ownership -- the responses differ.
func TestCheckpointOfDeletedCharacterIsNotFound(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	acct := newAccount(t, s, "player")

	c, _ := s.CreateCharacter(ctx, acct, "Doomed", "warrior", "tutorial")
	s.DeleteCharacter(ctx, acct, c.ID)

	if err := s.Checkpoint(ctx, c, 1); err != ErrNotFound {
		t.Errorf("checkpointing a deleted character returned %v, want ErrNotFound", err)
	}
}

func TestConcurrentCheckpointsNeverGoBackwards(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	acct := newAccount(t, s, "player")

	c, _ := s.CreateCharacter(ctx, acct, "Racer", "warrior", "tutorial")

	// Many writers with ascending tokens, run concurrently. Whatever order
	// they land in, the stored token must never decrease.
	var wg sync.WaitGroup
	for token := int64(1); token <= 20; token++ {
		wg.Add(1)
		go func(tok int64) {
			defer wg.Done()
			copy := c
			copy.Exp = tok * 10
			s.Checkpoint(ctx, copy, tok)
		}(token)
	}
	wg.Wait()

	loaded, _ := s.LoadCharacter(ctx, acct, c.ID)
	if loaded.LeaseToken < 1 {
		t.Errorf("lease token is %d after concurrent writes", loaded.LeaseToken)
	}
}

func TestCountAndListCharacters(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	acct := newAccount(t, s, "player")

	for _, name := range []string{"Aaa", "Bbb", "Ccc"} {
		if _, err := s.CreateCharacter(ctx, acct, name, "warrior", "tutorial"); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}

	n, err := s.CountCharacters(ctx, acct)
	if err != nil || n != 3 {
		t.Errorf("count = %d (err %v), want 3", n, err)
	}

	list, err := s.ListCharacters(ctx, acct)
	if err != nil || len(list) != 3 {
		t.Fatalf("list returned %d characters (err %v), want 3", len(list), err)
	}
	// Newest first, so the character select shows the most recent at the top.
	if list[0].Name != "Ccc" {
		t.Errorf("first listed character is %q, want the newest (Ccc)", list[0].Name)
	}
}

func TestNameAvailable(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	acct := newAccount(t, s, "player")

	if ok, _ := s.NameAvailable(ctx, "Fresh"); !ok {
		t.Error("an unused name was reported as taken")
	}

	s.CreateCharacter(ctx, acct, "Fresh", "warrior", "tutorial")

	if ok, _ := s.NameAvailable(ctx, "Fresh"); ok {
		t.Error("a used name was reported as available")
	}
	if ok, _ := s.NameAvailable(ctx, strings.ToUpper("fresh")); ok {
		t.Error("name availability is case-sensitive; it should not be")
	}
}
