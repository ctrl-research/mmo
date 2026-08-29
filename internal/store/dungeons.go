package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Dungeon lockouts.
//
// A lockout is a promise about the future -- "not before this time" -- so it
// is stored as the moment it expires rather than as the moment it was earned.
// Storing the clear and adding the duration at read time would mean every
// change to a dungeon's lockout silently rewriting history for everyone who
// had already cleared it.

// RecordClear writes a character's lockout for a dungeon, replacing any
// existing one.
func (s *Store) RecordClear(ctx context.Context, characterID uuid.UUID, dungeonID string, lockout time.Duration) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO dungeon_lockouts (character_id, dungeon_id, cleared_at, expires_at)
		 VALUES ($1, $2, now(), now() + $3::interval)
		 ON CONFLICT (character_id, dungeon_id) DO UPDATE
		 SET cleared_at = now(), expires_at = EXCLUDED.expires_at`,
		characterID, dungeonID, lockout)
	if err != nil {
		return fmt.Errorf("store: recording dungeon clear: %w", err)
	}
	return nil
}

// LockedOutUntil reports when a character may next enter a dungeon.
//
// A zero time means they may enter now, which covers both "never cleared it"
// and "cleared it long enough ago" -- the caller has no reason to tell those
// apart, and giving it the chance to would invite it to.
func (s *Store) LockedOutUntil(ctx context.Context, characterID uuid.UUID, dungeonID string) (time.Time, error) {
	var expires time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT expires_at FROM dungeon_lockouts
		 WHERE character_id = $1 AND dungeon_id = $2 AND expires_at > now()`,
		characterID, dungeonID).Scan(&expires)

	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("store: reading dungeon lockout: %w", err)
	}
	return expires, nil
}
