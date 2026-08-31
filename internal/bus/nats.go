package bus

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"
)

// NATS is a Bus backed by a NATS server, for when roles are split across
// nodes.
//
// A direct mapping rather than an adaptation, which is the whole reason the
// subject grammar in subject.go follows NATS's: a subject that routes on the
// in-process bus routes identically here, and nothing above this package can
// tell which one it has.
//
// The one place the two implementations genuinely differ is request/reply.
// InProc has no notion of a reply, so it builds one out of publish/subscribe --
// a private inbox subject and a correlation id in the envelope. NATS has
// request/reply natively, including a server-side answer to "is anybody
// listening", so it uses that rather than reimplementing it worse. Both satisfy
// the same contract; the conformance suite in bus_test.go is what says so.
type NATS struct {
	conn *nats.Conn

	// owned records whether this type opened the connection and must therefore
	// close it. A caller who passed one in keeps that responsibility: closing
	// somebody else's connection is how one component takes another offline.
	owned bool

	mu     sync.Mutex
	closed bool

	// drained is closed once the connection has finished draining, so Close
	// can wait for it. A drain that has not finished is not a drain: the
	// caller's next action -- exiting, or handing its rooms somewhere else --
	// assumes the traffic already published has left.
	drained chan struct{}

	// dropped counts messages the server or client discarded because a
	// subscriber could not keep up, mirroring InProc.Dropped. Both are the same
	// condition -- a handler doing work it should be handing off -- and it needs
	// to be visible on whichever bus is carrying the game.
	dropped atomic64
}

// natsPendingMsgs and natsPendingBytes bound one subscription's backlog.
//
// NATS defaults to a large pending queue, which turns a wedged handler into
// memory growth until the process dies. Bounding it turns that into dropped
// messages instead, which is what this bus promises anyway and what InProc
// already does. The message count matches InProc's queue depth so a slow
// handler behaves the same on both; the byte cap is the belt to that brace,
// because a few large messages exhaust memory long before 256 small ones.
const (
	natsPendingMsgs  = inProcQueueDepth
	natsPendingBytes = 16 << 20
)

// natsFlushTimeout bounds the flush that establishes a subscription.
//
// Short, because a subscribe that cannot reach the server in this long means
// the cluster is not working, and reporting that beats a room that starts
// believing it is listening.
const natsFlushTimeout = 2 * time.Second

// Connect opens a NATS bus.
//
// The options are opinionated rather than exposed. Reconnection is unlimited
// because a bus that gives up on the server turns a blip into an outage, and
// every subject on it is either best-effort or retried by its caller.
func Connect(url string) (*NATS, error) {
	if url == "" {
		url = nats.DefaultURL
	}

	b := &NATS{owned: true, drained: make(chan struct{})}

	conn, err := nats.Connect(url,
		nats.Name("mmo"),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
		// A slow consumer is the same condition InProc counts as a drop, and
		// the only place NATS reports it. Counted rather than logged per
		// occurrence: at the rate a wedged subscription produces these, logging
		// each one is its own outage.
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
			if errors.Is(err, nats.ErrSlowConsumer) {
				b.dropped.add(1)
			}
		}),
		// Drain finishes asynchronously, and Close has to be able to wait for
		// it. Without this the connection is still delivering after Close
		// returns, which shows up as a subscription that outlives the bus that
		// owned it -- found as a flaky test, and it would have been found in
		// production as a node that kept answering after it was drained.
		nats.ClosedHandler(func(*nats.Conn) { close(b.drained) }),
	)
	if err != nil {
		return nil, fmt.Errorf("bus: connecting to nats at %s: %w", url, err)
	}

	b.conn = conn
	return b, nil
}

// NewNATS wraps an existing connection, which the caller keeps responsibility
// for closing.
func NewNATS(conn *nats.Conn) *NATS { return &NATS{conn: conn} }

// Publish marshals msg and sends it.
//
// Deliberately not flushed. A flush is a round trip to the server, and one per
// publish would serialise every message on the network -- on a bus carrying
// per-tick traffic from many rooms that is the difference between a bus and a
// bottleneck. The interface promises best-effort delivery; this is what
// best-effort costs, and it is the same promise InProc keeps by dropping rather
// than blocking.
func (b *NATS) Publish(_ context.Context, subject string, msg proto.Message) error {
	if !validSubject(subject) {
		return &SubjectError{Subject: subject}
	}

	payload, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	if b.isClosed() {
		return ErrClosed
	}
	return b.translate(b.conn.Publish(subject, payload))
}

// Subscribe registers fn for messages matching pattern.
//
// The handler runs on the client's delivery goroutine for this subscription --
// the same arrangement InProc has, one goroutine per subscription, and the same
// obligation: handlers must not block.
func (b *NATS) Subscribe(ctx context.Context, pattern string, fn Handler) (Subscription, error) {
	return b.subscribe(ctx, pattern, func(m *nats.Msg) {
		fn(context.Background(), m.Subject, m.Data)
	})
}

// Request publishes and waits for one reply, using NATS's own request/reply.
//
// Native rather than rebuilt on publish/subscribe, because NATS already
// provides the two hard parts: a private reply inbox per request, and a
// server-side "nobody is listening". Reimplementing those would be a second
// correlation scheme to keep correct for no benefit -- and getting the first one
// right is why InProc carries a correlation id at all.
func (b *NATS) Request(ctx context.Context, subject string, msg proto.Message, reply proto.Message) error {
	if !validSubject(subject) {
		return &SubjectError{Subject: subject}
	}

	payload, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	if b.isClosed() {
		return ErrClosed
	}

	// The payload travels in an envelope for the same reason it does on InProc:
	// a responder's error has to reach the requester, and a bare payload has
	// nowhere to put one. ReplyTo and CorrelationId stay empty here, because
	// NATS supplies both and two answers to "where does the reply go" is one of
	// them being wrong.
	out, err := proto.Marshal(&mmov1.BusEnvelope{Payload: payload})
	if err != nil {
		return err
	}

	answer, err := b.conn.RequestWithContext(ctx, subject, out)
	switch {
	case errors.Is(err, nats.ErrNoResponders):
		return fmt.Errorf("%w: %s", ErrNoResponder, subject)
	case errors.Is(err, nats.ErrTimeout),
		errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, context.Canceled):
		return fmt.Errorf("%w: %s", ErrRequestTimeout, subject)
	case err != nil:
		return b.translate(err)
	}

	var env mmov1.BusEnvelope
	if err := proto.Unmarshal(answer.Data, &env); err != nil {
		return fmt.Errorf("bus: decoding reply from %s: %w", subject, err)
	}
	if env.GetError() != "" {
		return errors.New(env.GetError())
	}
	if reply == nil {
		return nil
	}
	return proto.Unmarshal(env.GetPayload(), reply)
}

// Respond registers a handler that answers requests on a subject.
//
// A message with no reply subject is a plain publish rather than a request. It
// is still handled -- a responder is a subscriber that happens to be able to
// answer -- but there is nowhere to send the answer, which is the same
// behaviour InProc has.
func (b *NATS) Respond(ctx context.Context, pattern string, fn Responder) (Subscription, error) {
	return b.subscribe(ctx, pattern, func(m *nats.Msg) {
		var env mmov1.BusEnvelope
		if err := proto.Unmarshal(m.Data, &env); err != nil {
			return
		}

		if m.Reply == "" {
			fn(context.Background(), m.Subject, env.GetPayload())
			return
		}

		reply, err := fn(context.Background(), m.Subject, env.GetPayload())
		data, marshalErr := proto.Marshal(replyEnvelope(reply, err))
		if marshalErr != nil {
			return
		}
		m.Respond(data)
	})
}

// subscribe is the shared body of Subscribe and Respond: the two differ only in
// what they do with a message, and everything else -- the bounded backlog, the
// context ending the subscription -- has to be identical or one of them behaves
// differently under load for no stated reason.
func (b *NATS) subscribe(ctx context.Context, pattern string, deliver nats.MsgHandler) (Subscription, error) {
	if pattern == "" {
		return nil, &SubjectError{Subject: pattern}
	}
	if b.isClosed() {
		return nil, ErrClosed
	}

	sub, err := b.conn.Subscribe(pattern, deliver)
	if err != nil {
		return nil, b.translate(err)
	}
	if err := sub.SetPendingLimits(natsPendingMsgs, natsPendingBytes); err != nil {
		sub.Unsubscribe()
		return nil, b.translate(err)
	}

	// Flushed, and this is the one place a flush belongs.
	//
	// A NATS subscription is a protocol message like any other, so until it
	// reaches the server the subscription does not exist -- and a caller that
	// subscribes and then publishes would miss its own message. Every caller
	// assumes the opposite, so the postcondition has to be true: when Subscribe
	// returns, the subscription is live.
	//
	// Affordable here in a way it is not in Publish: subscribing happens when a
	// room starts or a session joins, not on every tick. Found by removing the
	// flush from Publish and watching a cross-connection test stop receiving --
	// the flush there had been establishing subscriptions by accident.
	if err := b.conn.FlushTimeout(natsFlushTimeout); err != nil {
		sub.Unsubscribe()
		return nil, b.translate(err)
	}

	wrapped := &natsSub{sub: sub, done: make(chan struct{})}

	// The context ends the subscription, matching InProc. Its own goroutine
	// because NATS has no context-aware subscribe, and it exits when the
	// subscription is closed so a long-lived context does not leak one
	// goroutine per subscription.
	if ctx != nil && ctx.Done() != nil {
		go func() {
			select {
			case <-ctx.Done():
				wrapped.Close()
			case <-wrapped.done:
			}
		}()
	}
	return wrapped, nil
}

// Dropped returns the number of slow-consumer events seen on this connection.
//
// The NATS counterpart to InProc.Dropped, and the same signal: a non-zero and
// growing value means a handler is too slow and should be handing its work off.
func (b *NATS) Dropped() uint64 { return b.dropped.load() }

// Close shuts the bus down, and the connection with it when this type opened
// it.
func (b *NATS) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	b.mu.Unlock()

	if !b.owned {
		return nil
	}

	// Drain rather than Close, so messages already published reach the server.
	// A bus that discarded its own outbound queue on shutdown would lose
	// exactly the checkpoint and hand-off traffic a graceful drain exists to
	// deliver. Drain closes the connection when it finishes.
	if err := b.conn.Drain(); err != nil {
		// Draining failed, so fall back to closing outright: leaving the
		// connection open would hold the process up at exit.
		b.conn.Close()
		return nil
	}

	// Waited for, bounded. Returning before the drain completes would mean the
	// subscriptions this bus owned are still being delivered to after Close
	// said they were not, and a caller that closed one bus and opened another
	// would have both live at once.
	select {
	case <-b.drained:
	case <-time.After(natsDrainTimeout):
		// The drain is stuck. Closing outright loses whatever is left, which is
		// worse than waiting -- but waiting forever is worse still, because it
		// is a node that cannot be shut down.
		b.conn.Close()
	}
	return nil
}

// natsDrainTimeout bounds how long Close waits for a drain.
//
// Generous, because the traffic it is waiting for is a graceful hand-off and
// losing it is the failure this exists to prevent. Bounded, because a node that
// cannot be shut down is its own outage.
const natsDrainTimeout = 10 * time.Second

func (b *NATS) isClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

// translate maps a NATS transport error onto this package's vocabulary.
//
// Only where the meaning is genuinely the same. Anything else is wrapped and
// passed up rather than flattened into a sentinel it does not mean, which is
// how a connection problem comes to look like a subject problem.
//
// Deliberately no case for ErrNoResponders: this is reached from publish and
// subscribe, where it cannot happen, and Request handles its own so it can name
// the subject -- which for a misconfigured subject is the only useful part of
// the message. A case here was unreachable and is gone.
func (b *NATS) translate(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, nats.ErrConnectionClosed), errors.Is(err, nats.ErrConnectionDraining):
		return ErrClosed
	case errors.Is(err, nats.ErrTimeout):
		return ErrRequestTimeout
	default:
		return fmt.Errorf("bus: nats: %w", err)
	}
}

// natsSub is one NATS subscription behind this package's Subscription.
type natsSub struct {
	sub  *nats.Subscription
	done chan struct{}

	once sync.Once
}

// Close unregisters the subscription. Idempotent, and safe from inside the
// subscription's own handler.
func (s *natsSub) Close() {
	s.once.Do(func() {
		close(s.done)
		// Unsubscribe rather than Drain: Close means "stop delivering to me",
		// and draining would keep calling a handler the caller has finished
		// with.
		s.sub.Unsubscribe()
	})
}

// replyEnvelope frames a responder's answer, or its refusal.
//
// Shared by both implementations, because the framing is part of the contract
// rather than a transport detail: a caller must get the same error back
// whichever bus carried it.
func replyEnvelope(reply proto.Message, err error) *mmov1.BusEnvelope {
	out := &mmov1.BusEnvelope{}
	switch {
	case err != nil:
		out.Error = err.Error()
	case reply != nil:
		payload, marshalErr := proto.Marshal(reply)
		if marshalErr != nil {
			out.Error = marshalErr.Error()
		} else {
			out.Payload = payload
		}
	}
	return out
}

var _ Bus = (*NATS)(nil)
