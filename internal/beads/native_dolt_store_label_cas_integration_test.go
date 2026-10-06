//go:build integration

package beads

import (
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// nativeDoltLabelCASTiers are the two row tiers a native label CAS must cover:
// issues rows, and wisp rows, whose labels live in wisp_labels.
var nativeDoltLabelCASTiers = []struct {
	name      string
	ephemeral bool
}{
	{"issues", false},
	{"wisps", true},
}

// createNativeDoltLabelCASFixture creates a bead labeled keep and remove on
// the given tier of real Dolt and returns it as read back.
func createNativeDoltLabelCASFixture(t *testing.T, store *NativeDoltStore, ephemeral bool) Bead {
	t.Helper()
	created, err := store.Create(Bead{Title: "native label CAS", Labels: []string{"keep", "remove"}, Ephemeral: ephemeral})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Ephemeral != ephemeral {
		t.Fatalf("Ephemeral = %v, want %v: the fixture is on the wrong tier", got.Ephemeral, ephemeral)
	}
	return got
}

// TestNativeDoltLabelCASBumpsRowVersion proves, against real Dolt, that a
// label-only UpdateIfMatch moves the row version, so a CAS read before it
// fails after it. It first pins the premise that makes the stamp key
// necessary: upstream label writes alone leave the row version unchanged.
func TestNativeDoltLabelCASBumpsRowVersion(t *testing.T) {
	for _, tier := range nativeDoltLabelCASTiers {
		t.Run(tier.name, func(t *testing.T) {
			store := openRealNativeDoltStoreForCAS(t, "label-cas-"+tier.name)
			created := createNativeDoltLabelCASFixture(t, store, tier.ephemeral)

			if err := store.Update(created.ID, UpdateOpts{Labels: []string{"premise"}}); err != nil {
				t.Fatalf("unconditional label Update: %v", err)
			}
			premise, err := store.Get(created.ID)
			if err != nil {
				t.Fatalf("Get after label Update: %v", err)
			}
			if premise.Revision != created.Revision {
				t.Fatalf("premise changed: an upstream label-only write moved the row version %d -> %d; "+
					"beadmeta.LabelRevisionMetadataKey may no longer be needed", created.Revision, premise.Revision)
			}

			for _, opts := range []UpdateOpts{
				{Labels: []string{"added"}},
				{RemoveLabels: []string{"remove"}},
			} {
				before, err := store.Get(created.ID)
				if err != nil {
					t.Fatalf("Get: %v", err)
				}
				if err := store.UpdateIfMatch(created.ID, before.Revision, opts); err != nil {
					t.Fatalf("UpdateIfMatch(%+v): %v", opts, err)
				}
				after, err := store.Get(created.ID)
				if err != nil {
					t.Fatalf("Get after UpdateIfMatch: %v", err)
				}
				if after.Revision == before.Revision {
					t.Fatalf("label-only UpdateIfMatch(%+v) left the row version at %d", opts, before.Revision)
				}
				if after.Metadata[beadmeta.LabelRevisionMetadataKey] == before.Metadata[beadmeta.LabelRevisionMetadataKey] {
					t.Fatalf("%s stayed %q", beadmeta.LabelRevisionMetadataKey, after.Metadata[beadmeta.LabelRevisionMetadataKey])
				}
				err = store.UpdateIfMatch(created.ID, before.Revision, UpdateOpts{RemoveLabels: []string{"keep"}})
				if !IsPreconditionFailed(err) {
					t.Fatalf("UpdateIfMatch at the pre-label version = %v, want *PreconditionFailedError", err)
				}
			}
			final, err := store.Get(created.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			for _, label := range []string{"keep", "premise", "added"} {
				if !slices.Contains(final.Labels, label) {
					t.Fatalf("labels = %v, missing %q", final.Labels, label)
				}
			}
			if slices.Contains(final.Labels, "remove") {
				t.Fatalf("labels = %v, still carry the removed label", final.Labels)
			}
		})
	}
}

// TestNativeDoltLabelCASLosesToConcurrentWrite proves, against real Dolt, that
// a write landing between the read and the label CAS refuses the whole CAS:
// no label is added or removed.
func TestNativeDoltLabelCASLosesToConcurrentWrite(t *testing.T) {
	for _, tier := range nativeDoltLabelCASTiers {
		t.Run(tier.name, func(t *testing.T) {
			store := openRealNativeDoltStoreForCAS(t, "label-cas-race-"+tier.name)
			created := createNativeDoltLabelCASFixture(t, store, tier.ephemeral)

			if err := store.SetMetadata(created.ID, "external", "1"); err != nil {
				t.Fatalf("external SetMetadata: %v", err)
			}
			err := store.UpdateIfMatch(created.ID, created.Revision, UpdateOpts{
				Labels:       []string{"late"},
				RemoveLabels: []string{"keep"},
			})
			if !IsPreconditionFailed(err) {
				t.Fatalf("UpdateIfMatch after a concurrent write = %v, want *PreconditionFailedError", err)
			}
			after, err := store.Get(created.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if !slices.Contains(after.Labels, "keep") || slices.Contains(after.Labels, "late") {
				t.Fatalf("labels = %v after a refused CAS, want keep and no late", after.Labels)
			}
		})
	}
}
