package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Local account errors.
var (
	// ErrUsernameTaken means another account already holds the username.
	ErrUsernameTaken = errors.New("store: username is taken")

	// ErrAccountLocked means too many consecutive failures. It is deliberately
	// never surfaced to the person attempting to sign in -- telling them the
	// account exists and is locked confirms the username, which is exactly
	// what a guessing attack wants to learn.
	ErrAccountLocked = errors.New("store: account is temporarily locked")
)

// Lockout policy.
const (
	// MaxFailedAttempts before an account is locked.
	//
	// High enough that a person mistyping does not lock themselves out of
	// their own game, low enough that online guessing is hopeless. Offline
	// resistance comes from Argon2, not from this.
	MaxFailedAttempts = 10

	// LockoutDuration is how long a locked account stays locked.
	//
	// Bounded rather than permanent: a permanent lock turns a nuisance attack
	// into a denial of service against a real player, which is a worse outcome
	// than the guessing it prevents.
	LockoutDuration = 15 * time.Minute
)

// LocalCredential is a username and password bound to an account.
type LocalCredential struct {
	AccountID      uuid.UUID
	Username       string
	PasswordHash   string
	FailedAttempts int
	LockedUntil    *time.Time
}

// Locked reports whether the account is currently barred from signing in.
func (c LocalCredential) Locked(now time.Time) bool {
	return c.LockedUntil != nil && now.Before(*c.LockedUntil)
}

// CreateLocalAccount registers a username and password.
//
// The account, its identity row, and its credential are created in one
// transaction: a half-registered account with no way to sign in would be
// invisible to the person who created it and impossible for them to retry,
// since the username would already be taken.
func (s *Store) CreateLocalAccount(ctx context.Context, username, normalised, passwordHash string) (Account, error) {
	var acct Account

	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO accounts DEFAULT VALUES RETURNING id, created_at`,
		).Scan(&acct.ID, &acct.CreatedAt); err != nil {
			return fmt.Errorf("creating account: %w", err)
		}

		// The identity row uses the normalised username as its subject, which
		// is what lets the allowlist name someone before they have registered.
		if _, err := tx.Exec(ctx, `
			INSERT INTO identities (account_id, provider, subject, email)
			VALUES ($1, 'local', $2, '')`,
			acct.ID, normalised); err != nil {
			return fmt.Errorf("creating identity: %w", err)
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO local_credentials (account_id, username, password_hash)
			VALUES ($1, $2, $3)`,
			acct.ID, username, passwordHash); err != nil {
			return fmt.Errorf("creating credential: %w", err)
		}
		return nil
	})

	if err != nil {
		// The unique indexes are the authority on collisions, not a prior
		// lookup: two simultaneous registrations would both find the name free.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Account{}, ErrUsernameTaken
		}
		return Account{}, fmt.Errorf("store: creating local account: %w", err)
	}
	return acct, nil
}

// LocalCredentialByUsername looks up a credential.
//
// Returns ErrNotFound for an unknown username. Callers must take care that an
// unknown username and a wrong password are indistinguishable from outside --
// see DummyVerify.
func (s *Store) LocalCredentialByUsername(ctx context.Context, normalised string) (LocalCredential, error) {
	var c LocalCredential
	err := s.pool.QueryRow(ctx, `
		SELECT account_id, username, password_hash, failed_attempts, locked_until
		  FROM local_credentials
		 WHERE lower(username) = $1`,
		normalised,
	).Scan(&c.AccountID, &c.Username, &c.PasswordHash, &c.FailedAttempts, &c.LockedUntil)

	if errors.Is(err, pgx.ErrNoRows) {
		return LocalCredential{}, ErrNotFound
	}
	if err != nil {
		return LocalCredential{}, fmt.Errorf("store: reading local credential: %w", err)
	}
	return c, nil
}

// RecordLoginFailure increments the failure count and locks past the limit.
//
// Returns the resulting count, so a caller can log an account coming under
// attack without a second query.
func (s *Store) RecordLoginFailure(ctx context.Context, accountID uuid.UUID) (int, error) {
	var attempts int
	err := s.pool.QueryRow(ctx, `
		UPDATE local_credentials
		   SET failed_attempts = failed_attempts + 1,
		       locked_until = CASE
		           WHEN failed_attempts + 1 >= $2 THEN now() + $3::interval
		           ELSE locked_until
		       END,
		       updated_at = now()
		 WHERE account_id = $1
		 RETURNING failed_attempts`,
		accountID, MaxFailedAttempts, LockoutDuration.String(),
	).Scan(&attempts)

	if err != nil {
		return 0, fmt.Errorf("store: recording login failure: %w", err)
	}
	return attempts, nil
}

// ClearLoginFailures resets the counter after a successful sign-in.
func (s *Store) ClearLoginFailures(ctx context.Context, accountID uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE local_credentials
		   SET failed_attempts = 0, locked_until = NULL, updated_at = now()
		 WHERE account_id = $1`, accountID)
	if err != nil {
		return fmt.Errorf("store: clearing login failures: %w", err)
	}
	return nil
}

// UpdatePasswordHash replaces a stored hash.
//
// Used both for a deliberate password change and for transparently upgrading a
// hash made with weaker parameters, which is only possible at the moment of a
// successful sign-in.
func (s *Store) UpdatePasswordHash(ctx context.Context, accountID uuid.UUID, hash string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE local_credentials
		   SET password_hash = $2, updated_at = now()
		 WHERE account_id = $1`, accountID, hash)
	if err != nil {
		return fmt.Errorf("store: updating password: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UsernameAvailable reports whether a username is free.
//
// Advisory only, for a message before someone commits to a name. The unique
// index remains the authority.
func (s *Store) UsernameAvailable(ctx context.Context, normalised string) (bool, error) {
	var taken bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM local_credentials WHERE lower(username) = $1)`,
		normalised).Scan(&taken)
	if err != nil {
		return false, fmt.Errorf("store: checking username: %w", err)
	}
	return !taken, nil
}

// HasLocalAccounts reports whether any local account exists, so the server can
// warn when it starts with none and no other way to sign in.
func (s *Store) HasLocalAccounts(ctx context.Context) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM local_credentials)`).Scan(&exists)
	return exists, err
}

// LocalCredentialForAccount reads a credential by account, for a password
// change where the account is already known from the session.
func (s *Store) LocalCredentialForAccount(ctx context.Context, accountID uuid.UUID) (LocalCredential, error) {
	var c LocalCredential
	err := s.pool.QueryRow(ctx, `
		SELECT account_id, username, password_hash, failed_attempts, locked_until
		  FROM local_credentials
		 WHERE account_id = $1`,
		accountID,
	).Scan(&c.AccountID, &c.Username, &c.PasswordHash, &c.FailedAttempts, &c.LockedUntil)

	if errors.Is(err, pgx.ErrNoRows) {
		return LocalCredential{}, ErrNotFound
	}
	if err != nil {
		return LocalCredential{}, fmt.Errorf("store: reading local credential: %w", err)
	}
	return c, nil
}
