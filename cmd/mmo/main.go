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
	devAuth    bool
	logLevel   string
	logJSON    bool
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

	maps, err := loadMaps(cfg, log)
	if err != nil {
		return err
	}

	dir := directory.NewMemory(directory.NodeID(cfg.nodeID))
	defer dir.Close()

	var node *world.Node
	if roles[RoleWorld] {
		node, err = world.NewNode(world.Config{
			Directory:  dir,
			Maps:       maps,
			DefaultMap: cfg.defaultMap,
			Logger:     log,
			Observer:   m,
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
			Tickets:        gateway.NewTicketStore(),
			Metrics:        m,
			Logger:         log,
			AllowedOrigins: splitAndTrim(cfg.origins),
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
		log.Info("gateway listening", "addr", cfg.addr)
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
	log.Info("admin listening", "addr", cfg.adminAddr)

	errs := make(chan error, len(servers))
	for _, srv := range servers {
		go func(s *http.Server) {
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errs <- fmt.Errorf("listening on %s: %w", s.Addr, err)
			}
		}(srv)
	}

	log.Info("server ready", "roles", cfg.roles, "node", cfg.nodeID, "maps", len(maps))

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
	flag.StringVar(&cfg.origins, "origins", "",
		"comma-separated allowed WebSocket origins; empty means same-origin only")
	flag.BoolVar(&cfg.devAuth, "dev-auth", false,
		"issue game tickets with no identity check (development only)")
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

// loadMaps reads every map, from disk if --content-dir was given and from the
// embedded copy otherwise.
//
// Any invalid map fails startup. Never start with a partial world: a server
// that silently comes up with a broken map produces bug reports weeks later
// that trace back to a warning nobody read.
func loadMaps(cfg config, log *slog.Logger) (map[string]*content.Map, error) {
	var fsys fs.FS = gamedata.FS
	root := "maps"

	if cfg.contentDir != "" {
		fsys = os.DirFS(cfg.contentDir)
		root = "maps"
		log.Info("loading content from disk", "dir", cfg.contentDir)
	}

	entries, err := fs.ReadDir(fsys, root)
	if err != nil {
		return nil, fmt.Errorf("reading maps: %w", err)
	}

	maps := make(map[string]*content.Map)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tmj") {
			continue
		}
		m, err := content.LoadMap(fsys, root+"/"+e.Name())
		if err != nil {
			return nil, err
		}
		if _, dup := maps[m.ID]; dup {
			return nil, fmt.Errorf("two maps both declare the id %q", m.ID)
		}
		maps[m.ID] = m
		log.Debug("map loaded", "id", m.ID, "spawns", len(m.Spawns))
	}

	if len(maps) == 0 {
		return nil, errors.New("no maps found")
	}
	if _, ok := maps[cfg.defaultMap]; !ok {
		return nil, fmt.Errorf("default map %q was not found among the loaded maps", cfg.defaultMap)
	}
	return maps, nil
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
