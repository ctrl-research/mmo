package world

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ctrl-research/mmo/internal/content"
	"github.com/ctrl-research/mmo/internal/directory"
	"github.com/ctrl-research/mmo/internal/world/room"
)

// The session's half of a dungeon run: getting in, and getting out.
//
// The room owns the run -- stages, the clear, the wipe -- because all of that
// happens inside a tick. Everything here is what a tick must not do: read the
// database to check a lockout, write one on a clear, and run the transfer
// protocol to send a party home.

// ErrDungeonLocked is returned when a character has cleared a dungeon too
// recently to enter it again.
var ErrDungeonLocked = errors.New("dungeon lockout has not expired")

// ErrDungeonLevel is returned when a character is below a dungeon's minimum.
var ErrDungeonLevel = errors.New("below the dungeon's minimum level")

// EndRun is called from inside the room's tick when a run finishes.
func (s *Session) EndRun(res room.RunResult) {
	select {
	case s.runEnds <- res:
	default:
		// The queue is full, which means several runs ended at once for one
		// session -- which cannot happen. Dropping it leaves the character in
		// the instance, where the room's idle timeout will eventually collect
		// them; the alternative is blocking a tick every other player in the
		// dungeon is waiting on.
		s.log.Warn("dropped a dungeon run ending", "dungeon", res.Dungeon.ID)
	}
}

// handleRunEnd writes the lockout and sends the character home.
func (s *Session) handleRunEnd(res room.RunResult) {
	ctx, cancel := context.WithTimeout(context.Background(), TransferTimeout)
	defer cancel()

	// The lockout is written before the transfer, and a failure to write it
	// does not stop the transfer. Being left in a dead instance because the
	// database hiccuped is a much worse outcome than a lockout going
	// unrecorded, and the two are not equally recoverable.
	if res.Cleared && res.Dungeon.LockoutTicks > 0 {
		lockout := time.Duration(res.Dungeon.LockoutTicks) * time.Second / room.TickRate
		if err := s.node.store.RecordClear(ctx, s.characterID, res.Dungeon.ID, lockout); err != nil {
			s.log.Error("recording a dungeon clear", "dungeon", res.Dungeon.ID, "err", err)
		}
	}

	target, ok := s.node.content.Maps[res.Dungeon.ExitMap]
	if !ok {
		// Validated at load, so this is a bug rather than bad content.
		s.log.Error("dungeon exits to an unknown map", "map", res.Dungeon.ExitMap)
		handle, entityID := s.Where()
		handle.AbortTransfer(ctx, entityID, "the way out is missing")
		return
	}

	err := s.transfer(ctx, target, arrival{spawnPoint: res.Dungeon.ExitSpawn},
		func(ctx context.Context) (directory.Instance, error) {
			return s.node.dir.Join(ctx,
				roomKey(target, s.layerKey()), target.Capacity)
		})
	if err != nil {
		s.log.Warn("sending a character home from a dungeon", "err", err)
		handle, entityID := s.Where()
		handle.AbortTransfer(ctx, entityID, transferMessage(err))
	}
}

// checkDungeonEntry reports whether a character may enter a dungeon.
//
// Checked here rather than in the room, because both answers live outside the
// tick: the level is on the session and the lockout is in the database. It is
// checked on the way in rather than on the way out, so a party that cannot all
// get in finds out at the door.
func (s *Session) checkDungeonEntry(ctx context.Context, d *content.Dungeon) error {
	if level := s.characterLevel(ctx); d.MinLevel > 0 && level < d.MinLevel {
		return fmt.Errorf("%w: %s needs level %d, and you are %d", ErrDungeonLevel, d.Name, d.MinLevel, level)
	}

	until, err := s.node.store.LockedOutUntil(ctx, s.characterID, d.ID)
	if err != nil {
		// A lockout that cannot be read is not a lockout that has expired.
		// Refusing entry is the conservative answer: the cost is a player
		// waiting, and the cost of the other choice is the lockout meaning
		// nothing whenever the database is unwell.
		return fmt.Errorf("checking the lockout for %s: %w", d.Name, err)
	}
	if !until.IsZero() {
		return fmt.Errorf("%w: %s is available again in %s",
			ErrDungeonLocked, d.Name, until.Sub(time.Now()).Round(time.Minute))
	}
	return nil
}
