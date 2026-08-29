package gateway

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/ctrl-research/mmo/internal/content"
	"github.com/ctrl-research/mmo/internal/directory"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world"
	"github.com/ctrl-research/mmo/internal/world/room"
	"github.com/ctrl-research/mmo/internal/world/sim"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

const (
	// ProtocolVersion is bumped on any incompatible wire change. A mismatch is
	// rejected at the handshake rather than allowed to produce bugs that read
	// as physics problems.
	ProtocolVersion = 1

	// outboundQueue is how many messages may be pending for one client.
	//
	// About a second of ticks. Deep enough to ride out a stalled TCP window,
	// shallow enough that a client which cannot keep up is disconnected
	// promptly instead of accumulating snapshots it will render far too late.
	outboundQueue = 24

	// maxFrameBytes caps an inbound frame. Client messages are tens of bytes;
	// anything approaching this is a bug or an attack.
	maxFrameBytes = 8 * 1024

	// maxIntentsPerSecond bounds input rate. The client simulates at the
	// server's 20 Hz and sends one intent per simulated tick, so this is triple
	// the legitimate rate -- enough headroom for jitter and catch-up bursts.
	maxIntentsPerSecond = 60

	// handshakeTimeout is how long a connection may stay silent before sending
	// its Hello.
	handshakeTimeout = 10 * time.Second

	// writeTimeout bounds a single socket write, so one wedged client cannot
	// hold a writer goroutine forever.
	writeTimeout = 5 * time.Second
)

// session is one connected client.
//
// It owns two goroutines: a reader that decodes client messages and forwards
// intent to the room, and a writer that batches server messages into frames.
// The room calls Send from inside its tick loop, so Send must never block.
type session struct {
	conn *websocket.Conn
	log  *slog.Logger
	gw   *Gateway

	entityID room.EntityID
	handle   room.Handle
	name     string

	// play owns the character's lease and checkpoint loop. Closing it is what
	// makes a disconnect lossless rather than discarding up to one checkpoint
	// interval of progress.
	play world.PlayerSession

	out chan *mmov1.ServerMessage

	closeOnce sync.Once
	done      chan struct{}

	// Set by the writer when it closes the socket, so the reader reports the
	// cause rather than a generic read error.
	closeCode   uint32
	closeReason string
	closeMu     sync.Mutex
}

func newSession(conn *websocket.Conn, gw *Gateway, log *slog.Logger) *session {
	return &session{
		conn: conn,
		gw:   gw,
		log:  log,
		out:  make(chan *mmov1.ServerMessage, outboundQueue),
		done: make(chan struct{}),
	}
}

// Send queues a message for the client. It satisfies room.Sink.
//
// It never blocks. A full queue means the client is not draining fast enough,
// and the message is dropped rather than allowed to stall the tick loop for
// everyone else in the room. Sustained drops disconnect the client, because a
// client rendering second-old snapshots is worse off than one that reconnects.
func (s *session) Send(msg *mmov1.ServerMessage) {
	select {
	case s.out <- msg:
	case <-s.done:
	default:
		s.gw.metrics.OutboundDropped.Inc()
		s.closeWith(room.CloseRateLimited, "client too slow")
	}
}

// Close satisfies room.Sink.
func (s *session) Close(code uint32, reason string) { s.closeWith(code, reason) }

func (s *session) closeWith(code uint32, reason string) {
	s.closeOnce.Do(func() {
		s.closeMu.Lock()
		s.closeCode, s.closeReason = code, reason
		s.closeMu.Unlock()
		close(s.done)
	})
}

// run drives the session until it ends, then cleans up.
func (s *session) run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go s.writeLoop(ctx)

	err := s.readLoop(ctx)

	s.closeWith(room.CloseKicked, "connection ended")

	if s.play != nil {
		// A fresh context: the session's is already cancelled, and the
		// character still has to be dealt with.
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)

		// A dropped socket holds the character for a grace period rather than
		// removing it, so a transient blip mid-fight is not a wipe. Anything
		// deliberate -- a kick, a lost lease, a shutdown -- ends it outright,
		// because there is nothing to come back to.
		if s.gracefulDisconnect() {
			s.play.Disconnect(closeCtx)
		} else {
			s.play.Close(closeCtx)
		}
		closeCancel()
	}

	s.closeMu.Lock()
	code, reason := s.closeCode, s.closeReason
	s.closeMu.Unlock()

	s.conn.Close(websocket.StatusCode(code), truncateReason(reason))

	if err != nil && !isExpectedClose(err) {
		s.log.Debug("session ended", "err", err)
	}
}

// readLoop decodes inbound frames until the connection ends.
func (s *session) readLoop(ctx context.Context) error {
	s.conn.SetReadLimit(maxFrameBytes)

	// The first frame must be a Hello, and must arrive promptly.
	helloCtx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	err := s.readHello(helloCtx)
	cancel()
	if err != nil {
		return err
	}

	limiter := newRateLimiter(maxIntentsPerSecond)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.done:
			return nil
		default:
		}

		typ, data, err := s.conn.Read(ctx)
		if err != nil {
			return err
		}
		if typ != websocket.MessageBinary {
			s.closeWith(room.CloseKicked, "text frames are not part of the protocol")
			return errors.New("gateway: non-binary frame")
		}

		var env mmov1.Envelope
		if err := proto.Unmarshal(data, &env); err != nil {
			s.gw.metrics.InputsDropped.Inc()
			s.closeWith(room.CloseKicked, "malformed frame")
			return err
		}

		for _, cm := range env.GetClient() {
			s.handleClientMessage(ctx, cm, limiter)
		}
	}
}

// readHello performs the handshake: redeem the ticket, check versions, join.
func (s *session) readHello(ctx context.Context) error {
	typ, data, err := s.conn.Read(ctx)
	if err != nil {
		return err
	}
	if typ != websocket.MessageBinary {
		s.closeWith(room.CloseKicked, "text frames are not part of the protocol")
		return errors.New("gateway: non-binary handshake frame")
	}

	var env mmov1.Envelope
	if err := proto.Unmarshal(data, &env); err != nil {
		s.closeWith(room.CloseKicked, "malformed handshake")
		return err
	}

	msgs := env.GetClient()
	if len(msgs) == 0 || msgs[0].GetHello() == nil {
		s.closeWith(room.CloseKicked, "expected Hello")
		return errors.New("gateway: first message was not Hello")
	}
	hello := msgs[0].GetHello()

	if hello.GetProtocolVersion() != ProtocolVersion {
		s.closeWith(room.CloseProtocolVersion, "protocol version mismatch")
		return errors.New("gateway: protocol version mismatch")
	}
	if want := s.gw.contentHash; want != "" && hello.GetContentHash() != want {
		// A client that disagrees about content produces bug reports that are
		// nearly impossible to read, so refuse rather than tolerate it.
		s.closeWith(room.CloseContentHash, "content hash mismatch")
		return errors.New("gateway: content hash mismatch")
	}

	ticket, ok, err := s.gw.sessions.RedeemTicket(ctx, hello.GetTicket())
	if err != nil {
		s.closeWith(room.CloseServerShutdown, "could not verify ticket")
		return err
	}
	if !ok {
		// A ticket is single-use and short-lived, so this covers an expired
		// one, a replayed one, and a forged one -- all refused identically.
		s.closeWith(room.CloseTicketInvalid, "invalid or expired ticket")
		return errors.New("gateway: ticket rejected")
	}

	s.name = ticket.Name
	s.log = s.log.With("player", ticket.Name, "character", ticket.CharacterID.String())

	// The character named by the ticket was chosen over authenticated HTTP, so
	// the socket proves only that the ticket was held. Identity never enters
	// the game protocol.
	play, err := s.gw.world.Enter(ctx, ticket.AccountID, ticket.CharacterID, s)
	if err != nil {
		switch {
		case errors.Is(err, world.ErrCharacterBusy):
			// Someone -- possibly this same player in another tab -- already
			// has this character in play. Saying so beats a generic failure.
			s.closeWith(room.CloseLeaseLost, "this character is already in play")
		case errors.Is(err, room.ErrRoomFull):
			s.closeWith(room.CloseNotAllowed, "the room is full")
		default:
			s.closeWith(room.CloseServerShutdown, "could not enter the world")
		}
		return err
	}

	// Losing the lease mid-session must close the connection: the character
	// belongs to another node now, and this one must stop simulating it.
	play.OnOwnershipLost(func(reason string) {
		s.closeWith(room.CloseLeaseLost, reason)
	})

	s.play = play

	s.log.Info("player connected", "entity", uint32(play.EntityID()))
	return nil
}

// where returns the room and entity to address right now.
//
// Read per message rather than cached at connect: a transfer moves the
// character to a different room with a different entity id, and a cached pair
// would keep sending input into the map the player has just left.
func (s *session) where() (room.Handle, room.EntityID) {
	if s.play == nil {
		return nil, 0
	}
	return s.play.Where()
}

func (s *session) handleClientMessage(ctx context.Context, cm *mmov1.ClientMessage, limiter *rateLimiter) {
	handle, entityID := s.where()
	if handle == nil && cm.GetPing() == nil && cm.GetHello() == nil {
		return
	}

	switch {
	case cm.GetIntent() != nil:
		if !limiter.allow() {
			s.gw.metrics.InputsDropped.Inc()
			return
		}
		in := cm.GetIntent()
		s.gw.metrics.InputsReceived.Inc()

		// The simulation clamps too. Doing it here as well keeps a hostile
		// value from ever reaching the room, and keeps the clamp visible at
		// the trust boundary where it belongs.
		handle.Input(ctx, entityID, in.GetSeq(), sim.Input{
			MoveX: clampMoveX(in.GetMoveX()),
			Jump:  in.GetJump(),
			Up:    in.GetUp(),
			Down:  in.GetDown(),
		})

	case cm.GetCast() != nil:
		// Casts share the intent rate limit: a client legitimately produces at
		// most one per simulated tick, and the room bounds its own queue on
		// top of this.
		if !limiter.allow() {
			s.gw.metrics.InputsDropped.Inc()
			return
		}
		c := cm.GetCast()
		handle.Cast(ctx, entityID, c.GetSkillId(), c.GetFacingLeft())

	case cm.GetInteract() != nil:
		if !limiter.allow() {
			s.gw.metrics.InputsDropped.Inc()
			return
		}
		in := cm.GetInteract()
		if in.GetKind() == mmov1.InteractKind_INTERACT_KIND_LOOT {
			handle.Interact(ctx, entityID, room.EntityID(in.GetEntityId()), room.InteractLoot)
		}

	case cm.GetItemAction() != nil:
		if !limiter.allow() {
			s.gw.metrics.InputsDropped.Inc()
			return
		}
		s.handleItemAction(ctx, cm.GetItemAction())

	case cm.GetOpenWorldMap() != nil:
		if !limiter.allow() {
			s.gw.metrics.InputsDropped.Inc()
			return
		}
		s.Send(&mmov1.ServerMessage{
			Body: &mmov1.ServerMessage_Event{Event: &mmov1.Event{
				Body: &mmov1.Event_WorldMap{WorldMap: s.play.WorldMap(ctx)},
			}},
		})

	case cm.GetTravel() != nil:
		if !limiter.allow() {
			s.gw.metrics.InputsDropped.Inc()
			return
		}
		s.handleTravel(ctx, cm.GetTravel())

	case cm.GetPing() != nil:
		s.Send(&mmov1.ServerMessage{
			Body: &mmov1.ServerMessage_Pong{Pong: &mmov1.Pong{
				ClientTimeMs: cm.GetPing().GetClientTimeMs(),
			}},
		})

	case cm.GetHello() != nil:
		// A second Hello is always a client bug or an attempt to re-bind an
		// established connection to another identity.
		s.closeWith(room.CloseKicked, "duplicate Hello")
	}
}

func clampMoveX(v int32) int32 {
	if v > 1000 {
		return 1000
	}
	if v < -1000 {
		return -1000
	}
	return v
}

// writeLoop batches queued messages into frames.
//
// The room emits several messages per tick -- a snapshot plus any events -- and
// sending each as its own WebSocket frame would make per-frame overhead a
// meaningful share of the traffic. Draining everything currently queued into
// one Envelope batches a whole tick naturally, without needing to know about
// ticks.
func (s *session) writeLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.done:
			return
		case first := <-s.out:
			batch := append(make([]*mmov1.ServerMessage, 0, 8), first)
			batch = drain(s.out, batch)

			if err := s.writeBatch(ctx, batch); err != nil {
				s.closeWith(room.CloseKicked, "write failed")
				return
			}
		}
	}
}

// drain takes everything immediately available without waiting for more.
func drain(ch <-chan *mmov1.ServerMessage, batch []*mmov1.ServerMessage) []*mmov1.ServerMessage {
	for {
		select {
		case m := <-ch:
			batch = append(batch, m)
		default:
			return batch
		}
	}
}

func (s *session) writeBatch(ctx context.Context, batch []*mmov1.ServerMessage) error {
	payload, err := proto.Marshal(&mmov1.Envelope{Server: batch})
	if err != nil {
		return err
	}

	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()

	if err := s.conn.Write(writeCtx, websocket.MessageBinary, payload); err != nil {
		return err
	}

	snapshots := 0
	for _, m := range batch {
		if m.GetSnapshot() != nil {
			snapshots++
		}
	}
	if snapshots > 0 {
		s.gw.metrics.SnapshotsSent.Add(float64(snapshots))
		s.gw.metrics.SnapshotBytes.Add(float64(len(payload)))
	}
	return nil
}

func truncateReason(reason string) string {
	// The WebSocket close frame caps the reason at 123 bytes.
	const maxReason = 123
	if len(reason) > maxReason {
		return reason[:maxReason]
	}
	return reason
}

func isExpectedClose(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	status := websocket.CloseStatus(err)
	return status == websocket.StatusNormalClosure || status == websocket.StatusGoingAway
}

var _ room.Sink = (*session)(nil)

// gracefulDisconnect reports whether a closed connection should leave the
// character held for reconnection.
//
// Only an unexpected drop qualifies. A kick, a lost lease, or a shutdown means
// the character must leave now: holding it would let a kicked player return by
// reconnecting, and holding one whose lease moved would keep this node
// simulating something it no longer owns.
func (s *session) gracefulDisconnect() bool {
	s.closeMu.Lock()
	code := s.closeCode
	s.closeMu.Unlock()

	switch code {
	case room.CloseKicked, 0:
		return true
	default:
		return false
	}
}

// handleTravel forwards a fast-travel or channel-switch request.
func (s *session) handleTravel(ctx context.Context, req *mmov1.Travel) {
	if s.play == nil {
		return
	}

	var out world.TravelRequest
	switch {
	case req.GetWaypointId() != "":
		out.WaypointID = req.GetWaypointId()
	case req.GetChannelInstanceId() != 0:
		out.Channel = directory.InstanceID(req.GetChannelInstanceId())
	case req.GetNewChannel():
		out.NewChannel = true
	default:
		return
	}

	// Returns as soon as the request is queued: the transfer itself runs on
	// the session's goroutine, so this loop stays free to read the socket.
	if err := s.play.Travel(ctx, out); err != nil {
		s.Send(&mmov1.ServerMessage{
			Body: &mmov1.ServerMessage_Event{Event: &mmov1.Event{
				Body: &mmov1.Event_PortalRefused{PortalRefused: &mmov1.PortalRefused{
					Reason: world.TravelMessage(err),
				}},
			}},
		})
	}
}

// handleItemAction forwards an inventory request to the session.
//
// Handled off the tick loop, on the reader goroutine, because it writes to the
// database. The room learns only the result: a recomputed stat block.
func (s *session) handleItemAction(ctx context.Context, action *mmov1.ItemAction) {
	if s.play == nil {
		return
	}

	req := world.ItemAction{
		Slot:      int(action.GetSlot()),
		EquipSlot: content.EquipSlot(action.GetEquipSlot()),
	}

	switch action.GetKind() {
	case mmov1.ItemActionKind_ITEM_ACTION_KIND_MOVE:
		req.Kind = world.ItemMove
	case mmov1.ItemActionKind_ITEM_ACTION_KIND_EQUIP:
		req.Kind = world.ItemEquip
	case mmov1.ItemActionKind_ITEM_ACTION_KIND_UNEQUIP:
		req.Kind = world.ItemUnequip
	case mmov1.ItemActionKind_ITEM_ACTION_KIND_DESTROY:
		req.Kind = world.ItemDestroy
	default:
		return
	}

	if req.Kind != world.ItemUnequip {
		id, err := uuid.Parse(action.GetItemId())
		if err != nil {
			return
		}
		req.ItemID = id
	}

	// A generous timeout: this is a database round trip on a player-initiated
	// action, not something on the tick path.
	actionCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if err := s.play.ApplyItemAction(actionCtx, req); err != nil {
		// Refused actions are common and mostly benign -- a full inventory, a
		// level requirement, an item already moved. The client resyncs from
		// the inventory message it is about to receive anyway.
		s.log.Debug("item action refused", "err", err)
	}
}
