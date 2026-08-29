-- Guilds.
--
-- Durable, unlike parties. A party is a group formed for an evening and losing
-- it costs a regroup; a guild is an identity people build over months, and
-- losing one to a restart would be unforgivable. That difference is the whole
-- reason parties live in Redis and this lives here.

CREATE TABLE guilds (
    id              uuid PRIMARY KEY,
    name            text NOT NULL,

    -- Names are unique case-insensitively, so "Wardens" and "wardens" cannot
    -- both exist. The normalised form is stored rather than computed in the
    -- index so the same function decides uniqueness and lookup.
    normalised_name text NOT NULL,

    motd            text NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),

    -- Disbanding is a soft delete: the roster is the record of who was in it,
    -- and hard-deleting a guild takes that with it.
    disbanded_at    timestamptz
);

CREATE UNIQUE INDEX guilds_name_unique
    ON guilds (normalised_name) WHERE disbanded_at IS NULL;

CREATE TABLE guild_members (
    guild_id     uuid NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    character_id uuid NOT NULL REFERENCES characters(id) ON DELETE CASCADE,

    -- 3 leader, 2 officer, 1 member. A number rather than a text enum because
    -- every permission check is "at least this rank", which is a comparison.
    rank         smallint NOT NULL,

    joined_at    timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (guild_id, character_id)
);

-- A character belongs to at most one guild, enforced rather than assumed:
-- two memberships would give one character two rosters and two guild chats.
CREATE UNIQUE INDEX guild_members_one_per_character ON guild_members (character_id);
