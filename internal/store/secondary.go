package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// Secondary skill experience: woodcutting, mining, fishing, herbalism.
//
// Its own table rather than more jsonb on the character row, because these are
// aggregated: "what is the highest woodcutting level on the server" is a
// question the database should be able to answer without unpacking a document
// per character. The table has existed unused since migration 0002 for exactly
// this milestone.

// SecondaryExp returns a character's cumulative experience per skill.
func (s *Store) SecondaryExp(ctx context.Context, characterID uuid.UUID) (map[string]int64, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT skill_id, exp FROM secondary_skills WHERE character_id = $1`,
		characterID)
	if err != nil {
		return nil, fmt.Errorf("store: loading secondary skills: %w", err)
	}
	defer rows.Close()

	out := make(map[string]int64)
	for rows.Next() {
		var (
			id  string
			exp int64
		)
		if err := rows.Scan(&id, &exp); err != nil {
			return nil, err
		}
		out[id] = exp
	}
	return out, rows.Err()
}

// SaveSecondaryExp writes a character's cumulative experience per skill.
//
// GREATEST rather than a plain assignment, so a checkpoint can never move a
// skill backwards. Two sessions for one character should be impossible -- the
// ownership lease exists to make it so -- but a stale checkpoint arriving after
// a fresher one is not impossible at all, and "my woodcutting went down" is
// the one bug a levelling system must never have.
//
// One statement for the whole set: a player who has been gathering all evening
// has a handful of skills, and a round trip each would put per-skill latency on
// a path the checkpoint interval already accounts for once.
func (s *Store) SaveSecondaryExp(ctx context.Context, characterID uuid.UUID, exp map[string]int64) error {
	if len(exp) == 0 {
		return nil
	}

	skills := make([]string, 0, len(exp))
	amounts := make([]int64, 0, len(exp))
	for id, amount := range exp {
		if amount <= 0 {
			// Nothing to record. A skill at zero is indistinguishable from one
			// nobody has touched, and a row saying so is a row to migrate
			// later for no reason.
			continue
		}
		skills = append(skills, id)
		amounts = append(amounts, amount)
	}
	if len(skills) == 0 {
		return nil
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO secondary_skills (character_id, skill_id, exp)
		SELECT $1, skill, amount
		FROM unnest($2::text[], $3::bigint[]) AS t(skill, amount)
		ON CONFLICT (character_id, skill_id)
		DO UPDATE SET exp = GREATEST(secondary_skills.exp, EXCLUDED.exp)`,
		characterID, skills, amounts)
	if err != nil {
		return fmt.Errorf("store: saving secondary skills: %w", err)
	}
	return nil
}
