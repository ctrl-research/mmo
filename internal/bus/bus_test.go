package bus

import (
	"context"
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
