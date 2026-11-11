package runtime

import (
	"context"
	"errors"
	"time"
)

// ErrNudgeRejected reports that the runtime definitively refused a nudge: it
// answered, and the answer was no, so nothing was delivered. Unlike a
// timeout or a dropped connection, the outcome is known, so a caller that
// redelivers must do so under a new delivery key; under the old one a
// deduplicating runtime would replay the refusal. Providers wrap their own
// refusals with this sentinel so callers can dispatch with errors.Is.
var ErrNudgeRejected = errors.New("runtime rejected the nudge")

// DeliveryReceipt is what a runtime that deduplicates keyed deliveries
// ([KeyedNudgeProvider]) reports of one delivery. It names the runtime's own
// records only: never a delivery key, an op id or delivered text. The zero
// value reports nothing.
type DeliveryReceipt struct {
	// MessageID is the runtime's id for the delivered message.
	MessageID string
	// MessageAt is the creation time the runtime stamped on that message, at
	// the runtime's precision: for a delivery made now, the stamp this send
	// carried; for one LookupDelivery found, the stamp the runtime stored. A
	// send the runtime answered by replaying an earlier send's receipt may
	// report this send's stamp instead, so a caller that may be redelivering
	// looks the delivery up first.
	MessageAt time.Time
	// TurnID is the turn the delivery started, when the runtime names it;
	// empty when it does not (T3 V1).
	TurnID string
}

// IsZero reports a receipt that reports nothing.
func (r DeliveryReceipt) IsZero() bool {
	return r.MessageID == "" && r.MessageAt.IsZero() && r.TurnID == ""
}

// ErrDeliveryNotFound reports that a runtime holds no delivery made under a
// key: no send under it reached the runtime.
var ErrDeliveryNotFound = errors.New("runtime holds no delivery under the key")

// KeyedNudgeProvider is an optional extension for runtimes that deduplicate
// deliveries. NudgeKeyed delivers content like [Provider.Nudge], deriving the
// runtime's own idempotency ids from key, so a redelivery under the same key
// (after a timeout, a dropped connection or a crashed caller) replays the
// first delivery's outcome instead of delivering the content twice. It
// returns the runtime's receipt of the delivery, zero whenever the error is
// not nil.
//
// NudgeKeyed must deliver without waiting for the session to go idle: it
// also serves immediate delivery. [NudgeWithReceipt] sends a keyed immediate
// nudge through NudgeKeyed rather than [ImmediateNudgeProvider.NudgeNow],
// which takes no key, so a runtime whose keyed send waited for idle would
// turn every keyed immediate send into a wait-for-idle send.
//
// LookupDelivery reports, sending nothing, whether the runtime holds the
// delivery a send under key to session name made: its receipt when it does;
// an error wrapping [ErrDeliveryNotFound] when it holds none;
// [ErrSessionNotFound] when no session is named name; an error wrapping
// [ErrInteractionUnsupported] when the backend serving name keeps no keyed
// deliveries (a composite routing name to an unkeyed leaf); an error wrapping
// [ErrRuntimeUnavailable] when it cannot tell, including when the runtime's
// answer is too incomplete to prove the delivery absent.
type KeyedNudgeProvider interface {
	NudgeKeyed(name string, content []ContentBlock, key string) (DeliveryReceipt, error)
	LookupDelivery(ctx context.Context, name, key string) (DeliveryReceipt, error)
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
	_, err := nudgeKeyed(ctx, p, name, content)
	return err
}

// nudgeKeyed is NudgeWithDeliveryKey with the receipt p's NudgeKeyed
// reported: zero for an unkeyed nudge and on any error.
func nudgeKeyed(ctx context.Context, p Provider, name string, content []ContentBlock) (DeliveryReceipt, error) {
	if key := NudgeDeliveryKey(ctx); key != "" {
		if kp, ok := p.(KeyedNudgeProvider); ok {
			r, err := kp.NudgeKeyed(name, content, key)
			if err != nil {
				return DeliveryReceipt{}, err
			}
			return r, nil
		}
	}
	return DeliveryReceipt{}, p.Nudge(name, content)
}

// keysDeliveries reports whether the leaf backend serving name through p
// deduplicates keyed deliveries. Route knowledge is not consulted: a
// composite routes the call by the same route, and asking a leaf for a
// receipt or keeping a key on its nudge costs nothing when the route later
// changes.
func keysDeliveries(p Provider, name string) bool {
	if p == nil {
		return false
	}
	leaf, _, _ := ResolveBackend(p, name)
	_, ok := leaf.(KeyedNudgeProvider)
	return ok
}

// NudgeWithReceipt delivers content to name through p as [NudgeWithContext]
// documents, and returns the receipt p's leaf backend for name reported when
// that leaf deduplicates keyed deliveries; zero otherwise, and on any error.
// Under a delivery key, a nudge to such a leaf goes out keyed even when
// immediate: such a runtime starts the turn without an idle wait either way,
// and its NudgeNow would drop the key.
func NudgeWithReceipt(ctx context.Context, p Provider, name string, content []ContentBlock, immediate bool) (DeliveryReceipt, error) {
	f, fp, fenced, err := fencedProvider(ctx, p, name)
	if err != nil {
		return DeliveryReceipt{}, err
	}
	keyed := keysDeliveries(p, name)
	if fenced {
		n := FencedNudge{Fence: f, Immediate: immediate, DeliveryKey: NudgeDeliveryKey(ctx)}
		var r DeliveryReceipt
		if keyed {
			n.Receipt = &r
		}
		if err := fp.NudgeFenced(ctx, name, content, n); err != nil {
			return DeliveryReceipt{}, err
		}
		return r, nil
	}
	if immediate && (!keyed || NudgeDeliveryKey(ctx) == "") {
		if np, ok := p.(ImmediateNudgeProvider); ok {
			return DeliveryReceipt{}, np.NudgeNow(name, content)
		}
	}
	return nudgeKeyed(ctx, p, name, content)
}

// LookupDelivery asks p whether it holds the delivery a send under key made
// to session name, sending nothing. found, with the receipt, when it does.
// Not found, with a nil error, when key is empty, p keeps no keyed
// deliveries, its routed leaf does not ([ErrInteractionUnsupported]), it
// holds none ([ErrDeliveryNotFound]), or no session is named name
// ([ErrSessionNotFound] not joined with [ErrRuntimeUnavailable]). Any other
// answer is the error: p could not tell.
func LookupDelivery(ctx context.Context, p Provider, name, key string) (r DeliveryReceipt, found bool, err error) {
	if key == "" || p == nil {
		return DeliveryReceipt{}, false, nil
	}
	kp, ok := p.(KeyedNudgeProvider)
	if !ok {
		return DeliveryReceipt{}, false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r, err = kp.LookupDelivery(ctx, name, key)
	switch {
	case err == nil:
		return r, true, nil
	case errors.Is(err, ErrRuntimeUnavailable):
		return DeliveryReceipt{}, false, err
	case errors.Is(err, ErrDeliveryNotFound), errors.Is(err, ErrInteractionUnsupported), errors.Is(err, ErrSessionNotFound):
		return DeliveryReceipt{}, false, nil
	default:
		return DeliveryReceipt{}, false, err
	}
}
