package room

import (
	"github.com/ctrl-research/mmo/internal/content"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
)

// Running a dungeon.
//
// A dungeon room is an ordinary room with three extra facts: which stage it is
// on, whether the run is over, and how it ended. Everything else -- the mobs,
// the boss, the loot -- is the same machinery every other room uses.
//
// Progression is spawning. A stage's spawn points produce nothing until the
// stage begins, and the stage is cleared when every one of them has produced
// its whole population and none of it is left alive. That is why there are no
// doors: a wall that exists on the server and not on the client is prediction
// drift, and a wall that opens underfoot is a wall the client has to be told
// about. A mob that is not there yet needs no telling.

// RunState is how a dungeon run ended, or that it has not.
type RunState uint8

const (
	// RunActive is a run in progress.
	RunActive RunState = iota

	// RunCleared is the last stage finished.
	RunCleared

	// RunWiped is every player in the instance down at once.
	RunWiped
)

func (s RunState) String() string {
	switch s {
	case RunCleared:
		return "cleared"
	case RunWiped:
		return "wiped"
	default:
		return "active"
	}
}

// dungeonRun is the state of one instance's run.
type dungeonRun struct {
	def *content.Dungeon

	// stage indexes def.Stages. It only rises.
	stage int

	state RunState

	// endsAt is the tick the party is sent home, cleared or wiped. The delay
	// is for the players: being teleported out the instant a boss dies denies
	// them the moment they earned, and being teleported out the instant the
	// last of them falls denies them the chance to understand what happened.
	endsAt uint64
}

// startDungeon prepares a room that is running one.
func (r *Room) startDungeon(d *content.Dungeon) {
	r.dungeon = &dungeonRun{def: d}

	// Everything begins closed, and the first stage is opened below. A dungeon
	// whose spawn points were live before its first stage started would greet
	// the party with the whole instance at once.
	for _, sp := range r.allSpawns() {
		sp.stage, sp.gated = d.StageFor(sp.def.Name)
		sp.once = sp.gated
	}
	r.openStage(0)
}

// allSpawns returns every spawn point in the room, of every layer.
func (r *Room) allSpawns() []*spawnState {
	out := append([]*spawnState(nil), r.sharedSpawns...)
	for _, l := range r.layers {
		out = append(out, l.spawns...)
	}
	return out
}

// openStage lets a stage's spawn points start producing.
func (r *Room) openStage(stage int) {
	run := r.dungeon
	run.stage = stage

	for _, sp := range r.allSpawns() {
		if sp.gated && sp.stage == stage {
			sp.open = true
		}
	}

	r.announceRun()
}

// phaseDungeon advances the run: stages, the clear, and the wipe.
func (r *Room) phaseDungeon() {
	run := r.dungeon
	if run == nil || run.state != RunActive {
		r.finishRun()
		return
	}

	if r.partyIsDown() {
		r.endRun(RunWiped)
		return
	}

	if !r.stageIsClear(run.stage) {
		return
	}

	if run.stage+1 < len(run.def.Stages) {
		r.openStage(run.stage + 1)
		return
	}
	r.endRun(RunCleared)
}

// stageIsClear reports whether a stage has produced everything it holds and
// nothing of it is still alive.
//
// Both halves are needed. Counting only the living would clear a stage on the
// tick it opened, before anything had spawned; counting only what has spawned
// would clear it while the party was still fighting.
func (r *Room) stageIsClear(stage int) bool {
	for _, sp := range r.allSpawns() {
		if !sp.gated || sp.stage != stage {
			continue
		}
		if sp.spawned < sp.def.MaxAlive || sp.alive > 0 {
			return false
		}
	}
	return true
}

// partyIsDown reports whether every player in the instance is out of the fight
// at the same moment.
//
// An empty instance is not a wipe: everyone left, which the room's own idle
// timeout already handles, and calling it a wipe would burn a lockout for a
// party that simply logged off.
func (r *Room) partyIsDown() bool {
	any := false
	for _, id := range r.playerOrder {
		p := r.players[id]
		if p == nil {
			continue
		}
		// A disconnected character is neither up nor down: they are frozen and
		// invulnerable, and their party may still be fighting. Counting them
		// as down would wipe a run because somebody's wifi dropped.
		if p.frozen {
			continue
		}
		any = true
		if !isDowned(p.entity) {
			return false
		}
	}
	return any
}

// endRun records how the run finished and starts the clock to send everyone
// home.
func (r *Room) endRun(state RunState) {
	run := r.dungeon
	run.state = state
	run.endsAt = r.tick + uint64(runLingerTicks)

	r.announceRun()
	r.log.Info("dungeon run ended",
		"dungeon", run.def.ID, "state", state.String(), "stage", run.stage)
}

// runLingerTicks is how long a finished run stays before the party is sent
// home: long enough to loot what the boss dropped and to read what happened,
// short enough that nobody is left standing in a dead instance.
const runLingerTicks = 15 * TickRate

// finishRun sends everyone home once the linger has elapsed.
//
// Asked for every tick, and guarded per player by the transfer already being
// in flight rather than by a flag on the run. That is deliberate: a transfer
// that fails clears the flag, and this asks again on the next tick. A run-wide
// "already reported" would have made a single failed ejection permanent, and
// the character would be left in a dead instance until the room timed out
// underneath them.
func (r *Room) finishRun() {
	run := r.dungeon
	if run == nil || run.state == RunActive || r.tick < run.endsAt {
		return
	}

	for _, id := range r.playerOrder {
		p := r.players[id]
		if p == nil || p.events == nil || p.transferring {
			continue
		}
		p.transferring = true
		p.events.EndRun(RunResult{
			Player:      id,
			CharacterID: p.characterID,
			Dungeon:     run.def,
			Cleared:     run.state == RunCleared,
		})
	}
}

// announceRun tells the instance where the run stands.
func (r *Room) announceRun() {
	run := r.dungeon
	ev := &mmov1.DungeonState{
		DungeonId: run.def.ID,
		Name:      run.def.Name,
		Stage:     uint32(run.stage + 1),
		Stages:    uint32(len(run.def.Stages)),
		StageName: run.def.Stages[run.stage].Name,
		State:     run.state.String(),
	}
	if run.state != RunActive {
		ev.EndsInMs = uint32(runLingerTicks * 1000 / TickRate)
	}

	r.emit(&mmov1.Event{Body: &mmov1.Event_Dungeon{Dungeon: ev}}, SharedLayer)
}
