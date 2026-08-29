// Package auth handles identity: who a player is, and proving it to the game.
//
// Two ways in, and they make different trades:
//
//   - As an OIDC *relying party*, where no password ever reaches this system.
//     Preferable wherever a provider is available.
//   - With local accounts, where this server holds an Argon2id hash. Requiring
//     an external identity provider is a real barrier for a self-hosted game,
//     so this exists -- but custodying password hashes is an obligation rather
//     than a free convenience, which is why internal/auth/password.go is as
//     careful as it is.
//
// Either way, accounts are keyed by the provider's subject and an allowlist
// decides who may play, checked at registration and again at every sign-in.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Ephemeral stores short-lived, single-use values.
//
// Three things share this shape: the OAuth state and PKCE verifier between a
// login redirect and its callback, refresh tokens between rotations, and game
// tickets between an HTTP request and a WebSocket upgrade. All are secrets
// that must work exactly once and then be gone.
//
// Single-use is enforced by the store rather than by callers, because a
// caller that forgets to delete after reading turns a replay attack into a
// working login.
type Ephemeral interface {
	// Put stores a value under a key with a lifetime.
	Put(ctx context.Context, key string, value any, ttl time.Duration) error

	// Take retrieves and deletes a value atomically, reporting whether it was
	// present. A value that has expired is reported as absent.
	Take(ctx context.Context, key string, into any) (bool, error)

	Close() error
}

// NewKey returns a cryptographically random, URL-safe identifier.
//
// 32 bytes because these are bearer secrets: anyone holding one can use it,
// so guessing must be infeasible rather than merely unlikely.
func NewKey() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("auth: generating key: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// MemoryEphemeral keeps values in one process.
//
// Correct while a single gateway handles every connection. With several
// gateways a login can start on one and its callback land on another, so the
// Redis implementation is required -- which is why this is behind an interface
// rather than assumed.
type MemoryEphemeral struct {
	mu     sync.Mutex
	values map[string]memoryEntry
	now    func() time.Time
}

type memoryEntry struct {
	data      []byte
	expiresAt time.Time
}

// NewMemoryEphemeral returns an empty in-process store.
func NewMemoryEphemeral() *MemoryEphemeral {
	return &MemoryEphemeral{
		values: make(map[string]memoryEntry),
		now:    time.Now,
	}
}

// Put stores a value.
func (m *MemoryEphemeral) Put(_ context.Context, key string, value any, ttl time.Duration) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("auth: encoding value: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.sweepLocked()
	m.values[key] = memoryEntry{data: data, expiresAt: m.now().Add(ttl)}
	return nil
}

// Take retrieves and deletes a value.
func (m *MemoryEphemeral) Take(_ context.Context, key string, into any) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.values[key]
	if !ok {
		return false, nil
	}

	// Deleted whether or not it had expired: an expired value is spent, not
	// retryable, and leaving it behind only lets it be probed.
	delete(m.values, key)

	if m.now().After(entry.expiresAt) {
		return false, nil
	}
	if err := json.Unmarshal(entry.data, into); err != nil {
		return false, fmt.Errorf("auth: decoding value: %w", err)
	}
	return true, nil
}

// sweepLocked discards expired entries. Called on Put, which is frequent
// enough to keep the map bounded without a background goroutine.
func (m *MemoryEphemeral) sweepLocked() {
	now := m.now()
	for k, v := range m.values {
		if now.After(v.expiresAt) {
			delete(m.values, k)
		}
	}
}

// Close releases nothing; the method exists to satisfy Ephemeral.
func (m *MemoryEphemeral) Close() error { return nil }

// Len reports how many unexpired values are stored, for tests and metrics.
func (m *MemoryEphemeral) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	n := 0
	for _, v := range m.values {
		if now.Before(v.expiresAt) {
			n++
		}
	}
	return n
}

// RedisEphemeral shares values across gateways.
type RedisEphemeral struct {
	client *redis.Client
	prefix string
}

// NewRedisEphemeral returns a store backed by Redis.
func NewRedisEphemeral(client *redis.Client, prefix string) *RedisEphemeral {
	if prefix == "" {
		prefix = "mmo"
	}
	return &RedisEphemeral{client: client, prefix: prefix}
}

func (r *RedisEphemeral) key(k string) string { return r.prefix + ":eph:" + k }

// Put stores a value with a TTL.
func (r *RedisEphemeral) Put(ctx context.Context, key string, value any, ttl time.Duration) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("auth: encoding value: %w", err)
	}
	if err := r.client.Set(ctx, r.key(key), data, ttl).Err(); err != nil {
		return fmt.Errorf("auth: storing value: %w", err)
	}
	return nil
}

// Take retrieves and deletes a value atomically.
//
// GETDEL rather than GET followed by DEL: two clients redeeming the same
// ticket concurrently would both succeed with the latter, which is exactly the
// replay these tokens are single-use to prevent.
func (r *RedisEphemeral) Take(ctx context.Context, key string, into any) (bool, error) {
	data, err := r.client.GetDel(ctx, r.key(key)).Bytes()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("auth: taking value: %w", err)
	}
	if err := json.Unmarshal(data, into); err != nil {
		return false, fmt.Errorf("auth: decoding value: %w", err)
	}
	return true, nil
}

// Close releases the Redis client.
func (r *RedisEphemeral) Close() error { return r.client.Close() }

var (
	_ Ephemeral = (*MemoryEphemeral)(nil)
	_ Ephemeral = (*RedisEphemeral)(nil)
)
