package directory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// RedisParties is a Parties shared across nodes.
//
// A party spans rooms and nodes by definition, so its state was never going to
// survive on one node's heap once the roles split. This is the last of the
// coordination tables to move, and the one with the most rules: eleven methods,
// nested state, and an invitation that expires.
//
// Every method is a single Lua script, because every method is a check followed
// by a write and doing that from the client leaves a window. "Is there room?"
// then "add them" lets two simultaneous accepts both take the last slot; "am I
// the leader?" then "kick them" lets somebody who was demoted in between kick
// anyway. Redis runs a script to completion with nothing interleaved, which is
// the only reason the guards mean anything.
type RedisParties struct {
	client  *redis.Client
	prefix  string
	maxSize int

	// inviteTTL is per-instance so tests can use a short one and watch a real
	// invitation actually expire, rather than asserting against a fake clock
	// that proves only that the arithmetic is right.
	inviteTTL time.Duration
}

// NewRedisParties returns a Parties backed by Redis.
func NewRedisParties(client *redis.Client, prefix string, maxSize int) *RedisParties {
	if prefix == "" {
		prefix = "mmo"
	}
	if maxSize <= 0 {
		maxSize = 6
	}
	return &RedisParties{
		client:    client,
		prefix:    prefix,
		maxSize:   maxSize,
		inviteTTL: InviteTTL,
	}
}

// Key layout. The shared `{party}` hash tag keeps the roster, the membership
// index and every invitation in one slot, so a script may touch all three --
// which each of them has to, since a membership change that updated the roster
// but not the index would be a character in a party that does not know them.
func (r *RedisParties) base() string        { return r.prefix + ":{party}" }
func (r *RedisParties) partiesKey() string  { return r.base() + ":parties" }
func (r *RedisParties) memberOfKey() string { return r.base() + ":member_of" }
func (r *RedisParties) inviteKey(characterID string) string {
	return r.base() + ":invite:" + characterID
}

// An invitation is a key with a TTL rather than a field with a stored expiry.
//
// Redis does the expiring, which means there is no clock crossing the wire and
// no invitation left behind for somebody who was asked once and never logged in
// again. A stored deadline would need every read to check it and something to
// eventually sweep what nobody read.
type storedInvite struct {
	Party PartyID `json:"party"`
	From  string  `json:"from"`
}

type storedParty struct {
	ID      PartyID        `json:"id"`
	Leader  string         `json:"leader"`
	Members []storedMember `json:"members"`
	Loot    string         `json:"loot"`
}

type storedMember struct {
	CharacterID string `json:"id"`
	Name        string `json:"name"`
}

// partyLua is shared by every script below.
//
// Written once because the same three mistakes are available in each: reading a
// roster without noticing the index points at a party that is gone, saving one
// without re-encoding, and removing a member without considering that the party
// may no longer be worth having.
const partyLua = `
	local function partyOf(parties, memberOf, who)
		local id = redis.call('HGET', memberOf, who)
		if not id then return nil, nil end

		local raw = redis.call('HGET', parties, id)
		if not raw then
			-- An index pointing at a party that no longer exists. Cleaned up
			-- rather than reported, because there is nothing a caller could do
			-- about it and leaving it makes the character permanently unable
			-- to join anything.
			redis.call('HDEL', memberOf, who)
			return nil, nil
		end

		local ok, p = pcall(cjson.decode, raw)
		if not ok then return nil, nil end
		return id, p
	end

	local function save(parties, id, p)
		redis.call('HSET', parties, id, cjson.encode(p))
	end

	-- remove takes a character out of their party and reports what is left.
	--
	-- Returns a code, the party, and whether it disbanded. The party is
	-- returned even when it disbanded so the caller can tell the people who
	-- were in it -- which is the whole reason Leave returns a party at all.
	local function remove(parties, memberOf, who)
		local id, p = partyOf(parties, memberOf, who)
		if not p then
			redis.call('HDEL', memberOf, who)
			return 'not_in', nil
		end

		redis.call('HDEL', memberOf, who)
		for i, m in ipairs(p['members']) do
			if m['id'] == who then
				table.remove(p['members'], i)
				break
			end
		end

		-- A party of one is not a party.
		if #p['members'] <= 1 then
			for _, m in ipairs(p['members']) do
				redis.call('HDEL', memberOf, m['id'])
			end
			redis.call('HDEL', parties, id)
			-- Dropped rather than emptied. cjson encodes an empty table as {},
			-- an object, which does not decode into a JSON array -- and the
			-- table *is* empty by this point, because removing the member
			-- above already took the last one out. An absent field decodes to
			-- a nil slice, which is the contract: a disbanded party comes back
			-- with no members.
			p['members'] = nil
			return '', p
		end

		-- Leadership moves rather than the party dissolving.
		if p['leader'] == who then
			p['leader'] = p['members'][1]['id']
		end
		save(parties, id, p)
		return '', p
	end

	-- ledParty resolves the caller's party and checks they lead it.
	local function ledParty(parties, memberOf, leader)
		local id, p = partyOf(parties, memberOf, leader)
		if not p then return 'not_in', nil, nil end
		if p['leader'] ~= leader then return 'not_leader', nil, nil end
		return '', id, p
	end

	local function reply(code, p)
		if p == nil then return {code, ''} end
		return {code, cjson.encode(p)}
	end
`

// partyCreateScript starts a party with one member.
var partyCreateScript = redis.NewScript(partyLua + `
	local parties = KEYS[1]
	local memberOf = KEYS[2]
	local id = ARGV[1]
	local who = ARGV[2]
	local name = ARGV[3]
	local loot = ARGV[4]

	if redis.call('HEXISTS', memberOf, who) == 1 then
		return reply('already', nil)
	end

	local p = {
		id = id, leader = who, loot = loot,
		members = {{id = who, name = name}},
	}
	save(parties, id, p)
	redis.call('HSET', memberOf, who, id)
	return reply('', p)
`)

// partyInviteScript records an invitation, founding a party if the inviter has none.
//
// Anyone may invite; only the leader may kick. Gatekeeping growth through one
// person makes grouping tedious for no safety gain.
var partyInviteScript = redis.NewScript(partyLua + `
	local parties = KEYS[1]
	local memberOf = KEYS[2]
	local invite = KEYS[3]
	local from = ARGV[1]
	local fromName = ARGV[2]
	local to = ARGV[3]
	local newID = ARGV[4]
	local loot = ARGV[5]
	local maxSize = tonumber(ARGV[6])
	local ttl = tonumber(ARGV[7])

	if redis.call('HEXISTS', memberOf, to) == 1 then
		return reply('already', nil)
	end

	local id, p = partyOf(parties, memberOf, from)
	if p then
		if #p['members'] >= maxSize then
			return reply('full', nil)
		end
	else
		-- Founding on invite rather than requiring an explicit "make a party"
		-- step: nobody wants a party of one, and the only reason to make one
		-- is to invite somebody to it.
		id = newID
		p = {
			id = id, leader = from, loot = loot,
			members = {{id = from, name = fromName}},
		}
		save(parties, id, p)
		redis.call('HSET', memberOf, from, id)
	end

	redis.call('SET', invite, cjson.encode({party = id, from = from}), 'PX', ttl)
	return reply('', p)
`)

// partyAcceptScript turns an invitation into membership.
var partyAcceptScript = redis.NewScript(partyLua + `
	local parties = KEYS[1]
	local memberOf = KEYS[2]
	local invite = KEYS[3]
	local who = ARGV[1]
	local name = ARGV[2]
	local maxSize = tonumber(ARGV[3])

	local raw = redis.call('GET', invite)
	if not raw then
		return reply('no_invite', nil)
	end
	-- Spent whatever the outcome. An invitation that survives being refused is
	-- one the answer can be changed on later.
	redis.call('DEL', invite)

	if redis.call('HEXISTS', memberOf, who) == 1 then
		return reply('already', nil)
	end

	local ok, inv = pcall(cjson.decode, raw)
	if not ok then return reply('no_invite', nil) end

	local stored = redis.call('HGET', parties, inv['party'])
	if not stored then
		-- The party disbanded between the invitation and the answer.
		return reply('no_party', nil)
	end
	local decoded, p = pcall(cjson.decode, stored)
	if not decoded then return reply('no_party', nil) end

	if #p['members'] >= maxSize then
		return reply('full', nil)
	end

	table.insert(p['members'], {id = who, name = name})
	save(parties, inv['party'], p)
	redis.call('HSET', memberOf, who, inv['party'])
	return reply('', p)
`)

// partyLeaveScript removes a character from their own party.
var partyLeaveScript = redis.NewScript(partyLua + `
	local code, p = remove(KEYS[1], KEYS[2], ARGV[1])
	return reply(code, p)
`)

// partyKickScript removes another member. Only the leader may.
var partyKickScript = redis.NewScript(partyLua + `
	local parties = KEYS[1]
	local memberOf = KEYS[2]
	local leader = ARGV[1]
	local target = ARGV[2]

	local code, _, p = ledParty(parties, memberOf, leader)
	if code ~= '' then return reply(code, nil) end

	if target == leader then
		-- The leader kicking themselves would disband by the back door,
		-- skipping the leadership handover that Leave does.
		return reply('not_in', nil)
	end

	local found = false
	for _, m in ipairs(p['members']) do
		if m['id'] == target then found = true break end
	end
	if not found then return reply('not_in', nil) end

	local rcode, rp = remove(parties, memberOf, target)
	return reply(rcode, rp)
`)

// partyPromoteScript transfers leadership.
var partyPromoteScript = redis.NewScript(partyLua + `
	local parties = KEYS[1]
	local memberOf = KEYS[2]
	local leader = ARGV[1]
	local target = ARGV[2]

	local code, id, p = ledParty(parties, memberOf, leader)
	if code ~= '' then return reply(code, nil) end

	local found = false
	for _, m in ipairs(p['members']) do
		if m['id'] == target then found = true break end
	end
	if not found then return reply('not_in', nil) end

	p['leader'] = target
	save(parties, id, p)
	return reply('', p)
`)

// partyLootScript changes how drops are assigned.
var partyLootScript = redis.NewScript(partyLua + `
	local parties = KEYS[1]
	local memberOf = KEYS[2]
	local leader = ARGV[1]
	local rule = ARGV[2]

	local code, id, p = ledParty(parties, memberOf, leader)
	if code ~= '' then return reply(code, nil) end

	p['loot'] = rule
	save(parties, id, p)
	return reply('', p)
`)

// partyRenameScript updates a member's display name.
var partyRenameScript = redis.NewScript(partyLua + `
	local parties = KEYS[1]
	local memberOf = KEYS[2]
	local who = ARGV[1]
	local name = ARGV[2]

	local id, p = partyOf(parties, memberOf, who)
	if not p then return reply('not_in', nil) end

	for i, m in ipairs(p['members']) do
		if m['id'] == who then
			p['members'][i]['name'] = name
			save(parties, id, p)
			return reply('', p)
		end
	end
	return reply('not_in', nil)
`)

// partyOfScript reads the party a character is in.
//
// A script rather than two reads because the roster and the index have to be
// read together: between an HGET on one and an HGET on the other the party can
// disband, and the answer would be a roster for a party that no longer exists.
var partyOfScript = redis.NewScript(partyLua + `
	local id, p = partyOf(KEYS[1], KEYS[2], ARGV[1])
	if not p then return reply('not_in', nil) end
	return reply('', p)
`)

// Create starts a party with one member.
func (r *RedisParties) Create(ctx context.Context, founder Member) (Party, error) {
	return r.run(ctx, partyCreateScript,
		[]string{r.partiesKey(), r.memberOfKey()},
		uuid.NewString(), founder.CharacterID, founder.Name, LootFreeForAll,
	)
}

// Invite records an invitation, creating a party for the inviter if needed.
func (r *RedisParties) Invite(ctx context.Context, from Member, to string) (Party, error) {
	if from.CharacterID == to {
		return Party{}, fmt.Errorf("directory: cannot invite yourself")
	}
	return r.run(ctx, partyInviteScript,
		[]string{r.partiesKey(), r.memberOfKey(), r.inviteKey(to)},
		from.CharacterID, from.Name, to, uuid.NewString(), LootFreeForAll,
		r.maxSize, r.inviteTTL.Milliseconds(),
	)
}

// Accept turns an invitation into membership.
func (r *RedisParties) Accept(ctx context.Context, who Member) (Party, error) {
	return r.run(ctx, partyAcceptScript,
		[]string{r.partiesKey(), r.memberOfKey(), r.inviteKey(who.CharacterID)},
		who.CharacterID, who.Name, r.maxSize,
	)
}

// Decline discards an invitation.
func (r *RedisParties) Decline(ctx context.Context, characterID string) error {
	n, err := r.client.Del(ctx, r.inviteKey(characterID)).Result()
	if err != nil {
		return fmt.Errorf("directory: declining an invitation: %w", err)
	}
	if n == 0 {
		return ErrNoInvite
	}
	return nil
}

// Leave removes a character from their party.
func (r *RedisParties) Leave(ctx context.Context, characterID string) (Party, error) {
	return r.run(ctx, partyLeaveScript,
		[]string{r.partiesKey(), r.memberOfKey()}, characterID)
}

// Kick removes another member.
func (r *RedisParties) Kick(ctx context.Context, leader, target string) (Party, error) {
	return r.run(ctx, partyKickScript,
		[]string{r.partiesKey(), r.memberOfKey()}, leader, target)
}

// Promote transfers leadership.
func (r *RedisParties) Promote(ctx context.Context, leader, target string) (Party, error) {
	return r.run(ctx, partyPromoteScript,
		[]string{r.partiesKey(), r.memberOfKey()}, leader, target)
}

// SetLoot changes how drops are assigned.
func (r *RedisParties) SetLoot(ctx context.Context, leader, rule string) (Party, error) {
	if !ValidLootRule(rule) {
		return Party{}, fmt.Errorf("directory: unknown loot rule %q", rule)
	}
	return r.run(ctx, partyLootScript,
		[]string{r.partiesKey(), r.memberOfKey()}, leader, rule)
}

// Rename updates a member's display name.
func (r *RedisParties) Rename(ctx context.Context, characterID, name string) error {
	_, err := r.run(ctx, partyRenameScript,
		[]string{r.partiesKey(), r.memberOfKey()}, characterID, name)
	return err
}

// Of returns the party a character is in.
func (r *RedisParties) Of(ctx context.Context, characterID string) (Party, bool, error) {
	party, err := r.run(ctx, partyOfScript,
		[]string{r.partiesKey(), r.memberOfKey()}, characterID)
	if errors.Is(err, ErrNotInParty) {
		return Party{}, false, nil
	}
	if err != nil {
		return Party{}, false, err
	}
	return party, true, nil
}

// Close releases nothing: the client belongs to whoever opened it.
func (r *RedisParties) Close() error { return nil }

// run executes a script and turns its reply into a party or an error.
func (r *RedisParties) run(ctx context.Context, script *redis.Script, keys []string, args ...any) (Party, error) {
	res, err := script.Run(ctx, r.client, keys, args...).Slice()
	if err != nil {
		return Party{}, fmt.Errorf("directory: party operation: %w", err)
	}
	if len(res) != 2 {
		return Party{}, errors.New("directory: unexpected party response")
	}

	code, _ := res[0].(string)
	encoded, _ := res[1].(string)

	if err := partyError(code); err != nil {
		return Party{}, err
	}
	if encoded == "" {
		return Party{}, nil
	}

	var stored storedParty
	if err := json.Unmarshal([]byte(encoded), &stored); err != nil {
		return Party{}, fmt.Errorf("directory: decoding a party: %w", err)
	}

	party := Party{ID: stored.ID, Leader: stored.Leader, Loot: stored.Loot}
	for _, m := range stored.Members {
		party.Members = append(party.Members,
			Member{CharacterID: m.CharacterID, Name: m.Name})
	}
	return party, nil
}

// partyError maps a script's code onto the sentinel a caller compares against.
//
// Codes rather than messages because the caller distinguishes these by type:
// "the party is full" is something to tell the inviter, "you are not the
// leader" is a refusal, and Redis being unreachable is neither.
func partyError(code string) error {
	switch code {
	case "":
		return nil
	case "no_party":
		return ErrNoParty
	case "full":
		return ErrPartyFull
	case "already":
		return ErrAlreadyInParty
	case "not_in":
		return ErrNotInParty
	case "not_leader":
		return ErrNotLeader
	case "no_invite":
		return ErrNoInvite
	default:
		return fmt.Errorf("directory: unknown party result %q", code)
	}
}

var _ Parties = (*RedisParties)(nil)
