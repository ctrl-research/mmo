-- Dungeon lockouts.
--
-- One row per character per dungeon, holding when they may next enter. Per
-- character rather than per party, so a group carrying a friend through does
-- not spend the friend's lockout, and so leaving a party cannot launder one.
--
-- The row is replaced on each clear rather than appended to: what matters is
-- when the next attempt is allowed, and a history of every clear a character
-- has ever managed is a different table with a different purpose.
CREATE TABLE dungeon_lockouts (
    character_id UUID        NOT NULL REFERENCES characters (id) ON DELETE CASCADE,
    dungeon_id   TEXT        NOT NULL,
    cleared_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL,

    PRIMARY KEY (character_id, dungeon_id)
);

-- Every check is "is this character locked out of anything right now", asked
-- once at the entrance.
CREATE INDEX dungeon_lockouts_expiry_idx
    ON dungeon_lockouts (character_id, expires_at);
