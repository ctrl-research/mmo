package bus

import (
	"context"
	"sync"

	"google.golang.org/protobuf/proto"
)

// InProc is a Bus backed by Go channels, for when every role runs in one
// process. It is the hobby-scale implementation and the one used by tests.
//
// Each subscription owns a goroutine and a buffered queue. Publish marshals
// once and hands the same bytes to every match, so fan-out costs one
// marshalling regardless of subscriber count.
//
// A subscriber whose queue is full has its message dropped rather than
// blocking the publisher. Blocking would let one slow consumer stall the tick
// loop, which is a far worse failure than a lost message on a bus that is
// explicitly best-effort. Drops are counted so the condition is visible rather
// than silent.
type InProc struct {
	mu     sync.RWMutex
	subs   map[uint64]*inProcSub
	nextID uint64
	closed bool

	dropped atomic64
}

type inProcSub struct {
	id      uint64
	pattern string
	fn      Handler
	queue   chan busMsg
	done    chan struct{}
	bus     *InProc

	closeOnce sync.Once
}

type busMsg struct {
	ctx     context.Context
	subject string
	payload []byte
}

// inProcQueueDepth is how many messages may be pending for one subscriber.
//
// Deep enough to absorb a burst -- a room announcing many entities at once --
// but shallow enough that a wedged consumer is noticed quickly instead of
// accumulating a large backlog of stale messages it will process far too late.
const inProcQueueDepth = 256

// NewInProc returns a ready in-process bus.
func NewInProc() *InProc {
	return &InProc{subs: make(map[uint64]*inProcSub)}
}

// Publish marshals msg once and delivers it to every matching subscriber.
func (b *InProc) Publish(ctx context.Context, subject string, msg proto.Message) error {
	if !validSubject(subject) {
		return &SubjectError{Subject: subject}
	}

	payload, err := proto.Marshal(msg)
	if err != nil {
		return err
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.closed {
		return ErrClosed
	}

	for _, sub := range b.subs {
		if !matchSubject(sub.pattern, subject) {
			continue
		}
		select {
		case sub.queue <- busMsg{ctx: ctx, subject: subject, payload: payload}:
		default:
			// See the type comment: dropping beats blocking the publisher.
			b.dropped.add(1)
		}
	}
	return nil
}

// Subscribe registers fn for messages matching pattern.
func (b *InProc) Subscribe(ctx context.Context, pattern string, fn Handler) (Subscription, error) {
	if pattern == "" {
		return nil, &SubjectError{Subject: pattern}
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, ErrClosed
	}
	b.nextID++
	sub := &inProcSub{
		id:      b.nextID,
		pattern: pattern,
		fn:      fn,
		queue:   make(chan busMsg, inProcQueueDepth),
		done:    make(chan struct{}),
		bus:     b,
	}
	b.subs[sub.id] = sub
	b.mu.Unlock()

	go sub.run(ctx)
	return sub, nil
}

// Dropped returns the number of messages discarded because a subscriber's
// queue was full. A non-zero and growing value means a handler is too slow and
// should be doing its work elsewhere.
func (b *InProc) Dropped() uint64 { return b.dropped.load() }

// Close shuts down the bus and every subscription.
func (b *InProc) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	subs := make([]*inProcSub, 0, len(b.subs))
	for _, s := range b.subs {
		subs = append(subs, s)
	}
	b.subs = make(map[uint64]*inProcSub)
	b.mu.Unlock()

	for _, s := range subs {
		s.stop()
	}
	return nil
}

func (s *inProcSub) run(ctx context.Context) {
	for {
		select {
		case <-s.done:
			return
		case <-ctx.Done():
			s.Close()
			return
		case m := <-s.queue:
			s.fn(m.ctx, m.subject, m.payload)
		}
	}
}

// Close unregisters the subscription. It is safe to call more than once and
// safe to call from inside the subscription's own handler.
func (s *inProcSub) Close() {
	s.bus.mu.Lock()
	delete(s.bus.subs, s.id)
	s.bus.mu.Unlock()
	s.stop()
}

func (s *inProcSub) stop() {
	s.closeOnce.Do(func() { close(s.done) })
}

// SubjectError reports an unusable subject: empty, containing an empty token,
// or -- when publishing -- containing a wildcard.
type SubjectError struct {
	Subject string
}

func (e *SubjectError) Error() string {
	return "bus: invalid subject " + quote(e.Subject)
}

func quote(s string) string { return `"` + s + `"` }
