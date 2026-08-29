package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// Allocated passive nodes.
//
// The set is small -- a character at the level cap holds a couple of hundred
// nodes -- and it is always read whole, because allocation rules are about the
// shape of what is held rather than about individual nodes.

// AllocatedPassives returns the nodes a character holds.
func (s *Store) AllocatedPassives(ctx context.Context, characterID uuid.UUID) ([]int, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT node_id FROM character_passives WHERE character_id = $1 ORDER BY node_id`,
		characterID)
	if err != nil {
		return nil, fmt.Errorf("store: reading passives: %w", err)
	}
	defer rows.Close()

	var out []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// AllocatePassive records a node, reporting whether it was newly taken.
//
// The primary key is what makes double-allocation impossible: checking first
// and inserting after leaves a window where two requests both see the node
// free and both spend a point.
func (s *Store) AllocatePassive(ctx context.Context, characterID uuid.UUID, nodeID int) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO character_passives (character_id, node_id) VALUES ($1, $2)
		ON CONFLICT DO NOTHING`, characterID, nodeID)
	if err != nil {
		return false, fmt.Errorf("store: allocating passive: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// RefundPassives removes nodes, returning how many were actually held.
//
// Several at once because that is how a respec works: the rule is about the
// shape of what remains, so removing them one at a time would pass through
// states that are not allowed.
func (s *Store) RefundPassives(ctx context.Context, characterID uuid.UUID, nodeIDs []int) (int, error) {
	if len(nodeIDs) == 0 {
		return 0, nil
	}

	tag, err := s.pool.Exec(ctx,
		`DELETE FROM character_passives WHERE character_id = $1 AND node_id = ANY($2)`,
		characterID, nodeIDs)
	if err != nil {
		return 0, fmt.Errorf("store: refunding passives: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// RefundAllPassives clears a character's tree.
func (s *Store) RefundAllPassives(ctx context.Context, characterID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM character_passives WHERE character_id = $1`, characterID)
	if err != nil {
		return fmt.Errorf("store: clearing passives: %w", err)
	}
	return nil
}
