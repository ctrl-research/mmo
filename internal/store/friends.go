package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// Friends.
//
// One-way and durable: the list is a record of who you want to keep track of,
// and whether they happen to be online right now comes from presence, which is
// not this package's business.

// ErrFriendLimit means the list is full.
var ErrFriendLimit = errors.New("store: friends list is full")

// MaxFriends bounds a list. High enough that nobody real reaches it, low
// enough that a script cannot make one character's list a denial of service on
// every login that reads it.
const MaxFriends = 200

// Friend is one entry, with enough to show a row without a second query.
type Friend struct {
	CharacterID uuid.UUID
	Name        string
	Level       int
}

// AddFriend puts a character on somebody's list.
func (s *Store) AddFriend(ctx context.Context, characterID, friendID uuid.UUID) error {
	var count int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM friends WHERE character_id = $1`, characterID).
		Scan(&count); err != nil {
		return fmt.Errorf("store: counting friends: %w", err)
	}
	if count >= MaxFriends {
		return ErrFriendLimit
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO friends (character_id, friend_id) VALUES ($1, $2)
		ON CONFLICT DO NOTHING`, characterID, friendID)
	if err != nil {
		return fmt.Errorf("store: adding a friend: %w", err)
	}
	return nil
}

// RemoveFriend takes a character off somebody's list.
func (s *Store) RemoveFriend(ctx context.Context, characterID, friendID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM friends WHERE character_id = $1 AND friend_id = $2`,
		characterID, friendID)
	if err != nil {
		return fmt.Errorf("store: removing a friend: %w", err)
	}
	return nil
}

// Friends returns a character's list, alphabetically.
func (s *Store) Friends(ctx context.Context, characterID uuid.UUID) ([]Friend, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.id, c.name, c.level
		  FROM friends f
		  JOIN characters c ON c.id = f.friend_id
		 WHERE f.character_id = $1
		   AND c.deleted_at IS NULL
		 ORDER BY lower(c.name)`, characterID)
	if err != nil {
		return nil, fmt.Errorf("store: reading friends: %w", err)
	}
	defer rows.Close()

	var out []Friend
	for rows.Next() {
		var f Friend
		if err := rows.Scan(&f.CharacterID, &f.Name, &f.Level); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
