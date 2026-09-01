package world

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/ctrl-research/mmo/internal/bus"
	"github.com/ctrl-research/mmo/internal/directory"
	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"github.com/ctrl-research/mmo/internal/world/room"
	"github.com/ctrl-research/mmo/internal/world/sim"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

// A gateway with no world node of its own.
//
// This is what makes the gateway role independently deployable: it terminates
// sockets, checks what arrives, and forwards it to whichever world node holds
// the character. It never holds a room, a lease, or any part of the simulation.
//
// The asymmetry from the room protocol is here too, for the same reason. The
// player's four constant calls -- move, cast, interact, craft -- are published
// and not waited for, because a round trip per keypress would put the network
// between a key and the simulation. Everything else is a request that returns
// either nothing or a refusal worth showing the player.

// sessionCommandTimeout bounds a call to the world node holding a character.
const sessionCommandTimeout = 5 * time.Second

// enterTimeout bounds taking a character into the world.
//
// Longer than an ordinary command: it covers a lease, a database read and a
// room being started, any of which may be the first thing to touch a cold
// node.
const enterTimeout = 15 * time.Second

// RemoteWorld places characters on world nodes over the bus.
type RemoteWorld struct {
	bus bus.Bus
	dir directory.Directory
	log *slog.Logger

	// lifetime bounds the subscriptions this gateway opens for its players.
	//
	// The process, not the request. Enter is called with the handshake's
	// context, which is cancelled the moment the handshake finishes -- binding
	// a player's subscription to it means the world node talks to a
	// subscription that closed before the player finished loading, and the
	// symptom is somebody standing in a world that never sends them anything.
	lifetime context.Context

	// gatewayID names this gateway, so a world node knows where to send the
	// messages bound for its players' screens.
	gatewayID string

	mu   sync.Mutex
	held map[*remoteSession]struct{}
}

// nodeWatchInterval is how often a gateway checks that the nodes holding its
// players are still there.
//
// One call to the directory per gateway, not per player: a thousand sessions
// asking the same question is a thousand round trips for one answer.
const nodeWatchInterval = 5 * time.Second

// nodeMissesBeforeGiveUp is how many consecutive checks a node has to be absent
// for before its players are disconnected.
//
// Node liveness is already a TTL three heartbeats deep, so a node missing from
// one poll has been quiet for a while. Requiring two makes a stall in this
// gateway -- a long GC pause, a slow Redis call -- not enough on its own to
// throw everybody off a node that is fine.
const nodeMissesBeforeGiveUp = 2

// NewRemoteWorld returns a World that runs no simulation of its own.
//
// The context bounds every subscription it opens, so shutting the gateway down
// takes its players' subscriptions with it.
func NewRemoteWorld(ctx context.Context, b bus.Bus, dir directory.Directory, gatewayID string, log *slog.Logger) *RemoteWorld {
	w := &RemoteWorld{
		bus: b, dir: dir, log: log, lifetime: ctx,
		gatewayID: gatewayID,
		held:      make(map[*remoteSession]struct{}),
	}
	go w.watchNodes()
	return w
}

// watchNodes disconnects players whose world node has stopped existing.
//
// Nothing else notices. The calls a connected player makes constantly are
// published and not waited for -- that is the whole point of them -- so a
// gateway whose world node has died keeps accepting input and forwarding it to
// a subject nobody is subscribed to. The player sits in a world that has
// stopped moving, with an open connection and no error, indefinitely.
//
// A chaos run showed exactly that: killing one of three world nodes took the
// snapshot rate from twenty per player per second to nine, where it stayed for
// the rest of the run, while the gateway reported every player as connected and
// healthy. The players on the dead node were never told, so they never
// reconnected, so they never recovered.
//
// The directory already knows which nodes are alive -- it is what placement
// uses to avoid starting rooms on a node that has gone. This asks it.
func (w *RemoteWorld) watchNodes() {
	ticker := time.NewTicker(nodeWatchInterval)
	defer ticker.Stop()

	misses := make(map[directory.NodeID]int)

	for {
		select {
		case <-w.lifetime.Done():
			return
		case <-ticker.C:
		}

		nodes, err := w.dir.LiveNodes(w.lifetime)
		if err != nil {
			// Not evidence about any node. Acting on a failed lookup would
			// disconnect everybody the moment the directory hiccups.
			w.log.Warn("checking which world nodes are alive", "err", err)
			continue
		}

		live := make(map[directory.NodeID]bool, len(nodes))
		for _, n := range nodes {
			live[n] = true
			delete(misses, n)
		}

		for _, s := range w.sessionsOnDeadNodes(live) {
			misses[s.node]++
			if misses[s.node] < nodeMissesBeforeGiveUp {
				continue
			}
			w.log.Warn("the node holding a player has gone; closing the connection so they can come back",
				"node", s.node, "character", s.characterID)
			s.nodeDied()
		}
	}
}

// sessionsOnDeadNodes returns the sessions whose node is not in the live set.
func (w *RemoteWorld) sessionsOnDeadNodes(live map[directory.NodeID]bool) []*remoteSession {
	w.mu.Lock()
	defer w.mu.Unlock()

	var out []*remoteSession
	for s := range w.held {
		if !live[s.node] {
			out = append(out, s)
		}
	}
	return out
}

func (w *RemoteWorld) hold(s *remoteSession) {
	w.mu.Lock()
	w.held[s] = struct{}{}
	w.mu.Unlock()
}

func (w *RemoteWorld) release(s *remoteSession) {
	w.mu.Lock()
	delete(w.held, s)
	w.mu.Unlock()
}

// gatewayCallbackSubject is where one character's messages are delivered.
//
// Per character rather than per gateway, so a gateway subscribes only to the
// players it is actually holding sockets for.
func gatewayCallbackSubject(gatewayID, characterID string) string {
	return "gateway." + sanitiseSubject(gatewayID) + ".session." + sanitiseSubject(characterID)
}

// Enter asks a world node to take a character and returns a handle to it.
//
// The node is chosen from the ones the directory says are alive, at random.
// Random rather than least-loaded because the directory's load counter counts
// rooms, not sessions, and a gateway picking "the emptiest node" by that
// measure would send every login to whichever node happens to host the fewest
// rooms -- which is not the same question.
func (w *RemoteWorld) Enter(ctx context.Context, accountID, characterID uuid.UUID, sink room.Sink) (PlayerSession, error) {
	nodes, err := w.dir.LiveNodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("world: finding a world node: %w", err)
	}
	if len(nodes) == 0 {
		return nil, errors.New("world: no world node is running")
	}
	rand.Shuffle(len(nodes), func(i, j int) { nodes[i], nodes[j] = nodes[j], nodes[i] })

	subject := gatewayCallbackSubject(w.gatewayID, characterID.String())

	// Subscribed before the character is taken, and for the lifetime of the
	// gateway rather than of this call. A node begins sending as soon as it has
	// placed them, so a subscription taken afterwards would miss the Welcome --
	// and one bound to this context would be closed by the handshake finishing.
	session := &remoteSession{
		bus: w.bus, log: w.log,
		characterID: characterID,
		sink:        sink,
		callback:    subject,
	}
	if err := session.watch(w.lifetime); err != nil {
		return nil, err
	}

	var lastErr error
	for _, node := range nodes {
		reply, err := w.enterOn(ctx, node, accountID, characterID, subject)
		if err != nil {
			// This node is unreachable; another may not be. A gateway that
			// gave up on the first timeout would fail every login during a
			// rolling deploy.
			lastErr = err
			w.log.Warn("a world node did not answer", "node", node, "err", err)
			continue
		}
		if reply.GetBusy() {
			// Not worth trying elsewhere: the lease is held cluster-wide, so
			// every other node would say the same thing.
			session.stop()
			return nil, fmt.Errorf("%w: %s", ErrCharacterBusy, reply.GetError())
		}
		if e := reply.GetError(); e != "" {
			session.stop()
			return nil, errors.New(e)
		}

		session.node = node
		session.name = reply.GetName()
		session.entityID = room.EntityID(reply.GetEntityId())
		session.world = w

		// Registered only once it is actually on a node, so the watcher never
		// sees a session with no node to check.
		w.hold(session)
		return session, nil
	}

	session.stop()
	if lastErr != nil {
		return nil, fmt.Errorf("world: no world node accepted the character: %w", lastErr)
	}
	return nil, errors.New("world: no world node accepted the character")
}

func (w *RemoteWorld) enterOn(ctx context.Context, node directory.NodeID,
	accountID, characterID uuid.UUID, subject string) (*mmov1.EnterReply, error) {

	ctx, cancel := context.WithTimeout(ctx, enterTimeout)
	defer cancel()

	reply := &mmov1.EnterReply{}
	err := w.bus.Request(ctx, enterSubject(string(node)), &mmov1.EnterRequest{
		AccountId:       accountID.String(),
		CharacterId:     characterID.String(),
		CallbackSubject: subject,
	}, reply)
	if err != nil {
		return nil, err
	}
	return reply, nil
}

// remoteSession is a character being played through another process.
type remoteSession struct {
	bus  bus.Bus
	log  *slog.Logger
	node directory.NodeID

	characterID uuid.UUID
	name        string
	entityID    room.EntityID

	callback string

	// world is the gateway this session belongs to, so it can take itself out
	// of the watch list when it ends.
	world *RemoteWorld

	mu     sync.Mutex
	sub    bus.Subscription
	sink   room.Sink
	onLost func(reason string)

	// gone latches once the world node says it is no longer holding this
	// character. Further commands are pointless and the connection is closing.
	gone bool
}

// watch subscribes to everything the world node sends for this character.
func (s *remoteSession) watch(ctx context.Context) error {
	sub, err := s.bus.Subscribe(ctx, s.callback,
		func(_ context.Context, _ string, payload []byte) {
			var cb mmov1.SessionCallback
			if err := proto.Unmarshal(payload, &cb); err != nil {
				s.log.Error("decoding a message from a world node", "err", err)
				return
			}
			s.apply(&cb)
		})
	if err != nil {
		return fmt.Errorf("world: subscribing to a character's messages: %w", err)
	}

	s.mu.Lock()
	s.sub = sub
	s.mu.Unlock()
	return nil
}

func (s *remoteSession) apply(cb *mmov1.SessionCallback) {
	s.mu.Lock()
	sink, onLost := s.sink, s.onLost
	s.mu.Unlock()

	switch body := cb.GetBody().(type) {
	case *mmov1.SessionCallback_Send:
		var msg mmov1.ServerMessage
		if err := proto.Unmarshal(body.Send, &msg); err != nil {
			s.log.Error("decoding a server message from a world node", "err", err)
			return
		}
		if sink != nil {
			sink.Send(&msg)
		}

	case *mmov1.SessionCallback_Close:
		if sink != nil {
			sink.Close(body.Close.GetCode(), body.Close.GetReason())
		}

	case *mmov1.SessionCallback_OwnershipLost:
		if onLost != nil {
			onLost(body.OwnershipLost)
		}
	}
}

// nodeDied ends a session whose world node has gone.
//
// The same path as losing the lease, because it is the same situation from the
// player's side: this connection is no longer attached to anything, and the way
// back is a new one. Their character is still safe -- whatever the last
// checkpoint wrote is what they come back to -- and its lease will lapse on its
// own, which is what lets them come back at all.
func (s *remoteSession) nodeDied() {
	s.mu.Lock()
	if s.gone {
		s.mu.Unlock()
		return
	}
	s.gone = true
	onLost := s.onLost
	s.mu.Unlock()

	if onLost != nil {
		onLost("the server holding your character stopped responding")
	}
	s.stop()
}

func (s *remoteSession) stop() {
	if s.world != nil {
		s.world.release(s)
	}

	s.mu.Lock()
	sub := s.sub
	s.sub = nil
	s.mu.Unlock()

	if sub != nil {
		sub.Close()
	}
}

// send publishes a command without waiting for it to be applied.
func (s *remoteSession) send(ctx context.Context, body isSessionBody) {
	cmd := &mmov1.SessionCommand{CharacterId: s.characterID.String()}
	body.set(cmd)

	// Notify rather than Publish: the subject is served by Respond, which reads
	// every message as an envelope. See bus.Notify.
	if err := bus.Notify(ctx, s.bus, sessionSubject(string(s.node)), cmd); err != nil {
		s.log.Error("sending a session command", "node", s.node, "err", err)
	}
}

// call sends a command and waits for its answer.
func (s *remoteSession) call(ctx context.Context, body isSessionBody) (*mmov1.SessionReply, error) {
	cmd := &mmov1.SessionCommand{CharacterId: s.characterID.String()}
	body.set(cmd)

	ctx, cancel := context.WithTimeout(ctx, sessionCommandTimeout)
	defer cancel()

	reply := &mmov1.SessionReply{}
	if err := s.bus.Request(ctx, sessionSubject(string(s.node)), cmd, reply); err != nil {
		return nil, fmt.Errorf("world: reaching the node holding this character: %w", err)
	}
	if reply.GetGone() {
		s.mu.Lock()
		s.gone = true
		s.mu.Unlock()
		return reply, ErrCharacterGone
	}
	if e := reply.GetError(); e != "" {
		// A refusal the player is meant to read, not a fault. Returned as a
		// plain error so the gateway shows it the same way it shows a local
		// session's refusal.
		return reply, errors.New(e)
	}
	return reply, nil
}

// ErrCharacterGone means the world node is no longer holding the character.
var ErrCharacterGone = errors.New("world: the character is no longer in play here")

// isSessionBody sets one command's body. A tiny interface rather than a switch
// so that adding a command is one type and the compiler finds the call sites.
type isSessionBody interface{ set(*mmov1.SessionCommand) }

type bodyFunc func(*mmov1.SessionCommand)

func (f bodyFunc) set(c *mmov1.SessionCommand) { f(c) }

func (s *remoteSession) Input(ctx context.Context, seq uint32, in sim.Input) {
	s.send(ctx, bodyFunc(func(c *mmov1.SessionCommand) {
		c.Body = &mmov1.SessionCommand_Input{Input: &mmov1.SessionInput{
			Seq: seq, MoveX: in.MoveX, Jump: in.Jump, Up: in.Up, Down: in.Down,
		}}
	}))
}

func (s *remoteSession) Cast(ctx context.Context, skillID string, facingLeft bool) {
	s.send(ctx, bodyFunc(func(c *mmov1.SessionCommand) {
		c.Body = &mmov1.SessionCommand_Cast{Cast: &mmov1.SessionCast{
			SkillId: skillID, FacingLeft: facingLeft,
		}}
	}))
}

func (s *remoteSession) Interact(ctx context.Context, target room.EntityID, kind room.InteractKind) {
	s.send(ctx, bodyFunc(func(c *mmov1.SessionCommand) {
		c.Body = &mmov1.SessionCommand_Interact{Interact: &mmov1.SessionInteract{
			Target: uint32(target), Kind: uint32(kind),
		}}
	}))
}

func (s *remoteSession) Craft(ctx context.Context, station room.EntityID, recipe string) {
	s.send(ctx, bodyFunc(func(c *mmov1.SessionCommand) {
		c.Body = &mmov1.SessionCommand_Craft{Craft: &mmov1.SessionCraft{
			Station: uint32(station), RecipeId: recipe,
		}}
	}))
}

func (s *remoteSession) ApplyItemAction(ctx context.Context, action ItemAction) error {
	_, err := s.call(ctx, bodyFunc(func(c *mmov1.SessionCommand) {
		c.Body = &mmov1.SessionCommand_ItemAction{ItemAction: &mmov1.SessionItemAction{
			Kind: uint32(action.Kind), ItemId: action.ItemID.String(),
			Slot: int32(action.Slot), EquipSlot: string(action.EquipSlot),
		}}
	}))
	return err
}

func (s *remoteSession) SendChat(ctx context.Context, msg *mmov1.ChatSend) error {
	_, err := s.call(ctx, bodyFunc(func(c *mmov1.SessionCommand) {
		c.Body = &mmov1.SessionCommand_Chat{Chat: &mmov1.SessionChat{
			Channel: uint32(msg.GetChannel()), Body: msg.GetBody(), Target: msg.GetTarget(),
		}}
	}))
	return err
}

func (s *remoteSession) Party(ctx context.Context, req PartyRequest) error {
	_, err := s.call(ctx, bodyFunc(func(c *mmov1.SessionCommand) {
		c.Body = &mmov1.SessionCommand_Party{Party: &mmov1.SessionParty{
			Kind: uint32(req.Kind), Target: req.Target,
		}}
	}))
	return err
}

func (s *remoteSession) Guild(ctx context.Context, req GuildRequest) error {
	_, err := s.call(ctx, bodyFunc(func(c *mmov1.SessionCommand) {
		c.Body = &mmov1.SessionCommand_Guild{Guild: &mmov1.SessionGuild{
			Kind: uint32(req.Kind), Target: req.Target,
		}}
	}))
	return err
}

func (s *remoteSession) Social(ctx context.Context, req SocialRequest) error {
	_, err := s.call(ctx, bodyFunc(func(c *mmov1.SessionCommand) {
		c.Body = &mmov1.SessionCommand_Social{Social: &mmov1.SessionSocial{
			Kind: uint32(req.Kind), Target: req.Target,
		}}
	}))
	return err
}

func (s *remoteSession) SetBarSlot(ctx context.Context, req LoadoutRequest) error {
	_, err := s.call(ctx, bodyFunc(func(c *mmov1.SessionCommand) {
		c.Body = &mmov1.SessionCommand_Loadout{Loadout: &mmov1.SessionLoadout{
			Slot: int32(req.Slot), SkillId: req.SkillID, Supports: req.Supports,
		}}
	}))
	return err
}

func (s *remoteSession) Passive(ctx context.Context, req PassiveRequest) error {
	_, err := s.call(ctx, bodyFunc(func(c *mmov1.SessionCommand) {
		c.Body = &mmov1.SessionCommand_Passive{Passive: &mmov1.SessionPassive{
			Allocate: int32(req.Allocate), Refund: int32(req.Refund),
			RespecAll: req.RespecAll,
		}}
	}))
	return err
}

func (s *remoteSession) Travel(ctx context.Context, req TravelRequest) error {
	_, err := s.call(ctx, bodyFunc(func(c *mmov1.SessionCommand) {
		c.Body = &mmov1.SessionCommand_Travel{Travel: &mmov1.SessionTravel{
			WaypointId: req.WaypointID, Channel: uint64(req.Channel),
			NewChannel: req.NewChannel,
		}}
	}))
	return err
}

func (s *remoteSession) WorldMap(ctx context.Context) *mmov1.WorldMap {
	reply, err := s.call(ctx, bodyFunc(func(c *mmov1.SessionCommand) {
		c.Body = &mmov1.SessionCommand_WorldMap{WorldMap: &mmov1.SessionWorldMap{}}
	}))
	if err != nil {
		s.log.Error("reading the world map", "err", err)
		return nil
	}

	var m mmov1.WorldMap
	if err := proto.Unmarshal(reply.GetWorldMap(), &m); err != nil {
		s.log.Error("decoding the world map", "err", err)
		return nil
	}
	return &m
}

// InRoom is answered locally, never over the bus.
//
// The gateway asks this about every message a client sends, so a round trip
// here would put a request and a reply in front of every keypress -- the exact
// cost the fire-and-forget commands exist to avoid.
//
// Answering from what this side already knows is also faithful. A character is
// placed by the time Enter returns and stays placed for as long as the session
// lives: a transfer that succeeds puts them in another room and one that fails
// leaves them where they were. What ends it is the session ending, which
// arrives here as a Gone reply or a Close.
func (s *remoteSession) InRoom() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.gone
}

func (s *remoteSession) Close(ctx context.Context) {
	s.call(ctx, bodyFunc(func(c *mmov1.SessionCommand) {
		c.Body = &mmov1.SessionCommand_Close{Close: &mmov1.SessionClose{}}
	}))

	s.mu.Lock()
	s.gone = true
	s.mu.Unlock()

	s.stop()
}

func (s *remoteSession) Disconnect(ctx context.Context) {
	s.call(ctx, bodyFunc(func(c *mmov1.SessionCommand) {
		c.Body = &mmov1.SessionCommand_Disconnect{Disconnect: &mmov1.SessionDisconnect{}}
	}))
	// The subscription stays: a disconnect holds the character in the world for
	// a grace period, and a reconnect to this gateway picks the same subject
	// back up.
}

func (s *remoteSession) OnOwnershipLost(fn func(reason string)) {
	s.mu.Lock()
	s.onLost = fn
	s.mu.Unlock()
}

func (s *remoteSession) Name() string { return s.name }

// EntityID is the entity the character was given when it entered.
//
// Only the gateway's connection log uses it, and only once. It is not kept up
// to date across a transfer: the gateway has nothing to address with it, since
// everything it sends goes to the character rather than to a body.
func (s *remoteSession) EntityID() room.EntityID { return s.entityID }

// Handle and Where are not available to a gateway in another process.
//
// A room handle is an in-process reference. Returning nil rather than a proxy
// is deliberate: every caller in the gateway has been moved onto the session
// methods above, and a proxy here would be an invitation to add another.
func (s *remoteSession) Handle() room.Handle { return nil }

func (s *remoteSession) Where() (room.Handle, room.EntityID) { return nil, s.entityID }

var _ PlayerSession = (*remoteSession)(nil)
