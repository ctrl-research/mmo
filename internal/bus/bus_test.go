package bus

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"google.golang.org/protobuf/proto"
)

func TestMatchSubject(t *testing.T) {
	tests := []struct {
		pattern, subject string
		want             bool
	}{
		// Exact.
		{"room.42.input", "room.42.input", true},
		{"room.42.input", "room.43.input", false},

		// Single-token wildcard.
		{"room.*.input", "room.42.input", true},
		{"room.*.input", "room.42.output", false},
		{"room.*.input", "room.42.extra.input", false},
		{"*.42.input", "room.42.input", true},

		// Multi-token wildcard.
		{"room.>", "room.42.input", true},
		{"room.>", "room.42", true},
		{"chat.>", "chat.guild.7.message", true},
		{"room.>", "room", false}, // ">" must match at least one token
		{"room.>", "world.42", false},

		// Combined.
		{"room.*.>", "room.42.a.b.c", true},

		// Whole-token matching, not prefix matching.
		{"room.4", "room.42", false},
		{"room", "room.42", false},
		{"room.42.input", "room.42", false},
	}
	for _, tt := range tests {
		if got := matchSubject(tt.pattern, tt.subject); got != tt.want {
			t.Errorf("matchSubject(%q, %q) = %v, want %v", tt.pattern, tt.subject, got, tt.want)
		}
	}
}

func TestValidSubject(t *testing.T) {
	valid := []string{"room", "room.42", "room.42.input", "chat.guild.7"}
	for _, s := range valid {
		if !validSubject(s) {
			t.Errorf("validSubject(%q) = false, want true", s)
		}
	}
	// Publishing to a wildcard delivers nothing; catching it here turns a
	// silent no-op into an error at the call site.
	invalid := []string{"", "room.", ".room", "room..input", "room.*", "room.>", "*", ">"}
	for _, s := range invalid {
		if validSubject(s) {
			t.Errorf("validSubject(%q) = true, want false", s)
		}
	}
}

// collector accumulates delivered payloads for assertions.
type collector struct {
	mu   sync.Mutex
	got  [][]byte
	subj []string
	ch   chan struct{}
}

func newCollector() *collector { return &collector{ch: make(chan struct{}, 64)} }

func (c *collector) handle(_ context.Context, subject string, payload []byte) {
	c.mu.Lock()
	c.got = append(c.got, payload)
	c.subj = append(c.subj, subject)
	c.mu.Unlock()
	c.ch <- struct{}{}
}

func (c *collector) wait(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for i := 0; i < n; i++ {
		select {
		case <-c.ch:
		case <-deadline:
			c.mu.Lock()
			have := len(c.got)
			c.mu.Unlock()
			t.Fatalf("timed out waiting for %d messages, have %d", n, have)
		}
	}
}

func (c *collector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.got)
}

func testMsg(id uint32) *mmov1.PlayerLeft { return &mmov1.PlayerLeft{EntityId: id} }

func TestPublishDeliversToMatchingSubscriber(t *testing.T) {
	b := NewInProc()
	defer b.Close()
	ctx := context.Background()

	c := newCollector()
	if _, err := b.Subscribe(ctx, "room.42.input", c.handle); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	if err := b.Publish(ctx, "room.42.input", testMsg(7)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	c.wait(t, 1)

	var got mmov1.PlayerLeft
	c.mu.Lock()
	err := proto.Unmarshal(c.got[0], &got)
	c.mu.Unlock()
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.GetEntityId() != 7 {
		t.Errorf("entity id = %d, want 7", got.GetEntityId())
	}
}

func TestPublishSkipsNonMatchingSubscriber(t *testing.T) {
	b := NewInProc()
	defer b.Close()
	ctx := context.Background()

	match, other := newCollector(), newCollector()
	b.Subscribe(ctx, "room.42.>", match.handle)
	b.Subscribe(ctx, "room.99.>", other.handle)

	b.Publish(ctx, "room.42.input", testMsg(1))
	match.wait(t, 1)

	// Give a wrong delivery time to show up before asserting it did not.
	time.Sleep(50 * time.Millisecond)
	if n := other.count(); n != 0 {
		t.Errorf("non-matching subscriber received %d messages, want 0", n)
	}
}

func TestFanOutToMultipleSubscribers(t *testing.T) {
	b := NewInProc()
	defer b.Close()
	ctx := context.Background()

	cs := []*collector{newCollector(), newCollector(), newCollector()}
	for _, c := range cs {
		b.Subscribe(ctx, "room.>", c.handle)
	}

	b.Publish(ctx, "room.42.input", testMsg(1))
	for i, c := range cs {
		c.wait(t, 1)
		if c.count() != 1 {
			t.Errorf("subscriber %d got %d messages, want 1", i, c.count())
		}
	}
}

func TestSubscriptionCloseStopsDelivery(t *testing.T) {
	b := NewInProc()
	defer b.Close()
	ctx := context.Background()

	c := newCollector()
	sub, err := b.Subscribe(ctx, "room.>", c.handle)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	b.Publish(ctx, "room.1", testMsg(1))
	c.wait(t, 1)

	sub.Close()
	b.Publish(ctx, "room.2", testMsg(2))
	time.Sleep(50 * time.Millisecond)

	if n := c.count(); n != 1 {
		t.Errorf("received %d messages after close, want 1", n)
	}
}

func TestSubscriptionCloseIsIdempotent(t *testing.T) {
	b := NewInProc()
	defer b.Close()

	sub, _ := b.Subscribe(context.Background(), "room.>", func(context.Context, string, []byte) {})
	sub.Close()
	sub.Close() // must not panic on a double close
}

func TestPublishToWildcardIsAnError(t *testing.T) {
	b := NewInProc()
	defer b.Close()

	for _, subject := range []string{"room.*", "room.>", ""} {
		if err := b.Publish(context.Background(), subject, testMsg(1)); err == nil {
			t.Errorf("publishing to %q should be an error", subject)
		}
	}
}

func TestOperationsAfterCloseFail(t *testing.T) {
	b := NewInProc()
	ctx := context.Background()
	b.Close()

	if err := b.Publish(ctx, "room.1", testMsg(1)); err != ErrClosed {
		t.Errorf("publish after close = %v, want ErrClosed", err)
	}
	if _, err := b.Subscribe(ctx, "room.>", func(context.Context, string, []byte) {}); err != ErrClosed {
		t.Errorf("subscribe after close = %v, want ErrClosed", err)
	}
	if err := b.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
}

// A stalled handler must not block the publisher. The tick loop publishes, and
// stalling it because one subscriber is slow would be a far worse failure than
// dropping a message on a best-effort bus.
func TestSlowSubscriberDoesNotBlockPublisher(t *testing.T) {
	b := NewInProc()
	defer b.Close()
	ctx := context.Background()

	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	b.Subscribe(ctx, "room.>", func(context.Context, string, []byte) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
	})
	<-time.After(20 * time.Millisecond)

	b.Publish(ctx, "room.1", testMsg(1))
	<-entered // handler is now wedged

	done := make(chan struct{})
	go func() {
		for i := 0; i < inProcQueueDepth*3; i++ {
			b.Publish(ctx, "room.1", testMsg(uint32(i)))
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publisher blocked behind a stalled subscriber")
	}

	if b.Dropped() == 0 {
		t.Error("expected dropped messages to be counted when the queue overflows")
	}
	close(release)
}

func TestContextCancellationEndsSubscription(t *testing.T) {
	b := NewInProc()
	defer b.Close()

	ctx, cancel := context.WithCancel(context.Background())
	c := newCollector()
	b.Subscribe(ctx, "room.>", c.handle)

	b.Publish(context.Background(), "room.1", testMsg(1))
	c.wait(t, 1)

	cancel()
	time.Sleep(50 * time.Millisecond)

	b.Publish(context.Background(), "room.2", testMsg(2))
	time.Sleep(50 * time.Millisecond)

	if n := c.count(); n != 1 {
		t.Errorf("received %d messages after cancellation, want 1", n)
	}
}

func TestConcurrentPublishAndSubscribe(t *testing.T) {
	b := NewInProc()
	defer b.Close()
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				b.Publish(ctx, "room.1.input", testMsg(uint32(j)))
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				sub, err := b.Subscribe(ctx, "room.>", func(context.Context, string, []byte) {})
				if err == nil {
					sub.Close()
				}
			}
		}()
	}
	wg.Wait()
}

// --- request and reply ------------------------------------------------------

func TestRequestGetsAReply(t *testing.T) {
	b := NewInProc()
	defer b.Close()
	ctx := context.Background()

	sub, err := b.Respond(ctx, "world.transfer", func(_ context.Context, _ string, payload []byte) (proto.Message, error) {
		var req mmov1.PlayerLeft
		if err := proto.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		return &mmov1.PlayerJoined{EntityId: req.GetEntityId() * 2}, nil
	})
	if err != nil {
		t.Fatalf("respond: %v", err)
	}
	defer sub.Close()

	var reply mmov1.PlayerJoined
	if err := b.Request(ctx, "world.transfer", &mmov1.PlayerLeft{EntityId: 21}, &reply); err != nil {
		t.Fatalf("request: %v", err)
	}
	if reply.GetEntityId() != 42 {
		t.Errorf("reply carried %d, want 42", reply.GetEntityId())
	}
}

// A refusal must arrive immediately rather than as a timeout: "the destination
// rejected this" and "something is wrong with the cluster" call for completely
// different responses.
func TestResponderErrorReachesTheRequester(t *testing.T) {
	b := NewInProc()
	defer b.Close()
	ctx := context.Background()

	sub, _ := b.Respond(ctx, "world.transfer", func(context.Context, string, []byte) (proto.Message, error) {
		return nil, errors.New("destination is full")
	})
	defer sub.Close()

	start := time.Now()
	err := b.Request(ctx, "world.transfer", &mmov1.PlayerLeft{EntityId: 1}, &mmov1.PlayerJoined{})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a responder error did not reach the requester")
	}
	if !strings.Contains(err.Error(), "destination is full") {
		t.Errorf("error = %q, want the responder's own message", err)
	}
	if elapsed > time.Second {
		t.Errorf("the refusal took %v; it should be immediate, not a timeout", elapsed)
	}
}

// Nothing listening is a different problem from a slow listener -- usually a
// misconfigured subject or a node that never started.
func TestRequestWithNoResponder(t *testing.T) {
	b := NewInProc()
	defer b.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	err := b.Request(ctx, "world.nobody.listening", &mmov1.PlayerLeft{}, &mmov1.PlayerJoined{})

	if !errors.Is(err, ErrNoResponder) {
		t.Errorf("error = %v, want ErrNoResponder", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %v to report that nothing was listening", elapsed)
	}
}

func TestRequestTimesOut(t *testing.T) {
	b := NewInProc()
	defer b.Close()

	// A responder that never answers.
	sub, _ := b.Respond(context.Background(), "world.slow", func(ctx context.Context, _ string, _ []byte) (proto.Message, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	defer sub.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	if err := b.Request(ctx, "world.slow", &mmov1.PlayerLeft{}, &mmov1.PlayerJoined{}); !errors.Is(err, ErrRequestTimeout) {
		t.Errorf("error = %v, want ErrRequestTimeout", err)
	}
}

// A reply arriving after its request timed out must not be mistaken for the
// answer to the next request on the same inbox.
func TestLateReplyIsNotMistakenForTheNextOne(t *testing.T) {
	b := NewInProc()
	defer b.Close()

	release := make(chan struct{})
	sub, _ := b.Respond(context.Background(), "world.lagging", func(context.Context, string, []byte) (proto.Message, error) {
		<-release
		return &mmov1.PlayerJoined{EntityId: 111}, nil
	})
	defer sub.Close()

	// The first request gives up.
	first, cancelFirst := context.WithTimeout(context.Background(), 150*time.Millisecond)
	err := b.Request(first, "world.lagging", &mmov1.PlayerLeft{}, &mmov1.PlayerJoined{})
	cancelFirst()
	if !errors.Is(err, ErrRequestTimeout) {
		t.Fatalf("first request: %v, want a timeout", err)
	}

	// Its answer arrives late, into an inbox nobody is reading.
	close(release)
	time.Sleep(100 * time.Millisecond)

	// A second request gets its own answer, not the stale one.
	sub2, _ := b.Respond(context.Background(), "world.fresh", func(context.Context, string, []byte) (proto.Message, error) {
		return &mmov1.PlayerJoined{EntityId: 222}, nil
	})
	defer sub2.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var reply mmov1.PlayerJoined
	if err := b.Request(ctx, "world.fresh", &mmov1.PlayerLeft{}, &reply); err != nil {
		t.Fatalf("second request: %v", err)
	}
	if reply.GetEntityId() != 222 {
		t.Errorf("second request got %d, want 222", reply.GetEntityId())
	}
}

func TestConcurrentRequestsGetTheirOwnReplies(t *testing.T) {
	b := NewInProc()
	defer b.Close()
	ctx := context.Background()

	sub, _ := b.Respond(ctx, "world.echo", func(_ context.Context, _ string, payload []byte) (proto.Message, error) {
		var req mmov1.PlayerLeft
		proto.Unmarshal(payload, &req)
		return &mmov1.PlayerJoined{EntityId: req.GetEntityId()}, nil
	})
	defer sub.Close()

	const requests = 50
	var wg sync.WaitGroup
	mismatches := make(chan string, requests)

	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(n uint32) {
			defer wg.Done()

			reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()

			var reply mmov1.PlayerJoined
			if err := b.Request(reqCtx, "world.echo", &mmov1.PlayerLeft{EntityId: n}, &reply); err != nil {
				mismatches <- err.Error()
				return
			}
			if reply.GetEntityId() != n {
				mismatches <- "reply crossed between concurrent requests"
			}
		}(uint32(i + 1))
	}
	wg.Wait()
	close(mismatches)

	for m := range mismatches {
		t.Error(m)
	}
}

// A responder is also a subscriber, so a plain publish must still reach it
// rather than being dropped for lacking a reply subject.
func TestPlainPublishReachesAResponder(t *testing.T) {
	b := NewInProc()
	defer b.Close()
	ctx := context.Background()

	got := make(chan uint32, 1)
	sub, _ := b.Respond(ctx, "world.notify", func(_ context.Context, _ string, payload []byte) (proto.Message, error) {
		var msg mmov1.PlayerLeft
		proto.Unmarshal(payload, &msg)
		select {
		case got <- msg.GetEntityId():
		default:
		}
		return nil, nil
	})
	defer sub.Close()

	// Wrapped the way Request wraps, but with no reply subject.
	b.Publish(ctx, "world.notify", &mmov1.BusEnvelope{Payload: mustMarshal(&mmov1.PlayerLeft{EntityId: 7})})

	select {
	case id := <-got:
		if id != 7 {
			t.Errorf("responder received entity %d, want 7", id)
		}
	case <-time.After(2 * time.Second):
		t.Error("a plain publish never reached the responder")
	}
}

func mustMarshal(m proto.Message) []byte {
	b, err := proto.Marshal(m)
	if err != nil {
		panic(err)
	}
	return b
}
