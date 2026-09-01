package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"

	"github.com/BurntSushi/toml"
	"github.com/ctrl-research/mmo/internal/auth"
	"github.com/ctrl-research/mmo/internal/bus"
	"github.com/ctrl-research/mmo/internal/directory"
	"github.com/ctrl-research/mmo/internal/store"
	"github.com/redis/go-redis/v9"
)

// openRedis opens the one Redis client everything shares, or nothing.
//
// Nil means no Redis was configured, which every caller reads as "use the
// in-process implementation". Returning a nil client rather than a bool keeps
// the choice in one place: a caller cannot forget to check a flag it was never
// given.
func openRedis(ctx context.Context, cfg config) (*redis.Client, func(), error) {
	if cfg.redisAddr == "" {
		return nil, func() {}, nil
	}

	client := redis.NewClient(&redis.Options{Addr: cfg.redisAddr})
	if err := client.Ping(ctx).Err(); err != nil {
		client.Close()
		return nil, func() {}, fmt.Errorf("connecting to Redis at %s: %w", cfg.redisAddr, err)
	}
	return client, func() { client.Close() }, nil
}

// openParties returns the party table, shared across nodes when Redis is
// configured.
//
// A party spans rooms and nodes by definition, so with the roles split there is
// no single process whose heap could hold one.
func openParties(cfg config, client *redis.Client, maxSize int, log *slog.Logger) directory.Parties {
	if client == nil {
		log.Info("using an in-process party table; " +
			"set --redis-addr before running more than one world node")
		return directory.NewMemoryParties(maxSize)
	}
	log.Info("using Redis for parties", "addr", cfg.redisAddr)
	return directory.NewRedisParties(client, "mmo", maxSize)
}

// openCoordination sets up lease and token storage.
//
// Redis is optional at hobby scale. With one process, in-memory implementations
// are correct: they run the same acquire/renew/release protocol with the same
// fencing tokens, and the database-side fencing check -- which is what actually
// enforces the single-writer invariant -- is identical either way.
//
// It becomes required with several gateways, because a login can start on one
// and its callback land on another, and because two processes cannot share an
// in-memory lease table.
func openCoordination(ctx context.Context, cfg config, client *redis.Client, db *store.Store, log *slog.Logger) (directory.Leases, auth.Ephemeral, error) {
	if client == nil {
		log.Info("using in-process leases and token storage; " +
			"set --redis-addr before running more than one process")

		leases := directory.NewMemoryLeases()

		// Seeded above the tokens already in the database. Without this the
		// counter restarts at one after every restart, and every character
		// that played before it fails its first checkpoint -- the fencing
		// predicate cannot tell a restarted counter from a stale writer, and
		// it is right not to.
		highest, err := db.HighestLeaseToken(ctx)
		if err != nil {
			return nil, nil, err
		}
		leases.Seed(highest)
		if highest > 0 {
			log.Info("lease tokens resume above the stored high-water mark", "from", highest)
		}

		ephemeral := auth.NewMemoryEphemeral()
		return leases, ephemeral, nil
	}

	log.Info("using Redis for leases and token storage", "addr", cfg.redisAddr)
	return directory.NewRedisLeases(client, "mmo"),
		auth.NewRedisEphemeral(client, "mmo"),
		nil
}

// sessionSecret returns the key that signs session tokens.
//
// A generated secret is fine for local play but logs everyone out on every
// restart, and cannot work across several gateways -- each would sign with a
// different key and reject the others' sessions. So it warns rather than
// silently doing something surprising.
func sessionSecret(cfg config, log *slog.Logger) (string, error) {
	if cfg.sessionSecret != "" {
		if len(cfg.sessionSecret) < 32 {
			return "", fmt.Errorf("session secret must be at least 32 bytes, got %d", len(cfg.sessionSecret))
		}
		return cfg.sessionSecret, nil
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generating a session secret: %w", err)
	}
	log.Warn("no session secret configured; generated a temporary one. " +
		"Everyone is signed out on restart, and several gateways would reject each other's sessions. " +
		"Set --session-secret or SESSION_SECRET for anything long-lived.")
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// providersFile is the TOML shape of the OIDC provider configuration.
type providersFile struct {
	Provider map[string]struct {
		DisplayName  string   `toml:"display_name"`
		Issuer       string   `toml:"issuer"`
		ClientID     string   `toml:"client_id"`
		ClientSecret string   `toml:"client_secret"`
		Scopes       []string `toml:"scopes"`
	} `toml:"provider"`
}

// loadProviders reads the OIDC provider configuration.
//
// Client secrets are read from the environment rather than written into the
// file, so the configuration can be committed while the secrets are not: a
// value of "env:GOOGLE_CLIENT_SECRET" is looked up at load.
func loadProviders(path string) ([]auth.ProviderConfig, error) {
	if path == "" {
		return nil, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading providers file: %w", err)
	}

	var f providersFile
	if err := toml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parsing providers file: %w", err)
	}

	// Sorted so the login screen's order does not depend on map iteration.
	ids := make([]string, 0, len(f.Provider))
	for id := range f.Provider {
		ids = append(ids, id)
	}
	sortStrings(ids)

	out := make([]auth.ProviderConfig, 0, len(ids))
	for _, id := range ids {
		raw := f.Provider[id]

		secret, err := resolveSecret(raw.ClientSecret)
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", id, err)
		}

		out = append(out, auth.ProviderConfig{
			ID:           id,
			DisplayName:  raw.DisplayName,
			Issuer:       raw.Issuer,
			ClientID:     raw.ClientID,
			ClientSecret: secret,
			Scopes:       raw.Scopes,
		})
	}
	return out, nil
}

// resolveSecret expands an "env:NAME" reference.
func resolveSecret(value string) (string, error) {
	const prefix = "env:"
	if len(value) <= len(prefix) || value[:len(prefix)] != prefix {
		return value, nil
	}

	name := value[len(prefix):]
	secret := os.Getenv(name)
	if secret == "" {
		return "", fmt.Errorf("environment variable %s is not set", name)
	}
	return secret, nil
}

// sortStrings is a tiny insertion sort, to avoid importing sort for one call.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// envOr returns an environment variable or a fallback.
func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// openBus chooses the message bus.
//
// The in-process one is not a stand-in: with every role in one process it is
// the correct implementation, and it is the one the tests use. NATS becomes
// required the moment two processes have to see each other's rooms, and nothing
// above internal/bus changes when it does -- which is the claim M0 made by
// putting the interface there in the first place, and this is where it gets
// tested.
func openBus(cfg config, log *slog.Logger) (bus.Bus, error) {
	if cfg.natsURL == "" {
		log.Info("using the in-process message bus; " +
			"set --nats-url before running more than one process")
		return bus.NewInProc(), nil
	}

	b, err := bus.Connect(cfg.natsURL)
	if err != nil {
		return nil, err
	}
	log.Info("using NATS for the message bus", "url", cfg.natsURL)
	return b, nil
}

// openDirectory chooses where instance placement lives.
//
// Redis when --redis-addr is set, and this is not optional in the way leases
// are: two processes with in-memory directories do not disagree about placement,
// they are unaware of each other's rooms entirely, and a player sent to a
// channel on the other node arrives at a room that does not exist there.
//
// The Redis directory registers this node and heartbeats until it is closed,
// which is what makes a node that dies stop receiving new rooms.
func openDirectory(ctx context.Context, cfg config, client *redis.Client, log *slog.Logger) (directory.Directory, error) {
	node := directory.NodeID(cfg.nodeID)

	if client == nil {
		log.Info("using the in-process room directory; " +
			"set --redis-addr before running more than one process")
		return directory.NewMemory(node), nil
	}

	dir, err := directory.NewRedis(ctx, client, "mmo", node)
	if err != nil {
		return nil, err
	}
	log.Info("using Redis for the room directory", "addr", cfg.redisAddr, "node", node)
	return dir, nil
}

// openPresence chooses where "who is online, and on which node" lives.
//
// Shared through Redis when there is a Redis. With one process the in-memory
// table is correct: the node asking where a character is, is the node holding
// them.
func openPresence(cfg config, client *redis.Client, log *slog.Logger) directory.Presence {
	if client == nil {
		log.Info("using the in-process presence table; " +
			"set --redis-addr before running more than one process")
		return directory.NewMemoryPresence()
	}
	log.Info("using Redis for presence", "addr", cfg.redisAddr)
	return directory.NewRedisPresence(client, "mmo")
}
