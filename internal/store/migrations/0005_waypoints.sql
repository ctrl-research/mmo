-- Fast-travel destinations a character has unlocked.
--
-- Unlocked by visiting rather than granted, so the world map fills in as a
-- record of where someone has actually been.

CREATE TABLE waypoints (
    character_id uuid NOT NULL REFERENCES characters(id) ON DELETE CASCADE,
    waypoint_id  text NOT NULL,
    unlocked_at  timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (character_id, waypoint_id)
);
