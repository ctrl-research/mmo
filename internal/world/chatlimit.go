package world

import (
	"context"
	"sync"
	"time"

	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
)

// Rate limits and mutes.
//
// Both answer "may this player say this", and both are held per session
// because that is where the state belongs: a bucket shared between players
// would let one flood everyone else into silence, and a mute cached globally
// would need invalidating from wherever a moderator happened to be.

// chatBucket is a token bucket for one channel.
//
// Buckets start full, so somebody who logs in and immediately greets their
// party is not throttled for it. The channels have separate buckets because
// they cost different amounts to send: a global line reaches everyone online.
type chatBucket struct {
	perSecond float64
	tokens    float64
	last      time.Time
}

func (b *chatBucket) allow(now time.Time) bool {
	elapsed := now.Sub(b.last).Seconds()
	b.last = now

	b.tokens = min(b.tokens+elapsed*b.perSecond, b.burst())
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// burst is the bucket's ceiling: a minute's allowance, or one message,
// whichever is larger. A channel allowed six a minute should still permit a
// short burst rather than metering one message every ten seconds.
func (b *chatBucket) burst() float64 {
	return max(b.perSecond*60, 1)
}

// chatAllowed consumes one token for a channel.
func (s *Session) chatAllowed(channel mmov1.ChatChannel) bool {
	s.chatMu.Lock()
	defer s.chatMu.Unlock()

	if s.buckets == nil {
		// Built on first use rather than at construction: a session that never
		// says anything never needs one, and a map that has to be initialised
		// somewhere else is a nil dereference waiting for a new call site.
		s.buckets = make(map[string]*chatBucket)
	}

	name := channelName(channel)
	bucket, ok := s.buckets[name]
	if !ok {
		perMinute := s.node.content.Balance.Chat.ChatLimit(name)
		bucket = &chatBucket{
			perSecond: float64(perMinute) / 60,
			tokens:    float64(perMinute),
			last:      time.Now(),
		}
		s.buckets[name] = bucket
	}
	return bucket.allow(time.Now())
}

// channelName is the content key for a channel's limit.
func channelName(channel mmov1.ChatChannel) string {
	switch channel {
	case mmov1.ChatChannel_CHAT_CHANNEL_GLOBAL:
		return "global"
	case mmov1.ChatChannel_CHAT_CHANNEL_WHISPER:
		return "whisper"
	case mmov1.ChatChannel_CHAT_CHANNEL_PARTY:
		return "party"
	case mmov1.ChatChannel_CHAT_CHANNEL_GUILD:
		return "guild"
	default:
		return "local"
	}
}

// chatMute is a mute as the session needs it.
type chatMute struct {
	until  time.Time
	reason string
}

// muteCacheTTL is how long a mute check is trusted.
//
// A mute applied while somebody is online should take effect quickly, and a
// database read per chat message is a database read per chat message. Ten
// seconds is short enough that a moderator sees it work and long enough that a
// busy channel is not a query per line.
const muteCacheTTL = 10 * time.Second

// mute reports whether this character is muted, from a short-lived cache.
func (s *Session) mute(ctx context.Context) (chatMute, bool, error) {
	s.chatMu.Lock()
	if time.Now().Before(s.muteCheckedUntil) {
		m, muted := s.mutedAs, s.muted
		s.chatMu.Unlock()
		return m, muted, nil
	}
	s.chatMu.Unlock()

	record, muted, err := s.node.store.ActiveMute(ctx, s.characterID)
	if err != nil {
		return chatMute{}, false, err
	}

	var m chatMute
	if muted {
		m.reason = record.Reason
		if record.ExpiresAt != nil {
			m.until = *record.ExpiresAt
		}
	}

	s.chatMu.Lock()
	s.mutedAs, s.muted = m, muted
	s.muteCheckedUntil = time.Now().Add(muteCacheTTL)
	s.chatMu.Unlock()

	return m, muted, nil
}

// chatState is the per-session chat bookkeeping, kept together so the lock
// covering it is obviously the one that covers all of it.
type chatState struct {
	chatMu  sync.Mutex
	buckets map[string]*chatBucket

	muted            bool
	mutedAs          chatMute
	muteCheckedUntil time.Time
}
