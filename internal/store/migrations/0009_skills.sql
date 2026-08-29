-- The skill bar.
--
-- character_skills already exists from 0002: what a character has learned and
-- at what rank. This adds the other half -- which of them are on the bar, in
-- which slot, and with which supports linked.
--
-- Two tables rather than one because they answer different questions and
-- change at different rates: learning is progression, and the bar is a loadout
-- somebody rearranges between fights.
--
-- The supports are a column rather than a table because they are an ordered
-- list of at most a handful of ids that is always read and written whole.
-- Normalising it would buy nothing and cost a join on every login.

CREATE TABLE character_skill_bar (
    character_id uuid NOT NULL REFERENCES characters(id) ON DELETE CASCADE,

    -- The slot a player presses. Bounded so a client cannot invent a
    -- thousandth key binding and make the bar unbounded.
    slot         int  NOT NULL,

    skill_id     text NOT NULL,

    -- Linked supports, in the order they apply. Order matters: supports
    -- transform an effect list, and two that both scale damage compose
    -- differently depending on which repeats first.
    supports     text[] NOT NULL DEFAULT '{}',

    PRIMARY KEY (character_id, slot),
    CONSTRAINT character_skill_bar_slot_range CHECK (slot >= 0 AND slot < 8)
);

-- 0002 created character_skills without it; a bar entry is looked up by
-- character on every login.
CREATE INDEX character_skills_character_idx ON character_skills (character_id);
