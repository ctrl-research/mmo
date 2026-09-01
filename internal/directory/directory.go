// Package directory answers "where does this room live, and is there space in
// it?" for the rest of the system.
//
// It is the second of the two seams that let this codebase run as one process
// today and many nodes later (see docs/architecture.md). Two implementations
// satisfy the interface: Memory, used when everything runs in one process, and
// a Redis implementation added in M9. Nothing above this package knows which.
//
// Character ownership leases -- the fencing-token mechanism that stops two
// nodes mutating one character and duplicating its items -- also belong here.
// They arrive in M2 alongside persistence, since there is no character to
// lease until then. The interface is expected to grow, not to be replaced.
package directory

import (
	"context"
	"errors"
	"fmt"
)

// Errors returned by a Directory.
var (
	// ErrNoCapacity means every instance for a key is full and no more may be
	// created. Only private rooms can hit this; shared rooms scale out into
	// new channels instead.
	ErrNoCapacity = errors.New("directory: no capacity")

	// ErrUnknownInstance means the instance is not registered, usually because
	// it was already released.
	ErrUnknownInstance = errors.New("directory: unknown instance")

	// ErrNoLiveNode means no world node is currently eligible to host a room.
	//
	// Only a shared directory can report this: with one process the node asking
	// is the node that would host it. It is distinct from ErrNoCapacity because
	// the fix is different -- capacity means the world is full, this means there
	// is no world.
	ErrNoLiveNode = errors.New("directory: no live node")
)

// NodeID identifies one world node. With a single process there is exactly
// one, but the value is threaded through from the start so that the day there
// are several is a configuration change and not a code change.
type NodeID string

// InstanceID identifies one live room instance, globally and for its lifetime.
// IDs are never reused: a stale message addressed to a torn-down instance must
// fail to route rather than land in an unrelated room.
type InstanceID uint64

// Placement decides who may enter a room. It is independent of layering, which
// decides which entities inside a room a player can see -- see
// docs/architecture.md for why conflating the two is a mistake.
type Placement string

const (
	// PlacementShared is a public zone: MapleStory-style channels. Players
	// join the least-full instance under capacity, and a new one is created
	// when they are all full.
	PlacementShared Placement = "shared"

	// PlacementPrivate is a dungeon or boss room, keyed to a party or a single
	// character. It is created on demand and torn down once empty.
	PlacementPrivate Placement = "private"
)

// RoomKey names the logical room a player wants to be in, as opposed to the
// particular instance they end up in.
//
// For shared rooms OwnerKey is empty and many instances may satisfy one key.
// For private rooms OwnerKey is the party or character ID and exactly one
// instance satisfies the key.
type RoomKey struct {
	MapID     string
	Placement Placement
	OwnerKey  string
}

func (k RoomKey) String() string {
	if k.OwnerKey == "" {
		return fmt.Sprintf("%s/%s", k.MapID, k.Placement)
	}
	return fmt.Sprintf("%s/%s/%s", k.MapID, k.Placement, k.OwnerKey)
}

// Valid reports whether the key is well formed: a private room must name an
// owner, and a shared room must not.
func (k RoomKey) Valid() bool {
	switch k.Placement {
	case PlacementShared:
		return k.MapID != "" && k.OwnerKey == ""
	case PlacementPrivate:
		return k.MapID != "" && k.OwnerKey != ""
	default:
		return false
	}
}

// Instance is a live room instance and its current occupancy.
type Instance struct {
	ID       InstanceID
	Node     NodeID
	Key      RoomKey
	Players  int
	Capacity int
}

// Full reports whether the instance has no room for another player.
func (i Instance) Full() bool { return i.Players >= i.Capacity }

// Directory resolves room keys to instances and tracks their occupancy.
//
// Implementations must be safe for concurrent use: gateways on several
// goroutines place players while world nodes release instances.
//
// The read methods return errors even though the in-memory implementation can
// never fail one. A network-backed directory can, and the failure has to be
// distinguishable from the answer: an unreachable Redis returning "this map has
// no channels" or "that instance does not exist" is not a degraded answer, it is
// a wrong one, and a caller acting on it refuses a channel switch to a channel
// that is running fine.
type Directory interface {
	// Join reserves a slot for one player in an instance satisfying key,
	// creating an instance if none has room. The returned Instance reflects
	// occupancy including the caller.
	//
	// Reservation and placement are one atomic step on purpose. Choosing an
	// instance and then incrementing its count separately lets two
	// simultaneous joins both pick the same last free slot.
	Join(ctx context.Context, key RoomKey, capacity int) (Instance, error)

	// JoinInstance reserves a slot in one named instance, rather than letting
	// the directory choose.
	//
	// This is channel switching: a player picking channel 3 means that
	// instance and no other, so the placement that spreads players across the
	// least-full channel is exactly the wrong behaviour. Fails with
	// ErrNoCapacity if it filled up in the meantime and ErrUnknownInstance if
	// it was torn down, both of which the caller reports rather than silently
	// substituting a different channel.
	JoinInstance(ctx context.Context, id InstanceID) (Instance, error)

	// NewInstance creates an additional instance for a key and reserves a slot
	// in it, whether or not the existing ones have room.
	//
	// Join deliberately fills the least-full channel, which is right for
	// placement and wrong for a player who wants out of the one they are in.
	// Fails for a private key, which is one instance by definition.
	NewInstance(ctx context.Context, key RoomKey, capacity int) (Instance, error)

	// InstancesFor returns every live instance satisfying a key, ordered by
	// ID. For a shared map this is the channel list.
	InstancesFor(ctx context.Context, key RoomKey) ([]Instance, error)

	// Leave releases a slot previously reserved by Join. Releasing the last
	// slot in an instance does not destroy it: the world node decides when to
	// tear a room down, since only it knows whether the room still has work
	// to do.
	Leave(ctx context.Context, id InstanceID) error

	// Withdraw takes this node out of the set placement chooses from, without
	// removing anything it is already hosting.
	//
	// The first step of draining. A node that is shutting down still has rooms
	// and characters to finish with, and it needs to stop being sent new ones
	// while it does -- otherwise a character arrives at a node that is halfway
	// through closing its sessions, which is the one place they cannot be
	// looked after. Letting the liveness TTL do it instead means fifteen
	// seconds of new arrivals into a process that is leaving.
	//
	// Idempotent, and safe on a node that was never registered.
	Withdraw(ctx context.Context) error

	// LiveNodes lists the world nodes currently heartbeating.
	//
	// On the interface because a gateway deployed on its own has to find a
	// world node to hand a character to, and the directory is already the
	// thing that knows which ones are alive -- it is what placement uses to
	// avoid starting rooms on a node that has gone.
	LiveNodes(ctx context.Context) ([]NodeID, error)

	// Release removes an instance entirely. The world node calls this after
	// tearing the room down.
	Release(ctx context.Context, id InstanceID) error

	// TryRelease removes an instance only if nobody occupies a slot in it,
	// reporting whether it did.
	//
	// This is what makes tearing an idle room down safe. Checking occupancy
	// and releasing separately leaves a window where a player joins the
	// instance between the two, and the room they were placed in stops
	// ticking a moment later. Doing both under the directory's own lock closes
	// it: after a successful TryRelease no further placement can name the
	// instance, and a failed one means somebody arrived and the room should
	// keep running.
	TryRelease(ctx context.Context, id InstanceID) (bool, error)

	// Lookup returns a single instance, and whether it exists.
	Lookup(ctx context.Context, id InstanceID) (Instance, bool, error)

	// List returns every live instance, ordered by ID so callers -- metrics,
	// admin views, tests -- see a stable sequence.
	List(ctx context.Context) ([]Instance, error)

	// Close releases any resources held by the implementation.
	Close() error
}
