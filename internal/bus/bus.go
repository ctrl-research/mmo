// Package bus carries messages between components that must not touch each
// other's state directly.
//
// Rooms are independent, single-goroutine simulations. Anything that crosses a
// room boundary -- chat, party, guild, presence, character transfer -- goes
// through here, never through a shared pointer or a direct call. That rule is
// what lets rooms be redistributed across nodes later without a redesign, and
// it is the reason this package exists at hobby scale where a direct call
// would obviously work (see AGENTS.md invariant 1).
//
// Two implementations satisfy the interface: InProc, backed by Go channels,
// used when every role runs in one process; and a NATS implementation, added
// in M9, used when roles are split across nodes. Nothing above this package
// knows which one it has.
package bus

import (
	"context"
	"errors"

	"google.golang.org/protobuf/proto"
)

// ErrClosed is returned by a Bus that has been shut down.
var ErrClosed = errors.New("bus: closed")

// Handler receives one delivered message. The payload is the marshalled
// protobuf body; the handler knows which type to expect from the subject.
//
// Handlers must not block: a slow handler delays every other subscriber on the
// same bus. Work that might block belongs on the receiving component's own
// goroutine, reached by handing it the message and returning.
type Handler func(ctx context.Context, subject string, payload []byte)

// Subscription is a live subscription. Close is idempotent.
type Subscription interface {
	Close()
}

// Bus is publish/subscribe messaging with hierarchical subjects.
//
// Subjects are dot-separated tokens that encode routing, following NATS
// conventions so the NATS implementation is a direct mapping:
//
//	room.42.input        one room's inbound commands
//	room.*.lifecycle     lifecycle of any room ("*" matches one token)
//	chat.guild.7         one guild's chat
//	chat.>               every chat subject ("&gt;" matches the rest)
//
// A message that cannot be addressed by a subject is a design smell: it
// usually means two components are sharing state they should not.
type Bus interface {
	// Publish delivers msg to every matching subscriber. Delivery is
	// best-effort and unordered across subjects; within a single subject,
	// messages from one publisher arrive in order.
	Publish(ctx context.Context, subject string, msg proto.Message) error

	// Subscribe registers fn for every message matching pattern, which may
	// contain the "*" and "&gt;" wildcards.
	Subscribe(ctx context.Context, pattern string, fn Handler) (Subscription, error)

	// Request publishes a message and waits for a single reply.
	//
	// Built on top of publish/subscribe rather than being a separate
	// mechanism: the requester subscribes to a private inbox and names it in
	// the envelope. That keeps one transport rather than two, and means the
	// NATS implementation maps onto NATS's own request/reply directly.
	Request(ctx context.Context, subject string, msg proto.Message, reply proto.Message) error

	// Respond registers a handler that answers requests on a subject.
	//
	// A responder returning an error sends it back to the requester, so a
	// refusal arrives immediately rather than as a timeout -- the difference
	// between "the destination rejected this" and "something is wrong with the
	// cluster".
	Respond(ctx context.Context, pattern string, fn Responder) (Subscription, error)

	// Close shuts down the bus and releases every subscription.
	Close() error
}

// Responder answers a request. Returning an error sends it to the requester.
type Responder func(ctx context.Context, subject string, payload []byte) (proto.Message, error)

// ErrRequestTimeout means no reply arrived before the context expired.
var ErrRequestTimeout = errors.New("bus: request timed out")

// ErrNoResponder means nothing was listening on the subject.
//
// Distinct from a timeout on purpose: nothing listening usually means a
// misconfigured subject or a node that never started, which is a different
// problem from one that is merely slow.
var ErrNoResponder = errors.New("bus: no responder")
