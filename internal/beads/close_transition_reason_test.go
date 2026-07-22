package beads_test

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestCloseTransitionerEmptyReasonPreservesStampedReason(t *testing.T) {
	const stampedReason = "reason stamped before close"

	forEachCloseTransitionStore(t, func(t *testing.T, stores closeTransitionStorePair) {
		for _, tt := range []struct {
			name   string
			reason string
		}{
			{name: "empty", reason: ""},
			{name: "whitespace", reason: " \t\n "},
		} {
			t.Run(tt.name, func(t *testing.T) {
				created, err := stores.primary.Create(beads.Bead{
					Title: "pre-stamped close target",
					Metadata: beads.StringMap{
						"close_reason": stampedReason,
						"existing":     "kept",
					},
				})
				if err != nil {
					t.Fatalf("Create: %v", err)
				}

				transition, err := requireCloseTransitioner(t, stores.primary).CloseWithReasonIfOpen(created.ID, tt.reason)
				if err != nil {
					t.Fatalf("CloseWithReasonIfOpen(%q): %v", tt.reason, err)
				}
				if !transition.Transitioned {
					t.Fatal("CloseWithReasonIfOpen Transitioned = false, want true")
				}
				if got := transition.Before.Metadata["close_reason"]; got != stampedReason {
					t.Fatalf("Before.Metadata[close_reason] = %q, want %q", got, stampedReason)
				}
				if got := transition.After.Metadata["close_reason"]; got != stampedReason {
					t.Fatalf("After.Metadata[close_reason] = %q, want preserved reason %q", got, stampedReason)
				}

				durable := getCloseTransitionBead(t, stores.peer, created.ID)
				if got := durable.Metadata["close_reason"]; got != stampedReason {
					t.Fatalf("durable Metadata[close_reason] = %q, want preserved reason %q", got, stampedReason)
				}
				if durable.Status != "closed" {
					t.Fatalf("durable Status = %q, want closed", durable.Status)
				}
			})
		}
	})
}
