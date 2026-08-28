//go:build js && wasm

// Command simwasm exposes the simulation to the browser.
//
// This is what makes client-side prediction exact rather than approximate. The
// client does not reimplement the physics -- it runs the server's own compiled
// code, so a predicted position and the authoritative one agree bit for bit
// given the same inputs. The alternative, two implementations kept in step by
// hand, drifts, and the drift surfaces as rubber-banding that is very hard to
// trace back to its cause.
//
// Build with `make wasm`.
package main

import (
	"syscall/js"

	"github.com/ctrl-research/mmo/internal/world/sim"
)

// state is the module's single set of globals. WebAssembly here is
// single-threaded and driven from one JavaScript context, so no locking is
// needed -- and adding any would violate the purity the sim package depends on.
var state struct {
	world  *sim.World
	tuning sim.Tuning

	// scratch avoids allocating a Body per call. At 20 Hz with replay on every
	// snapshot, Step is called far more often than once per tick.
	scratch sim.Body
	buf     []byte
}

func main() {
	state.tuning = sim.DefaultTuning()
	state.buf = make([]byte, sim.BodySize)

	js.Global().Set("__sim", js.ValueOf(map[string]any{
		"bodySize":    sim.BodySize,
		"tickRate":    20,
		"fracBits":    8,
		"setWorld":    js.FuncOf(setWorld),
		"step":        js.FuncOf(step),
		"settle":      js.FuncOf(settle),
		"tuning":      js.FuncOf(tuning),
		"encodeInput": js.FuncOf(encodeInput),
	}))

	// Signal readiness only after the API is installed, so the client can
	// await it rather than poll.
	if ready := js.Global().Get("__simReady"); ready.Type() == js.TypeFunction {
		ready.Invoke()
	}

	// A Go WASM module exits when main returns, taking the exported functions
	// with it. Block forever so they stay callable.
	select {}
}

// setWorld uploads the map's collision geometry. It is called once per map.
//
// Returns true if the buffer was well formed. A false here means the client
// and server disagree about the encoding, and the caller must refuse to run
// rather than simulate a different world from the server.
func setWorld(_ js.Value, args []js.Value) any {
	if len(args) != 1 {
		return false
	}
	raw := make([]byte, args[0].Get("length").Int())
	js.CopyBytesToGo(raw, args[0])

	w, ok := sim.UnmarshalWorld(raw)
	if !ok {
		return false
	}
	state.world = w
	return true
}

// step advances one body by one tick, in place.
//
// The body is passed as a Uint8Array that the caller owns and reuses, so a
// step costs two copies of 28 bytes and no allocation on either side.
func step(_ js.Value, args []js.Value) any {
	if state.world == nil || len(args) != 2 {
		return false
	}
	js.CopyBytesToGo(state.buf, args[0])
	sim.UnmarshalBody(state.buf, &state.scratch)

	sim.Step(&state.scratch, sim.DecodeInput(int32(args[1].Int())), state.world, &state.tuning)

	sim.MarshalBody(state.buf, &state.scratch)
	js.CopyBytesToJS(args[0], state.buf)
	return true
}

// settle computes a freshly placed body's contact state without advancing
// time, so a player who presses jump on their first tick after spawning or
// arriving through a portal actually jumps.
func settle(_ js.Value, args []js.Value) any {
	if state.world == nil || len(args) != 1 {
		return false
	}
	js.CopyBytesToGo(state.buf, args[0])
	sim.UnmarshalBody(state.buf, &state.scratch)

	sim.Settle(&state.scratch, state.world, &state.tuning)

	sim.MarshalBody(state.buf, &state.scratch)
	js.CopyBytesToJS(args[0], state.buf)
	return true
}

// tuning exposes the movement constants the renderer needs for smoothing
// thresholds, so those numbers are defined in exactly one place.
func tuning(js.Value, []js.Value) any {
	return map[string]any{
		"runSpeed":    int(state.tuning.RunSpeed),
		"jumpVel":     int(state.tuning.JumpVel),
		"gravity":     int(state.tuning.Gravity),
		"terminalVel": int(state.tuning.TerminalVel),
	}
}

func encodeInput(_ js.Value, args []js.Value) any {
	if len(args) != 4 {
		return 0
	}
	return int(sim.EncodeInput(sim.Input{
		MoveX: int32(args[0].Int()),
		Jump:  args[1].Bool(),
		Up:    args[2].Bool(),
		Down:  args[3].Bool(),
	}))
}
