package t3bridge

import (
	"context"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// LookupDelivery implements [runtime.KeyedNudgeProvider]: it reports, sending
// nothing, whether the thread bound to name holds the message a turn start
// under key carried, by the message id deriveDeliveryIDs gives key on that
// thread. It reads an uncached snapshot. The receipt names the message and
// the createdAt T3 stored for it (zero when that does not parse: the
// message's presence alone decides). No such message is an error wrapping
// [runtime.ErrDeliveryNotFound]. A snapshot thread whose messages field is
// absent wraps [runtime.ErrRuntimeUnavailable]: such a snapshot cannot prove
// the delivery absent, and a caller told "not found" would resend under a
// key T3 may already hold. Its other errors are
// freshSessionThread's: [runtime.ErrRuntimeUnavailable] when T3 cannot tell,
// [runtime.ErrSessionNotFound] when no thread is bound to name. T3 V1 never
// names the turn a message started, so the receipt has no TurnID.
func (p *Provider) LookupDelivery(ctx context.Context, name, key string) (runtime.DeliveryReceipt, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	thread, err := p.freshSessionThread(ctx, name)
	if err != nil {
		return runtime.DeliveryReceipt{}, err
	}
	binding := snapshotThreadBinding(thread)
	ids := deriveDeliveryIDs(threadCustomMetadata(thread)["gc.city"], binding.ThreadID, key)
	messages, ok := thread["messages"].([]interface{})
	if !ok {
		return runtime.DeliveryReceipt{}, fmt.Errorf("%w: t3 thread %s for %q carries no messages in its snapshot", runtime.ErrRuntimeUnavailable, binding.ThreadID, name)
	}
	for _, raw := range messages {
		msg, _ := raw.(map[string]interface{})
		if id, _ := msg["id"].(string); id != ids.messageID {
			continue
		}
		r := runtime.DeliveryReceipt{MessageID: ids.messageID}
		if created, _ := msg["createdAt"].(string); created != "" {
			if at, err := time.Parse(time.RFC3339Nano, created); err == nil {
				r.MessageAt = at.UTC()
			}
		}
		return r, nil
	}
	return runtime.DeliveryReceipt{}, fmt.Errorf("t3 thread %s for %q: %w", binding.ThreadID, name, runtime.ErrDeliveryNotFound)
}
