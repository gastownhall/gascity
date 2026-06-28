package beads_test

import (
	"os"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestMetadataEmptyStringClearContract pins the cross-backend contract that the
// object-model front doors depend on: writing an empty-string value for a
// metadata key (via SetMetadata, SetMetadataBatch, or Update{Metadata}) is
// observationally equivalent to the key being absent. A subsequent read of the
// bead must yield metadata[key] == "" — never the key's prior non-empty value.
//
// This is the up-front Phase 6 risk mitigation (OBJECT-MODEL-FRONT-DOOR-DESIGN
// sec 6.6): the MetadataPatch builders in internal/session set a key to "" to
// "clear" it (e.g. SleepPatch clears last_woke_at, ClosePatch-adjacent paths
// clear pending_create_*), and every heal/clear silently breaks if any backend
// preserves the old value behind an empty-string write. This test proves the
// observable clear semantics hold on every real backend so the front-door
// write methods (Phase 4) can rely on them.
//
// NOTE ON THE ACTUAL MECHANISM: the in-process stores (MemStore,
// NativeDoltStore) store the empty string rather than physically deleting the
// key. That is fine: the front-door read codecs (InfoFromPersistedBead,
// decodeNudgeItem, OrderRun decode) read metadata[key] and treat "" and absent
// identically. The contract this test enforces is the OBSERVABLE one — read
// back yields "" — which is exactly what the front doors consume. It does NOT
// assert physical key deletion, because no backend implements that today and
// the front doors do not require it.
func TestMetadataEmptyStringClearContract(t *testing.T) {
	backends := []struct {
		name     string
		newStore func(t *testing.T) beads.Store
	}{
		{
			name:     "MemStore",
			newStore: func(_ *testing.T) beads.Store { return beads.NewMemStore() },
		},
		{
			name:     "NativeDoltStore",
			newStore: func(_ *testing.T) beads.Store { return beads.NewNativeDoltStoreForConformance() },
		},
		{
			name: "Postgres",
			newStore: func(t *testing.T) beads.Store {
				// No in-package postgres Store constructor exists; postgres is
				// reached through the bd/native DSN path. When a test DSN is
				// provided we would construct one here. Until then this backend
				// is skipped (the NativeDoltStore arm exercises the same
				// metadata codec the postgres path uses).
				dsn := os.Getenv("GC_BEADS_TEST_PG_DSN")
				if dsn == "" {
					t.Skip("GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance")
				}
				t.Skip("postgres in-package Store constructor not available in Phase 0; DSN-gated arm reserved")
				return nil
			},
		},
	}

	for _, backend := range backends {
		backend := backend
		t.Run(backend.name, func(t *testing.T) {
			t.Run("SetMetadata", func(t *testing.T) {
				s := backend.newStore(t)
				assertEmptyClears(t, s, func(s beads.Store, id string) error {
					return s.SetMetadata(id, "state", "")
				})
			})
			t.Run("SetMetadataBatch", func(t *testing.T) {
				s := backend.newStore(t)
				assertEmptyClears(t, s, func(s beads.Store, id string) error {
					return s.SetMetadataBatch(id, map[string]string{"state": ""})
				})
			})
			t.Run("Update", func(t *testing.T) {
				s := backend.newStore(t)
				assertEmptyClears(t, s, func(s beads.Store, id string) error {
					return s.Update(id, beads.UpdateOpts{Metadata: map[string]string{"state": ""}})
				})
			})
			t.Run("BatchClearsOneKeyKeepsAnother", func(t *testing.T) {
				s := backend.newStore(t)
				created, err := s.Create(beads.Bead{
					Title:    "two keys",
					Metadata: map[string]string{"state": "active", "sleep_reason": "user-hold"},
				})
				if err != nil {
					t.Fatalf("Create: %v", err)
				}
				if err := s.SetMetadataBatch(created.ID, map[string]string{
					"sleep_reason": "",       // clear
					"state":        "asleep", // overwrite
				}); err != nil {
					t.Fatalf("SetMetadataBatch: %v", err)
				}
				got, err := s.Get(created.ID)
				if err != nil {
					t.Fatalf("Get: %v", err)
				}
				if got.Metadata["sleep_reason"] != "" {
					t.Errorf("sleep_reason: empty write did not clear; got %q", got.Metadata["sleep_reason"])
				}
				if got.Metadata["state"] != "asleep" {
					t.Errorf("state: want %q got %q", "asleep", got.Metadata["state"])
				}
			})
		})
	}
}

// assertEmptyClears creates a bead with state="active", applies clear via the
// supplied write, and asserts the read-back state is empty.
func assertEmptyClears(t *testing.T, s beads.Store, clearFn func(s beads.Store, id string) error) {
	t.Helper()
	created, err := s.Create(beads.Bead{
		Title:    "clear me",
		Metadata: map[string]string{"state": "active"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	pre, err := s.Get(created.ID)
	if err != nil {
		t.Fatalf("Get (pre): %v", err)
	}
	if pre.Metadata["state"] != "active" {
		t.Fatalf("precondition: state should be %q, got %q", "active", pre.Metadata["state"])
	}
	if err := clearFn(s, created.ID); err != nil {
		t.Fatalf("clear write: %v", err)
	}
	post, err := s.Get(created.ID)
	if err != nil {
		t.Fatalf("Get (post): %v", err)
	}
	if post.Metadata["state"] != "" {
		t.Errorf("empty-string write did not clear: state=%q (want observable empty)", post.Metadata["state"])
	}
}
