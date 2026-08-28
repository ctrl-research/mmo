package room

import (
	"slices"

	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
)

// phaseSnapshot builds and sends one snapshot per player.
//
// Each viewer gets a different snapshot: they see the shared layer plus their
// own, and they are delta-compressed against what they individually were last
// sent.
func (r *Room) phaseSnapshot() {
	for _, id := range r.playerOrder {
		p := r.players[id]
		if p == nil {
			continue
		}
		p.sink.Send(&mmov1.ServerMessage{
			Body: &mmov1.ServerMessage_Snapshot{Snapshot: r.buildSnapshot(p)},
		})
	}
}

// buildSnapshot renders the world as one viewer sees it.
//
// Delta compression is against what this viewer was last *sent*, not against
// what they last acknowledged. That is sound because the transport is a
// WebSocket: TCP guarantees ordered, reliable delivery, so anything sent has
// either arrived or the connection is gone. Acknowledgements are still carried
// on Intent, because the moment snapshots move to an unreliable channel
// (WebTransport datagrams) this shortcut stops holding and the baseline must
// come from a real ack.
func (r *Room) buildSnapshot(p *player) *mmov1.Snapshot {
	snap := &mmov1.Snapshot{
		Tick:    r.tick,
		AckSeq:  p.ackSeq,
		Self:    p.entity.state(true),
		Entered: nil,
	}

	// Iterate the viewer's visible set directly rather than walking every
	// entity and filtering afterwards. With per-player mob layering the room
	// holds roughly layers x mobs entities, so filtering per viewer would be
	// O(players x layers x mobs) per tick and would dominate the budget
	// (AGENTS.md invariant 5).
	//
	// M0 has only shared-layer entities, so this walks the entity list once.
	// M1 replaces the loop with a per-layer index; the snapshot shape does not
	// change.
	// Reused across ticks rather than reallocated per player per tick; at
	// 20 Hz with a full room that would be thousands of short-lived maps a
	// second for no benefit.
	seen := p.seenScratch
	clear(seen)

	for _, e := range r.entities {
		if !visible(p.entity, e) {
			continue
		}
		// Self is carried in full in its own field and must not be duplicated
		// into the delta list.
		if e.ID == p.entity.ID {
			seen[e.ID] = struct{}{}
			p.sent[e.ID] = e.view()
			continue
		}

		seen[e.ID] = struct{}{}
		cur := e.view()

		prev, known := p.sent[e.ID]
		if !known {
			// Newly visible: send it complete. A delta against a baseline the
			// client does not have would be unreadable.
			snap.Entered = append(snap.Entered, e.state(false))
			p.sent[e.ID] = cur
			continue
		}

		if d, changed := cur.delta(e.ID, prev); changed {
			snap.Entities = append(snap.Entities, d)
			p.sent[e.ID] = cur
		}
	}

	// Anything previously sent but no longer visible has died, left the room,
	// or moved out of the viewer's layer.
	for id := range p.sent {
		if _, still := seen[id]; !still {
			snap.Removed = append(snap.Removed, uint32(id))
			delete(p.sent, id)
		}
	}
	// Map iteration order is random. The client treats removals as a set, so
	// order does not change behaviour, but a room's output should be
	// reproducible tick for tick -- that is what makes replay a usable
	// debugging tool.
	slices.Sort(snap.Removed)

	return snap
}
