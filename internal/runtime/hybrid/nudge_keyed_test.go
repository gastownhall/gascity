package hybrid

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// NudgeKeyed reaches the routed backend with its key when that backend
// deduplicates keyed deliveries, and falls back to its Nudge otherwise.
func TestNudgeKeyedForwardsToTheRoutedBackend(t *testing.T) {
	plainLeaf := runtime.NewFake()
	keyedLeaf := runtime.NewFake()
	p := New(plainLeaf, runtime.NewFakeProfile(runtime.ProfileT3Bridge, runtime.FullFake{Fake: keyedLeaf}), func(name string) bool { return name == "keyed" })

	if _, err := p.NudgeKeyed("keyed", runtime.TextContent("hi"), "batch-1"); err != nil {
		t.Fatalf("NudgeKeyed(keyed): %v", err)
	}
	if _, err := p.NudgeKeyed("plain", runtime.TextContent("hi"), "batch-1"); err != nil {
		t.Fatalf("NudgeKeyed(plain): %v", err)
	}
	if len(keyedLeaf.Calls) != 1 || keyedLeaf.Calls[0].DeliveryKey != "batch-1" {
		t.Fatalf("keyed leaf calls = %+v, want one Nudge carrying batch-1", keyedLeaf.Calls)
	}
	if len(plainLeaf.Calls) != 1 || plainLeaf.Calls[0].Method != "Nudge" || plainLeaf.Calls[0].DeliveryKey != "" {
		t.Fatalf("plain leaf calls = %+v, want one plain Nudge", plainLeaf.Calls)
	}
}

// SessionBinding answers from the routed backend when it hosts sessions in
// threads of its own, and reports no binding otherwise.
func TestSessionBindingForwardsToTheRoutedBackend(t *testing.T) {
	keyedLeaf := runtime.NewFake()
	keyedLeaf.Bindings = map[string]runtime.SessionBinding{"keyed": {ThreadID: "thread-1", ProjectID: "project-1"}}
	p := New(runtime.NewFake(), runtime.NewFakeProfile(runtime.ProfileT3Bridge, runtime.FullFake{Fake: keyedLeaf}), func(name string) bool { return name == "keyed" })

	if got, err := p.SessionBinding("keyed"); err != nil || got != keyedLeaf.Bindings["keyed"] {
		t.Fatalf("SessionBinding(keyed) = %+v, %v; want %+v", got, err, keyedLeaf.Bindings["keyed"])
	}
	if got, err := p.SessionBinding("plain"); err != nil || got != (runtime.SessionBinding{}) {
		t.Fatalf("SessionBinding(plain) = %+v, %v; want none", got, err)
	}
}

// Kills a composite that answers not found for a keyed leaf, or calls a
// plain leaf (M17): LookupDelivery forwards to the keyed leaf, and is
// ErrInteractionUnsupported with no call for a plain one. NudgeKeyed
// returns the keyed leaf's receipt and none from the plain leaf.
func TestLookupDeliveryForwardsOnlyToAKeyedBackend(t *testing.T) {
	plainLeaf := runtime.NewFake()
	keyedLeaf := runtime.NewFake()
	p := New(plainLeaf, runtime.NewFakeProfile(runtime.ProfileT3Bridge, runtime.FullFake{Fake: keyedLeaf}), func(name string) bool { return name == "keyed" })
	r, err := p.NudgeKeyed("keyed", runtime.TextContent("hi"), "batch-1")
	if err != nil || r.IsZero() || r != keyedLeaf.KeyedDeliveries["keyed"]["batch-1"] {
		t.Fatalf("NudgeKeyed(keyed) = %+v, %v; want the leaf's receipt", r, err)
	}
	if r, err := p.NudgeKeyed("plain", runtime.TextContent("hi"), "batch-1"); err != nil || !r.IsZero() {
		t.Fatalf("NudgeKeyed(plain) = %+v, %v; want no receipt", r, err)
	}
	plainLeaf.Calls = nil
	if got, err := p.LookupDelivery(context.Background(), "keyed", "batch-1"); err != nil || got != r {
		t.Fatalf("LookupDelivery(keyed) = %+v, %v; want %+v", got, err, r)
	}
	if n := keyedLeaf.CountCalls("LookupDelivery", "keyed"); n != 1 {
		t.Fatalf("keyed leaf lookups = %d, want 1", n)
	}
	if _, err := p.LookupDelivery(context.Background(), "plain", "batch-1"); !errors.Is(err, runtime.ErrInteractionUnsupported) {
		t.Fatalf("LookupDelivery(plain) = %v, want ErrInteractionUnsupported", err)
	}
	if len(plainLeaf.SnapshotCalls()) != 0 {
		t.Fatalf("plain leaf calls = %+v, want none", plainLeaf.SnapshotCalls())
	}
}
