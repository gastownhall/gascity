package t3bridge

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// lookupProviders are the raw bridge and the seam-backed provider production
// builds, each as a KeyedNudgeProvider reading server.
func lookupProviders() map[string]func(*testing.T, *t3BridgeTestServer) runtime.KeyedNudgeProvider {
	return map[string]func(*testing.T, *t3BridgeTestServer) runtime.KeyedNudgeProvider{
		"raw": func(t *testing.T, server *t3BridgeTestServer) runtime.KeyedNudgeProvider {
			return newTurnFenceTestProvider(t, server)
		},
		"seam-backed": func(t *testing.T, server *t3BridgeTestServer) runtime.KeyedNudgeProvider {
			_, sp := newSeamBackedSnapshotTestProvider(t, server.wsURL())
			return sp.(runtime.KeyedNudgeProvider)
		},
	}
}

// withMessages is boundThreadSnapshot("ready") whose thread carries messages.
func withMessages(messages ...map[string]interface{}) map[string]interface{} {
	snapshot := boundThreadSnapshot("ready")
	thread := snapshot["threads"].([]interface{})[0].(map[string]interface{})
	list := []interface{}{}
	for _, m := range messages {
		list = append(list, m)
	}
	thread["messages"] = list
	return snapshot
}

// t3Message is a stored T3 message of id created at createdAt.
func t3Message(id, createdAt string) map[string]interface{} {
	return map[string]interface{}{"id": id, "role": "user", "text": "hello", "createdAt": createdAt}
}

const lookupStored = "2026-10-19T08:00:00.123Z"

// Kills ids derived without the thread's city or thread id (M13), a lookup
// that matches by text or on any thread (M14), and a seam that does not pass
// it through (M16). The stored createdAt is reported, not now.
func TestLookupDeliveryFindsTheDerivedMessage(t *testing.T) {
	for label, newProvider := range lookupProviders() {
		t.Run(label, func(t *testing.T) {
			ids := deriveDeliveryIDs("city-a", "thread-agent-a", "gc:op-1:1")
			server := newTurnFenceServer(t, withMessages(t3Message("other", lookupStored), t3Message(ids.messageID, lookupStored)))
			p := newProvider(t, server)
			r, err := p.LookupDelivery(t.Context(), "agent-a", "gc:op-1:1")
			if err != nil {
				t.Fatalf("LookupDelivery: %v", err)
			}
			want, _ := time.Parse(time.RFC3339Nano, lookupStored)
			if r.MessageID != ids.messageID || !r.MessageAt.Equal(want) || r.TurnID != "" {
				t.Fatalf("receipt = %+v, want %s at %v", r, ids.messageID, want)
			}
		})
	}
}

// Kills a lookup that matches by text, on another thread or in another city.
func TestLookupDeliveryIsThreadScoped(t *testing.T) {
	for label, newProvider := range lookupProviders() {
		t.Run(label, func(t *testing.T) {
			for _, id := range []string{
				deriveDeliveryIDs("city-a", "thread-other", "gc:op-1:1").messageID,
				deriveDeliveryIDs("city-b", "thread-agent-a", "gc:op-1:1").messageID,
				deriveDeliveryIDs("city-a", "thread-agent-a", "gc:op-2:1").messageID,
			} {
				server := newTurnFenceServer(t, withMessages(t3Message(id, lookupStored)))
				p := newProvider(t, server)
				if _, err := p.LookupDelivery(t.Context(), "agent-a", "gc:op-1:1"); !errors.Is(err, runtime.ErrDeliveryNotFound) {
					t.Fatalf("LookupDelivery with only %s = %v, want ErrDeliveryNotFound", id, err)
				}
			}
		})
	}
}

// Kills a lookup served from the snapshot cache (M15).
func TestLookupDeliveryReadsFresh(t *testing.T) {
	ids := deriveDeliveryIDs("city-a", "thread-agent-a", "k")
	server := newTurnFenceServer(t, withMessages(), withMessages(t3Message(ids.messageID, lookupStored)))
	p := newTurnFenceTestProvider(t, server)
	if _, err := p.rpcSnapshot(); err != nil {
		t.Fatalf("warming the cache: %v", err)
	}
	before := server.snapshotCalls()
	if _, err := p.LookupDelivery(t.Context(), "agent-a", "k"); err != nil {
		t.Fatalf("LookupDelivery with a stale cache: %v", err)
	}
	if server.snapshotCalls() != before+1 {
		t.Fatalf("snapshot reads = %d, want %d: the lookup must read fresh", server.snapshotCalls(), before+1)
	}
}

// The lookup's errors: it reports an inability to tell as unavailable, an
// unbound name as not found, and a snapshot thread whose messages field is
// absent as unavailable: such a snapshot cannot prove the delivery absent,
// and a caller trusting "not found" would resend (DES-T3I section 14 item 8).
// A thread whose message list is empty holds no delivery. A match whose
// createdAt does not parse is still found. The start-grace case drives the
// raw bridge's start bookkeeping, so it runs on the raw provider only.
func TestLookupDeliveryErrors(t *testing.T) {
	ids := deriveDeliveryIDs("city-a", "thread-agent-a", "k")
	noMessages := boundThreadSnapshot("ready")
	for label, newProvider := range lookupProviders() {
		t.Run(label, func(t *testing.T) {
			for _, tc := range []struct {
				name     string
				snapshot map[string]interface{}
				outage   bool
				cancel   bool
				grace    bool
				want     error
				found    bool
			}{
				{name: "unbound", snapshot: map[string]interface{}{"threads": []interface{}{}}, want: runtime.ErrSessionNotFound},
				{name: "start grace", snapshot: map[string]interface{}{"threads": []interface{}{}}, grace: true, want: runtime.ErrRuntimeUnavailable},
				{name: "outage", outage: true, want: runtime.ErrRuntimeUnavailable},
				{name: "canceled", snapshot: noMessages, cancel: true, want: runtime.ErrRuntimeUnavailable},
				{name: "messages absent", snapshot: noMessages, want: runtime.ErrRuntimeUnavailable},
				{name: "messages empty", snapshot: withMessages(), want: runtime.ErrDeliveryNotFound},
				{name: "malformed createdAt", snapshot: withMessages(t3Message(ids.messageID, "yesterday")), found: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					if tc.grace && label != "raw" {
						t.Skip("start grace is raw-bridge bookkeeping")
					}
					server := newTurnFenceServer(t, tc.snapshot)
					if tc.outage {
						server.setSnapshots(nil)
					}
					p := newProvider(t, server)
					if tc.grace {
						p.(*Provider).setRecentStart("agent-a", time.Now())
					}
					ctx := t.Context()
					if tc.cancel {
						var cancel context.CancelFunc
						ctx, cancel = context.WithCancel(ctx)
						cancel()
					}
					r, err := p.LookupDelivery(ctx, "agent-a", "k")
					if tc.found {
						if err != nil || r.MessageID != ids.messageID || !r.MessageAt.IsZero() {
							t.Fatalf("LookupDelivery = %+v, %v; want found with no stamp", r, err)
						}
						return
					}
					if !errors.Is(err, tc.want) || !r.IsZero() {
						t.Fatalf("LookupDelivery = %+v, %v; want %v", r, err, tc.want)
					}
					if tc.cancel && !errors.Is(err, context.Canceled) {
						t.Fatalf("LookupDelivery under a canceled ctx = %v, want it to wrap context.Canceled", err)
					}
				})
			}
		})
	}
}

// A lookup sends nothing.
func TestLookupDeliverySendsNothing(t *testing.T) {
	for label, newProvider := range lookupProviders() {
		t.Run(label, func(t *testing.T) {
			ids := deriveDeliveryIDs("city-a", "thread-agent-a", "k")
			server := newTurnFenceServer(t, withMessages(t3Message(ids.messageID, lookupStored)))
			p := newProvider(t, server)
			for _, key := range []string{"k", "missing"} {
				_, _ = p.LookupDelivery(t.Context(), "agent-a", key)
			}
			if got := server.commandTypes(); len(got) != 0 {
				t.Fatalf("dispatched %v during lookups, want nothing", got)
			}
		})
	}
}
