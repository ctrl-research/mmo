package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Account is a player's login, independent of any particular identity
// provider.
type Account struct {
	ID          uuid.UUID
	CreatedAt   time.Time
	LastLoginAt *time.Time
	BannedUntil *time.Time
	Notes       string
}

// Banned reports whether the account is currently barred.
func (a Account) Banned(now time.Time) bool {
	return a.BannedUntil != nil && now.Before(*a.BannedUntil)
}

// Identity is one external login bound to an account.
type Identity struct {
	ID        uuid.UUID
	AccountID uuid.UUID
	Provider  string
	Subject   string
	Email     string
}

// UpsertIdentity finds the account behind an external identity, creating both
// on first sight.
//
// The provider's subject is the key, never the email: providers allow email
// changes, and some reuse addresses. An account whose email changes upstream
// must remain the same account.
func (s *Store) UpsertIdentity(ctx context.Context, provider, subject, email string) (Account, Identity, error) {
	if provider == "" || subject == "" {
		return Account{}, Identity{}, errors.New("store: provider and subject are required")
	}

	var acct Account
	var ident Identity

	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			SELECT i.id, i.account_id, i.provider, i.subject, i.email
			  FROM identities i
			 WHERE i.provider = $1 AND i.subject = $2`,
			provider, subject,
		).Scan(&ident.ID, &ident.AccountID, &ident.Provider, &ident.Subject, &ident.Email)

		switch {
		case err == nil:
			// Refresh the stored email, which may have changed upstream.
			if ident.Email != email && email != "" {
				if _, err := tx.Exec(ctx,
					`UPDATE identities SET email = $1 WHERE id = $2`, email, ident.ID); err != nil {
					return err
				}
				ident.Email = email
			}

		case errors.Is(err, pgx.ErrNoRows):
			if err := tx.QueryRow(ctx,
				`INSERT INTO accounts DEFAULT VALUES RETURNING id, created_at`,
			).Scan(&acct.ID, &acct.CreatedAt); err != nil {
				return fmt.Errorf("creating account: %w", err)
			}

			if err := tx.QueryRow(ctx, `
				INSERT INTO identities (account_id, provider, subject, email)
				VALUES ($1, $2, $3, $4)
				RETURNING id, account_id, provider, subject, email`,
				acct.ID, provider, subject, email,
			).Scan(&ident.ID, &ident.AccountID, &ident.Provider, &ident.Subject, &ident.Email); err != nil {
				return fmt.Errorf("creating identity: %w", err)
			}

		default:
			return err
		}

		return tx.QueryRow(ctx, `
			UPDATE accounts SET last_login_at = now()
			 WHERE id = $1
			 RETURNING id, created_at, last_login_at, banned_until, notes`,
			ident.AccountID,
		).Scan(&acct.ID, &acct.CreatedAt, &acct.LastLoginAt, &acct.BannedUntil, &acct.Notes)
	})

	if err != nil {
		return Account{}, Identity{}, fmt.Errorf("store: upserting identity: %w", err)
	}
	return acct, ident, nil
}

// Allowed reports whether an identity may play.
//
// Checked at account creation and again at every login. Checking only at
// creation would let a revoked player keep access forever simply by having
// signed in once.
//
// An empty allowlist admits nobody. That is deliberate: the failure mode of
// "empty means open" is a server anyone can join, discovered after the fact.
func (s *Store) Allowed(ctx context.Context, provider, subject, email string) (bool, error) {
	domain := ""
	if at := strings.LastIndex(email, "@"); at >= 0 {
		domain = email[at+1:]
	}

	var allowed bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM allowlist
			 WHERE (provider = '' OR provider = $1)
			   AND (
			        (match_kind = 'subject'      AND match_value = $2)
			     OR (match_kind = 'email'        AND $3 <> '' AND lower(match_value) = lower($3))
			     OR (match_kind = 'email_domain' AND $4 <> '' AND lower(match_value) = lower($4))
			   )
		)`,
		provider, subject, email, domain,
	).Scan(&allowed)
	if err != nil {
		return false, fmt.Errorf("store: checking allowlist: %w", err)
	}
	return allowed, nil
}

// AllowlistEntry is one rule about who may play.
type AllowlistEntry struct {
	ID         uuid.UUID
	Provider   string
	MatchKind  string
	MatchValue string
	Note       string
	AddedAt    time.Time
}

// Allowlist match kinds.
const (
	MatchSubject     = "subject"
	MatchEmail       = "email"
	MatchEmailDomain = "email_domain"
)

// AddAllowlistEntry grants access. Re-adding an existing rule is not an error.
func (s *Store) AddAllowlistEntry(ctx context.Context, provider, kind, value, note string) error {
	switch kind {
	case MatchSubject, MatchEmail, MatchEmailDomain:
	default:
		return fmt.Errorf("store: unknown allowlist match kind %q", kind)
	}
	if value == "" {
		return errors.New("store: allowlist value cannot be empty")
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO allowlist (provider, match_kind, match_value, note)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (provider, match_kind, match_value) DO NOTHING`,
		provider, kind, value, note)
	if err != nil {
		return fmt.Errorf("store: adding allowlist entry: %w", err)
	}
	return nil
}

// RemoveAllowlistEntry revokes access. It takes effect at the revoked player's
// next login, since the allowlist is re-checked every time.
func (s *Store) RemoveAllowlistEntry(ctx context.Context, provider, kind, value string) error {
	_, err := s.pool.Exec(ctx, `
		DELETE FROM allowlist
		 WHERE provider = $1 AND match_kind = $2 AND match_value = $3`,
		provider, kind, value)
	if err != nil {
		return fmt.Errorf("store: removing allowlist entry: %w", err)
	}
	return nil
}

// ListAllowlist returns every rule, oldest first.
func (s *Store) ListAllowlist(ctx context.Context) ([]AllowlistEntry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, provider, match_kind::text, match_value, note, added_at
		  FROM allowlist ORDER BY added_at`)
	if err != nil {
		return nil, fmt.Errorf("store: listing allowlist: %w", err)
	}
	defer rows.Close()

	var out []AllowlistEntry
	for rows.Next() {
		var e AllowlistEntry
		if err := rows.Scan(&e.ID, &e.Provider, &e.MatchKind, &e.MatchValue, &e.Note, &e.AddedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// AllowlistSize reports how many rules exist, so the server can warn loudly
// when it starts with an empty one and nobody can log in.
func (s *Store) AllowlistSize(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM allowlist`).Scan(&n)
	return n, err
}
