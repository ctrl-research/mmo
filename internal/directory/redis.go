package directory

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisLeases is a Leases shared across nodes.
//
// The protocol is the same one MemoryLeases implements; only the storage
// differs, which is what lets the whole system run as one process today and
// many nodes later without the calling code changing.
//
// Every operation that must not race is a Lua script, so the check and the
// write are one atomic step on the server. A read-then-write from the client
// would leave a window in which another node acquires between the two -- which
// is precisely the situation leases exist to prevent.
type RedisLeases struct {
	client *redis.Client
	prefix string
	now    func() time.Time
}

// NewRedisLeases returns a Leases backed by Redis.
func NewRedisLeases(client *redis.Client, prefix string) *RedisLeases {
	if prefix == "" {
		prefix = "mmo"
	}
	return &RedisLeases{client: client, prefix: prefix, now: time.Now}
}

func (r *RedisLeases) key(characterID string) string {
	return r.prefix + ":lease:char:" + characterID
}

func (r *RedisLeases) counterKey() string { return r.prefix + ":lease:counter" }

// acquireScript takes a lease only if none is held.
//
// The token counter is incremented unconditionally on success, including when
// reclaiming an expired lease. That is deliberate: the previous holder may
// still be running and about to write, and its lower token is what the
// database rejects.
var acquireScript = redis.NewScript(`
	local key = KEYS[1]
	local counter = KEYS[2]
	local node = ARGV[1]
	local ttl = tonumber(ARGV[2])

	if redis.call('EXISTS', key) == 1 then
		return {0, redis.call('GET', key)}
	end

	local token = redis.call('INCR', counter)
	redis.call('SET', key, node .. ':' .. token, 'PX', ttl)
	return {1, tostring(token)}
`)

// renewScript extends a lease only if the caller still holds it, compared by
// token rather than by node -- the same node may hold a newer lease after a
// reconnect, and the older session must not extend it.
var renewScript = redis.NewScript(`
	local key = KEYS[1]
	local expected = ARGV[1]
	local ttl = tonumber(ARGV[2])

	if redis.call('GET', key) ~= expected then
		return 0
	end
	redis.call('PEXPIRE', key, ttl)
	return 1
`)

// releaseScript deletes the lease only if the caller still holds it.
//
// An unconditional delete would let a straggler from a previous session revoke
// the current owner's lease, which is exactly what fencing exists to prevent.
var releaseScript = redis.NewScript(`
	local key = KEYS[1]
	local expected = ARGV[1]

	if redis.call('GET', key) == expected then
		redis.call('DEL', key)
	end
	return 1
`)

// seedScript raises the token counter to at least a floor, never lowering it.
//
// Monotonic because several nodes start at once and each seeds from the same
// database: the last one to run must not undo the others, and a node that
// started before a busy period must not drag the counter back.
var seedScript = redis.NewScript(`
	local counter = KEYS[1]
	local floor = tonumber(ARGV[1])
	local current = tonumber(redis.call('GET', counter) or '0')

	if current < floor then
		redis.call('SET', counter, floor)
		return {floor, 1}
	end
	return {current, 0}
`)

// Seed raises the fencing counter above a floor, which must be the highest
// token any character has already been written with.
//
// The counter lives in Redis and the tokens it is compared against live in
// Postgres, so this is the one value in Redis whose loss is not free. Every
// other thing in there is reconstructible and losing it costs a disconnect --
// but a counter that restarts at one issues tokens *below* what the database
// already holds, and the fencing predicate correctly rejects every checkpoint
// from every character that has played before. The symptom is not an error at
// startup: it is players losing their progress one lease renewal at a time,
// with the database doing exactly what it was told to.
//
// Redis is normally durable enough for this. "Normally" is doing a lot of work
// in that sentence: an eviction policy, a flush, or a restore from an empty
// snapshot all lose it, and the in-memory implementation has always seeded
// itself for precisely this reason.
// It reports whether the counter actually had to be raised. Equal is the
// healthy steady state -- a checkpoint writes the token it holds, so the
// database catches up to the counter and the two match on the next start.
// Warning on that would mean warning on every start, which is how a real
// warning gets ignored.
func (r *RedisLeases) Seed(ctx context.Context, floor int64) (at int64, raised bool, err error) {
	if floor <= 0 {
		return 0, false, nil
	}

	res, err := seedScript.Run(ctx, r.client, []string{r.counterKey()}, floor).Slice()
	if err != nil {
		return 0, false, fmt.Errorf("directory: seeding the lease counter: %w", err)
	}
	if len(res) != 2 {
		return 0, false, errors.New("directory: unexpected seed response")
	}

	at, _ = res[0].(int64)
	flag, _ := res[1].(int64)
	return at, flag == 1, nil
}

// Acquire takes exclusive ownership of a character.
func (r *RedisLeases) Acquire(ctx context.Context, characterID string, node NodeID) (Lease, error) {
	if characterID == "" {
		return Lease{}, errors.New("directory: character ID is required")
	}

	res, err := acquireScript.Run(ctx, r.client,
		[]string{r.key(characterID), r.counterKey()},
		string(node), LeaseTTL.Milliseconds(),
	).Slice()
	if err != nil {
		return Lease{}, fmt.Errorf("directory: acquiring lease: %w", err)
	}
	if len(res) != 2 {
		return Lease{}, errors.New("directory: unexpected acquire response")
	}

	ok, _ := res[0].(int64)
	value, _ := res[1].(string)

	if ok != 1 {
		holder := value
		if i := strings.LastIndex(value, ":"); i >= 0 {
			holder = value[:i]
		}
		return Lease{}, fmt.Errorf("%w: held by %s", ErrLeaseHeld, holder)
	}

	token, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return Lease{}, fmt.Errorf("directory: parsing lease token: %w", err)
	}

	return Lease{
		CharacterID: characterID,
		Node:        node,
		Token:       token,
		ExpiresAt:   r.now().Add(LeaseTTL),
	}, nil
}

// Renew extends a lease this node still holds.
func (r *RedisLeases) Renew(ctx context.Context, l Lease) (Lease, error) {
	ok, err := renewScript.Run(ctx, r.client,
		[]string{r.key(l.CharacterID)},
		leaseValue(l), LeaseTTL.Milliseconds(),
	).Int64()
	if err != nil {
		return Lease{}, fmt.Errorf("directory: renewing lease: %w", err)
	}
	if ok != 1 {
		return Lease{}, ErrLeaseLost
	}

	l.ExpiresAt = r.now().Add(LeaseTTL)
	return l, nil
}

// Release relinquishes ownership.
func (r *RedisLeases) Release(ctx context.Context, l Lease) error {
	if err := releaseScript.Run(ctx, r.client,
		[]string{r.key(l.CharacterID)}, leaseValue(l),
	).Err(); err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("directory: releasing lease: %w", err)
	}
	return nil
}

// Close releases nothing: the client belongs to whoever opened it.
//
// It used to close the client, which was harmless while every user opened its
// own. One shared client made that a component unilaterally closing a
// connection the directory, presence and token storage are still using.
func (r *RedisLeases) Close() error { return nil }

// leaseValue is the stored representation: node and token together, so a
// comparison establishes both who holds it and which lease it is.
func leaseValue(l Lease) string {
	return string(l.Node) + ":" + strconv.FormatInt(l.Token, 10)
}

var _ Leases = (*RedisLeases)(nil)
