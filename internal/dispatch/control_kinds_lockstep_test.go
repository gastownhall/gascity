package dispatch

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// TestProcessControlCoversEveryControlKind keeps the ProcessControl switch and
// beadmeta.ControlKinds in lockstep: every declared control kind must have a
// switch case (the processor may fail on the minimal bead, but never with the
// unsupported-kind error), and a kind outside the vocabulary must hard-error.
// Adding a case to the switch without declaring the kind in beadmeta (or vice
// versa) fails here or in beadmeta.TestControlKindsExact.
func TestProcessControlCoversEveryControlKind(t *testing.T) {
	for _, kind := range beadmeta.ControlKinds {
		t.Run(kind, func(t *testing.T) {
			store := beads.NewMemStore()
			bead, err := store.Create(beads.Bead{
				Title:    "lockstep probe " + kind,
				Metadata: map[string]string{beadmeta.KindMetadataKey: kind},
			})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			_, err = ProcessControl(store, bead, ProcessOptions{})
			if err != nil && strings.Contains(err.Error(), "unsupported control bead kind") {
				t.Errorf("ProcessControl rejected declared control kind %q as unsupported: %v", kind, err)
			}
		})
	}

	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:    "lockstep probe unknown",
		Metadata: map[string]string{beadmeta.KindMetadataKey: "not-a-control-kind"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := ProcessControl(store, bead, ProcessOptions{}); err == nil || !strings.Contains(err.Error(), "unsupported control bead kind") {
		t.Errorf("ProcessControl(unknown kind) error = %v, want unsupported-kind error", err)
	}
}
