-- Allocated passive nodes.
--
-- A row per node rather than an array on the character, because the questions
-- asked of it are per-node -- "is this allocated", "how many points are spent"
-- -- and because the primary key is what makes allocating the same node twice
-- impossible rather than merely unlikely.
--
-- Refunding deletes. There is no history worth keeping here: a respec is a
-- decision about the future, not a record of the past, and a table that only
-- grows would grow every time somebody experimented.

CREATE TABLE character_passives (
    character_id uuid NOT NULL REFERENCES characters(id) ON DELETE CASCADE,
    node_id      int  NOT NULL,
    allocated_at timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (character_id, node_id)
);

CREATE INDEX character_passives_character_idx ON character_passives (character_id);
