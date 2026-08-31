package content

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/BurntSushi/toml"
)

// Zone events.
//
// An event is a stretch of time during which a map is a different place: an
// extra wave of things to fight, sometimes with a mini-boss at the head of it.
// It is the answer to a zone being finished once it is cleared -- nothing
// respawns into a *situation*, and an event is a situation.
//
// Two ways to start one, because they answer different questions. A timed
// event is the world doing something on its own, and is what makes standing in
// a zone worth doing. A shrine is the player choosing to start something, and
// is what makes walking past one a decision.

// EventTrigger is how an event begins.
type EventTrigger string

const (
	// TriggerTimer starts on a period, by itself.
	TriggerTimer EventTrigger = "timer"

	// TriggerShrine starts when a player touches the shrine named by the
	// event. Nothing happens until somebody chooses to.
	TriggerShrine EventTrigger = "shrine"
)

var validEventTriggers = map[EventTrigger]bool{
	TriggerTimer:  true,
	TriggerShrine: true,
}

// Event is one zone event.
type Event struct {
	ID   string
	Name string

	// Map is the map it happens on.
	Map string

	Trigger EventTrigger

	// Announce is what the room is told when it starts. An event nobody
	// noticed starting is an event nobody takes part in.
	Announce string

	// Shrine names the map's shrine object, for a shrine-triggered event.
	Shrine string

	// EveryTicks is how long a timed event waits between runs, measured from
	// the end of the last one.
	//
	// One knob per trigger, deliberately. A timed event has a period and a
	// shrine has a cooldown, and giving both to both made "when does this
	// start again" have two answers -- which is one of them being wrong.
	EveryTicks int

	// CooldownTicks is how long after a shrine event ends before the shrine
	// works again. Without it a shrine is a button somebody stands next to and
	// presses forever.
	CooldownTicks int

	// DurationTicks bounds a run. An event that only ended when its mobs were
	// dead would be permanent the first time a party gave up on one.
	DurationTicks int

	// Spawns names the map's mob spawn points that belong to this event. They
	// produce nothing at any other time.
	Spawns []string
}

type eventsFile struct {
	Event map[string]struct {
		Name       string   `toml:"name"`
		Map        string   `toml:"map"`
		Trigger    string   `toml:"trigger"`
		Announce   string   `toml:"announce"`
		Shrine     string   `toml:"shrine"`
		EveryMs    int      `toml:"every_ms"`
		CooldownMs int      `toml:"cooldown_ms"`
		DurationMs int      `toml:"duration_ms"`
		Spawns     []string `toml:"spawns"`
	} `toml:"event"`
}

func (c *Content) loadEvents(fsys fs.FS, rec *hashRecorder) error {
	files, err := listFiles(fsys, "events", ".toml")
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}

	for _, name := range files {
		data, err := rec.readAndRecord(name)
		if err != nil {
			return err
		}

		var f eventsFile
		if err := toml.Unmarshal(data, &f); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}

		for id, raw := range f.Event {
			if _, dup := c.Events[id]; dup {
				return fmt.Errorf("%s: event %q is defined twice", name, id)
			}
			if raw.Name == "" {
				return fmt.Errorf("%s: event %q has no name", name, id)
			}
			if raw.Map == "" {
				return fmt.Errorf("%s: event %q names no map", name, id)
			}
			if len(raw.Spawns) == 0 {
				return fmt.Errorf("%s: event %q names no spawn points, so nothing would happen when it started",
					name, id)
			}
			if raw.DurationMs <= 0 {
				return fmt.Errorf("%s: event %q has no duration; an event that only ended when its mobs died would be permanent the first time a party walked away",
					name, id)
			}

			trigger := EventTrigger(raw.Trigger)
			if !validEventTriggers[trigger] {
				return fmt.Errorf("%s: event %q has unknown trigger %q, want timer or shrine",
					name, id, raw.Trigger)
			}
			switch trigger {
			case TriggerTimer:
				if raw.EveryMs <= 0 {
					return fmt.Errorf("%s: event %q is timed but has no every_ms, so it would never start",
						name, id)
				}
				if raw.CooldownMs > 0 {
					return fmt.Errorf("%s: event %q is timed and also sets cooldown_ms; every_ms is already the wait between runs, and two answers to when it starts again is one of them being wrong",
						name, id)
				}
			case TriggerShrine:
				if raw.Shrine == "" {
					return fmt.Errorf("%s: event %q is shrine-triggered but names no shrine",
						name, id)
				}
				if raw.CooldownMs <= 0 {
					return fmt.Errorf("%s: event %q is shrine-triggered but has no cooldown_ms, so the shrine is a button somebody stands next to and presses forever",
						name, id)
				}
				if raw.EveryMs > 0 {
					return fmt.Errorf("%s: event %q is shrine-triggered and also sets every_ms; a shrine event starts when somebody starts it, and never otherwise",
						name, id)
				}
			}

			c.Events[id] = &Event{
				ID:            id,
				Name:          raw.Name,
				Map:           raw.Map,
				Trigger:       trigger,
				Announce:      raw.Announce,
				Shrine:        raw.Shrine,
				EveryTicks:    msToTicks(raw.EveryMs, TickRate),
				CooldownTicks: msToTicks(raw.CooldownMs, TickRate),
				DurationTicks: msToTicks(raw.DurationMs, TickRate),
				Spawns:        raw.Spawns,
			}
		}
	}
	return nil
}

// validateEvents checks each event against the map it happens on.
//
// Every one of these loads cleanly and then fails in play: an event naming a
// renamed spawn point starts, announces itself, and produces nothing.
func (c *Content) validateEvents() error {
	for id, ev := range c.Events {
		m, ok := c.Maps[ev.Map]
		if !ok {
			return fmt.Errorf("content: event %q happens on unknown map %q", id, ev.Map)
		}
		for _, spawn := range ev.Spawns {
			if !m.HasMobSpawn(spawn) {
				return fmt.Errorf("content: event %q names spawn point %q, which map %q does not have",
					id, spawn, ev.Map)
			}
		}
		if ev.Trigger == TriggerShrine && !m.HasShrine(ev.Shrine) {
			return fmt.Errorf("content: event %q needs shrine %q, which map %q does not have",
				id, ev.Shrine, ev.Map)
		}
	}

	// The other direction: a shrine nothing listens to is a thing a player can
	// walk into that does nothing, which is worse than no shrine at all.
	for id, m := range c.Maps {
		for _, s := range m.Shrines {
			if c.EventForShrine(id, s.Name) == nil {
				return fmt.Errorf("content: map %q has shrine %q but no event is triggered by it",
					id, s.Name)
			}
		}
	}
	return nil
}

// EventsForMap returns every event that happens on a map, in a stable order.
func (c *Content) EventsForMap(mapID string) []*Event {
	var out []*Event
	for _, id := range sortedKeys(c.Events) {
		if c.Events[id].Map == mapID {
			out = append(out, c.Events[id])
		}
	}
	return out
}

// EventForShrine returns the event a shrine starts, or nil.
func (c *Content) EventForShrine(mapID, shrine string) *Event {
	for _, id := range sortedKeys(c.Events) {
		ev := c.Events[id]
		if ev.Map == mapID && ev.Trigger == TriggerShrine && ev.Shrine == shrine {
			return ev
		}
	}
	return nil
}

// EventSpawns reports whether a spawn point belongs to any event, and which.
func (c *Content) EventForSpawn(mapID, spawn string) *Event {
	for _, id := range sortedKeys(c.Events) {
		ev := c.Events[id]
		if ev.Map != mapID {
			continue
		}
		for _, s := range ev.Spawns {
			if s == spawn {
				return ev
			}
		}
	}
	return nil
}
