package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// Waypoints a character has unlocked by visiting them.
//
// Unlocked by visiting rather than granted, so the world map fills in as a
// record of where someone has actually been -- which makes it worth looking at
// rather than a list of everything that exists.

// UnlockWaypoint records that a character has reached a waypoint.
//
// Idempotent, since a character can walk over one repeatedly and the room only
// suppresses repeats for the current session.
func (s *Store) UnlockWaypoint(ctx context.Context, characterID uuid.UUID, waypointID string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO waypoints (character_id, waypoint_id)
		VALUES ($1, $2)
		ON CONFLICT (character_id, waypoint_id) DO NOTHING`,
		characterID, waypointID)
	if err != nil {
		return fmt.Errorf("store: unlocking waypoint: %w", err)
	}
	return nil
}

// CharacterWaypoints returns the waypoints a character has unlocked.
func (s *Store) CharacterWaypoints(ctx context.Context, characterID uuid.UUID) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT waypoint_id FROM waypoints WHERE character_id = $1 ORDER BY unlocked_at`,
		characterID)
	if err != nil {
		return nil, fmt.Errorf("store: loading waypoints: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
