package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"time"

	"github.com/coder/websocket"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"google.golang.org/protobuf/proto"
)

// One bot: a player-shaped client with no screen.
//
// It takes exactly the path a browser takes -- sign in, pick a character, ask
// for a ticket, open a socket, say hello -- because a load test that skipped
// any of it would be measuring something the game does not do. The ticket in
// particular is the whole reason the gateway can be scaled: it is issued over
// authenticated HTTP and redeemed once on the socket.

// bot is one simulated player.
type bot struct {
	name   string
	server string
	stats  *stats

	http   *http.Client
	ticket string

	conn *websocket.Conn

	// entity is this bot's own entity, from the Welcome. Used to find itself
	// in a snapshot.
	entity uint32

	// skill is the first thing on the bar, cast on a timer. Empty until the
	// server sends the bar, which is why casting starts a moment after moving.
	skill string

	// pending holds the rest of a batch that has been read but not consumed.
	// Only the read goroutine touches it.
	pending []*mmov1.ServerMessage

	// handshake is what the server said it speaks. Read from it rather than
	// compiled in, so a bot built against a different commit fails at the
	// handshake with something that says so.
	handshake handshake

	// exp is the cumulative experience the last snapshot reported, and
	// expAtLoss is what it was when the connection went. Compared on the first
	// snapshot after reconnecting to see what the crash cost.
	exp        uint64
	expAtLoss  uint64
	recovering bool
}

// observeExp records progress, and compares it across a reconnect.
//
// Only across a reconnect. Experience is not monotonic in play: dying charges a
// share of the progress made toward the current level, and these bots run at
// mobs constantly, so a version that treated every decrease as lost progress
// reported hundreds of "losses" in a run where nothing had gone wrong at all.
// That was the first thing this measurement said, and it was wrong.
//
// What a crash costs is the difference between what the character had when the
// connection went and what it has on the first snapshot of the next one --
// whatever the last checkpoint had not yet written.
func (b *bot) observeExp(seen uint64) {
	if b.recovering {
		b.recovering = false
		if seen < b.expAtLoss {
			b.stats.lostProgress(b.expAtLoss - seen)
		}
	}
	b.exp = seen
}

// disconnected marks the point a comparison should be made from.
func (b *bot) disconnected() {
	b.expAtLoss = b.exp
	b.recovering = true
}

func newBot(name, server string, hs handshake, st *stats) *bot {
	jar, _ := cookiejar.New(nil)
	return &bot{
		name:      name,
		server:    server,
		handshake: hs,
		stats:     st,
		http: &http.Client{
			Jar:     jar,
			Timeout: 30 * time.Second,
		},
	}
}

// signIn takes the browser's path to a ticket.
func (b *bot) signIn(ctx context.Context) error {
	if err := b.post(ctx, "/auth/dev/login",
		map[string]string{"subject": b.name}, nil); err != nil {
		return fmt.Errorf("dev sign-in (is the server running with --dev-auth?): %w", err)
	}

	characterID, err := b.character(ctx)
	if err != nil {
		return err
	}

	var reply struct {
		Ticket string `json:"ticket"`
	}
	if err := b.post(ctx, "/api/ticket",
		map[string]string{"characterId": characterID}, &reply); err != nil {
		return fmt.Errorf("ticket: %w", err)
	}
	b.ticket = reply.Ticket
	return nil
}

// character returns this bot's character, making one if it has none.
//
// Reused rather than recreated: a load test is run over and over against the
// same database, and a bot that made a character every time would leave
// thousands behind and eventually hit the six-per-account cap.
func (b *bot) character(ctx context.Context) (string, error) {
	var existing struct {
		Characters []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"characters"`
	}
	if err := b.get(ctx, "/api/characters", &existing); err == nil {
		for _, c := range existing.Characters {
			if strings.EqualFold(c.Name, b.name) {
				return c.ID, nil
			}
		}
	}

	var created struct {
		ID string `json:"id"`
	}
	err := b.post(ctx, "/api/characters", map[string]string{
		"name": b.name,
		// Warrior: it fights in melee, which is the expensive path to
		// simulate. A load test wants the cost, not the comfort.
		"class": "warrior",
	}, &created)
	if err != nil {
		return "", fmt.Errorf("create character: %w", err)
	}
	return created.ID, nil
}

// connect opens the socket and completes the handshake.
func (b *bot) connect(ctx context.Context) error {
	url := strings.Replace(b.server, "http", "ws", 1) + "/ws"
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPClient: b.http,
	})
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	// A player's own snapshots are small, but a busy room's are not, and a bot
	// that refused a large one would drop out exactly when the test got
	// interesting.
	conn.SetReadLimit(4 << 20)
	b.conn = conn

	if err := b.send(ctx, &mmov1.ClientMessage{
		Body: &mmov1.ClientMessage_Hello{Hello: &mmov1.Hello{
			Ticket:          b.ticket,
			ProtocolVersion: b.handshake.Protocol,
			// The server refuses a client whose content does not match its
			// own, because the alternative is a bot walking through walls that
			// exist and reading it as a physics bug.
			ContentHash: b.handshake.Content,
		}},
	}); err != nil {
		return fmt.Errorf("hello: %w", err)
	}

	// The Welcome is the server saying the character is in a room. Until it
	// arrives there is nothing to send input about.
	deadline, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	for {
		msg, err := b.recv(deadline)
		if err != nil {
			return fmt.Errorf("waiting for the welcome: %w", err)
		}
		if w := msg.GetWelcome(); w != nil {
			b.entity = w.GetEntityId()
			return nil
		}
		if k := msg.GetKick(); k != nil {
			return fmt.Errorf("kicked before entering: %s", k.GetReason())
		}
	}
}

// play runs one bot until the context ends.
//
// Two goroutines: one reading, one acting. Reading has to be continuous --
// a client that stops draining its socket is a client the server's write
// buffer fills up behind, and it gets disconnected for being slow rather than
// for anything the test meant to measure.
func (b *bot) play(ctx context.Context, inputRate time.Duration) {
	defer b.conn.CloseNow()

	read := make(chan struct{})
	go func() {
		defer close(read)
		b.readLoop(ctx)
	}()

	b.actLoop(ctx, inputRate)
	<-read

	// Whatever this character had when the connection ended is what the next
	// one is measured against.
	b.disconnected()
}

func (b *bot) readLoop(ctx context.Context) {
	for {
		msg, err := b.recv(ctx)
		if err != nil {
			if ctx.Err() == nil {
				// Why, not just that it happened. "47 bots dropped" is a
				// number to go and read logs about; "47 bots dropped with
				// StatusCode(4005) this character is already in play" is an
				// answer.
				b.stats.dropped(err)
			}
			return
		}

		switch {
		case msg.GetSnapshot() != nil:
			b.stats.snapshots.Add(1)

			// Cumulative experience, from the player's own entity -- the one
			// field a snapshot never delta-compresses, so it is complete in
			// every message. It only ever goes up, which makes it the thing to
			// watch across a node dying: whatever a checkpoint had not yet
			// written is what a kill costs.
			if self := msg.GetSnapshot().GetSelf(); self != nil {
				b.observeExp(self.GetExp())
			}

		case msg.GetPong() != nil:
			// The round trip a player would feel: sent from the act loop with
			// the time on it, measured here.
			sent := time.UnixMilli(int64(msg.GetPong().GetClientTimeMs()))
			b.stats.observeRTT(time.Since(sent))

		case msg.GetKick() != nil:
			k := msg.GetKick()
			b.stats.kicked(fmt.Sprintf("code %d: %s", k.GetCode(), k.GetReason()))
			return

		case msg.GetEvent() != nil:
			b.stats.events.Add(1)
			if bar := msg.GetEvent().GetSkillBar(); bar != nil {
				if slots := bar.GetSlots(); len(slots) > 0 {
					b.skill = slots[0].GetSkillId()
				}
			}
		}
	}
}

// actLoop sends what a player sends: movement every tick, a swing on a beat,
// and a ping to measure the round trip.
func (b *bot) actLoop(ctx context.Context, inputRate time.Duration) {
	input := time.NewTicker(inputRate)
	defer input.Stop()

	// Not a round number and not shared: every bot casting on the same beat
	// would produce a load pattern no real population makes, and would hide
	// exactly the smoothing the room's action tick is there to provide.
	cast := time.NewTicker(700*time.Millisecond + time.Duration(rand.IntN(600))*time.Millisecond)
	defer cast.Stop()

	ping := time.NewTicker(time.Second)
	defer ping.Stop()

	var seq uint32
	// Each bot turns around on its own schedule, so they spread out along the
	// map instead of moving as one block.
	direction := int32(1000)
	if rand.IntN(2) == 0 {
		direction = -1000
	}
	turn := time.NewTicker(time.Duration(3+rand.IntN(5)) * time.Second)
	defer turn.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-turn.C:
			direction = -direction

		case <-input.C:
			seq++
			err := b.send(ctx, &mmov1.ClientMessage{
				Body: &mmov1.ClientMessage_Intent{Intent: &mmov1.Intent{
					Seq:   seq,
					MoveX: direction,
					// A jump every so often, so the bots are not a line of
					// players walking into the same wall.
					Jump: rand.IntN(20) == 0,
				}},
			})
			if err != nil {
				return
			}
			b.stats.inputs.Add(1)

		case <-cast.C:
			if b.skill == "" {
				continue
			}
			err := b.send(ctx, &mmov1.ClientMessage{
				Body: &mmov1.ClientMessage_Cast{Cast: &mmov1.Cast{
					SkillId:    b.skill,
					FacingLeft: direction < 0,
				}},
			})
			if err != nil {
				return
			}
			b.stats.casts.Add(1)

		case <-ping.C:
			err := b.send(ctx, &mmov1.ClientMessage{
				Body: &mmov1.ClientMessage_Ping{Ping: &mmov1.Ping{
					ClientTimeMs: uint64(time.Now().UnixMilli()),
				}},
			})
			if err != nil {
				return
			}
		}
	}
}

// send wraps a message in an envelope, which is what the wire carries.
//
// Every frame is an Envelope even when it holds one message: the server
// batches a tick's worth of messages into one, and a client that sent a bare
// ClientMessage would be decoded as an envelope with no messages in it. That
// reads as "malformed handshake" rather than as anything about framing, which
// is exactly how long it took to find.
func (b *bot) send(ctx context.Context, msgs ...*mmov1.ClientMessage) error {
	raw, err := proto.Marshal(&mmov1.Envelope{Client: msgs})
	if err != nil {
		return err
	}
	b.stats.bytesOut.Add(uint64(len(raw)))
	return b.conn.Write(ctx, websocket.MessageBinary, raw)
}

// recv returns the next message, unpacking envelopes as they arrive.
//
// One frame can carry several messages -- that is the point of batching -- so
// what has been read and not yet returned is kept here rather than thrown
// away. Dropping the tail of a batch would silently lose most of a busy
// room's traffic and make the bot look faster than a real client.
func (b *bot) recv(ctx context.Context) (*mmov1.ServerMessage, error) {
	for len(b.pending) == 0 {
		_, raw, err := b.conn.Read(ctx)
		if err != nil {
			return nil, err
		}
		b.stats.bytesIn.Add(uint64(len(raw)))

		var env mmov1.Envelope
		if err := proto.Unmarshal(raw, &env); err != nil {
			return nil, err
		}
		b.pending = env.GetServer()
		b.stats.frames.Add(1)
	}

	msg := b.pending[0]
	b.pending = b.pending[1:]
	return msg, nil
}

// --- the HTTP half ----------------------------------------------------------

func (b *bot) post(ctx context.Context, path string, body any, into any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.server+path,
		strings.NewReader(string(raw)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return b.do(req, into)
}

func (b *bot) get(ctx context.Context, path string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.server+path, nil)
	if err != nil {
		return err
	}
	return b.do(req, into)
}

func (b *bot) do(req *http.Request, into any) error {
	resp, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		// The body, not just the code: the server explains refusals, and
		// "400" on its own sends whoever is running the test reading logs.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s %s: %d %s",
			req.Method, req.URL.Path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if into == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

// handshake is what a server says it speaks, from its own /healthz.
type handshake struct {
	Status   string `json:"status"`
	Protocol uint32 `json:"protocol"`
	Content  string `json:"content"`
}

// askHandshake reads the protocol version and content hash once, for everyone.
//
// Once rather than per bot: it is the same answer a thousand times, and a
// thousand requests before the run has started is a load test of /healthz.
func askHandshake(ctx context.Context, server string) (handshake, error) {
	probe := newBot("probe", server, handshake{}, newStats())

	var hs handshake
	if err := probe.get(ctx, "/healthz", &hs); err != nil {
		return handshake{}, fmt.Errorf("asking %s what it speaks: %w", server, err)
	}
	if hs.Protocol == 0 {
		return handshake{}, fmt.Errorf("%s did not say which protocol it speaks", server)
	}
	return hs, nil
}
