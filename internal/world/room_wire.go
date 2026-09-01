package world

import (
	"encoding/json"

	"github.com/ctrl-research/mmo/internal/content"
	"github.com/ctrl-research/mmo/internal/fixed"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world/items"
	"github.com/ctrl-research/mmo/internal/world/room"
	"github.com/ctrl-research/mmo/internal/world/sim"
	"github.com/ctrl-research/mmo/internal/world/stats"
)

// Putting room.Handle on the wire.
//
// The conversions live together rather than beside the code that uses them,
// because they come in pairs and a pair that drifts is a field that silently
// stops travelling. Every one of these has an encode and a decode next to each
// other so the omission is visible.
//
// What deliberately does not travel: anything that is an in-process reference
// (a Sink, a SessionEvents) and anything that comes from content. Content
// values are named and resolved from the receiving node's own copy, the same
// way an incoming transfer resolves its spawn -- see acceptTransfer.

func encodeProgress(p room.Progress) *mmov1.WireProgress {
	return &mmov1.WireProgress{
		Level: int32(p.Level), Exp: p.Exp, Gold: p.Gold, MapId: p.MapID,
	}
}

func decodeProgress(p *mmov1.WireProgress) room.Progress {
	if p == nil {
		return room.Progress{}
	}
	return room.Progress{
		Level: int(p.GetLevel()), Exp: p.GetExp(), Gold: p.GetGold(), MapID: p.GetMapId(),
	}
}

func encodeLoadout(slots []room.LoadoutSlot) []*mmov1.WireLoadoutSlot {
	if slots == nil {
		return nil
	}
	out := make([]*mmov1.WireLoadoutSlot, 0, len(slots))
	for _, s := range slots {
		out = append(out, &mmov1.WireLoadoutSlot{
			SkillId: s.SkillID, Rank: int32(s.Rank), Supports: s.Supports,
		})
	}
	return out
}

func decodeLoadout(slots []*mmov1.WireLoadoutSlot) []room.LoadoutSlot {
	if slots == nil {
		return nil
	}
	out := make([]room.LoadoutSlot, 0, len(slots))
	for _, s := range slots {
		out = append(out, room.LoadoutSlot{
			SkillID: s.GetSkillId(), Rank: int(s.GetRank()), Supports: s.GetSupports(),
		})
	}
	return out
}

func encodeSecondary(p room.SecondaryProgress) map[string]int64 {
	if p == nil {
		return nil
	}
	out := make(map[string]int64, len(p))
	for k, v := range p {
		out[k] = v
	}
	return out
}

func decodeSecondary(m map[string]int64) room.SecondaryProgress {
	if m == nil {
		return nil
	}
	out := make(room.SecondaryProgress, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func encodeVec(v sim.Vec) *mmov1.WireVec {
	return &mmov1.WireVec{X: int32(v.X), Y: int32(v.Y)}
}

// Positions travel as their raw fixed-point units, not as pixels. Converting
// to a float and back would round, and a spawn point that is a fraction of a
// unit off is a character standing inside the floor.

func decodeVec(v *mmov1.WireVec) sim.Vec {
	if v == nil {
		return sim.Vec{}
	}
	return sim.Vec{X: fixed.F(v.GetX()), Y: fixed.F(v.GetY())}
}

func encodeStats(b *stats.Block) *mmov1.WireStats {
	if b == nil {
		return nil
	}
	base, flat, increased, more := b.Layers()
	as64 := func(vs []stats.Value) []int64 {
		out := make([]int64, len(vs))
		for i, v := range vs {
			out[i] = int64(v)
		}
		return out
	}
	return &mmov1.WireStats{
		Base: as64(base), Flat: as64(flat),
		Increased: as64(increased), More: as64(more),
	}
}

// decodeStats reports failure rather than substituting an empty block.
//
// A block of zeroes is not a neutral value: the "more" layer is a product, so
// every stat would be multiplied by zero and the character would do no damage
// and have no life. Refusing leaves the room with the block it already had,
// which is stale but playable.
func decodeStats(s *mmov1.WireStats) (*stats.Block, bool) {
	if s == nil {
		return nil, false
	}
	asValue := func(vs []int64) []stats.Value {
		out := make([]stats.Value, len(vs))
		for i, v := range vs {
			out[i] = stats.Value(v)
		}
		return out
	}
	return stats.Rebuild(
		asValue(s.GetBase()), asValue(s.GetFlat()),
		asValue(s.GetIncreased()), asValue(s.GetMore()),
	)
}

func encodeDerived(d room.Derived) *mmov1.WireDerived {
	power := make(map[string]int32, len(d.ToolPower))
	for k, v := range d.ToolPower {
		power[k] = int32(v)
	}
	return &mmov1.WireDerived{
		Block: encodeStats(d.Block), MaxHp: d.MaxHP, ToolPower: power,
	}
}

func decodeDerived(d *mmov1.WireDerived) (room.Derived, bool) {
	if d == nil {
		return room.Derived{}, false
	}
	block, ok := decodeStats(d.GetBlock())
	if !ok {
		return room.Derived{}, false
	}

	power := make(map[string]int, len(d.GetToolPower()))
	for k, v := range d.GetToolPower() {
		power[k] = int(v)
	}
	return room.Derived{Block: block, MaxHP: d.GetMaxHp(), ToolPower: power}, true
}

func encodeSnapshot(s room.Snapshot) *mmov1.WireSnapshot {
	// A state that will not encode is sent as empty rather than dropping the
	// whole snapshot: the progress is still worth checkpointing, and the room
	// treats absent state as "wherever the character was".
	raw, err := room.MarshalState(s.State)
	if err != nil {
		raw = nil
	}
	return &mmov1.WireSnapshot{
		Progress:  encodeProgress(s.Progress),
		State:     raw,
		Secondary: encodeSecondary(s.Secondary),
	}
}

func decodeSnapshot(s *mmov1.WireSnapshot) room.Snapshot {
	if s == nil {
		return room.Snapshot{}
	}
	out := room.Snapshot{
		Progress:  decodeProgress(s.GetProgress()),
		Secondary: decodeSecondary(s.GetSecondary()),
	}
	if len(s.GetState()) > 0 {
		out.State = room.UnmarshalState(s.GetState())
	}
	return out
}

func encodeJoinSpec(spec room.JoinSpec) *mmov1.WireJoinSpec {
	raw, err := room.MarshalState(spec.State)
	if err != nil {
		raw = nil
	}
	return &mmov1.WireJoinSpec{
		CharacterId:    spec.CharacterID,
		Name:           spec.Name,
		Progress:       encodeProgress(spec.Progress),
		State:          raw,
		Fresh:          spec.Fresh,
		Loadout:        encodeLoadout(spec.Loadout),
		LayerKey:       spec.LayerKey,
		KnownWaypoints: spec.KnownWaypoints,
		Secondary:      encodeSecondary(spec.Secondary),
		Spawn:          encodeVec(spec.Spawn),
		Arrived:        spec.Arrived,
	}
}

// decodeJoinSpec rebuilds a spec without its Sink or Events.
//
// Those are in-process references and cannot travel; the caller attaches its
// connection afterwards, which is the order a transfer already uses.
func decodeJoinSpec(s *mmov1.WireJoinSpec) room.JoinSpec {
	if s == nil {
		return room.JoinSpec{}
	}
	spec := room.JoinSpec{
		CharacterID:    s.GetCharacterId(),
		Name:           s.GetName(),
		Progress:       decodeProgress(s.GetProgress()),
		Fresh:          s.GetFresh(),
		Loadout:        decodeLoadout(s.GetLoadout()),
		LayerKey:       s.GetLayerKey(),
		KnownWaypoints: s.GetKnownWaypoints(),
		Secondary:      decodeSecondary(s.GetSecondary()),
		Spawn:          decodeVec(s.GetSpawn()),
		Arrived:        s.GetArrived(),
	}
	if len(s.GetState()) > 0 {
		spec.State = room.UnmarshalState(s.GetState())
	}
	return spec
}

func encodeItem(inst *items.Instance) []byte {
	if inst == nil {
		return nil
	}
	raw, err := json.Marshal(inst)
	if err != nil {
		return nil
	}
	return raw
}

func decodeItem(raw []byte) *items.Instance {
	if len(raw) == 0 {
		return nil
	}
	var inst items.Instance
	if err := json.Unmarshal(raw, &inst); err != nil {
		return nil
	}
	return &inst
}

// portalIndex finds a portal's position in its map, which is how it is named
// on the wire.
//
// Compared by identity of the fields that make a portal what it is rather than
// by pointer: the room holds portals from its own content, and the value that
// reaches the session came from the same load.
func portalIndex(m *content.Map, p content.Portal) (uint32, bool) {
	for i := range m.Portals {
		if m.Portals[i] == p {
			return uint32(i), true
		}
	}
	return 0, false
}

func portalAt(m *content.Map, index uint32) (content.Portal, bool) {
	if m == nil || int(index) >= len(m.Portals) {
		return content.Portal{}, false
	}
	return m.Portals[index], true
}
