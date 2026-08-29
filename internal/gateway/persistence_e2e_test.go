package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world/room"
	"github.com/google/uuid"
)

// The M2 exit criteria, over a real socket and a real database.
//
// These are the tests that answer "does logging out and back in actually
// work", which is the whole point of the milestone and is not something the
// component tests can establish on their own.

// player is a signed-in account with a character, so a test can disconnect and
// reconnect as the same person.
type player struct {
	client      *http.Client
	characterID string
	name        string
}

func (ts *testServer) signUp(t *testing.T, name string) *player {
	t.Helper()

	client := &http.Client{Jar: newJar()}

	resp, err := client.Post(ts.url+"/auth/dev/login", "application/json",
		strings.NewReader(`{"subject":"`+name+`"}`))
	if err != nil {
		t.Fatalf("dev login: %v", err)
	}
	resp.Body.Close()

	created, err := client.Post(ts.url+"/api/characters", "application/json",
		strings.NewReader(`{"name":"`+name+`"}`))
	if err != nil {
		t.Fatalf("create character: %v", err)
	}
	defer created.Body.Close()
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create character returned %d", created.StatusCode)
	}

	var c struct {
		ID string `json:"id"`
	}
	json.NewDecoder(created.Body).Decode(&c)

	return &player{client: client, characterID: c.ID, name: name}
}

// connect opens a game socket as this player and completes the handshake.
func (ts *testServer) connect(t *testing.T, p *player) *client {
	t.Helper()

	c := ts.dial(t)
	c.hello(ts.ticketFor(t, p.client, p.characterID), ProtocolVersion)
	c.awaitWelcome()
	return c
}

// drive runs input at the simulation's rate for a while, so a test can make
// real progress rather than only asserting on a stationary character.
func drive(t *testing.T, c *client, d time.Duration, attack bool) {
	t.Helper()

	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(room.TickPeriod)
		defer ticker.Stop()
		seq := uint32(0)
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				seq++
				c.intent(seq, 1000, false)
				if attack && seq%5 == 0 {
					c.cast("slash", false)
				}
			}
		}
	}()

	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		c.collect(200 * time.Millisecond)
	}
	close(stop)
}

// The headline exit criterion: play, log out, log back in, and find everything
// where it was left.
func TestProgressSurvivesReconnect(t *testing.T) {
	ts := newTestServer(t)
	p := ts.signUp(t, "Persistent")

	first := ts.connect(t, p)
	drive(t, first, 6*time.Second, true)

	// Capture what the server believes before disconnecting.
	_, snaps := first.collect(500 * time.Millisecond)
	if len(snaps) == 0 {
		t.Fatal("no snapshots before disconnect")
	}
	before := snaps[len(snaps)-1].GetSelf()
	beforeX, beforeExp, beforeLevel := before.GetX(), before.GetExp(), before.GetLevel()

	if beforeX == 0 {
		t.Fatal("character never moved; nothing to persist")
	}

	// Disconnecting triggers the final checkpoint, which is what makes logging
	// out lossless rather than discarding up to a whole interval of progress.
	first.conn.Close(websocket.StatusNormalClosure, "logging out")

	// The lease has to be released before the same character can be played
	// again, so wait for the server to finish tearing the session down.
	waitUntil(t, 5*time.Second, func() bool {
		return ts.characterFree(t, p)
	})

	second := ts.connect(t, p)
	_, after := second.collect(1500 * time.Millisecond)
	if len(after) == 0 {
		t.Fatal("no snapshots after reconnecting")
	}
	self := after[len(after)-1].GetSelf()

	if self.GetLevel() != beforeLevel {
		t.Errorf("level is %d after reconnecting, was %d", self.GetLevel(), beforeLevel)
	}
	if self.GetExp() != beforeExp {
		t.Errorf("experience is %d after reconnecting, was %d", self.GetExp(), beforeExp)
	}

	// Position is restored rather than reset to the spawn point. Exact
	// equality would be wrong -- gravity runs on the first tick -- so allow a
	// small settle.
	const tolerance = 64 * 256 // 64 world units, in fixed-point
	if diff := abs32(self.GetX() - beforeX); diff > tolerance {
		t.Errorf("resumed at x=%d, was at x=%d; the character did not resume in place",
			self.GetX(), beforeX)
	}
}

// The single-writer invariant, from the outside: the same character cannot be
// in play twice, which is what stops two sessions duplicating its items.
func TestSameCharacterCannotBePlayedTwice(t *testing.T) {
	ts := newTestServer(t)
	p := ts.signUp(t, "Contested")

	first := ts.connect(t, p)
	defer first.conn.Close(websocket.StatusNormalClosure, "")

	// A second connection for the same character, as if from another tab or
	// another machine.
	second := ts.dial(t)
	second.hello(ts.ticketFor(t, p.client, p.characterID), ProtocolVersion)

	_, err := second.recv(3 * time.Second)
	if err == nil {
		t.Fatal("the same character was admitted twice")
	}
	if code := websocket.CloseStatus(err); code != room.CloseLeaseLost {
		t.Errorf("close code = %d, want %d (lease lost)", code, room.CloseLeaseLost)
	}
}

// Two different characters on one account are independent, so the lease is on
// the character rather than on the account.
func TestTwoCharactersOnOneAccountCanBothPlay(t *testing.T) {
	ts := newTestServer(t)
	p := ts.signUp(t, "Firstborn")

	// A second character on the same account.
	created, err := p.client.Post(ts.url+"/api/characters", "application/json",
		strings.NewReader(`{"name":"Secondborn"}`))
	if err != nil {
		t.Fatalf("create second character: %v", err)
	}
	defer created.Body.Close()
	var other struct {
		ID string `json:"id"`
	}
	json.NewDecoder(created.Body).Decode(&other)

	a := ts.connect(t, p)
	defer a.conn.Close(websocket.StatusNormalClosure, "")

	b := ts.dial(t)
	b.hello(ts.ticketFor(t, p.client, other.ID), ProtocolVersion)
	if w := b.awaitWelcome(); w.GetEntityId() == 0 {
		t.Error("the account's second character could not enter")
	}
}

// A character released cleanly must be immediately playable again, or a
// reconnect after a normal logout would be refused until the lease expired.
func TestCharacterIsPlayableAgainAfterALogout(t *testing.T) {
	ts := newTestServer(t)
	p := ts.signUp(t, "Rejoiner")

	first := ts.connect(t, p)
	first.conn.Close(websocket.StatusNormalClosure, "logging out")

	waitUntil(t, 5*time.Second, func() bool { return ts.characterFree(t, p) })

	second := ts.dial(t)
	second.hello(ts.ticketFor(t, p.client, p.characterID), ProtocolVersion)
	if w := second.awaitWelcome(); w.GetEntityId() == 0 {
		t.Error("could not rejoin after a clean logout")
	}
}

// A ticket names the character, so identity never enters the game protocol.
// A ticket for someone else's character must not be obtainable at all.
func TestTicketCannotNameAnotherAccountsCharacter(t *testing.T) {
	ts := newTestServer(t)

	owner := ts.signUp(t, "Owner")
	intruder := ts.signUp(t, "Intruder")

	resp, err := intruder.client.Post(ts.url+"/api/ticket", "application/json",
		strings.NewReader(`{"characterId":"`+owner.characterID+`"}`))
	if err != nil {
		t.Fatalf("ticket: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("a ticket for another account's character returned %d, want 404", resp.StatusCode)
	}
}

// A forged ticket must be refused, since the socket's only proof of identity
// is that the ticket was held.
func TestForgedTicketIsRefused(t *testing.T) {
	ts := newTestServer(t)

	c := ts.dial(t)
	c.hello(uuid.NewString(), ProtocolVersion)

	if _, err := c.recv(3 * time.Second); err == nil {
		t.Fatal("a forged ticket was accepted")
	} else if code := websocket.CloseStatus(err); code != room.CloseTicketInvalid {
		t.Errorf("close code = %d, want %d", code, room.CloseTicketInvalid)
	}
}

// The character's state carries its saved name, not one the client chose.
func TestCharacterNameComesFromTheDatabase(t *testing.T) {
	ts := newTestServer(t)
	p := ts.signUp(t, "Authentic")

	c := ts.connect(t, p)
	_, snaps := c.collect(800 * time.Millisecond)
	if len(snaps) == 0 {
		t.Fatal("no snapshots")
	}
	if got := snaps[len(snaps)-1].GetSelf().GetName(); got != "Authentic" {
		t.Errorf("character name is %q, want the stored name", got)
	}
}

// --- helpers ----------------------------------------------------------------

// characterFree reports whether the character's lease has been released, which
// is how a test knows the previous session has finished tearing down.
func (ts *testServer) characterFree(t *testing.T, p *player) bool {
	t.Helper()

	id, err := uuid.Parse(p.characterID)
	if err != nil {
		t.Fatalf("parse character id: %v", err)
	}

	// Probing by taking the lease and immediately releasing it: if it can be
	// taken, the previous session is done.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	lease, err := ts.leases.Acquire(ctx, id.String(), "probe")
	if err != nil {
		return false
	}
	ts.leases.Release(ctx, lease)
	return true
}

func waitUntil(t *testing.T, within time.Duration, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("condition was not met in time")
}

func abs32(v int32) int32 {
	if v < 0 {
		return -v
	}
	return v
}

var _ = mmov1.EntityKind_ENTITY_KIND_PLAYER
