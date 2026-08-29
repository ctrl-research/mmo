package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Chat mutes.
//
// Append-only, like the item journal and for the same reason: a mute that is
// deleted when lifted leaves no answer to "was this player ever muted, and
// what for". Unmuting writes a new row with an expiry in the past.

// Mute is one moderation action against a character.
type Mute struct {
	CharacterID uuid.UUID

	// ExpiresAt is nil for an indefinite mute.
	ExpiresAt *time.Time

	Reason    string
	CreatedBy string
	CreatedAt time.Time
}

// Active reports whether the mute is in force at a given time.
func (m Mute) Active(now time.Time) bool {
	return m.ExpiresAt == nil || m.ExpiresAt.After(now)
}

// MuteCharacter records a mute. A nil expiry means indefinite.
func (s *Store) MuteCharacter(ctx context.Context, characterID uuid.UUID,
	expires *time.Time, reason, by string,
) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO chat_mutes (character_id, expires_at, reason, created_by)
		VALUES ($1, $2, $3, $4)`,
		characterID, expires, reason, by)
	if err != nil {
		return fmt.Errorf("store: muting character: %w", err)
	}
	return nil
}

// UnmuteCharacter lifts a mute by recording one that has already expired.
//
// A new row rather than a delete, so the history of what was done to an
// account survives the decision to undo it.
func (s *Store) UnmuteCharacter(ctx context.Context, characterID uuid.UUID, by string) error {
	lifted := time.Now().Add(-time.Second)
	return s.MuteCharacter(ctx, characterID, &lifted, "lifted", by)
}

// ActiveMute returns the mute currently in force for a character, if any.
//
// Only the most recent row matters: a later mute supersedes an earlier one,
// and lifting writes a later row with an expiry in the past.
func (s *Store) ActiveMute(ctx context.Context, characterID uuid.UUID) (Mute, bool, error) {
	var m Mute
	m.CharacterID = characterID

	err := s.pool.QueryRow(ctx, `
		SELECT expires_at, reason, created_by, created_at
		  FROM chat_mutes
		 WHERE character_id = $1
		 ORDER BY created_at DESC, id DESC
		 LIMIT 1`, characterID).
		Scan(&m.ExpiresAt, &m.Reason, &m.CreatedBy, &m.CreatedAt)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Mute{}, false, nil
	case err != nil:
		return Mute{}, false, fmt.Errorf("store: reading mute: %w", err)
	}

	if !m.Active(time.Now()) {
		return Mute{}, false, nil
	}
	return m, true, nil
}
