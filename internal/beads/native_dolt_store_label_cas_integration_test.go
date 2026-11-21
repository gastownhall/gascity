//go:build integration

package beads

import (
	"fmt"
	"slices"
	"strings"
	"sync"
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

// forEachNativeDoltLabelCASCase runs fn on a fresh store for each engine and
// tier. On the sql-server engine a wisp's writes go through the transaction's
// second (ignored-tables) connection, a path embedded Dolt never takes.
func forEachNativeDoltLabelCASCase(t *testing.T, fn func(t *testing.T, store *NativeDoltStore, ephemeral bool)) {
	t.Helper()
	engines := []struct {
		name string
		open func(t *testing.T) *NativeDoltStore
	}{
		{"embedded", func(t *testing.T) *NativeDoltStore { return openRealNativeDoltStoreForCAS(t, "label-cas") }},
		{"sql-server", openServerNativeDoltStoreForMergeProof},
	}
	for _, engine := range engines {
		for _, tier := range nativeDoltLabelCASTiers {
			t.Run(engine.name+"/"+tier.name, func(t *testing.T) {
				fn(t, engine.open(t), tier.ephemeral)
			})
		}
	}
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
	forEachNativeDoltLabelCASCase(t, func(t *testing.T, store *NativeDoltStore, ephemeral bool) {
		created := createNativeDoltLabelCASFixture(t, store, ephemeral)

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

// TestNativeDoltLabelCASLosesToConcurrentWrite proves, against real Dolt, that
// a write landing between the read and the label CAS refuses the whole CAS:
// no label is added or removed.
func TestNativeDoltLabelCASLosesToConcurrentWrite(t *testing.T) {
	forEachNativeDoltLabelCASCase(t, func(t *testing.T, store *NativeDoltStore, ephemeral bool) {
		created := createNativeDoltLabelCASFixture(t, store, ephemeral)

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

// TestNativeDoltLabelCASRace races N label CASes at one revision, round after
// round, on real Dolt. Each round must have exactly one winner, every loser
// must report a precondition failure, and the labels applied must be exactly
// the winner's (a loser applies nothing). This is the test that sees a version
// check performed outside the transaction: every racer passes such a check.
func TestNativeDoltLabelCASRace(t *testing.T) {
	const writers, rounds = 8, 6
	forEachNativeDoltLabelCASCase(t, func(t *testing.T, store *NativeDoltStore, ephemeral bool) {
		created := createNativeDoltLabelCASFixture(t, store, ephemeral)
		for round := range rounds {
			base, err := store.Get(created.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			var (
				mu      sync.Mutex
				winners []string
				losses  []error
				wg      sync.WaitGroup
			)
			start := make(chan struct{})
			for i := range writers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					label := fmt.Sprintf("r%d-w%d", round, i)
					opts := UpdateOpts{Labels: []string{label}}
					if i%2 == 0 {
						opts.Metadata = map[string]string{"winner": label}
					}
					<-start
					err := store.UpdateIfMatch(created.ID, base.Revision, opts)
					mu.Lock()
					defer mu.Unlock()
					if err == nil {
						winners = append(winners, label)
					} else {
						losses = append(losses, err)
					}
				}()
			}
			close(start)
			wg.Wait()

			if len(winners) != 1 {
				t.Fatalf("round %d: %d winners at one revision (%v), want exactly 1", round, len(winners), winners)
			}
			for _, err := range losses {
				if !IsPreconditionFailed(err) {
					t.Fatalf("round %d: a loser returned %v, want *PreconditionFailedError", round, err)
				}
			}
			after, err := store.Get(created.ID)
			if err != nil {
				t.Fatalf("Get after round %d: %v", round, err)
			}
			var applied []string
			for _, label := range after.Labels {
				if strings.HasPrefix(label, fmt.Sprintf("r%d-", round)) {
					applied = append(applied, label)
				}
			}
			if !slices.Equal(applied, winners) {
				t.Fatalf("round %d: applied labels %v, want exactly the winner's %v", round, applied, winners)
			}
			if after.Revision == base.Revision {
				t.Fatalf("round %d: the winning label CAS left the row version at %d", round, base.Revision)
			}
		}
	})
}
