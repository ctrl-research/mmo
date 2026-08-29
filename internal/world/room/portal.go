package room

import (
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
)

// Portals and waypoints.
//
// The room only *detects* these. Acting on one means a database write and a
// transfer negotiated over the bus, neither of which a tick can wait for, so
// the room reports and the session decides.

// portalCooldownTicks is how long after arriving a character ignores portals.
//
// Without it, arriving on a spawn point that overlaps the return portal sends
// the character straight back, and they bounce between two maps forever. One
// second is comfortably longer than it takes to walk clear.
const portalCooldownTicks = 20

// phasePortals checks whether anyone is standing in a portal or on a waypoint.
func (r *Room) phasePortals() {
	if r.mapDef == nil {
		return
	}

	for _, id := range r.playerOrder {
		p := r.players[id]
		if p == nil || p.frozen || p.transferring {
			continue
		}
		// A body lying where it fell is not a character travelling. Without
		// this, dying on top of a portal sends the corpse through it and the
		// character comes back somewhere they never walked to.
		if isDowned(p.entity) {
			continue
		}

		body := p.entity.Body.Bounds()

		if w, ok := r.mapDef.WaypointAt(body); ok && !p.knownWaypoints[w.ID] {
			// Remembered locally as well as persisted, so a player standing on
			// a waypoint does not generate one write per tick.
			p.knownWaypoints[w.ID] = true
			if p.events != nil {
				p.events.DiscoverWaypoint(id, p.characterID, w.ID)
			}
			r.emitTo(id, &mmov1.Event{Body: &mmov1.Event_WaypointFound{
				WaypointFound: &mmov1.WaypointFound{
					WaypointId: w.ID,
					Name:       w.Name,
					MapId:      r.cfg.MapID,
				},
			}})
		}

		if r.tick < p.portalReadyAt {
			continue
		}

		portal, ok := r.mapDef.PortalAt(body)
		if !ok {
			continue
		}

		if portal.RequiredLevel > 0 && p.entity.Player != nil &&
			p.entity.Player.Level < portal.RequiredLevel {
			// Refused rather than silently ignored: a portal that does nothing
			// reads as broken, and the level requirement is the whole reason
			// the gate exists.
			if r.tick >= p.portalNoticeAt {
				p.portalNoticeAt = r.tick + portalCooldownTicks
				r.emitTo(id, &mmov1.Event{Body: &mmov1.Event_PortalRefused{
					PortalRefused: &mmov1.PortalRefused{
						TargetMap:     portal.TargetMap,
						RequiredLevel: uint32(portal.RequiredLevel),
					},
				}})
			}
			continue
		}

		if p.events == nil {
			continue
		}

		// Marked before the session is told, so a player straddling a portal
		// does not start a second transfer on the next tick.
		p.transferring = true
		p.events.EnterPortal(PortalRequest{
			Player:      id,
			CharacterID: p.characterID,
			Portal:      portal,
			Tick:        r.tick,
		})
	}
}

// abortTransfer releases a player whose transfer failed, so they can try again.
func (r *Room) abortTransfer(id EntityID, reason string) {
	p, ok := r.players[id]
	if !ok {
		return
	}

	p.transferring = false
	// A cooldown before the portal fires again, or a failure loops: the player
	// is still standing in the portal that just refused them.
	p.portalReadyAt = r.tick + portalCooldownTicks

	if reason != "" {
		r.emitTo(id, &mmov1.Event{Body: &mmov1.Event_PortalRefused{
			PortalRefused: &mmov1.PortalRefused{Reason: reason},
		}})
	}
}

// markArrived starts a character's portal cooldown after a transfer.
func (r *Room) markArrived(id EntityID) {
	if p, ok := r.players[id]; ok {
		p.portalReadyAt = r.tick + portalCooldownTicks
	}
}
