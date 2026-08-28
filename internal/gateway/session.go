package gateway

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world/room"
	"github.com/ctrl-research/mmo/internal/world/sim"
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
	if s.handle != nil && s.entityID != 0 {
		// Use a fresh context: the session's is already cancelled, and the
		// room must still learn the player is gone.
		leaveCtx, leaveCancel := context.WithTimeout(context.Background(), 5*time.Second)
		s.handle.Leave(leaveCtx, s.entityID)
		leaveCancel()
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

	ticket, ok := s.gw.tickets.Redeem(hello.GetTicket())
	if !ok {
		s.closeWith(room.CloseTicketInvalid, "invalid or expired ticket")
		return errors.New("gateway: ticket rejected")
	}
	s.name = ticket.Name
	s.log = s.log.With("player", ticket.Name)

	handle, err := s.gw.rooms.Handle(ctx)
	if err != nil {
		s.closeWith(room.CloseServerShutdown, "no room available")
		return err
	}
	id, err := handle.Join(ctx, ticket.Name, s)
	if err != nil {
		if errors.Is(err, room.ErrRoomFull) {
			s.closeWith(room.CloseNotAllowed, "room is full")
		} else {
			s.closeWith(room.CloseServerShutdown, "join failed")
		}
		return err
	}

	s.handle, s.entityID = handle, id
	s.log.Info("player connected", "entity", uint32(id))
	return nil
}

func (s *session) handleClientMessage(ctx context.Context, cm *mmov1.ClientMessage, limiter *rateLimiter) {
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
		s.handle.Input(ctx, s.entityID, in.GetSeq(), sim.Input{
			MoveX: clampMoveX(in.GetMoveX()),
			Jump:  in.GetJump(),
			Up:    in.GetUp(),
			Down:  in.GetDown(),
		})

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
