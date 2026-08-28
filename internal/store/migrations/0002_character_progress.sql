-- Per-character progression that does not belong in the character row.
--
-- Separate tables rather than more jsonb, because these are queried and
-- aggregated: "which passives are allocated" and "what is my woodcutting
-- level" are questions the database should be able to answer.

CREATE TABLE character_skills (
    character_id uuid NOT NULL REFERENCES characters(id) ON DELETE CASCADE,
    skill_id     text NOT NULL,
    rank         int  NOT NULL DEFAULT 1,

    PRIMARY KEY (character_id, skill_id),
    CONSTRAINT character_skills_rank_positive CHECK (rank >= 1)
);

-- Secondary skills level from use rather than from combat experience, on the
-- OSRS curve. Unused until M8; the table exists now so that milestone is
-- content and logic rather than a migration on live data.
CREATE TABLE secondary_skills (
    character_id uuid   NOT NULL REFERENCES characters(id) ON DELETE CASCADE,
    skill_id     text   NOT NULL,
    exp          bigint NOT NULL DEFAULT 0,

    PRIMARY KEY (character_id, skill_id),
    CONSTRAINT secondary_skills_exp_nonneg CHECK (exp >= 0)
);
