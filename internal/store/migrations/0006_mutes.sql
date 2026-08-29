-- Chat mutes.
--
-- One row per mute rather than a flag on the character, so a mute has a reason
-- and an expiry, and lifting one leaves a record that it happened. A boolean
-- column answers "is this player muted" and nothing else -- not who did it,
-- not why, and not whether it was meant to be permanent.

CREATE TABLE chat_mutes (
    id           bigserial PRIMARY KEY,
    character_id uuid NOT NULL REFERENCES characters(id) ON DELETE CASCADE,

    -- NULL means indefinite. An expiry in the past is a lifted mute, which is
    -- how unmuting works: no row is ever deleted.
    expires_at   timestamptz,

    reason       text NOT NULL DEFAULT '',
    created_by   text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- The lookup on every chat message: the most recent mute for a character.
CREATE INDEX chat_mutes_character_idx ON chat_mutes (character_id, created_at DESC);
