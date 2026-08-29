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
	"sort"
	"strings"
	"syscall"
	"time"

	gamedata "github.com/ctrl-research/mmo/content"
	"github.com/ctrl-research/mmo/internal/auth"
	"github.com/ctrl-research/mmo/internal/bus"
	"github.com/ctrl-research/mmo/internal/content"
	"github.com/ctrl-research/mmo/internal/directory"
	"github.com/ctrl-research/mmo/internal/gateway"
	"github.com/ctrl-research/mmo/internal/metrics"
	"github.com/ctrl-research/mmo/internal/store"
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

	databaseURL   string
	redisAddr     string
	sessionSecret string
	publicURL     string
	secureCookies bool
	providersFile string
	seedAllowlist string
	localAuth     bool
}

func main() {
	// Administration subcommands run and exit rather than starting a server.
	// Checked before flag parsing so "mmo allow jonathan" is not read as a
	// malformed server invocation.
	if handled, err := runAdmin(os.Args[1:]); handled {
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}

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

	db, err := store.Open(ctx, store.Config{URL: cfg.databaseURL, Logger: log})
	if err != nil {
		return err
	}
	defer db.Close()

	if err := seedAllowlist(ctx, db, cfg.seedAllowlist, log); err != nil {
		return err
	}

	allowed, err := db.AllowlistSize(ctx)
	if err != nil {
		return err
	}
	if allowed == 0 && !cfg.devAuth {
		// An empty allowlist admits nobody, which is the safe default but
		// looks exactly like a broken server to whoever tries to sign in.
		log.Warn("the allowlist is empty, so nobody can sign in. " +
			"Add someone with 'mmo allow USERNAME', or start with --dev-auth for local play")
	}

	dir := directory.NewMemory(directory.NodeID(cfg.nodeID))
	defer dir.Close()

	// In-process channels at this scale. Transfers run over it either way, so
	// the distributed path is exercised from the first portal rather than
	// first meeting reality in M9.
	msgBus := bus.NewInProc()
	defer msgBus.Close()

	// Presence answers "where is this character", which is what a whisper
	// needs; parties own membership, which spans rooms and nodes. Both are
	// ephemeral: losing them costs a regroup, never data, which is what makes
	// them Redis's problem at scale rather than Postgres's.
	presence := directory.NewMemoryPresence()
	defer presence.Close()

	parties := directory.NewMemoryParties(game.Balance.Party.MaxSize)
	defer parties.Close()

	// Redis is optional at hobby scale: with one process, in-memory leases and
	// token storage are correct, and the fencing check in Postgres -- which is
	// what actually enforces single-writer -- is identical either way. With
	// several gateways it becomes required, because a login can start on one
	// and its callback land on another.
	leases, ephemeral, closeRedis, err := openCoordination(ctx, cfg, db, log)
	if err != nil {
		return err
	}
	defer closeRedis()

	var node *world.Node
	if roles[RoleWorld] {
		node, err = world.NewNode(world.Config{
			Directory:  dir,
			Leases:     leases,
			Store:      db,
			Bus:        msgBus,
			Presence:   presence,
			Parties:    parties,
			NodeID:     cfg.nodeID,
			Content:    game,
			DefaultMap: cfg.defaultMap,
			Logger:     log,
			Observer:   m,
			Seed:       cfg.seed,
		})
		if err != nil {
			return err
		}
		if err := node.Start(ctx); err != nil {
			return err
		}
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

		secret, err := sessionSecret(cfg, log)
		if err != nil {
			return err
		}

		providerConfigs, err := loadProviders(cfg.providersFile)
		if err != nil {
			return err
		}

		sessions, err := auth.NewSessions([]byte(secret), ephemeral, cfg.secureCookies)
		if err != nil {
			return err
		}

		redirectBase := cfg.publicURL
		if redirectBase == "" && len(providerConfigs) > 0 {
			// The redirect URI must match what is registered with the
			// provider exactly, so guessing it would fail in a way that is
			// confusing to diagnose.
			return errors.New("--public-url is required when OIDC providers are configured")
		}
		providers, err := auth.NewRegistry(ctx, providerConfigs, redirectBase+"/auth/callback")
		if err != nil {
			return err
		}

		identity, err := auth.NewService(auth.ServiceConfig{
			Store:      db,
			Sessions:   sessions,
			Providers:  providers,
			Logger:     log,
			DevAuth:    cfg.devAuth,
			Classes:    classInfo(game),
			LocalAuth:  cfg.localAuth,
			DefaultMap: cfg.defaultMap,
		})
		if err != nil {
			return err
		}

		gw, err := gateway.New(gateway.Config{
			World:          node,
			Maps:           game.Maps,
			ContentHash:    game.Hash,
			Sessions:       sessions,
			Metrics:        m,
			Logger:         log,
			AllowedOrigins: splitAndTrim(cfg.origins),
			ClientDir:      cfg.clientDir,
			Passives:       game.Passives,
			DevAuth:        cfg.devAuth,
			Identity:       identity,
		})
		if err != nil {
			return err
		}
		log.Info("identity ready",
			"providers", providers.Len(), "local_auth", cfg.localAuth, "dev_auth", cfg.devAuth)
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
	flag.BoolVar(&cfg.localAuth, "local-auth", true,
		"allow username and password accounts held by this server")
	flag.BoolVar(&cfg.devAuth, "dev-auth", false,
		"issue game tickets with no identity check (development only)")
	flag.StringVar(&cfg.databaseURL, "database-url",
		envOr("DATABASE_URL", "postgres://mmo:devpassword@localhost:5432/mmo?sslmode=disable"),
		"Postgres connection string")
	flag.StringVar(&cfg.redisAddr, "redis-addr", envOr("REDIS_ADDR", ""),
		"Redis address; empty keeps leases and tokens in this process, which is correct for a single node")
	flag.StringVar(&cfg.sessionSecret, "session-secret", os.Getenv("SESSION_SECRET"),
		"secret signing session tokens; a random one is generated if unset, which logs everyone out on restart")
	flag.StringVar(&cfg.publicURL, "public-url", envOr("PUBLIC_URL", ""),
		"externally reachable base URL, used to build the OIDC redirect URI")
	flag.BoolVar(&cfg.secureCookies, "secure-cookies", false,
		"mark session cookies Secure; required for HTTPS, must be off for plain-HTTP local play")
	flag.StringVar(&cfg.providersFile, "providers", envOr("PROVIDERS_FILE", ""),
		"TOML file describing OIDC providers")
	flag.StringVar(&cfg.seedAllowlist, "seed-allowlist", envOr("SEED_ALLOWLIST", ""),
		"comma-separated local usernames added to the allowlist at boot; additive, never removes")
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

// seedAllowlist ensures every username in a comma-separated list can sign in.
//
// Additive only. Making the environment authoritative would silently undo
// `mmo revoke` on the next restart, and a revocation that comes back on its
// own is worse than no revocation at all. Every write is INSERT ... ON
// CONFLICT DO NOTHING, so a restart with an unchanged list is a no-op.
//
// Local subject entries only, matching what `mmo allow` defaults to. Seeding
// a provider's users would mean encoding provider names in the environment,
// which is what --providers already does properly.
func seedAllowlist(ctx context.Context, db *store.Store, list string, log *slog.Logger) error {
	raw := splitAndTrim(list)
	if len(raw) == 0 {
		return nil
	}

	names := make([]string, 0, len(raw))
	for _, r := range raw {
		// Normalised on the way in, because sign-in normalises too: an entry
		// stored as "Alice" would never match a login as "alice".
		names = append(names, auth.NormaliseUsername(r))
	}

	for _, name := range names {
		if err := db.AddAllowlistEntry(ctx, "local", store.MatchSubject, name,
			"seeded from SEED_ALLOWLIST"); err != nil {
			return fmt.Errorf("seeding allowlist with %q: %w", name, err)
		}
	}
	log.Info("allowlist seeded", "usernames", names)
	return nil
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

// classInfo flattens the content classes for the character screen.
//
// Flattened rather than passed through, because internal/auth has no business
// importing the simulation's content model to render a menu -- and because the
// screen needs a stable order, which a map does not have.
func classInfo(game *content.Content) []auth.ClassInfo {
	out := make([]auth.ClassInfo, 0, len(game.Classes))
	for _, c := range game.Classes {
		out = append(out, auth.ClassInfo{
			ID:          c.ID,
			Name:        c.Name,
			Description: c.Description,
			PrimaryStat: c.PrimaryStat,
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
