package bus

import (
	"context"
	"sync"
	"testing"
	"time"

	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"google.golang.org/protobuf/proto"
)

// Subject matching, and the two properties that belong to InProc alone.
//
// Everything a caller can rely on lives in conformance_test.go, asserted
// against every implementation. What stays here is either a pure function or a
// property of this transport rather than a promise to callers -- dropping
// rather than blocking, and the subscription map's own locking. Adding a
// behavioural test here instead of there is how the two implementations come to
// disagree without anyone noticing.

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

// Registering and unregistering subscriptions while publishing must not race.
//
// InProc-specific because the race it guards is InProc's own: a map of
// subscriptions mutated by Subscribe and Close while Publish walks it. NATS
// keeps that bookkeeping in its client, which has its own tests for it.
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

func mustMarshal(m proto.Message) []byte {
	b, err := proto.Marshal(m)
	if err != nil {
		panic(err)
	}
	return b
}
