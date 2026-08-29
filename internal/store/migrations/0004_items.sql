-- Items, containers, and the audit journal.
--
-- Item duplication is the defining bug of every MMO that has ever shipped one.
-- The defences here are structural rather than procedural, because a rule
-- enforced in Go is a rule some future code path forgets:
--
--   * An item always has exactly one location. container_id is NOT NULL, so
--     there is no "in limbo" state for a code path to leave one in.
--   * UNIQUE (container_id, slot) means the database refuses to put two items
--     in one slot -- not an assertion that a new caller can skip.
--   * Moving an item is an UPDATE of its location, never DELETE then INSERT.
--     A delete-then-insert that fails between the two destroys an item, and a
--     retry of the insert alone duplicates one.
--
-- See docs/data-model.md.

CREATE TYPE container_kind AS ENUM (
    'inventory',
    'equipment',
    'bank',
    'trade',
    'mail'
);

CREATE TABLE containers (
    id   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind container_kind NOT NULL,

    -- Who the container belongs to. Character-owned for now; account-owned
    -- shared storage lands with banks.
    owner_type text NOT NULL,
    owner_id   uuid NOT NULL,

    capacity int NOT NULL,

    created_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT containers_capacity_positive CHECK (capacity > 0),

    -- One container of each kind per owner, so "the character's inventory" is
    -- unambiguous rather than a query that might return two.
    UNIQUE (owner_type, owner_id, kind)
);

CREATE INDEX containers_owner_idx ON containers (owner_type, owner_id);

CREATE TYPE item_rarity AS ENUM ('normal', 'magic', 'rare', 'unique');

CREATE TABLE item_instances (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    base_id    text NOT NULL,
    rarity     item_rarity NOT NULL DEFAULT 'normal',
    item_level int NOT NULL DEFAULT 1,

    -- The rolled modifiers, stored rather than re-derived. Deriving from a
    -- seed at load would mean a rebalance silently rewrites items already in
    -- players' stashes.
    mods jsonb NOT NULL DEFAULT '{}',

    stack_size int NOT NULL DEFAULT 1,

    -- NOT NULL: there is no location-less state for an item to get lost in.
    container_id uuid NOT NULL REFERENCES containers(id) ON DELETE CASCADE,
    slot         int  NOT NULL,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT item_stack_positive CHECK (stack_size > 0),
    CONSTRAINT item_slot_nonneg CHECK (slot >= 0),

    -- The database refuses to put two items in one slot.
    --
    -- Deferrable so a swap can be two plain updates inside one transaction,
    -- with uniqueness checked at commit. The alternative -- parking one item
    -- somewhere unreachable first -- needs a slot value no real container can
    -- hold, which means weakening the non-negative check that stops a bug
    -- writing an item to slot -1 for real.
    CONSTRAINT item_instances_location_unique
        UNIQUE (container_id, slot) DEFERRABLE INITIALLY IMMEDIATE
);

CREATE INDEX item_instances_container_idx ON item_instances (container_id);

-- Every movement of every item, append-only.
--
-- One insert per move, which is trivial next to the tick loop, and it buys
-- three things worth far more: duplication becomes detectable, support
-- requests become answerable, and a dupe exploit becomes reversible instead of
-- a server wipe.
--
-- Built now rather than after the first dupe, because retrofitting an audit
-- log once one has already happened means you cannot tell which items were
-- legitimate.
CREATE TABLE item_events (
    id      bigserial PRIMARY KEY,
    item_id uuid NOT NULL,

    -- create | pickup | move | equip | unequip | drop | vendor | destroy
    kind text NOT NULL,

    from_container uuid,
    from_slot      int,
    to_container   uuid,
    to_slot        int,

    actor_character_id uuid,

    -- The room tick, so an event can be lined up against a replay.
    tick bigint,

    at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX item_events_item_idx ON item_events (item_id, id);
CREATE INDEX item_events_actor_idx ON item_events (actor_character_id, at);
