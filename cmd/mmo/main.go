// Command mmo is the game server.
//
// One binary hosts every role. Which roles it runs is chosen at startup, so
// the same build serves a laptop running the whole game in one process and a
// cluster running gateways, world nodes, and social services separately:
//
//	mmo --roles=all                  hobby scale, one process
//	mmo --roles=gateway              scaled out, separate deployments
//	mmo --roles=world
//
// See docs/architecture.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	gamedata "github.com/ctrl-research/mmo/content"
	"github.com/ctrl-research/mmo/internal/content"
	"github.com/ctrl-research/mmo/internal/directory"
	"github.com/ctrl-research/mmo/internal/gateway"
	"github.com/ctrl-research/mmo/internal/metrics"
	"github.com/ctrl-research/mmo/internal/world"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Role is one responsibility the process can take on.
type Role string

const (
	RoleGateway Role = "gateway"
	RoleWorld   Role = "world"
)

type config struct {
	roles      string
	addr       string
	adminAddr  string
	nodeID     string
	contentDir string
	defaultMap string
	origins    string
	clientDir  string
	devAuth    bool
	logLevel   string
	logJSON    bool
	seed       uint64
}

func main() {
	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := parseFlags()

	log, err := newLogger(cfg)
	if err != nil {
		return err
	}
	slog.SetDefault(log)

	roles, err := parseRoles(cfg.roles)
	if err != nil {
		return err
	}

	// Ctrl-C and SIGTERM begin a graceful shutdown; a second signal is handled
	// by the runtime's default and kills the process, so an operator is never
	// stuck waiting on a shutdown that has itself wedged.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	registry := prometheus.NewRegistry()
	registry.MustRegister(
		prometheus.NewGoCollector(),
		prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}),
	)
	m := metrics.New(registry)

	game, err := loadContent(cfg, log)
	if err != nil {
		return err
	}
	log.Info("content loaded",
		"hash", game.Hash, "maps", len(game.Maps), "mobs", len(game.Mobs),
		"items", len(game.Items), "skills", len(game.Skills))

	dir := directory.NewMemory(directory.NodeID(cfg.nodeID))
	defer dir.Close()

	var node *world.Node
	if roles[RoleWorld] {
		node, err = world.NewNode(world.Config{
			Directory:  dir,
			Content:    game,
			DefaultMap: cfg.defaultMap,
			Logger:     log,
			Observer:   m,
			Seed:       cfg.seed,
		})
		if err != nil {
			return err
		}
		node.Start(ctx)
		defer node.Stop()
	}

	var servers []*http.Server

	if roles[RoleGateway] {
		if node == nil {
			// Splitting these roles needs a bus-backed room provider, which
			// arrives in M9. Failing loudly beats starting a gateway that
			// cannot place anyone.
			return errors.New("running --roles=gateway without world requires the NATS bus (M9)")
		}
		if cfg.devAuth {
			log.Warn("development authentication is enabled: " +
				"anyone who can reach this server can obtain a ticket for any name")
		}

		gw, err := gateway.New(gateway.Config{
			Rooms:          node,
			Maps:           game.Maps,
			ContentHash:    game.Hash,
			Tickets:        gateway.NewTicketStore(),
			Metrics:        m,
			Logger:         log,
			AllowedOrigins: splitAndTrim(cfg.origins),
			ClientDir:      cfg.clientDir,
			DevAuth:        cfg.devAuth,
		})
		if err != nil {
			return err
		}
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = gw.Shutdown(shutdownCtx)
		}()

		servers = append(servers, &http.Server{
			Addr:              cfg.addr,
			Handler:           gw.Routes(),
			ReadHeaderTimeout: 10 * time.Second,
			// No WriteTimeout: a WebSocket connection is long-lived by design
			// and a write deadline on the whole connection would sever it.
			// Individual writes are bounded inside the session instead.
		})
	}

	// The admin server carries metrics and pprof. It is always a separate
	// listener so it can be bound to a private interface and never exposed
	// alongside the game port.
	admin := http.NewServeMux()
	admin.Handle("GET /metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	admin.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok\n"))
	})
	registerPprof(admin)
	servers = append(servers, &http.Server{
		Addr:              cfg.adminAddr,
		Handler:           admin,
		ReadHeaderTimeout: 10 * time.Second,
	})

	// Bind every port before serving any of them, and before logging that
	// anything is listening. Starting the goroutines first means a bind
	// failure arrives after the "listening" and "ready" lines have already
	// printed, so the log claims success a moment before the process dies --
	// exactly the wrong thing to read while diagnosing a port collision.
	listeners := make([]net.Listener, 0, len(servers))
	for _, srv := range servers {
		ln, err := net.Listen("tcp", srv.Addr)
		if err != nil {
			for _, open := range listeners {
				open.Close()
			}
			return describeListenError(srv.Addr, err)
		}
		listeners = append(listeners, ln)
		log.Info("listening", "addr", ln.Addr().String())
	}

	errs := make(chan error, len(servers))
	for i, srv := range servers {
		go func(s *http.Server, ln net.Listener) {
			if err := s.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errs <- fmt.Errorf("serving on %s: %w", s.Addr, err)
			}
		}(srv, listeners[i])
	}

	log.Info("server ready", "roles", cfg.roles, "node", cfg.nodeID, "content", game.Hash)

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, srv := range servers {
		_ = srv.Shutdown(shutdownCtx)
	}
	return nil
}

func parseFlags() config {
	var cfg config
	flag.StringVar(&cfg.roles, "roles", "all", "comma-separated roles: all, gateway, world")
	flag.StringVar(&cfg.addr, "addr", ":8080", "game HTTP and WebSocket listen address")
	flag.StringVar(&cfg.adminAddr, "admin-addr", "127.0.0.1:9090",
		"metrics and pprof listen address; keep this off the public interface")
	flag.StringVar(&cfg.nodeID, "node-id", defaultNodeID(), "identifier for this node")
	flag.StringVar(&cfg.contentDir, "content-dir", "",
		"load content from this directory instead of the embedded copy (for live editing)")
	flag.StringVar(&cfg.defaultMap, "default-map", "tutorial", "map new players start in")
	flag.StringVar(&cfg.clientDir, "client-dir", "",
		"serve the built client from this directory; empty disables static serving")
	flag.StringVar(&cfg.origins, "origins", "",
		"comma-separated allowed WebSocket origins; empty means same-origin only")
	flag.BoolVar(&cfg.devAuth, "dev-auth", false,
		"issue game tickets with no identity check (development only)")
	flag.Uint64Var(&cfg.seed, "seed", 0,
		"fixed simulation seed, making a session reproducible; 0 draws a fresh one")
	flag.StringVar(&cfg.logLevel, "log-level", "info", "debug, info, warn, or error")
	flag.BoolVar(&cfg.logJSON, "log-json", false, "emit logs as JSON")
	flag.Parse()
	return cfg
}

func parseRoles(s string) (map[Role]bool, error) {
	roles := make(map[Role]bool)
	for _, part := range splitAndTrim(s) {
		switch Role(part) {
		case "all":
			roles[RoleGateway] = true
			roles[RoleWorld] = true
		case RoleGateway:
			roles[RoleGateway] = true
		case RoleWorld:
			roles[RoleWorld] = true
		default:
			return nil, fmt.Errorf("unknown role %q; want all, gateway, or world", part)
		}
	}
	if len(roles) == 0 {
		return nil, errors.New("no roles selected")
	}
	return roles, nil
}

// loadContent reads every content file, from disk if --content-dir was given
// and from the embedded copy otherwise.
//
// Any invalid content fails startup. Never start with a partial world: a
// server that silently comes up with a broken drop table produces bug reports
// weeks later that trace back to a warning nobody read.
func loadContent(cfg config, log *slog.Logger) (*content.Content, error) {
	var fsys fs.FS = gamedata.FS
	if cfg.contentDir != "" {
		fsys = os.DirFS(cfg.contentDir)
		log.Info("loading content from disk", "dir", cfg.contentDir)
	}

	game, err := content.Load(fsys)
	if err != nil {
		return nil, err
	}
	if _, ok := game.Maps[cfg.defaultMap]; !ok {
		return nil, fmt.Errorf("default map %q was not found among the loaded maps", cfg.defaultMap)
	}
	return game, nil
}

func newLogger(cfg config) (*slog.Logger, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.logLevel)); err != nil {
		return nil, fmt.Errorf("invalid log level %q", cfg.logLevel)
	}
	opts := &slog.HandlerOptions{Level: level}
	if cfg.logJSON {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts)), nil
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts)), nil
}

func defaultNodeID() string {
	if host, err := os.Hostname(); err == nil && host != "" {
		return host
	}
	return "node-1"
}

func splitAndTrim(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// describeListenError turns a bind failure into something actionable.
//
// A port collision is by far the most common way starting this fails, and
// 8080 in particular is occupied on a lot of developer machines by Docker,
// Lima, or another service. The raw syscall error says what happened but not
// what to do, and the resulting symptom downstream -- a dev proxy forwarding
// to whatever else holds the port -- is genuinely confusing to trace back.
func describeListenError(addr string, err error) error {
	if errors.Is(err, syscall.EADDRINUSE) {
		return fmt.Errorf(
			"port %s is already in use by another process.\n"+
				"  Find it with:  lsof -nP -iTCP%s -sTCP:LISTEN\n"+
				"  Or pick another port:  --addr=:8088\n"+
				"  If you are also running the Vite dev server, point it at the same port:\n"+
				"    MMO_SERVER=http://localhost:8088 npm --prefix client run dev",
			addr, portOf(addr))
	}
	return fmt.Errorf("listening on %s: %w", addr, err)
}

// portOf extracts ":8080" from an address like "0.0.0.0:8080".
func portOf(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[i:]
	}
	return addr
}
