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
	"github.com/ctrl-research/mmo/internal/auth"
	"github.com/ctrl-research/mmo/internal/content"
	"github.com/ctrl-research/mmo/internal/metrics"
	"github.com/ctrl-research/mmo/internal/world"
	"github.com/ctrl-research/mmo/internal/world/room"
	"github.com/google/uuid"
)

// World places a character into the simulation.
//
// The gateway deliberately knows nothing about leases, checkpoints, or which
// node hosts which room. It hands over an authenticated identity and receives
// something it can route packets to and close when the socket ends.
type World interface {
	// Enter takes ownership of a character and places it in a room.
	Enter(ctx context.Context, accountID, characterID uuid.UUID, sink room.Sink) (world.PlayerSession, error)
}

// PlayerSession is one character in play.
type PlayerSession = world.PlayerSession

// Config configures a Gateway.
type Config struct {
	World       World
	Maps        map[string]*content.Map
	Tickets     *TicketStore
	Metrics     *metrics.Metrics
	Logger      *slog.Logger
	ContentHash string

	// Sessions redeems the single-use tickets issued over authenticated HTTP.
	Sessions *auth.Sessions

	// Identity serves the sign-in and character-selection endpoints. They are
	// mounted here so the whole game -- page, API, and socket -- lives on one
	// origin, which is what keeps the same-origin WebSocket check workable
	// without an allowlist.
	Identity interface{ Routes(*http.ServeMux) }

	// ClientDir serves the built client from this directory. Empty disables
	// static serving, which is what the Vite dev server wants during
	// development.
	ClientDir string

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
	world       World
	maps        map[string]*content.Map
	tickets     *TicketStore
	metrics     *metrics.Metrics
	log         *slog.Logger
	contentHash string
	origins     []string
	devAuth     bool
	clientDir   string
	sessions    *auth.Sessions
	identity    interface{ Routes(*http.ServeMux) }

	// conns is the set of live connections, distinct from the auth sessions
	// above: one is "who is connected right now", the other is "who is signed
	// in".
	mu    sync.Mutex
	conns map[*session]struct{}
}

// New builds a Gateway.
func New(cfg Config) (*Gateway, error) {
	if cfg.World == nil {
		return nil, errors.New("gateway: a World is required")
	}
	if cfg.Sessions == nil {
		return nil, errors.New("gateway: auth sessions are required")
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
	if cfg.Maps == nil {
		cfg.Maps = map[string]*content.Map{}
	}
	return &Gateway{
		world:       cfg.World,
		sessions:    cfg.Sessions,
		maps:        cfg.Maps,
		tickets:     cfg.Tickets,
		metrics:     cfg.Metrics,
		log:         cfg.Logger,
		contentHash: cfg.ContentHash,
		origins:     cfg.AllowedOrigins,
		devAuth:     cfg.DevAuth,
		clientDir:   cfg.ClientDir,
		identity:    cfg.Identity,
		conns:       make(map[*session]struct{}),
	}, nil
}

// Routes returns the gateway's HTTP handler.
func (g *Gateway) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", g.handleHealth)
	mux.HandleFunc("GET /ws", g.handleWebSocket)
	mux.HandleFunc("GET /api/map/{id}/collision", g.handleMapCollision)
	mux.HandleFunc("GET /api/map/{id}/source", g.handleMapSource)

	if g.identity != nil {
		g.identity.Routes(mux)
	}
	// Registered last and at the root, so it never shadows an API route.
	if g.clientDir != "" {
		mux.Handle("GET /", staticHandler(g.clientDir))
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
	g.conns[s] = struct{}{}
	g.mu.Unlock()

	g.metrics.Connections.Inc()
	g.metrics.ConnectionsMade.Inc()

	defer func() {
		g.mu.Lock()
		delete(g.conns, s)
		g.mu.Unlock()
		g.metrics.Connections.Dec()
	}()

	s.run(r.Context())
}

// Shutdown closes every live session.
func (g *Gateway) Shutdown(ctx context.Context) error {
	g.mu.Lock()
	sessions := make([]*session, 0, len(g.conns))
	for s := range g.conns {
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
		n := len(g.conns)
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
