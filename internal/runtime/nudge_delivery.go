package runtime

import (
	"context"
	"errors"
)

// ErrNudgeRejected reports that the runtime definitively refused a nudge: it
// answered, and the answer was no, so nothing was delivered. Unlike a
// timeout or a dropped connection, the outcome is known, so a caller that
// redelivers must do so under a new delivery key; under the old one a
// deduplicating runtime would replay the refusal. Providers wrap their own
// refusals with this sentinel so callers can dispatch with errors.Is.
var ErrNudgeRejected = errors.New("runtime rejected the nudge")

// KeyedNudgeProvider is an optional extension for runtimes that deduplicate
// deliveries. NudgeKeyed delivers content like [Provider.Nudge], deriving the
// runtime's own idempotency ids from key, so a redelivery under the same key
// (after a timeout, a dropped connection or a crashed caller) replays the
// first delivery's outcome instead of delivering the content twice.
type KeyedNudgeProvider interface {
	NudgeKeyed(name string, content []ContentBlock, key string) error
}

type nudgeDeliveryKeyContextKey struct{}

// WithNudgeDeliveryKey returns ctx carrying key, the caller's stable identity
// for one delivery. A caller that redelivers the same content passes the same
// key; [NudgeWithDeliveryKey] hands it to a [KeyedNudgeProvider].
//
// The returned context must carry exactly one delivery: a deduplicating
// runtime drops a second, different nudge sent under a context derived from
// it as a replay of the first. Callers outside the worker boundary set
// worker.NudgeRequest.DeliveryKey instead, which scopes the key to one
// request.
func WithNudgeDeliveryKey(ctx context.Context, key string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, nudgeDeliveryKeyContextKey{}, key)
}

// NudgeDeliveryKey returns the delivery key ctx carries, or "" when it carries
// none.
func NudgeDeliveryKey(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	key, _ := ctx.Value(nudgeDeliveryKeyContextKey{}).(string)
	return key
}

// NudgeWithDeliveryKey delivers content to name through p. When ctx carries a
// delivery key and p is a [KeyedNudgeProvider], the key goes with it;
// otherwise this is p.Nudge.
func NudgeWithDeliveryKey(ctx context.Context, p Provider, name string, content []ContentBlock) error {
	if key := NudgeDeliveryKey(ctx); key != "" {
		if kp, ok := p.(KeyedNudgeProvider); ok {
			return kp.NudgeKeyed(name, content, key)
		}
	}
	return p.Nudge(name, content)
}
