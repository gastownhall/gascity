package auto

import (
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// NudgeKeyed reaches the routed backend with its key when that backend
// deduplicates keyed deliveries, and falls back to its Nudge otherwise.
func TestNudgeKeyedForwardsToTheRoutedBackend(t *testing.T) {
	keyedLeaf := runtime.NewFake()
	plainLeaf := runtime.NewFake()
	p := New(runtime.NewFakeProfile(runtime.ProfileT3Bridge, runtime.FullFake{Fake: keyedLeaf}), plainLeaf)
	p.RouteACP("plain")

	if err := p.NudgeKeyed("keyed", runtime.TextContent("hi"), "batch-1"); err != nil {
		t.Fatalf("NudgeKeyed(keyed): %v", err)
	}
	if err := p.NudgeKeyed("plain", runtime.TextContent("hi"), "batch-1"); err != nil {
		t.Fatalf("NudgeKeyed(plain): %v", err)
	}
	if len(keyedLeaf.Calls) != 1 || keyedLeaf.Calls[0].DeliveryKey != "batch-1" {
		t.Fatalf("keyed leaf calls = %+v, want one Nudge carrying batch-1", keyedLeaf.Calls)
	}
	if len(plainLeaf.Calls) != 1 || plainLeaf.Calls[0].Method != "Nudge" || plainLeaf.Calls[0].DeliveryKey != "" {
		t.Fatalf("plain leaf calls = %+v, want one plain Nudge", plainLeaf.Calls)
	}
}
