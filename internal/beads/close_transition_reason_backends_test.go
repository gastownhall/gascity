package beads

import "testing"

func TestBdStoreCloseTransitionEmptyReasonPreservesStampedReason(t *testing.T) {
	const stampedReason = "reason stamped before close"

	for _, tt := range []struct {
		name   string
		reason string
	}{
		{name: "empty", reason: ""},
		{name: "whitespace", reason: " \t\n "},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fixture := &bdCloseTransitionFixture{status: "open", reason: stampedReason}
			store := NewBdStore("/city", fixture.runner)

			transition, err := store.CloseWithReasonIfOpen("bd-42", tt.reason)
			if err != nil {
				t.Fatalf("CloseWithReasonIfOpen(%q): %v", tt.reason, err)
			}
			if !transition.Transitioned {
				t.Fatal("Transitioned = false, want true")
			}
			if got := transition.Before.Metadata["close_reason"]; got != stampedReason {
				t.Fatalf("Before close_reason = %q, want %q", got, stampedReason)
			}
			if got := transition.After.Metadata["close_reason"]; got != stampedReason {
				t.Fatalf("After close_reason = %q, want preserved reason %q", got, stampedReason)
			}
			if fixture.reason != stampedReason {
				t.Fatalf("bd close reason = %q, want preserved reason %q", fixture.reason, stampedReason)
			}
			if fixture.closeCalls != 1 {
				t.Fatalf("bd close calls = %d, want 1", fixture.closeCalls)
			}
		})
	}
}

func TestNativeDoltStoreCloseTransitionEmptyReasonPreservesStampedReason(t *testing.T) {
	const stampedReason = "reason stamped before close"

	for _, tt := range []struct {
		name   string
		reason string
	}{
		{name: "empty", reason: ""},
		{name: "whitespace", reason: " \t\n "},
	} {
		t.Run(tt.name, func(t *testing.T) {
			storage := newNativeCloseTransitionStorage()
			store := newNativeDoltStoreForTest(storage)
			created, err := store.Create(Bead{
				Title: "pre-stamped close target",
				Metadata: StringMap{
					"close_reason": stampedReason,
					"existing":     "kept",
				},
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}

			transition, err := store.CloseWithReasonIfOpen(created.ID, tt.reason)
			if err != nil {
				t.Fatalf("CloseWithReasonIfOpen(%q): %v", tt.reason, err)
			}
			if !transition.Transitioned {
				t.Fatal("Transitioned = false, want true")
			}
			if got := transition.Before.Metadata["close_reason"]; got != stampedReason {
				t.Fatalf("Before close_reason = %q, want %q", got, stampedReason)
			}
			if got := transition.After.Metadata["close_reason"]; got != stampedReason {
				t.Fatalf("After close_reason = %q, want preserved reason %q", got, stampedReason)
			}

			storage.mu.Lock()
			durableReason := storage.reason
			storage.mu.Unlock()
			if durableReason != stampedReason {
				t.Fatalf("native close reason = %q, want preserved reason %q", durableReason, stampedReason)
			}
		})
	}
}
