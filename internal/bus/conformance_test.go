package bus

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"
)

// The bus contract, run against every implementation.
//
// This file exists because a second implementation is only useful if it is
// genuinely interchangeable, and "genuinely" is not something a separate test
// file per implementation can establish -- two suites drift, and the drift is
// invisible until the day roles are split across nodes and a subject that
// worked in one process stops working. Every behaviour anything above this
// package relies on is asserted here, once, against both.
//
// What is deliberately *not* here: how each implementation achieves it. InProc
// drops messages when a queue fills and counts them; NATS bounds a
// subscription's backlog and the server reports a slow consumer. Those live in
// bus_test.go and nats_test.go respectively, because they are properties of the
// transport rather than promises to callers.

// busUnderTest names one implementation and how to open a fresh one.
type busUnderTest struct {
	name string
	open func(t *testing.T) Bus
}

// implementations returns every bus available to test.
//
// InProc always. NATS only when MMO_TEST_NATS_URL points at a server, the same
// arrangement the store tests use for Postgres: the behaviour that matters is
// the server's, and a fake would assert that the Go code calls the client
// methods it already calls.
func implementations() []busUnderTest {
	impls := []busUnderTest{{
		name: "inproc",
		open: func(t *testing.T) Bus {
			t.Helper()
			b := NewInProc()
			t.Cleanup(func() { b.Close() })
			return b
		},
	}}

	if url := os.Getenv("MMO_TEST_NATS_URL"); url != "" {
		impls = append(impls, busUnderTest{
			name: "nats",
			open: func(t *testing.T) Bus {
				t.Helper()
				b, err := Connect(url)
				if err != nil {
					t.Fatalf("connect to nats at %s: %v", url, err)
				}
				t.Cleanup(func() { b.Close() })
				return b
			},
		})
	}
	return impls
}

// eachBus runs fn against every implementation as its own subtest.
func eachBus(t *testing.T, fn func(t *testing.T, b Bus)) {
	t.Helper()
	for _, impl := range implementations() {
		t.Run(impl.name, func(t *testing.T) { fn(t, impl.open(t)) })
	}
}

// settle gives a wrong delivery time to show up before a test asserts it did
// not. Long enough for a network round trip, because one of the buses has one.
const settle = 150 * time.Millisecond

// --- publish and subscribe --------------------------------------------------

func TestBusPublishDeliversToMatchingSubscriber(t *testing.T) {
	eachBus(t, func(t *testing.T, b Bus) {
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
	})
}

func TestBusPublishSkipsNonMatchingSubscriber(t *testing.T) {
	eachBus(t, func(t *testing.T, b Bus) {
		ctx := context.Background()

		match, other := newCollector(), newCollector()
		b.Subscribe(ctx, "room.42.>", match.handle)
		b.Subscribe(ctx, "room.99.>", other.handle)

		b.Publish(ctx, "room.42.input", testMsg(1))
		match.wait(t, 1)

		time.Sleep(settle)
		if n := other.count(); n != 0 {
			t.Errorf("non-matching subscriber received %d messages, want 0", n)
		}
	})
}

func TestBusFanOutToMultipleSubscribers(t *testing.T) {
	eachBus(t, func(t *testing.T, b Bus) {
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
	})
}

// The wildcards route the same on both, which is the whole reason subject.go
// follows NATS's grammar rather than inventing one.
func TestBusWildcardsRouteIdentically(t *testing.T) {
	cases := []struct {
		pattern string
		subject string
		want    bool
	}{
		{"room.*", "room.42", true},
		{"room.*", "room.42.input", false},
		{"room.*.input", "room.42.input", true},
		{"room.>", "room.42.input", true},
		{"room.>", "room", false},
		{"chat.guild.7", "chat.guild.7", true},
		{"chat.guild.7", "chat.guild.8", false},
	}

	eachBus(t, func(t *testing.T, b Bus) {
		ctx := context.Background()

		for _, tc := range cases {
			c := newCollector()
			sub, err := b.Subscribe(ctx, tc.pattern, c.handle)
			if err != nil {
				t.Fatalf("subscribe %q: %v", tc.pattern, err)
			}

			if err := b.Publish(ctx, tc.subject, testMsg(1)); err != nil {
				t.Fatalf("publish %q: %v", tc.subject, err)
			}

			if tc.want {
				c.wait(t, 1)
			} else {
				time.Sleep(settle)
				if n := c.count(); n != 0 {
					t.Errorf("pattern %q received %q, want no match", tc.pattern, tc.subject)
				}
			}
			sub.Close()
		}
	})
}

func TestBusSubscriptionCloseStopsDelivery(t *testing.T) {
	eachBus(t, func(t *testing.T, b Bus) {
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
		time.Sleep(settle)

		if n := c.count(); n != 1 {
			t.Errorf("received %d messages after close, want 1", n)
		}
	})
}

func TestBusSubscriptionCloseIsIdempotent(t *testing.T) {
	eachBus(t, func(t *testing.T, b Bus) {
		sub, err := b.Subscribe(context.Background(), "room.>", func(context.Context, string, []byte) {})
		if err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		sub.Close()
		sub.Close() // must not panic on a double close
	})
}

// Publishing to a wildcard is always a bug, and one that silently delivers
// nothing if it is not caught.
func TestBusPublishToWildcardIsAnError(t *testing.T) {
	eachBus(t, func(t *testing.T, b Bus) {
		for _, subject := range []string{"room.*", "room.>", "", "room..input"} {
			if err := b.Publish(context.Background(), subject, testMsg(1)); err == nil {
				t.Errorf("publishing to %q should be an error", subject)
			}
		}
	})
}

// An empty pattern subscribes to nothing and cannot be intended.
//
// The error *type* is asserted, not just its presence. A caller distinguishing
// "this subject is wrong" from "the cluster is unreachable" does it by type, and
// a transport that reported a bad subject as a connection failure would send
// somebody looking at the wrong thing. Both implementations owe the same answer.
func TestBusSubscribeToAnEmptyPatternIsASubjectError(t *testing.T) {
	eachBus(t, func(t *testing.T, b Bus) {
		var want *SubjectError

		_, err := b.Subscribe(context.Background(), "", func(context.Context, string, []byte) {})
		if !errors.As(err, &want) {
			t.Errorf("subscribing to an empty pattern = %v, want a *SubjectError", err)
		}

		_, err = b.Respond(context.Background(), "", func(context.Context, string, []byte) (proto.Message, error) {
			return nil, nil
		})
		if !errors.As(err, &want) {
			t.Errorf("responding on an empty pattern = %v, want a *SubjectError", err)
		}
	})
}

func TestBusOperationsAfterCloseFail(t *testing.T) {
	for _, impl := range implementations() {
		t.Run(impl.name, func(t *testing.T) {
			b := impl.open(t)
			ctx := context.Background()
			b.Close()

			if err := b.Publish(ctx, "room.1", testMsg(1)); !errors.Is(err, ErrClosed) {
				t.Errorf("publish after close = %v, want ErrClosed", err)
			}
			if _, err := b.Subscribe(ctx, "room.>", func(context.Context, string, []byte) {}); !errors.Is(err, ErrClosed) {
				t.Errorf("subscribe after close = %v, want ErrClosed", err)
			}
			if err := b.Close(); err != nil {
				t.Errorf("second Close = %v, want nil", err)
			}
		})
	}
}

func TestBusContextCancellationEndsSubscription(t *testing.T) {
	eachBus(t, func(t *testing.T, b Bus) {
		ctx, cancel := context.WithCancel(context.Background())
		c := newCollector()
		if _, err := b.Subscribe(ctx, "room.>", c.handle); err != nil {
			t.Fatalf("subscribe: %v", err)
		}

		b.Publish(context.Background(), "room.1", testMsg(1))
		c.wait(t, 1)

		cancel()
		time.Sleep(settle)

		b.Publish(context.Background(), "room.2", testMsg(2))
		time.Sleep(settle)

		if n := c.count(); n != 1 {
			t.Errorf("received %d messages after cancellation, want 1", n)
		}
	})
}

// --- request and reply ------------------------------------------------------

func TestBusRequestGetsAReply(t *testing.T) {
	eachBus(t, func(t *testing.T, b Bus) {
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

		reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

		var reply mmov1.PlayerJoined
		if err := b.Request(reqCtx, "world.transfer", &mmov1.PlayerLeft{EntityId: 21}, &reply); err != nil {
			t.Fatalf("request: %v", err)
		}
		if reply.GetEntityId() != 42 {
			t.Errorf("reply carried %d, want 42", reply.GetEntityId())
		}
	})
}

// A refusal must arrive immediately rather than as a timeout: "the destination
// rejected this" and "something is wrong with the cluster" call for completely
// different responses.
func TestBusResponderErrorReachesTheRequester(t *testing.T) {
	eachBus(t, func(t *testing.T, b Bus) {
		ctx := context.Background()

		sub, err := b.Respond(ctx, "world.transfer", func(context.Context, string, []byte) (proto.Message, error) {
			return nil, errors.New("destination is full")
		})
		if err != nil {
			t.Fatalf("respond: %v", err)
		}
		defer sub.Close()

		reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

		start := time.Now()
		err = b.Request(reqCtx, "world.transfer", &mmov1.PlayerLeft{EntityId: 1}, &mmov1.PlayerJoined{})
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
	})
}

// Nothing listening is a different problem from a slow listener -- usually a
// misconfigured subject or a node that never started -- and it has to be
// reported as one rather than as a timeout.
func TestBusRequestWithNoResponder(t *testing.T) {
	eachBus(t, func(t *testing.T, b Bus) {
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
	})
}

func TestBusRequestTimesOut(t *testing.T) {
	eachBus(t, func(t *testing.T, b Bus) {
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })

		sub, err := b.Respond(context.Background(), "world.slow", func(context.Context, string, []byte) (proto.Message, error) {
			<-release
			return &mmov1.PlayerJoined{}, nil
		})
		if err != nil {
			t.Fatalf("respond: %v", err)
		}
		defer sub.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()

		if err := b.Request(ctx, "world.slow", &mmov1.PlayerLeft{}, &mmov1.PlayerJoined{}); !errors.Is(err, ErrRequestTimeout) {
			t.Errorf("error = %v, want ErrRequestTimeout", err)
		}
	})
}

// A reply arriving after its request timed out must not be mistaken for the
// answer to the next one.
//
// InProc needs a correlation id to get this right, because its inboxes are its
// own; NATS gets it from a fresh reply subject per request. The contract is the
// same either way, which is why it is asserted here rather than only against
// the one that had to work for it.
func TestBusLateReplyIsNotMistakenForTheNextOne(t *testing.T) {
	eachBus(t, func(t *testing.T, b Bus) {
		release := make(chan struct{})
		sub, err := b.Respond(context.Background(), "world.lagging", func(context.Context, string, []byte) (proto.Message, error) {
			<-release
			return &mmov1.PlayerJoined{EntityId: 111}, nil
		})
		if err != nil {
			t.Fatalf("respond: %v", err)
		}
		defer sub.Close()

		// The first request gives up.
		first, cancelFirst := context.WithTimeout(context.Background(), 250*time.Millisecond)
		err = b.Request(first, "world.lagging", &mmov1.PlayerLeft{}, &mmov1.PlayerJoined{})
		cancelFirst()
		if !errors.Is(err, ErrRequestTimeout) {
			t.Fatalf("first request: %v, want a timeout", err)
		}

		// Its answer arrives late, into an inbox nobody is reading.
		close(release)
		time.Sleep(settle)

		// A second request gets its own answer, not the stale one.
		sub2, err := b.Respond(context.Background(), "world.fresh", func(context.Context, string, []byte) (proto.Message, error) {
			return &mmov1.PlayerJoined{EntityId: 222}, nil
		})
		if err != nil {
			t.Fatalf("respond: %v", err)
		}
		defer sub2.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		var reply mmov1.PlayerJoined
		if err := b.Request(ctx, "world.fresh", &mmov1.PlayerLeft{}, &reply); err != nil {
			t.Fatalf("second request: %v", err)
		}
		if reply.GetEntityId() != 222 {
			t.Errorf("second request got %d, want 222", reply.GetEntityId())
		}
	})
}

func TestBusConcurrentRequestsGetTheirOwnReplies(t *testing.T) {
	eachBus(t, func(t *testing.T, b Bus) {
		ctx := context.Background()

		sub, err := b.Respond(ctx, "world.echo", func(_ context.Context, _ string, payload []byte) (proto.Message, error) {
			var req mmov1.PlayerLeft
			proto.Unmarshal(payload, &req)
			return &mmov1.PlayerJoined{EntityId: req.GetEntityId()}, nil
		})
		if err != nil {
			t.Fatalf("respond: %v", err)
		}
		defer sub.Close()

		const requests = 50
		var wg sync.WaitGroup
		problems := make(chan string, requests)

		for i := 0; i < requests; i++ {
			wg.Add(1)
			go func(n uint32) {
				defer wg.Done()

				reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()

				var reply mmov1.PlayerJoined
				if err := b.Request(reqCtx, "world.echo", &mmov1.PlayerLeft{EntityId: n}, &reply); err != nil {
					problems <- err.Error()
					return
				}
				if reply.GetEntityId() != n {
					problems <- "reply crossed between concurrent requests"
				}
			}(uint32(i + 1))
		}
		wg.Wait()
		close(problems)

		for m := range problems {
			t.Error(m)
		}
	})
}

// A responder is also a subscriber, so a plain publish must still reach it
// rather than being dropped for lacking a reply subject.
func TestBusPlainPublishReachesAResponder(t *testing.T) {
	eachBus(t, func(t *testing.T, b Bus) {
		ctx := context.Background()

		got := make(chan uint32, 1)
		sub, err := b.Respond(ctx, "world.notify", func(_ context.Context, _ string, payload []byte) (proto.Message, error) {
			var msg mmov1.PlayerLeft
			proto.Unmarshal(payload, &msg)
			select {
			case got <- msg.GetEntityId():
			default:
			}
			return nil, nil
		})
		if err != nil {
			t.Fatalf("respond: %v", err)
		}
		defer sub.Close()

		// Wrapped the way Request wraps, but with no reply subject.
		b.Publish(ctx, "world.notify", &mmov1.BusEnvelope{Payload: mustMarshal(&mmov1.PlayerLeft{EntityId: 7})})

		select {
		case id := <-got:
			if id != 7 {
				t.Errorf("responder received entity %d, want 7", id)
			}
		case <-time.After(5 * time.Second):
			t.Error("a plain publish never reached the responder")
		}
	})
}

// --- the NATS implementation's own properties -------------------------------

// A wrapped connection is the caller's to close.
//
// Closing one this package did not open is how a component that shares a
// connection takes every other user of it offline.
func TestNATSDoesNotCloseABorrowedConnection(t *testing.T) {
	url := os.Getenv("MMO_TEST_NATS_URL")
	if url == "" {
		t.Skip("MMO_TEST_NATS_URL is not set")
	}

	conn, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()

	b := NewNATS(conn)
	if err := b.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if conn.IsClosed() {
		t.Error("closing the bus closed a connection it did not open")
	}
	// And the connection is still usable, which is the point.
	if err := conn.Publish("still.working", nil); err != nil {
		t.Errorf("borrowed connection is unusable after the bus closed: %v", err)
	}

	// But the *bus* is closed, and has to say so. This is the case that makes
	// the closed check in Publish and Subscribe load-bearing rather than
	// redundant with the connection's own: an owned connection reports itself
	// closed, and a borrowed one never will.
	ctx := context.Background()
	if err := b.Publish(ctx, "room.1", testMsg(1)); !errors.Is(err, ErrClosed) {
		t.Errorf("publish on a closed bus with a live connection = %v, want ErrClosed", err)
	}
	if _, err := b.Subscribe(ctx, "room.>", func(context.Context, string, []byte) {}); !errors.Is(err, ErrClosed) {
		t.Errorf("subscribe on a closed bus with a live connection = %v, want ErrClosed", err)
	}
}

// Two connections to the same server are two nodes as far as the game is
// concerned, and a message published on one has to reach a subscriber on the
// other. This is the property the whole milestone rests on and the one thing
// InProc cannot demonstrate.
func TestNATSCarriesMessagesBetweenConnections(t *testing.T) {
	url := os.Getenv("MMO_TEST_NATS_URL")
	if url == "" {
		t.Skip("MMO_TEST_NATS_URL is not set")
	}

	publisher, err := Connect(url)
	if err != nil {
		t.Fatalf("connect publisher: %v", err)
	}
	defer publisher.Close()

	subscriber, err := Connect(url)
	if err != nil {
		t.Fatalf("connect subscriber: %v", err)
	}
	defer subscriber.Close()

	ctx := context.Background()
	c := newCollector()
	if _, err := subscriber.Subscribe(ctx, "room.>", c.handle); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	if err := publisher.Publish(ctx, "room.42.input", testMsg(9)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	c.wait(t, 1)

	// And a request on one connection is answered by a responder on the other,
	// which is what character transfer between nodes actually needs.
	sub, err := subscriber.Respond(ctx, "world.transfer", func(context.Context, string, []byte) (proto.Message, error) {
		return &mmov1.PlayerJoined{EntityId: 77}, nil
	})
	if err != nil {
		t.Fatalf("respond: %v", err)
	}
	defer sub.Close()

	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var reply mmov1.PlayerJoined
	if err := publisher.Request(reqCtx, "world.transfer", &mmov1.PlayerLeft{}, &reply); err != nil {
		t.Fatalf("cross-connection request: %v", err)
	}
	if reply.GetEntityId() != 77 {
		t.Errorf("reply carried %d, want 77", reply.GetEntityId())
	}
}

// A responder can be told something without being asked anything.
//
// Half of the room protocol is one-way -- input arrives every client tick and
// a round trip per keypress is not an option -- while the other half needs an
// answer. Both go to one responder, so a one-way send has to reach it.
//
// The trap this guards is that a plain Publish of the payload does not fail,
// it is *reinterpreted*: Respond decodes every message as an envelope, and any
// message decodes as some envelope, so the handler runs with an empty payload
// and the caller sees a successful send. That silence is why Notify exists and
// why this is asserted rather than assumed.
func TestBusNotifyReachesAResponder(t *testing.T) {
	eachBus(t, func(t *testing.T, b Bus) {
		ctx := context.Background()

		got := make(chan string, 1)
		sub, err := b.Respond(ctx, "notify.subject",
			func(_ context.Context, _ string, payload []byte) (proto.Message, error) {
				var msg mmov1.RoomClosed
				if err := proto.Unmarshal(payload, &msg); err != nil {
					return nil, err
				}
				got <- msg.GetMapId()
				return nil, nil
			})
		if err != nil {
			t.Fatalf("respond: %v", err)
		}
		defer sub.Close()

		if err := Notify(ctx, b, "notify.subject", &mmov1.RoomClosed{MapId: "henesys"}); err != nil {
			t.Fatalf("notify: %v", err)
		}

		select {
		case mapID := <-got:
			if mapID != "henesys" {
				t.Errorf("the responder saw map %q, want henesys", mapID)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("a one-way message never reached the responder")
		}
	})
}

// Notifying does not leave the responder waiting to reply to nobody.
func TestBusNotifyExpectsNoReply(t *testing.T) {
	eachBus(t, func(t *testing.T, b Bus) {
		ctx := context.Background()

		done := make(chan struct{}, 1)
		sub, err := b.Respond(ctx, "notify.noreply",
			func(_ context.Context, _ string, _ []byte) (proto.Message, error) {
				done <- struct{}{}
				// A responder that answers a notification is answering an
				// address that does not exist. It must not block or fail.
				return &mmov1.RoomClosed{MapId: "ignored"}, nil
			})
		if err != nil {
			t.Fatalf("respond: %v", err)
		}
		defer sub.Close()

		if err := Notify(ctx, b, "notify.noreply", &mmov1.RoomClosed{}); err != nil {
			t.Fatalf("notify: %v", err)
		}

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("the responder never ran")
		}
	})
}
