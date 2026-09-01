package bus

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"

	mmov1 "github.com/ctrl-research/mmo/internal/wire/mmo/v1"
	"google.golang.org/protobuf/proto"
)

// Request and reply over publish/subscribe.
//
// The bus has no notion of a response, so one is built here: the requester
// subscribes to a private inbox subject, names it in the envelope, and the
// responder publishes the answer there.
//
// A correlation id is carried even though the inbox is private, because a
// reply that arrives after its request timed out would otherwise be mistaken
// for the answer to the next request on the same inbox.

// inboxPrefix namespaces reply subjects so they cannot collide with game
// subjects.
const inboxPrefix = "_inbox."

// newInbox returns a unique reply subject.
func newInbox() (string, error) {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("bus: generating inbox: %w", err)
	}
	return inboxPrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

// Request publishes a message and waits for one reply.
func (b *InProc) Request(ctx context.Context, subject string, msg proto.Message, reply proto.Message) error {
	if !validSubject(subject) {
		return &SubjectError{Subject: subject}
	}

	payload, err := proto.Marshal(msg)
	if err != nil {
		return err
	}

	inbox, err := newInbox()
	if err != nil {
		return err
	}
	correlation, err := newInbox()
	if err != nil {
		return err
	}

	type result struct {
		payload []byte
		err     error
	}
	answers := make(chan result, 1)

	// Subscribed before publishing, or a fast responder could answer before
	// anyone is listening and the reply would be dropped.
	sub, err := b.Subscribe(ctx, inbox, func(_ context.Context, _ string, data []byte) {
		var env mmov1.BusEnvelope
		if err := proto.Unmarshal(data, &env); err != nil {
			return
		}
		if env.GetCorrelationId() != correlation {
			return
		}
		if env.GetError() != "" {
			select {
			case answers <- result{err: errors.New(env.GetError())}:
			default:
			}
			return
		}
		select {
		case answers <- result{payload: env.GetPayload()}:
		default:
		}
	})
	if err != nil {
		return err
	}
	defer sub.Close()

	envelope := &mmov1.BusEnvelope{
		ReplyTo:       inbox,
		CorrelationId: correlation,
		Payload:       payload,
	}

	delivered, err := b.publishCounted(ctx, subject, envelope)
	if err != nil {
		return err
	}
	if delivered == 0 {
		// Nothing was listening. Reporting that immediately beats making the
		// caller wait out a timeout for an answer that was never coming.
		return fmt.Errorf("%w: %s", ErrNoResponder, subject)
	}

	select {
	case res := <-answers:
		if res.err != nil {
			return res.err
		}
		if reply == nil {
			return nil
		}
		return proto.Unmarshal(res.payload, reply)

	case <-ctx.Done():
		return fmt.Errorf("%w: %s", ErrRequestTimeout, subject)
	}
}

// Respond registers a handler that answers requests.
func (b *InProc) Respond(ctx context.Context, pattern string, fn Responder) (Subscription, error) {
	return b.Subscribe(ctx, pattern, func(msgCtx context.Context, subject string, data []byte) {
		var env mmov1.BusEnvelope
		if err := proto.Unmarshal(data, &env); err != nil {
			return
		}
		if env.GetReplyTo() == "" {
			// A plain publish rather than a request. Handled, but with nowhere
			// to send an answer.
			fn(msgCtx, subject, env.GetPayload())
			return
		}

		reply, err := fn(msgCtx, subject, env.GetPayload())

		// The framing is shared with the NATS implementation, because it is
		// part of the contract rather than a transport detail: a caller must
		// get the same error back whichever bus carried it. The correlation id
		// is the part only this implementation needs, because only this one
		// has to route the reply itself.
		out := replyEnvelope(reply, err)
		out.CorrelationId = env.GetCorrelationId()

		// A background context: the message's context may already be done, and
		// the requester is still waiting for an answer either way.
		b.Publish(context.Background(), env.GetReplyTo(), out)
	})
}

// publishCounted publishes and reports how many subscribers matched.
//
// The count is what lets Request distinguish "nobody is listening" from "the
// listener is slow", which are different problems with different fixes.
func (b *InProc) publishCounted(ctx context.Context, subject string, msg proto.Message) (int, error) {
	payload, err := proto.Marshal(msg)
	if err != nil {
		return 0, err
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.closed {
		return 0, ErrClosed
	}

	delivered := 0
	for _, sub := range b.subs {
		if !matchSubject(sub.pattern, subject) {
			continue
		}
		delivered++
		select {
		case sub.queue <- busMsg{ctx: ctx, subject: subject, payload: payload}:
		default:
			b.dropped.add(1)
		}
	}
	return delivered, nil
}

// Notify sends a one-way message to a subject served by Respond.
//
// Respond reads every message as a BusEnvelope, because that is how a request
// carries its reply subject. A plain Publish of the payload itself is therefore
// not dropped with an error but *reinterpreted*: proto decodes the command as
// an envelope, finds nothing in the payload field, and hands the responder an
// empty message. The send succeeds, the handler runs, and nothing happens.
//
// So a fire-and-forget call to a responder has to be wrapped the same way a
// request is, minus the reply subject. This is that, named so the next caller
// does not have to rediscover it.
func Notify(ctx context.Context, b Bus, subject string, msg proto.Message) error {
	payload, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	return b.Publish(ctx, subject, &mmov1.BusEnvelope{Payload: payload})
}
