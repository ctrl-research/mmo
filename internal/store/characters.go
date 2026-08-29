package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Character is a persisted character.
type Character struct {
	ID        uuid.UUID
	AccountID uuid.UUID

	Name    string
	ClassID string

	Level int
	Exp   int64
	Gold  int64

	MapID      string
	SpawnPoint string

	// State is everything the simulation needs that has no column: position,
	// velocity, HP, MP, cooldowns. Opaque here on purpose -- its shape belongs
	// to the simulation, and a column per field would mean a migration every
	// time a body gains one.
	State json.RawMessage

	// LeaseToken is the fencing token of the writer that last owned this
	// character.
	LeaseToken int64

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Character name rules.
const (
	MinNameLen = 3
	MaxNameLen = 16
)

// ValidateName reports whether a name is acceptable.
//
// Deliberately narrow: letters and digits only, starting with a letter. A
// permissive ruleset invites names that are indistinguishable from each other
// or from system messages, and tightening it later means renaming real
// characters.
func ValidateName(name string) error {
	if n := len([]rune(name)); n < MinNameLen || n > MaxNameLen {
		return fmt.Errorf("name must be %d to %d characters", MinNameLen, MaxNameLen)
	}
	for i, r := range name {
		switch {
		case unicode.IsLetter(r):
		case unicode.IsDigit(r) && i > 0:
		default:
			if i == 0 {
				return errors.New("name must start with a letter")
			}
			return errors.New("name may contain only letters and digits")
		}
	}
	return nil
}

// CreateCharacter creates a character for an account.
func (s *Store) CreateCharacter(ctx context.Context, accountID uuid.UUID, name, classID, mapID string) (Character, error) {
	if err := ValidateName(name); err != nil {
		return Character{}, err
	}

	var c Character
	err := s.pool.QueryRow(ctx, `
		INSERT INTO characters (account_id, name, class_id, map_id, state)
		VALUES ($1, $2, $3, $4, '{}'::jsonb)
		RETURNING id, account_id, name, class_id, level, exp, gold,
		          map_id, spawn_point, state, lease_token, created_at, updated_at`,
		accountID, name, classID, mapID,
	).Scan(&c.ID, &c.AccountID, &c.Name, &c.ClassID, &c.Level, &c.Exp, &c.Gold,
		&c.MapID, &c.SpawnPoint, &c.State, &c.LeaseToken, &c.CreatedAt, &c.UpdatedAt)

	if err != nil {
		// The unique index is the authority on name collisions, not a prior
		// SELECT: two simultaneous creates would both pass a check-then-insert.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Character{}, ErrNameTaken
		}
		return Character{}, fmt.Errorf("store: creating character: %w", err)
	}
	return c, nil
}

// ListCharacters returns an account's living characters, newest first.
func (s *Store) ListCharacters(ctx context.Context, accountID uuid.UUID) ([]Character, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, account_id, name, class_id, level, exp, gold,
		       map_id, spawn_point, state, lease_token, created_at, updated_at
		  FROM characters
		 WHERE account_id = $1 AND deleted_at IS NULL
		 ORDER BY created_at DESC`, accountID)
	if err != nil {
		return nil, fmt.Errorf("store: listing characters: %w", err)
	}
	defer rows.Close()

	var out []Character
	for rows.Next() {
		var c Character
		if err := rows.Scan(&c.ID, &c.AccountID, &c.Name, &c.ClassID, &c.Level, &c.Exp, &c.Gold,
			&c.MapID, &c.SpawnPoint, &c.State, &c.LeaseToken, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// LoadCharacter reads one character, verifying it belongs to the account.
//
// Ownership is part of the query rather than a check afterwards, so a caller
// cannot forget it and load someone else's character by ID.
func (s *Store) LoadCharacter(ctx context.Context, accountID, characterID uuid.UUID) (Character, error) {
	var c Character
	err := s.pool.QueryRow(ctx, `
		SELECT id, account_id, name, class_id, level, exp, gold,
		       map_id, spawn_point, state, lease_token, created_at, updated_at
		  FROM characters
		 WHERE id = $1 AND account_id = $2 AND deleted_at IS NULL`,
		characterID, accountID,
	).Scan(&c.ID, &c.AccountID, &c.Name, &c.ClassID, &c.Level, &c.Exp, &c.Gold,
		&c.MapID, &c.SpawnPoint, &c.State, &c.LeaseToken, &c.CreatedAt, &c.UpdatedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		return Character{}, ErrNotFound
	}
	if err != nil {
		return Character{}, fmt.Errorf("store: loading character: %w", err)
	}
	return c, nil
}

// DeleteCharacter soft-deletes a character, freeing its name for reuse.
func (s *Store) DeleteCharacter(ctx context.Context, accountID, characterID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE characters SET deleted_at = now()
		 WHERE id = $1 AND account_id = $2 AND deleted_at IS NULL`,
		characterID, accountID)
	if err != nil {
		return fmt.Errorf("store: deleting character: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Checkpoint writes a character's live state back, fenced by a lease token.
//
// This is the guarantee the whole single-writer invariant rests on. Redis
// provides mutual exclusion *most* of the time; a process paused long enough
// by GC or a partition can wake believing it still holds a lease that has
// since been granted elsewhere. The `lease_token <= $token` predicate is what
// makes correctness hold anyway: the stale writer's token is lower, so its
// write affects no rows and is rejected.
//
// A rejection is never routine. It means this process lost ownership, and the
// only safe response is to discard the in-memory copy -- never to retry, which
// would be attempting to overwrite a newer owner's work.
func (s *Store) Checkpoint(ctx context.Context, c Character, token int64) error {
	state := c.State
	if len(state) == 0 {
		state = json.RawMessage("{}")
	}

	tag, err := s.pool.Exec(ctx, `
		UPDATE characters
		   SET level = $1, exp = $2, gold = $3,
		       map_id = $4, spawn_point = $5, state = $6,
		       lease_token = $7, updated_at = now()
		 WHERE id = $8
		   AND deleted_at IS NULL
		   AND lease_token <= $7`,
		c.Level, c.Exp, c.Gold, c.MapID, c.SpawnPoint, state, token, c.ID)
	if err != nil {
		return fmt.Errorf("store: checkpointing character: %w", err)
	}

	if tag.RowsAffected() == 0 {
		// Distinguish "someone else owns this now" from "this character does
		// not exist", because they call for different responses: the first
		// means drop the session, the second means a bug in the caller.
		var exists bool
		if err := s.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM characters WHERE id = $1 AND deleted_at IS NULL)`,
			c.ID).Scan(&exists); err != nil {
			return fmt.Errorf("store: checkpointing character: %w", err)
		}
		if !exists {
			return ErrNotFound
		}
		return ErrStaleWrite
	}
	return nil
}

// NameAvailable reports whether a name is free.
//
// Advisory only, for showing a message before a player commits to a name. The
// unique index remains the authority: two simultaneous creates would both see
// a free name here.
func (s *Store) NameAvailable(ctx context.Context, name string) (bool, error) {
	var taken bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM characters
			 WHERE lower(name) = lower($1) AND deleted_at IS NULL
		)`, strings.TrimSpace(name)).Scan(&taken)
	if err != nil {
		return false, fmt.Errorf("store: checking name: %w", err)
	}
	return !taken, nil
}

// CountCharacters returns how many living characters an account has, so a
// limit can be enforced before creating another.
func (s *Store) CountCharacters(ctx context.Context, accountID uuid.UUID) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM characters WHERE account_id = $1 AND deleted_at IS NULL`,
		accountID).Scan(&n)
	return n, err
}
