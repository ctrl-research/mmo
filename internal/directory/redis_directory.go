package directory

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis is a Directory shared across nodes.
//
// The protocol is the one Memory implements; only the storage differs, which is
// what lets the whole system run as one process today and many nodes later
// without the calling code changing.
//
// Every operation that must not race is a Lua script, so the check and the write
// are one atomic step on the server. That is not a nicety here: "find the
// least-full channel and reserve a slot in it" done as a read then a write lets
// two simultaneous joins both take the same last slot, and for a private room it
// lets two members of one party each get their own dungeon -- the exact bug M7
// found and fixed on the other side of this interface.
//
// # Keys and Redis Cluster
//
// Every key carries the same `{dir}` hash tag, so in a clustered Redis they all
// land in one slot and the scripts stay valid. That caps the directory at one
// slot's worth of throughput, which is the right trade: the whole point of this
// data is that several nodes agree about it, and agreement needs the keys to be
// reachable from one script. A directory holding a few hundred rooms is nowhere
// near a slot's capacity.
type Redis struct {
	client *redis.Client
	prefix string
	node   NodeID
	now    func() time.Time

	// stop ends the heartbeat. A node that stops heartbeating stops being a
	// placement target, which is what makes killing a node survivable rather
	// than a source of rooms nobody is hosting.
	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// NodeTTL is how long a node stays a placement target without heartbeating.
//
// Longer than the heartbeat interval by a wide margin, because losing a node
// from the placement set is disruptive and a missed beat is not. Shorter than a
// player would tolerate waiting, because until it expires the directory keeps
// placing rooms on a node that is not there.
const (
	NodeTTL           = 15 * time.Second
	nodeHeartbeatTick = 5 * time.Second
)

// NewRedis returns a Directory backed by Redis, and registers this node as a
// placement target.
//
// Registration and heartbeating are the constructor's job so the call site
// stays the same shape as NewMemory's: a node announces itself by existing.
func NewRedis(ctx context.Context, client *redis.Client, prefix string, node NodeID) (*Redis, error) {
	if prefix == "" {
		prefix = "mmo"
	}
	if node == "" {
		return nil, errors.New("directory: a node ID is required")
	}

	r := &Redis{
		client: client,
		prefix: prefix,
		node:   node,
		now:    time.Now,
		stop:   make(chan struct{}),
	}

	if err := r.register(ctx); err != nil {
		return nil, err
	}

	r.wg.Add(1)
	go r.heartbeat()
	return r, nil
}

// NewRedisReader returns a directory for a process that reads placement but
// hosts nothing -- a gateway.
//
// It never registers and never heartbeats, which is the whole point. A process
// that registers is offering to host rooms, and placement takes the offer: a
// gateway that registered would be chosen to run a map it has no simulation
// for, and the player sent there would wait out a timeout on a room nobody is
// going to start. It would also be picked by another gateway looking for
// somewhere to put a character.
//
// There is no node ID because there is nothing to identify: this process is
// never the answer to "where should this room go".
func NewRedisReader(client *redis.Client, prefix string) *Redis {
	if prefix == "" {
		prefix = "mmo"
	}
	return &Redis{
		client: client,
		prefix: prefix,
		now:    time.Now,
		stop:   make(chan struct{}),
	}
}

// Key layout. The shared `{dir}` hash tag is deliberate -- see the type comment.
func (r *Redis) base() string       { return r.prefix + ":{dir}" }
func (r *Redis) seqKey() string     { return r.base() + ":seq" }
func (r *Redis) allKey() string     { return r.base() + ":all" }
func (r *Redis) nodesKey() string   { return r.base() + ":nodes" }
func (r *Redis) aliveKey() string   { return r.base() + ":alive" }
func (r *Redis) loadKey() string    { return r.base() + ":load" }
func (r *Redis) nodeSeqKey() string { return r.base() + ":nodeseq" }

func (r *Redis) keySetKey(key RoomKey) string { return r.base() + ":key:" + key.String() }
func (r *Redis) instPrefix() string           { return r.base() + ":inst:" }

// keySetPrefix is the stem of every per-room-key set, so a script can find the
// set an instance belongs to from the instance's own fields.
func (r *Redis) keySetPrefix() string { return r.base() + ":key:" }

// register adds this node to the placement set.
//
// The registration score is assigned once and kept: it is what breaks a tie
// between equally loaded nodes, and a node that reconnected to the front of the
// queue would take a disproportionate share of new rooms every time it
// restarted.
var registerScript = redis.NewScript(`
	local nodes = KEYS[1]
	local alive = KEYS[2]
	local nodeseq = KEYS[3]
	local node = ARGV[1]
	local expiry = ARGV[2]

	if redis.call('ZSCORE', nodes, node) == false then
		local seq = redis.call('INCR', nodeseq)
		redis.call('ZADD', nodes, seq, node)
	end
	redis.call('ZADD', alive, expiry, node)
	return 1
`)

func (r *Redis) register(ctx context.Context) error {
	expiry := r.now().Add(NodeTTL).UnixMilli()
	err := registerScript.Run(ctx, r.client,
		[]string{r.nodesKey(), r.aliveKey(), r.nodeSeqKey()},
		string(r.node), expiry,
	).Err()
	if err != nil {
		return fmt.Errorf("directory: registering node %s: %w", r.node, err)
	}
	return nil
}

func (r *Redis) heartbeat() {
	defer r.wg.Done()

	ticker := time.NewTicker(nodeHeartbeatTick)
	defer ticker.Stop()

	for {
		select {
		case <-r.stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), nodeHeartbeatTick)
			// Errors are not fatal: the TTL is several beats long, so one
			// failure costs nothing and the next tick fixes it. A node that
			// cannot reach Redis at all will expire, which is correct.
			_ = r.register(ctx)
			cancel()
		}
	}
}

// Nodes returns the registered nodes in registration order, whether or not they
// are currently alive.
func (r *Redis) Nodes(ctx context.Context) ([]NodeID, error) {
	members, err := r.client.ZRange(ctx, r.nodesKey(), 0, -1).Result()
	if err != nil {
		return nil, fmt.Errorf("directory: listing nodes: %w", err)
	}
	out := make([]NodeID, 0, len(members))
	for _, m := range members {
		out = append(out, NodeID(m))
	}
	return out, nil
}

// LiveNodes returns the nodes currently eligible to host a room.
func (r *Redis) LiveNodes(ctx context.Context) ([]NodeID, error) {
	now := strconv.FormatInt(r.now().UnixMilli(), 10)
	members, err := r.client.ZRangeByScore(ctx, r.aliveKey(), &redis.ZRangeBy{
		Min: now,
		Max: "+inf",
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("directory: listing live nodes: %w", err)
	}

	// Ordered by registration rather than by expiry, so the answer is stable
	// and matches what placement will do.
	registered, err := r.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	live := make(map[NodeID]bool, len(members))
	for _, m := range members {
		live[NodeID(m)] = true
	}

	out := make([]NodeID, 0, len(members))
	for _, n := range registered {
		if live[n] {
			out = append(out, n)
		}
	}
	return out, nil
}

// The placement helper, shared by the two scripts that create an instance.
//
// Fewest rooms wins, ties broken by registration order -- the same rule Memory
// applies, and for the same reason: a room costs a goroutine and a tick loop
// whether or not anyone is in it, so counting rooms rather than players is what
// reflects the real cost.
const placeLua = `
	local function place(nodes, alive, load, now)
		local live = {}
		for _, n in ipairs(redis.call('ZRANGEBYSCORE', alive, now, '+inf')) do
			live[n] = true
		end

		local best, bestLoad
		for _, n in ipairs(redis.call('ZRANGE', nodes, 0, -1)) do
			if live[n] then
				local l = tonumber(redis.call('ZSCORE', load, n) or 0)
				if best == nil or l < bestLoad then
					best, bestLoad = n, l
				end
			end
		end
		return best
	end

	-- liveSet is the nodes heartbeating right now.
	local function liveSet(alive, now)
		local live = {}
		for _, n in ipairs(redis.call('ZRANGEBYSCORE', alive, now, '+inf')) do
			live[n] = true
		end
		return live
	end

	-- reap removes an instance whose host is not there any more.
	--
	-- A room is hosted by a process. When that process dies the room dies with
	-- it, but its registration does not: it stays in the directory holding a
	-- slot count for players who no longer exist, and placement keeps handing
	-- it out. Everyone sent there waits out a request to a node that will never
	-- answer, and the login fails -- forever, because nothing ever removed the
	-- registration.
	--
	-- Cleaned up at the moment it is noticed rather than by a sweeper. The
	-- process that would run a sweep is the one that just found the problem,
	-- and a background job is another thing to deploy and another thing to
	-- forget to deploy.
	local function reap(all, load, instPrefix, keySetPrefix, id)
		local inst = instPrefix .. id
		local h = redis.call('HMGET', inst, 'node', 'map', 'placement', 'owner')
		if not h[1] then return end

		local keystr
		if h[4] == '' or h[4] == false then
			keystr = h[2] .. '/' .. h[3]
		else
			keystr = h[2] .. '/' .. h[3] .. '/' .. h[4]
		end

		redis.call('ZREM', keySetPrefix .. keystr, id)
		redis.call('ZREM', all, id)
		redis.call('DEL', inst)
		if redis.call('ZSCORE', load, h[1]) then
			redis.call('ZINCRBY', load, -1, h[1])
		end
	end

	local function create(seq, keyset, all, load, instPrefix, node, map, placement, owner, capacity)
		local id = redis.call('INCR', seq)
		local key = instPrefix .. id
		redis.call('HSET', key,
			'node', node, 'map', map, 'placement', placement,
			'owner', owner, 'players', 1, 'capacity', capacity)
		redis.call('ZADD', keyset, id, id)
		redis.call('ZADD', all, id, id)
		redis.call('ZINCRBY', load, 1, node)
		return {1, tostring(id), node, map, placement, owner, '1', capacity}
	end
`

// joinScript reserves a slot in the least-full instance for a key, creating one
// if none has room.
//
// Reservation and placement are one atomic step, which is the whole reason this
// is a script. Doing it as a read then a write is what lets two simultaneous
// joins take the same last slot, and lets a private key end up with two
// instances.
var joinScript = redis.NewScript(placeLua + `
	local keyset = KEYS[1]
	local all = KEYS[2]
	local seq = KEYS[3]
	local nodes = KEYS[4]
	local alive = KEYS[5]
	local load = KEYS[6]

	local capacity = tonumber(ARGV[1])
	local map = ARGV[2]
	local placement = ARGV[3]
	local owner = ARGV[4]
	local now = ARGV[5]
	local instPrefix = ARGV[6]
	local keySetPrefix = ARGV[7]

	-- The emptiest instance with room. Ascending id, so a tie takes the lowest,
	-- matching Memory. Filling the emptiest rather than the fullest spreads
	-- players across channels: under per-player mob layering a packed room is
	-- disproportionately expensive.
	local live = liveSet(alive, now)
	local ids = redis.call('ZRANGE', keyset, 0, -1)
	local best, bestPlayers
	local surviving = 0
	for _, id in ipairs(ids) do
		local h = redis.call('HMGET', instPrefix .. id, 'players', 'capacity', 'node')
		if h[1] then
			-- A room on a node that is not there is not a room. Removed rather
			-- than skipped: left in place it keeps its slot count and its share
			-- of the node's load forever, and every future placement has to
			-- step over it again.
			if not live[h[3]] then
				reap(all, load, instPrefix, keySetPrefix, id)
			else
				surviving = surviving + 1
				local players, cap = tonumber(h[1]), tonumber(h[2])
				if players < cap and (best == nil or players < bestPlayers) then
					best, bestPlayers = id, players
				end
			end
		end
	end

	if best ~= nil then
		local players = redis.call('HINCRBY', instPrefix .. best, 'players', 1)
		local h = redis.call('HMGET', instPrefix .. best,
			'node', 'map', 'placement', 'owner', 'capacity')
		return {1, best, h[1], h[2], h[3], h[4], tostring(players), h[5]}
	end

	-- A private room is a single instance by definition: if the one that exists
	-- is full, the party is full, and creating a second would split the group
	-- across two dungeons.
	if placement == 'private' and surviving > 0 then
		return {0}
	end

	local node = place(nodes, alive, load, now)
	if node == nil then
		return {-1}
	end
	return create(seq, keyset, all, load, instPrefix, node, map, placement, owner, capacity)
`)

// newInstanceScript creates an additional instance for a key whether or not the
// existing ones have room.
var newInstanceScript = redis.NewScript(placeLua + `
	local keyset = KEYS[1]
	local all = KEYS[2]
	local seq = KEYS[3]
	local nodes = KEYS[4]
	local alive = KEYS[5]
	local load = KEYS[6]

	local capacity = tonumber(ARGV[1])
	local map = ARGV[2]
	local placement = ARGV[3]
	local owner = ARGV[4]
	local now = ARGV[5]
	local instPrefix = ARGV[6]

	local node = place(nodes, alive, load, now)
	if node == nil then
		return {-1}
	end
	return create(seq, keyset, all, load, instPrefix, node, map, placement, owner, capacity)
`)

// joinInstanceScript reserves a slot in one named instance.
var joinInstanceScript = redis.NewScript(placeLua + `
	local inst = KEYS[1]
	local alive = KEYS[2]
	local all = KEYS[3]
	local load = KEYS[4]
	local id = ARGV[1]
	local now = ARGV[2]
	local instPrefix = ARGV[3]
	local keySetPrefix = ARGV[4]

	if redis.call('EXISTS', inst) == 0 then
		return {-1}
	end
	local h = redis.call('HMGET', inst,
		'node', 'map', 'placement', 'owner', 'players', 'capacity')

	-- A channel hosted by a node that has gone is not a channel to switch to.
	-- Reported as unknown rather than full: it is not that the room is busy,
	-- it is that there is no room. Reaped on the way out for the same reason
	-- placement does it -- otherwise it is offered again on the next attempt.
	if not liveSet(alive, now)[h[1]] then
		reap(all, load, instPrefix, keySetPrefix, id)
		return {-1}
	end

	if tonumber(h[5]) >= tonumber(h[6]) then
		return {0}
	end

	local players = redis.call('HINCRBY', inst, 'players', 1)
	return {1, id, h[1], h[2], h[3], h[4], tostring(players), h[6]}
`)

// leaveScript releases one reserved slot, floored at zero.
//
// Floored rather than allowed to go negative: a double release is a bug
// somewhere else, and a negative count would make the instance look like it had
// room forever.
var leaveScript = redis.NewScript(`
	local inst = KEYS[1]

	if redis.call('EXISTS', inst) == 0 then
		return -1
	end
	if tonumber(redis.call('HGET', inst, 'players')) > 0 then
		redis.call('HINCRBY', inst, 'players', -1)
	end
	return 1
`)

// releaseScript removes an instance entirely.
//
// The key set to remove it from is derived inside the script from the
// instance's own fields, because the caller does not reliably know it: reading
// the instance first and then releasing leaves a window in which it changed.
// See the type comment for why building a key here is safe.
var releaseInstanceScript = redis.NewScript(`
	local all = KEYS[1]
	local load = KEYS[2]
	local instPrefix = ARGV[1]
	local keySetPrefix = ARGV[2]
	local id = ARGV[3]
	local onlyIfEmpty = ARGV[4] == '1'

	local inst = instPrefix .. id
	if redis.call('EXISTS', inst) == 0 then
		-- Already gone. For TryRelease that is the outcome the caller wanted;
		-- Release reports it as unknown, which the Go side decides.
		return 0
	end
	if onlyIfEmpty and tonumber(redis.call('HGET', inst, 'players')) > 0 then
		return -1
	end

	local h = redis.call('HMGET', inst, 'node', 'map', 'placement', 'owner')
	local keystr
	if h[4] == '' then
		keystr = h[2] .. '/' .. h[3]
	else
		keystr = h[2] .. '/' .. h[3] .. '/' .. h[4]
	end

	redis.call('ZREM', keySetPrefix .. keystr, id)
	redis.call('ZREM', all, id)
	redis.call('DEL', inst)
	if redis.call('ZSCORE', load, h[1]) then
		redis.call('ZINCRBY', load, -1, h[1])
	end
	return 1
`)

// Join reserves a slot, creating an instance if necessary.
func (r *Redis) Join(ctx context.Context, key RoomKey, capacity int) (Instance, error) {
	if !key.Valid() {
		return Instance{}, &KeyError{Key: key}
	}
	if capacity <= 0 {
		return Instance{}, &CapacityError{Capacity: capacity}
	}
	return r.runPlacement(ctx, joinScript, key, capacity)
}

// NewInstance creates an additional instance for a key.
func (r *Redis) NewInstance(ctx context.Context, key RoomKey, capacity int) (Instance, error) {
	if !key.Valid() {
		return Instance{}, &KeyError{Key: key}
	}
	if capacity <= 0 {
		return Instance{}, &CapacityError{Capacity: capacity}
	}
	if key.Placement == PlacementPrivate {
		// One instance per owner is what private means. A second would split a
		// party across two dungeons.
		return Instance{}, ErrNoCapacity
	}
	return r.runPlacement(ctx, newInstanceScript, key, capacity)
}

func (r *Redis) runPlacement(ctx context.Context, script *redis.Script, key RoomKey, capacity int) (Instance, error) {
	res, err := script.Run(ctx, r.client,
		[]string{
			r.keySetKey(key), r.allKey(), r.seqKey(),
			r.nodesKey(), r.aliveKey(), r.loadKey(),
		},
		capacity, key.MapID, string(key.Placement), key.OwnerKey,
		r.now().UnixMilli(), r.instPrefix(), r.keySetPrefix(),
	).Slice()
	if err != nil {
		return Instance{}, fmt.Errorf("directory: placing in %s: %w", key, err)
	}
	return decodePlacement(res, key)
}

// JoinInstance reserves a slot in one named instance.
func (r *Redis) JoinInstance(ctx context.Context, id InstanceID) (Instance, error) {
	res, err := joinInstanceScript.Run(ctx, r.client,
		[]string{r.instKey(id), r.aliveKey(), r.allKey(), r.loadKey()},
		strconv.FormatUint(uint64(id), 10), r.now().UnixMilli(),
		r.instPrefix(), r.keySetPrefix(),
	).Slice()
	if err != nil {
		return Instance{}, fmt.Errorf("directory: joining instance %d: %w", id, err)
	}
	return decodePlacement(res, RoomKey{})
}

// Leave releases one reserved slot.
func (r *Redis) Leave(ctx context.Context, id InstanceID) error {
	status, err := leaveScript.Run(ctx, r.client, []string{r.instKey(id)}).Int64()
	if err != nil {
		return fmt.Errorf("directory: leaving instance %d: %w", id, err)
	}
	if status == -1 {
		return ErrUnknownInstance
	}
	return nil
}

// Release removes an instance entirely.
func (r *Redis) Release(ctx context.Context, id InstanceID) error {
	status, err := r.release(ctx, id, false)
	if err != nil {
		return err
	}
	if status == 0 {
		return ErrUnknownInstance
	}
	return nil
}

// TryRelease removes an instance only if it is unoccupied.
func (r *Redis) TryRelease(ctx context.Context, id InstanceID) (bool, error) {
	status, err := r.release(ctx, id, true)
	if err != nil {
		return false, err
	}
	switch status {
	case -1:
		// Somebody is in it, so the room should keep running.
		return false, nil
	default:
		// Released, or already gone. Both are the outcome the caller wanted.
		return true, nil
	}
}

func (r *Redis) release(ctx context.Context, id InstanceID, onlyIfEmpty bool) (int64, error) {
	empty := "0"
	if onlyIfEmpty {
		empty = "1"
	}
	status, err := releaseInstanceScript.Run(ctx, r.client,
		[]string{r.allKey(), r.loadKey()},
		r.instPrefix(), r.base()+":key:", strconv.FormatUint(uint64(id), 10), empty,
	).Int64()
	if err != nil {
		return 0, fmt.Errorf("directory: releasing instance %d: %w", id, err)
	}
	return status, nil
}

// InstancesFor returns every live instance satisfying a key, ordered by ID.
// InstancesFor lists the channels of a map that are actually reachable.
//
// Instances on nodes that have stopped heartbeating are left out. This is what
// a player is shown when they open the channel list, and offering a channel
// whose host has gone is offering a door that leads nowhere: they pick it, the
// switch fails, and nothing about it tells them why. Placement reaps those
// registrations; this only declines to advertise them, because listing is a
// read and a read should not be the thing that changes the world.
func (r *Redis) InstancesFor(ctx context.Context, key RoomKey) ([]Instance, error) {
	ids, err := r.client.ZRange(ctx, r.keySetKey(key), 0, -1).Result()
	if err != nil {
		return nil, fmt.Errorf("directory: listing instances for %s: %w", key, err)
	}

	all, err := r.loadAll(ctx, ids)
	if err != nil {
		return nil, err
	}

	live, err := r.LiveNodes(ctx)
	if err != nil {
		return nil, err
	}
	running := make(map[NodeID]bool, len(live))
	for _, n := range live {
		running[n] = true
	}

	out := make([]Instance, 0, len(all))
	for _, inst := range all {
		if running[inst.Node] {
			out = append(out, inst)
		}
	}
	return out, nil
}

// Lookup returns one instance by ID.
func (r *Redis) Lookup(ctx context.Context, id InstanceID) (Instance, bool, error) {
	fields, err := r.client.HGetAll(ctx, r.instKey(id)).Result()
	if err != nil {
		return Instance{}, false, fmt.Errorf("directory: looking up instance %d: %w", id, err)
	}
	if len(fields) == 0 {
		return Instance{}, false, nil
	}
	return instanceFromHash(id, fields), true, nil
}

// List returns every live instance ordered by ID.
func (r *Redis) List(ctx context.Context) ([]Instance, error) {
	ids, err := r.client.ZRange(ctx, r.allKey(), 0, -1).Result()
	if err != nil {
		return nil, fmt.Errorf("directory: listing instances: %w", err)
	}
	return r.loadAll(ctx, ids)
}

// loadAll fetches several instances, skipping any that vanished in between.
//
// One pipeline rather than a round trip each: listing channels happens whenever
// a player opens the world map, and a round trip per channel would make that
// scale with how many channels a popular map has.
func (r *Redis) loadAll(ctx context.Context, ids []string) ([]Instance, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	pipe := r.client.Pipeline()
	cmds := make([]*redis.MapStringStringCmd, 0, len(ids))
	parsed := make([]InstanceID, 0, len(ids))
	for _, raw := range ids {
		id, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			continue
		}
		parsed = append(parsed, InstanceID(id))
		cmds = append(cmds, pipe.HGetAll(ctx, r.instPrefix()+raw))
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("directory: reading instances: %w", err)
	}

	out := make([]Instance, 0, len(cmds))
	for i, cmd := range cmds {
		fields, err := cmd.Result()
		if err != nil || len(fields) == 0 {
			// Released between the listing and the read. Skipping it is right:
			// it is no longer a place a player can be sent.
			continue
		}
		out = append(out, instanceFromHash(parsed[i], fields))
	}

	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Close stops the heartbeat, so this node stops being a placement target.
//
// The registration is deliberately left behind. Its score is what keeps
// placement order stable across restarts, and the liveness entry expiring is
// what takes the node out of service -- removing the registration too would
// make a restart look like a brand-new node and reshuffle placement.
func (r *Redis) Close() error {
	r.stopOnce.Do(func() { close(r.stop) })
	r.wg.Wait()
	return nil
}

func (r *Redis) instKey(id InstanceID) string {
	return r.instPrefix() + strconv.FormatUint(uint64(id), 10)
}

// decodePlacement turns a script's reply into an Instance or the error it
// stands for.
func decodePlacement(res []any, key RoomKey) (Instance, error) {
	if len(res) == 0 {
		return Instance{}, errors.New("directory: empty placement response")
	}

	status, _ := res[0].(int64)
	switch status {
	case 0:
		return Instance{}, ErrNoCapacity
	case -1:
		if len(res) == 1 && key == (RoomKey{}) {
			// JoinInstance's "no such instance". Placement's -1 means there was
			// no live node, which is a different problem and is reported below.
			return Instance{}, ErrUnknownInstance
		}
		return Instance{}, ErrNoLiveNode
	case 1:
	default:
		return Instance{}, fmt.Errorf("directory: unexpected placement status %d", status)
	}

	if len(res) != 8 {
		return Instance{}, errors.New("directory: unexpected placement response")
	}

	id, err := strconv.ParseUint(asString(res[1]), 10, 64)
	if err != nil {
		return Instance{}, fmt.Errorf("directory: parsing instance id: %w", err)
	}
	players, _ := strconv.Atoi(asString(res[6]))
	capacity, _ := strconv.Atoi(asString(res[7]))

	return Instance{
		ID:   InstanceID(id),
		Node: NodeID(asString(res[2])),
		Key: RoomKey{
			MapID:     asString(res[3]),
			Placement: Placement(asString(res[4])),
			OwnerKey:  asString(res[5]),
		},
		Players:  players,
		Capacity: capacity,
	}, nil
}

func instanceFromHash(id InstanceID, fields map[string]string) Instance {
	players, _ := strconv.Atoi(fields["players"])
	capacity, _ := strconv.Atoi(fields["capacity"])
	return Instance{
		ID:   id,
		Node: NodeID(fields["node"]),
		Key: RoomKey{
			MapID:     fields["map"],
			Placement: Placement(fields["placement"]),
			OwnerKey:  fields["owner"],
		},
		Players:  players,
		Capacity: capacity,
	}
}

// asString reads a Lua reply element, which may arrive as a string or as an
// integer depending on how the script produced it.
func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case int64:
		return strconv.FormatInt(t, 10)
	default:
		return ""
	}
}

var _ Directory = (*Redis)(nil)
