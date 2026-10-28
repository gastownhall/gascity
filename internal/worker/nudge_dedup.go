package worker

import (
	"context"
	"fmt"

	"github.com/gastownhall/gascity/internal/runtime"
)

// DeliveryKeysDeduplicated reports whether the runtime serving session name
// through sp deduplicates keyed deliveries: a nudge sent under a
// NudgeRequest.DeliveryKey the runtime already applied replays the first
// delivery's outcome instead of delivering the text again. It reads the leaf
// backend a composite routes name to, since a composite forwards keyed
// nudges for every session whatever its backend does, and answers false for
// a route the composite cannot vouch for: a caller that assumes no
// deduplication only repeats itself, while one that wrongly assumes it can
// lose a repeat it needed to mark.
func DeliveryKeysDeduplicated(sp runtime.Provider, name string) bool {
	if sp == nil {
		return false
	}
	leaf, _, known := runtime.ResolveBackend(sp, name)
	if !known || leaf == nil {
		return false
	}
	_, ok := leaf.(runtime.KeyedNudgeProvider)
	return ok
}

// heldDelivery runs a redelivery's lookup: found reports the receipt of the
// delivery the runtime already holds under key, so nothing is sent again. A
// lookup that cannot tell is an error wrapping runtime.ErrNudgeNotSent,
// since nothing was sent, and the lookup's cause. With no redelivery or no
// key it looks nothing up.
func heldDelivery(ctx context.Context, redelivery bool, key string, lookup func(context.Context, string) (runtime.DeliveryReceipt, bool, error)) (runtime.DeliveryReceipt, bool, error) {
	if !redelivery || key == "" {
		return runtime.DeliveryReceipt{}, false, nil
	}
	r, found, err := lookup(ctx, key)
	if err != nil {
		return runtime.DeliveryReceipt{}, false, fmt.Errorf("looking up the earlier delivery: %w: %w", runtime.ErrNudgeNotSent, err)
	}
	return r, found, nil
}
