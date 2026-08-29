-- Friends.
--
-- One-way, like OSRS: adding somebody puts them on your list and needs no
-- approval from them. A mutual model needs a request, an accept, a decline,
-- and a pending state, all so that two people can see each other's online
-- status -- which is what a list is for and what a block list is against.

CREATE TABLE friends (
    character_id uuid NOT NULL REFERENCES characters(id) ON DELETE CASCADE,
    friend_id    uuid NOT NULL REFERENCES characters(id) ON DELETE CASCADE,
    created_at   timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (character_id, friend_id),

    -- Adding yourself would show you online in your own list forever.
    CONSTRAINT friends_not_self CHECK (character_id <> friend_id)
);
