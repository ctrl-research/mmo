package directory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// RedisPresence is a Presence shared across nodes.
//
// This is the table a whisper is routed through: "which node is holding Alice"
// is a question one node asks about a session on another, and with a presence
// table per process the answer is always "nobody". Losing it costs everyone
// their friends list for a moment and nothing else, which is what makes Redis
// the right home for it and Postgres the wrong one.
//
// Two indexes, kept in step by Lua: characters by ID, and IDs by normalised
// name. Two writes from the client would leave a window in which a renamed
// character is reachable under both names, or neither.
//
// Records carry the node holding the session, and a node clears its own on
// startup -- see ForgetNode. There is deliberately no TTL: a session announces
// once and re-announces on transfer, so a TTL would need a heartbeat per
// character, and the cost of that is real while the benefit is covered by the
// startup sweep for the common case. What is *not* covered is a node that dies
// and never comes back, whose characters stay listed until something clears
// them; that belongs with the chaos-testing item in M9, where a node death is
// actually simulated.
type RedisPresence struct {
	client *redis.Client
	prefix string
}

// NewRedisPresence returns a Presence backed by Redis.
func NewRedisPresence(client *redis.Client, prefix string) *RedisPresence {
	if prefix == "" {
		prefix = "mmo"
	}
	return &RedisPresence{client: client, prefix: prefix}
}

// Key layout. The shared `{pres}` hash tag keeps both indexes in one slot so the
// scripts stay valid on a clustered Redis -- the same trade the directory makes,
// and for the same reason: the two indexes have to be updated together.
func (r *RedisPresence) base() string      { return r.prefix + ":{pres}" }
func (r *RedisPresence) byIDKey() string   { return r.base() + ":by_id" }
func (r *RedisPresence) byNameKey() string { return r.base() + ":by_name" }

// storedOnline is the wire form of an Online record.
//
// JSON rather than a packed string: a character name can contain anything, and a
// separator-delimited encoding is one escaping bug away from a name that breaks
// the table. This is ephemeral coordination data, so the field names cost
// nothing worth counting.
type storedOnline struct {
	CharacterID string `json:"id"`
	Name        string `json:"name"`
	Node        string `json:"node"`
	MapID       string `json:"map"`
	Away        bool   `json:"away,omitempty"`

	// Norm is the name's lookup form, stored rather than derived.
	//
	// The scripts need it to drop a stale name index, and Lua cannot compute it:
	// string.lower is ASCII-only and does not trim, so it disagrees with
	// NormaliseName for any name with an accent or a stray space -- and a
	// disagreement here leaves a name pointing at a character forever. Writing
	// it down means there is one definition of the lookup form and it lives in
	// Go.
	Norm string `json:"norm"`
}

// announceScript writes both indexes together, dropping any stale name.
//
// A re-announce under a different name would otherwise leave the old name
// pointing at this character forever -- which is a whisper that reaches somebody
// who no longer exists under that name.
var announceScript = redis.NewScript(`
	local byID = KEYS[1]
	local byName = KEYS[2]
	local id = ARGV[1]
	local normalised = ARGV[2]
	local record = ARGV[3]
	local previous = redis.call('HGET', byID, id)

	if previous then
		local ok, old = pcall(cjson.decode, previous)
		if ok and old['norm'] and old['norm'] ~= normalised then
			-- Only if it still points at us. A name reused by a different
			-- character must not be deleted out from under them.
			if redis.call('HGET', byName, old['norm']) == id then
				redis.call('HDEL', byName, old['norm'])
			end
		end
	end

	redis.call('HSET', byID, id, record)
	redis.call('HSET', byName, normalised, id)
	return 1
`)

// forgetScript removes both indexes together.
var forgetScript = redis.NewScript(`
	local byID = KEYS[1]
	local byName = KEYS[2]
	local id = ARGV[1]
	local record = redis.call('HGET', byID, id)

	if not record then
		return 0
	end

	local ok, who = pcall(cjson.decode, record)
	if ok and who['norm'] then
		-- Only if it still points at us, for the same reason as above.
		if redis.call('HGET', byName, who['norm']) == id then
			redis.call('HDEL', byName, who['norm'])
		end
	end
	redis.call('HDEL', byID, id)
	return 1
`)

// forgetNodeScript removes every character attributed to one node.
var forgetNodeScript = redis.NewScript(`
	local byID = KEYS[1]
	local byName = KEYS[2]
	local node = ARGV[1]

	local all = redis.call('HGETALL', byID)
	local removed = 0
	for i = 1, #all, 2 do
		local id, record = all[i], all[i + 1]
		local ok, who = pcall(cjson.decode, record)
		if ok and who['node'] == node then
			if who['norm'] and redis.call('HGET', byName, who['norm']) == id then
				redis.call('HDEL', byName, who['norm'])
			end
			redis.call('HDEL', byID, id)
			removed = removed + 1
		end
	end
	return removed
`)

// Announce records a character as online.
func (r *RedisPresence) Announce(ctx context.Context, who Online) error {
	if who.CharacterID == "" {
		return fmt.Errorf("directory: presence needs a character ID")
	}

	record, err := json.Marshal(storedOnline{
		CharacterID: who.CharacterID,
		Name:        who.Name,
		Node:        string(who.Node),
		MapID:       who.MapID,
		Away:        who.Away,
		Norm:        NormaliseName(who.Name),
	})
	if err != nil {
		return fmt.Errorf("directory: encoding presence: %w", err)
	}

	err = announceScript.Run(ctx, r.client,
		[]string{r.byIDKey(), r.byNameKey()},
		who.CharacterID, NormaliseName(who.Name), string(record),
	).Err()
	if err != nil {
		return fmt.Errorf("directory: announcing %s: %w", who.CharacterID, err)
	}
	return nil
}

// Forget removes a character.
func (r *RedisPresence) Forget(ctx context.Context, characterID string) error {
	err := forgetScript.Run(ctx, r.client,
		[]string{r.byIDKey(), r.byNameKey()}, characterID,
	).Err()
	if err != nil {
		return fmt.Errorf("directory: forgetting %s: %w", characterID, err)
	}
	return nil
}

// ForgetNode removes every character attributed to a node.
func (r *RedisPresence) ForgetNode(ctx context.Context, node NodeID) error {
	err := forgetNodeScript.Run(ctx, r.client,
		[]string{r.byIDKey(), r.byNameKey()}, string(node),
	).Err()
	if err != nil {
		return fmt.Errorf("directory: forgetting node %s: %w", node, err)
	}
	return nil
}

// ByName finds an online character by display name.
func (r *RedisPresence) ByName(ctx context.Context, name string) (Online, bool, error) {
	id, err := r.client.HGet(ctx, r.byNameKey(), NormaliseName(name)).Result()
	if errors.Is(err, redis.Nil) {
		return Online{}, false, nil
	}
	if err != nil {
		return Online{}, false, fmt.Errorf("directory: looking up %q: %w", name, err)
	}
	return r.ByID(ctx, id)
}

// ByID finds an online character by ID.
func (r *RedisPresence) ByID(ctx context.Context, characterID string) (Online, bool, error) {
	record, err := r.client.HGet(ctx, r.byIDKey(), characterID).Result()
	if errors.Is(err, redis.Nil) {
		return Online{}, false, nil
	}
	if err != nil {
		return Online{}, false, fmt.Errorf("directory: looking up %s: %w", characterID, err)
	}

	who, ok := decodeOnline(record)
	if !ok {
		// A record that cannot be decoded is treated as absent rather than
		// returned as an error: it is ephemeral data, and a whisper refused
		// because of a bad row helps nobody.
		return Online{}, false, nil
	}
	return who, true, nil
}

// List returns everyone online, ordered by name.
func (r *RedisPresence) List(ctx context.Context) ([]Online, error) {
	records, err := r.client.HGetAll(ctx, r.byIDKey()).Result()
	if err != nil {
		return nil, fmt.Errorf("directory: listing presence: %w", err)
	}

	out := make([]Online, 0, len(records))
	for _, record := range records {
		if who, ok := decodeOnline(record); ok {
			out = append(out, who)
		}
	}
	// A roster that reshuffles every refresh is unusable.
	sortOnline(out)
	return out, nil
}

// Close releases nothing: the client belongs to whoever opened it.
func (r *RedisPresence) Close() error { return nil }

func decodeOnline(record string) (Online, bool) {
	var stored storedOnline
	if err := json.Unmarshal([]byte(record), &stored); err != nil {
		return Online{}, false
	}
	return Online{
		CharacterID: stored.CharacterID,
		Name:        stored.Name,
		Node:        NodeID(stored.Node),
		MapID:       stored.MapID,
		Away:        stored.Away,
	}, true
}

var _ Presence = (*RedisPresence)(nil)
