package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Guilds.
//
// Durable, unlike parties: a guild is an identity people build over months,
// and the roster survives everybody logging out. That is the whole reason this
// is in Postgres and party membership is not.
//
// Ranks are numbers rather than a text enum because every permission check is
// "at least this rank", which is a comparison and not a lookup table.

// uniqueViolation is Postgres's SQLSTATE for a unique index conflict. Named
// because the constraint is doing the work here: checking first and inserting
// after leaves a window where two simultaneous requests both succeed.
const uniqueViolation = "23505"

// Guild ranks.
const (
	RankMember  = 1
	RankOfficer = 2
	RankLeader  = 3
)

// Guild errors.
var (
	ErrGuildNameTaken = errors.New("store: that guild name is taken")
	ErrNotInGuild     = errors.New("store: not in a guild")
	ErrAlreadyInGuild = errors.New("store: already in a guild")
	ErrGuildRank      = errors.New("store: rank does not permit that")
)

// Guild is one guild and its motd.
type Guild struct {
	ID   uuid.UUID
	Name string
	MOTD string
}

// GuildMember is one character on a roster.
type GuildMember struct {
	CharacterID uuid.UUID
	Name        string
	Rank        int
	Level       int
}

// RankName turns a rank into something to show.
func RankName(rank int) string {
	switch rank {
	case RankLeader:
		return "leader"
	case RankOfficer:
		return "officer"
	default:
		return "member"
	}
}

// NormaliseGuildName is the uniqueness and lookup form of a guild name.
func NormaliseGuildName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// CreateGuild founds a guild with one member, who becomes its leader.
func (s *Store) CreateGuild(ctx context.Context, founder uuid.UUID, name string) (Guild, error) {
	name = strings.TrimSpace(name)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Guild{}, fmt.Errorf("store: creating guild: %w", err)
	}
	defer tx.Rollback(ctx)

	g := Guild{ID: uuid.New(), Name: name}

	_, err = tx.Exec(ctx,
		`INSERT INTO guilds (id, name, normalised_name) VALUES ($1, $2, $3)`,
		g.ID, g.Name, NormaliseGuildName(name))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			return Guild{}, ErrGuildNameTaken
		}
		return Guild{}, fmt.Errorf("store: creating guild: %w", err)
	}

	// The unique index on character_id is what makes "one guild per character"
	// true rather than intended: checking first and inserting after leaves a
	// window where two simultaneous creates both succeed.
	_, err = tx.Exec(ctx,
		`INSERT INTO guild_members (guild_id, character_id, rank) VALUES ($1, $2, $3)`,
		g.ID, founder, RankLeader)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			return Guild{}, ErrAlreadyInGuild
		}
		return Guild{}, fmt.Errorf("store: adding the founder: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Guild{}, fmt.Errorf("store: creating guild: %w", err)
	}
	return g, nil
}

// GuildOf returns the guild a character belongs to, and their rank in it.
func (s *Store) GuildOf(ctx context.Context, characterID uuid.UUID) (Guild, int, error) {
	var g Guild
	var rank int

	err := s.pool.QueryRow(ctx, `
		SELECT g.id, g.name, g.motd, m.rank
		  FROM guild_members m
		  JOIN guilds g ON g.id = m.guild_id
		 WHERE m.character_id = $1
		   AND g.disbanded_at IS NULL`, characterID).
		Scan(&g.ID, &g.Name, &g.MOTD, &rank)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Guild{}, 0, ErrNotInGuild
	case err != nil:
		return Guild{}, 0, fmt.Errorf("store: reading guild: %w", err)
	}
	return g, rank, nil
}

// GuildRoster returns everybody in a guild, highest rank first.
func (s *Store) GuildRoster(ctx context.Context, guildID uuid.UUID) ([]GuildMember, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT m.character_id, c.name, m.rank, c.level
		  FROM guild_members m
		  JOIN characters c ON c.id = m.character_id
		 WHERE m.guild_id = $1
		   AND c.deleted_at IS NULL
		 ORDER BY m.rank DESC, lower(c.name)`, guildID)
	if err != nil {
		return nil, fmt.Errorf("store: reading roster: %w", err)
	}
	defer rows.Close()

	var out []GuildMember
	for rows.Next() {
		var m GuildMember
		if err := rows.Scan(&m.CharacterID, &m.Name, &m.Rank, &m.Level); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// AddGuildMember puts a character on a roster.
func (s *Store) AddGuildMember(ctx context.Context, guildID, characterID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO guild_members (guild_id, character_id, rank) VALUES ($1, $2, $3)`,
		guildID, characterID, RankMember)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			return ErrAlreadyInGuild
		}
		return fmt.Errorf("store: adding a guild member: %w", err)
	}
	return nil
}

// RemoveGuildMember takes a character off a roster, reporting whether the
// guild still has anybody in it.
func (s *Store) RemoveGuildMember(ctx context.Context, guildID, characterID uuid.UUID) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: removing a guild member: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`DELETE FROM guild_members WHERE guild_id = $1 AND character_id = $2`,
		guildID, characterID); err != nil {
		return 0, fmt.Errorf("store: removing a guild member: %w", err)
	}

	var remaining int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM guild_members WHERE guild_id = $1`, guildID).
		Scan(&remaining); err != nil {
		return 0, fmt.Errorf("store: counting the roster: %w", err)
	}

	// An empty guild is disbanded rather than left to sit as a name nobody can
	// claim: the roster is gone, so there is nothing left to preserve.
	if remaining == 0 {
		if _, err := tx.Exec(ctx,
			`UPDATE guilds SET disbanded_at = now() WHERE id = $1`, guildID); err != nil {
			return 0, fmt.Errorf("store: disbanding an empty guild: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("store: removing a guild member: %w", err)
	}
	return remaining, nil
}

// SetGuildRank changes a member's rank.
func (s *Store) SetGuildRank(ctx context.Context, guildID, characterID uuid.UUID, rank int) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE guild_members SET rank = $1 WHERE guild_id = $2 AND character_id = $3`,
		rank, guildID, characterID)
	if err != nil {
		return fmt.Errorf("store: setting a guild rank: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotInGuild
	}
	return nil
}

// SetGuildMOTD replaces the message of the day.
func (s *Store) SetGuildMOTD(ctx context.Context, guildID uuid.UUID, motd string) error {
	_, err := s.pool.Exec(ctx, `UPDATE guilds SET motd = $1 WHERE id = $2`, motd, guildID)
	if err != nil {
		return fmt.Errorf("store: setting the motd: %w", err)
	}
	return nil
}

// DisbandGuild marks a guild gone, keeping the roster as a record of who was
// in it.
func (s *Store) DisbandGuild(ctx context.Context, guildID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE guilds SET disbanded_at = now() WHERE id = $1`, guildID)
	if err != nil {
		return fmt.Errorf("store: disbanding guild: %w", err)
	}
	return nil
}
