package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// What a character can cast.
//
// Learned skills and the bar are separate because they answer different
// questions and change at different rates: learning is progression, and the
// bar is a loadout somebody rearranges between fights.

// MaxSkillSlots is how many skills fit on the bar. Bounded here as well as in
// the schema, so a client cannot invent a thousandth key binding.
const MaxSkillSlots = 8

// LearnedSkill is one skill a character knows.
type LearnedSkill struct {
	SkillID string
	Rank    int
}

// BarSlot is one entry on the skill bar.
type BarSlot struct {
	Slot     int
	SkillID  string
	Supports []string
}

// LearnSkills grants several skills at once, raising the rank of any already
// known.
//
// One statement rather than a loop, because this runs on a character's first
// login and a round trip per starting skill is a round trip per starting skill
// on the path a player is waiting on.
func (s *Store) LearnSkills(ctx context.Context, characterID uuid.UUID, skillIDs []string) error {
	if len(skillIDs) == 0 {
		return nil
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO character_skills (character_id, skill_id, rank)
		SELECT $1, skill_id, 1 FROM unnest($2::text[]) AS skill_id
		ON CONFLICT (character_id, skill_id)
		DO UPDATE SET rank = GREATEST(character_skills.rank, EXCLUDED.rank)`,
		characterID, skillIDs)
	if err != nil {
		return fmt.Errorf("store: learning skills: %w", err)
	}
	return nil
}

// SeedSkillBar fills empty slots from a list of skills, in order.
//
// One statement, for the same reason: this is the first-login path, and eight
// round trips to lay out a bar is eight round trips a player waits through.
// Existing slots are left alone, so it cannot overwrite a loadout.
func (s *Store) SeedSkillBar(ctx context.Context, characterID uuid.UUID, skillIDs []string) error {
	if len(skillIDs) == 0 {
		return nil
	}
	if len(skillIDs) > MaxSkillSlots {
		skillIDs = skillIDs[:MaxSkillSlots]
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO character_skill_bar (character_id, slot, skill_id)
		SELECT $1, ordinality - 1, skill_id
		  FROM unnest($2::text[]) WITH ORDINALITY AS t(skill_id, ordinality)
		ON CONFLICT (character_id, slot) DO NOTHING`,
		characterID, skillIDs)
	if err != nil {
		return fmt.Errorf("store: seeding the skill bar: %w", err)
	}
	return nil
}

// LearnSkill grants a skill, or raises its rank if already known.
func (s *Store) LearnSkill(ctx context.Context, characterID uuid.UUID, skillID string, rank int) error {
	if rank <= 0 {
		rank = 1
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO character_skills (character_id, skill_id, rank)
		VALUES ($1, $2, $3)
		ON CONFLICT (character_id, skill_id)
		DO UPDATE SET rank = GREATEST(character_skills.rank, EXCLUDED.rank)`,
		characterID, skillID, rank)
	if err != nil {
		return fmt.Errorf("store: learning skill: %w", err)
	}
	return nil
}

// LearnedSkills returns everything a character knows.
func (s *Store) LearnedSkills(ctx context.Context, characterID uuid.UUID) ([]LearnedSkill, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT skill_id, rank FROM character_skills WHERE character_id = $1 ORDER BY skill_id`,
		characterID)
	if err != nil {
		return nil, fmt.Errorf("store: reading skills: %w", err)
	}
	defer rows.Close()

	var out []LearnedSkill
	for rows.Next() {
		var l LearnedSkill
		if err := rows.Scan(&l.SkillID, &l.Rank); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// SkillBar returns a character's loadout, ordered by slot.
func (s *Store) SkillBar(ctx context.Context, characterID uuid.UUID) ([]BarSlot, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT slot, skill_id, supports FROM character_skill_bar
		  WHERE character_id = $1 ORDER BY slot`, characterID)
	if err != nil {
		return nil, fmt.Errorf("store: reading skill bar: %w", err)
	}
	defer rows.Close()

	var out []BarSlot
	for rows.Next() {
		var b BarSlot
		if err := rows.Scan(&b.Slot, &b.SkillID, &b.Supports); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// SetBarSlot puts a skill and its supports in a slot, or clears it.
//
// Written whole rather than patched: the supports are an ordered list that is
// always read and written together, and a partial update would leave a slot
// holding a skill with somebody else's supports attached.
func (s *Store) SetBarSlot(ctx context.Context, characterID uuid.UUID, slot int, skillID string, supports []string) error {
	if slot < 0 || slot >= MaxSkillSlots {
		return fmt.Errorf("store: slot %d is outside the bar", slot)
	}

	if skillID == "" {
		_, err := s.pool.Exec(ctx,
			`DELETE FROM character_skill_bar WHERE character_id = $1 AND slot = $2`,
			characterID, slot)
		if err != nil {
			return fmt.Errorf("store: clearing bar slot: %w", err)
		}
		return nil
	}

	if supports == nil {
		supports = []string{}
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO character_skill_bar (character_id, slot, skill_id, supports)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (character_id, slot)
		DO UPDATE SET skill_id = EXCLUDED.skill_id, supports = EXCLUDED.supports`,
		characterID, slot, skillID, supports)
	if err != nil {
		return fmt.Errorf("store: setting bar slot: %w", err)
	}
	return nil
}
