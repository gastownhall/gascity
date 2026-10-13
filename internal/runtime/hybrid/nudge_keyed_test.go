package hybrid

import (
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// NudgeKeyed reaches the routed backend with its key when that backend
// deduplicates keyed deliveries, and falls back to its Nudge otherwise.
func TestNudgeKeyedForwardsToTheRoutedBackend(t *testing.T) {
	plainLeaf := runtime.NewFake()
	keyedLeaf := runtime.NewFake()
	p := New(plainLeaf, runtime.NewFakeProfile(runtime.ProfileT3Bridge, runtime.FullFake{Fake: keyedLeaf}), func(name string) bool { return name == "keyed" })

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
