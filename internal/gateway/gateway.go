// Package gateway terminates client connections.
//
// It owns WebSocket sockets, the handshake, and routing between a connection
// and the room a player is in. It holds no simulation state: the gateway is
// stateless apart from live sessions, which is what lets it scale
// horizontally later with any client able to reach any gateway.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/ctrl-research/mmo/internal/metrics"
	"github.com/ctrl-research/mmo/internal/world/room"
)

// RoomProvider hands out a Handle for the room a connecting player belongs in.
//
// M0 has a single room, so the implementation is trivial. M4 replaces it with
// one that consults the directory, places the player in the least-full
// channel, and returns a remote handle when the room lives on another node --
// without the gateway changing, because it only ever sees a Handle.
type RoomProvider interface {
	Handle(ctx context.Context) (room.Handle, error)
}

// Config configures a Gateway.
type Config struct {
	Rooms       RoomProvider
	Tickets     *TicketStore
	Metrics     *metrics.Metrics
	Logger      *slog.Logger
	ContentHash string

	// AllowedOrigins is passed to the WebSocket accept check. Empty means
	// same-origin only, which is the right default: a browser will happily
	// open a socket from any page unless told otherwise.
	AllowedOrigins []string

	// DevAuth issues tickets without any identity check.
	//
	// M0 has no identity provider yet, so this is how a player gets a ticket.
	// It must never be enabled in a deployment reachable by anyone untrusted,
	// and the server refuses to start with it on unless it is asked for
	// explicitly.
	DevAuth bool
}

// Gateway serves the game's HTTP and WebSocket endpoints.
type Gateway struct {
	rooms       RoomProvider
	tickets     *TicketStore
	metrics     *metrics.Metrics
	log         *slog.Logger
	contentHash string
	origins     []string
	devAuth     bool

	mu       sync.Mutex
	sessions map[*session]struct{}
}

// New builds a Gateway.
func New(cfg Config) (*Gateway, error) {
	if cfg.Rooms == nil {
		return nil, errors.New("gateway: RoomProvider is required")
	}
	if cfg.Tickets == nil {
		cfg.Tickets = NewTicketStore()
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Metrics == nil {
		return nil, errors.New("gateway: Metrics is required")
	}
	return &Gateway{
		rooms:       cfg.Rooms,
		tickets:     cfg.Tickets,
		metrics:     cfg.Metrics,
		log:         cfg.Logger,
		contentHash: cfg.ContentHash,
		origins:     cfg.AllowedOrigins,
		devAuth:     cfg.DevAuth,
		sessions:    make(map[*session]struct{}),
	}, nil
}

// Routes returns the gateway's HTTP handler.
func (g *Gateway) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", g.handleHealth)
	mux.HandleFunc("GET /ws", g.handleWebSocket)
	if g.devAuth {
		mux.HandleFunc("POST /api/dev/ticket", g.handleDevTicket)
	}
	return mux
}

func (g *Gateway) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status":   "ok",
		"protocol": ProtocolVersion,
		"content":  g.contentHash,
	})
}

// handleDevTicket issues a ticket without authenticating anyone.
//
// This is the M0 stand-in for the OIDC flow that arrives in M2. It exists so
// the game is playable before identity is built, and it is gated behind an
// explicit flag so it cannot be switched on by accident.
func (g *Gateway) handleDevTicket(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	name := sanitiseName(req.Name)
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	id, err := g.tickets.Issue("dev:"+name, name)
	if err != nil {
		g.log.Error("issuing dev ticket", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ticket":    id,
		"expiresIn": int(TicketTTL.Seconds()),
	})
}

// sanitiseName trims a display name and bounds its length. It deliberately
// does not reject unusual characters: rendering is the renderer's problem, and
// the name never reaches a query, a shell, or a template.
func sanitiseName(s string) string {
	const maxName = 24
	out := make([]rune, 0, maxName)
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue // control characters
		}
		out = append(out, r)
		if len(out) == maxName {
			break
		}
	}
	// Trim surrounding spaces without pulling in strings for one call.
	start, end := 0, len(out)
	for start < end && out[start] == ' ' {
		start++
	}
	for end > start && out[end-1] == ' ' {
		end--
	}
	return string(out[start:end])
}

func (g *Gateway) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: g.origins,
		// The protocol is already protobuf, which does not compress usefully,
		// and per-message deflate costs CPU on the tick path.
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		g.log.Debug("websocket upgrade failed", "err", err)
		return
	}

	s := newSession(conn, g, g.log.With("remote", r.RemoteAddr))

	g.mu.Lock()
	g.sessions[s] = struct{}{}
	g.mu.Unlock()

	g.metrics.Connections.Inc()
	g.metrics.ConnectionsMade.Inc()

	defer func() {
		g.mu.Lock()
		delete(g.sessions, s)
		g.mu.Unlock()
		g.metrics.Connections.Dec()
	}()

	s.run(r.Context())
}

// Shutdown closes every live session.
func (g *Gateway) Shutdown(ctx context.Context) error {
	g.mu.Lock()
	sessions := make([]*session, 0, len(g.sessions))
	for s := range g.sessions {
		sessions = append(sessions, s)
	}
	g.mu.Unlock()

	for _, s := range sessions {
		s.closeWith(room.CloseServerShutdown, "server shutting down")
	}

	// Give writers a moment to flush their close frames so clients see a
	// reason rather than a dropped socket.
	deadline := time.After(2 * time.Second)
	for {
		g.mu.Lock()
		n := len(g.sessions)
		g.mu.Unlock()
		if n == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return nil
		case <-time.After(20 * time.Millisecond):
		}
	}
}
