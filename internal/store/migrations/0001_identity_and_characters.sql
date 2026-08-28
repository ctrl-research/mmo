-- Identity and characters.
--
-- Migrations are forward-only. Down-migrations are written for a rollback that
-- almost never happens, are almost never tested, and give false confidence.
-- Schema changes follow expand -> backfill -> contract so a deploy is never
-- simultaneously required with a schema flip.

-- Accounts -------------------------------------------------------------------

CREATE TABLE accounts (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_login_at timestamptz,
    banned_until  timestamptz,
    notes         text NOT NULL DEFAULT ''
);

-- An account is reached through one or more external identities. Keeping them
-- separate means a player can add a second provider later without the account
-- being tied to whichever one they happened to use first.
CREATE TABLE identities (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    provider   text NOT NULL,
    subject    text NOT NULL,
    email      text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),

    -- The provider's subject is the stable identifier. Email is stored for
    -- display and for allowlisting, but is never the key: providers allow
    -- email changes, and some reuse addresses.
    UNIQUE (provider, subject)
);

CREATE INDEX identities_account_idx ON identities (account_id);

-- Allowlist ------------------------------------------------------------------

CREATE TYPE allowlist_match AS ENUM ('subject', 'email', 'email_domain');

-- Who may play. Checked at account creation and again at every login, because
-- an entry can be removed and a revoked player must not keep access simply by
-- having signed in once.
CREATE TABLE allowlist (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    provider    text NOT NULL DEFAULT '',  -- empty matches any provider
    match_kind  allowlist_match NOT NULL,
    match_value text NOT NULL,
    note        text NOT NULL DEFAULT '',
    added_at    timestamptz NOT NULL DEFAULT now(),

    UNIQUE (provider, match_kind, match_value)
);

-- Matching is case-insensitive: nobody expects an allowlist to care whether an
-- address was typed in capitals.
CREATE INDEX allowlist_lookup_idx ON allowlist (match_kind, lower(match_value));

-- Characters -----------------------------------------------------------------

CREATE TABLE characters (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,

    name     text NOT NULL,
    class_id text NOT NULL DEFAULT 'warrior',

    level int    NOT NULL DEFAULT 1,
    exp   bigint NOT NULL DEFAULT 0,
    gold  bigint NOT NULL DEFAULT 0,

    -- Where the character is, so logging back in resumes in place.
    map_id      text NOT NULL,
    spawn_point text NOT NULL DEFAULT '',

    -- Everything the simulation needs that has no column of its own: position,
    -- velocity, HP, MP, cooldowns. Deliberately opaque here -- the shape
    -- belongs to the simulation, and giving each field a column would mean a
    -- migration every time the body gains one.
    state jsonb NOT NULL DEFAULT '{}',

    -- Fencing token for the single-writer invariant. Every write carries the
    -- holder's token and is rejected if a higher one has been issued, which is
    -- what makes the guarantee hold even when a lease expires under a paused
    -- process. See docs/data-model.md.
    lease_token bigint NOT NULL DEFAULT 0,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz,

    CONSTRAINT characters_level_positive CHECK (level >= 1),
    CONSTRAINT characters_exp_nonneg CHECK (exp >= 0),
    CONSTRAINT characters_gold_nonneg CHECK (gold >= 0)
);

-- Names are unique case-insensitively among living characters. The partial
-- index frees a name when a character is deleted, which is what players
-- expect; the alternative reserves every name ever used, forever.
CREATE UNIQUE INDEX characters_name_unique
    ON characters (lower(name))
    WHERE deleted_at IS NULL;

CREATE INDEX characters_account_idx
    ON characters (account_id)
    WHERE deleted_at IS NULL;
