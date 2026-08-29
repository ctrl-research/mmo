package store

import (
	"context"
	"testing"
	"time"
)

// Dungeon lockouts.
//
// A lockout is a promise about the future, so what is stored is when it
// expires rather than when it was earned. These check that the promise is kept
// and that it eventually stops being one.

func TestNoLockoutUntilSomethingIsCleared(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	char := testCharacter(t, s)

	until, err := s.LockedOutUntil(ctx, char, "test_crypt")
	if err != nil {
		t.Fatalf("reading a lockout that does not exist: %v", err)
	}
	if !until.IsZero() {
		t.Errorf("locked out of a dungeon never cleared, until %v", until)
	}
}

func TestClearingADungeonLocksItOut(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	char := testCharacter(t, s)

	if err := s.RecordClear(ctx, char, "test_crypt", time.Hour); err != nil {
		t.Fatalf("recording a clear: %v", err)
	}

	until, err := s.LockedOutUntil(ctx, char, "test_crypt")
	if err != nil {
		t.Fatalf("reading the lockout: %v", err)
	}
	if until.IsZero() {
		t.Fatal("cleared a dungeon and was not locked out of it")
	}
	if left := time.Until(until); left < 55*time.Minute || left > time.Hour+time.Minute {
		t.Errorf("locked out for %v, want about an hour", left)
	}
}

// A lockout is per dungeon. Clearing one must not bar the others.
func TestALockoutIsPerDungeon(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	char := testCharacter(t, s)

	if err := s.RecordClear(ctx, char, "test_crypt", time.Hour); err != nil {
		t.Fatalf("recording a clear: %v", err)
	}

	until, err := s.LockedOutUntil(ctx, char, "some_other_place")
	if err != nil {
		t.Fatalf("reading the other lockout: %v", err)
	}
	if !until.IsZero() {
		t.Error("clearing one dungeon locked another")
	}
}

// A lockout is per character, which is what stops a party carrying a friend
// through from spending the friend's, and what stops leaving a party from
// laundering one.
func TestALockoutIsPerCharacter(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	acct := newAccount(t, s, "two-characters")
	first, err := s.CreateCharacter(ctx, acct, "First", "warrior", "tutorial")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	second, err := s.CreateCharacter(ctx, acct, "Second", "warrior", "tutorial")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := s.RecordClear(ctx, first.ID, "test_crypt", time.Hour); err != nil {
		t.Fatalf("recording a clear: %v", err)
	}

	until, err := s.LockedOutUntil(ctx, second.ID, "test_crypt")
	if err != nil {
		t.Fatalf("reading the second character's lockout: %v", err)
	}
	if !until.IsZero() {
		t.Error("one character's clear locked out another on the same account")
	}
}

// An expired lockout is not a lockout. Stored as an expiry rather than as the
// moment of the clear, so this is a comparison rather than arithmetic against
// whatever the dungeon's duration happens to be today.
func TestAnExpiredLockoutLetsYouBackIn(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	char := testCharacter(t, s)

	// Already over by the time it is written.
	if err := s.RecordClear(ctx, char, "test_crypt", -time.Minute); err != nil {
		t.Fatalf("recording a clear: %v", err)
	}

	until, err := s.LockedOutUntil(ctx, char, "test_crypt")
	if err != nil {
		t.Fatalf("reading the lockout: %v", err)
	}
	if !until.IsZero() {
		t.Errorf("still locked out until %v, which is in the past", until)
	}
}

// Clearing again replaces the lockout rather than adding a second row. What
// matters is when the next attempt is allowed, and two answers to that is one
// of them being wrong.
func TestClearingAgainReplacesTheLockout(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	char := testCharacter(t, s)

	if err := s.RecordClear(ctx, char, "test_crypt", time.Minute); err != nil {
		t.Fatalf("first clear: %v", err)
	}
	first, _ := s.LockedOutUntil(ctx, char, "test_crypt")

	if err := s.RecordClear(ctx, char, "test_crypt", time.Hour); err != nil {
		t.Fatalf("second clear: %v", err)
	}
	second, err := s.LockedOutUntil(ctx, char, "test_crypt")
	if err != nil {
		t.Fatalf("reading the lockout: %v", err)
	}

	if !second.After(first) {
		t.Errorf("second clear left the lockout at %v, not after %v", second, first)
	}
}
