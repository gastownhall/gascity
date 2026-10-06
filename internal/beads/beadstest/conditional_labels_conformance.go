package beadstest

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// RunConditionalLabelsConformance asserts the one property every
// ConditionalWriter owes for Labels and RemoveLabels in UpdateIfMatch: they are
// applied under the revision check, or refused, and never applied unguarded.
//
// A guarded store applies them with the row fields in one fenced write, moves
// the revision even for a label-only change (so a CAS read before it fails
// after it), and refuses them at a revision another write has moved. A
// refusing store returns *beads.ConditionalUpdateFieldUnsupportedError and
// mutates nothing, not even the row fields that rode along.
func RunConditionalLabelsConformance(t *testing.T, name string, open func(t *testing.T) beads.Store, guarded bool) {
	t.Helper()
	if !guarded {
		t.Run(name+"/labels_refused_without_mutation", func(t *testing.T) {
			s, w, created := conformanceLabelsFixture(t, open)
			for _, opts := range []beads.UpdateOpts{
				{Labels: []string{"added"}, Metadata: map[string]string{"k": "v"}},
				{RemoveLabels: []string{"remove"}, Metadata: map[string]string{"k": "v"}},
			} {
				err := w.UpdateIfMatch(created.ID, created.Revision, opts)
				var unsupported *beads.ConditionalUpdateFieldUnsupportedError
				if !errors.As(err, &unsupported) {
					t.Fatalf("UpdateIfMatch(%+v) = %v, want *ConditionalUpdateFieldUnsupportedError", opts, err)
				}
				if after := conformanceLabelsGet(t, s, created.ID); !reflect.DeepEqual(after, created) {
					t.Fatalf("refused label update mutated bead: before=%#v after=%#v", created, after)
				}
			}
		})
		return
	}

	t.Run(name+"/labels_applied_with_row_fields", func(t *testing.T) {
		s, w, created := conformanceLabelsFixture(t, open)
		if err := w.UpdateIfMatch(created.ID, created.Revision, beads.UpdateOpts{
			Labels:       []string{"added"},
			RemoveLabels: []string{"remove"},
			Metadata:     map[string]string{"k": "v"},
		}); err != nil {
			t.Fatalf("UpdateIfMatch: %v", err)
		}
		after := conformanceLabelsGet(t, s, created.ID)
		conformanceWantLabels(t, after, []string{"keep", "added"}, []string{"remove"})
		if after.Metadata["k"] != "v" {
			t.Fatalf("metadata k = %q, want v", after.Metadata["k"])
		}
		if after.Revision == created.Revision {
			t.Fatalf("revision stayed %d after a fenced label write", after.Revision)
		}
	})

	t.Run(name+"/label_only_write_moves_revision", func(t *testing.T) {
		s, w, created := conformanceLabelsFixture(t, open)
		for _, step := range []struct {
			opts         beads.UpdateOpts
			want, absent []string
		}{
			{beads.UpdateOpts{Labels: []string{"only"}}, []string{"keep", "remove", "only"}, nil},
			{beads.UpdateOpts{RemoveLabels: []string{"remove"}}, []string{"keep", "only"}, []string{"remove"}},
		} {
			before := conformanceLabelsGet(t, s, created.ID)
			if err := w.UpdateIfMatch(created.ID, before.Revision, step.opts); err != nil {
				t.Fatalf("UpdateIfMatch(%+v): %v", step.opts, err)
			}
			after := conformanceLabelsGet(t, s, created.ID)
			conformanceWantLabels(t, after, step.want, step.absent)
			if after.Revision == before.Revision {
				t.Fatalf("label-only UpdateIfMatch(%+v) left the revision at %d; a later CAS cannot see it", step.opts, before.Revision)
			}
			// The pre-write revision is now stale: a second writer holding it loses.
			err := w.UpdateIfMatch(created.ID, before.Revision, beads.UpdateOpts{Labels: []string{"stale"}})
			if !beads.IsPreconditionFailed(err) {
				t.Fatalf("UpdateIfMatch at the pre-label revision = %v, want *PreconditionFailedError", err)
			}
			conformanceWantLabels(t, conformanceLabelsGet(t, s, created.ID), nil, []string{"stale"})
		}
	})

	t.Run(name+"/labels_lose_to_concurrent_write", func(t *testing.T) {
		s, w, created := conformanceLabelsFixture(t, open)
		if err := s.SetMetadata(created.ID, "external", "1"); err != nil {
			t.Fatalf("external SetMetadata: %v", err)
		}
		err := w.UpdateIfMatch(created.ID, created.Revision, beads.UpdateOpts{
			Labels:       []string{"late"},
			RemoveLabels: []string{"keep"},
		})
		if !beads.IsPreconditionFailed(err) {
			t.Fatalf("UpdateIfMatch after a concurrent write = %v, want *PreconditionFailedError", err)
		}
		conformanceWantLabels(t, conformanceLabelsGet(t, s, created.ID), []string{"keep", "remove"}, []string{"late"})
	})
}

// conformanceLabelsFixture creates a bead labeled keep and remove and returns
// it as read back, with its revision.
func conformanceLabelsFixture(t *testing.T, open func(t *testing.T) beads.Store) (beads.Store, beads.ConditionalWriter, beads.Bead) {
	t.Helper()
	s := open(t)
	w := conformanceWriterFor(t, s)
	created, err := s.Create(beads.Bead{Title: "labels-cas", Labels: []string{"keep", "remove"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return s, w, conformanceLabelsGet(t, s, created.ID)
}

func conformanceLabelsGet(t *testing.T, s beads.Store, id string) beads.Bead {
	t.Helper()
	b, err := s.Get(id)
	if err != nil {
		t.Fatalf("Get(%q): %v", id, err)
	}
	return b
}

// conformanceWantLabels fails unless b carries every label in want and none in
// absent. Order and duplicates are store-specific and not asserted.
func conformanceWantLabels(t *testing.T, b beads.Bead, want, absent []string) {
	t.Helper()
	for _, label := range want {
		if !slices.Contains(b.Labels, label) {
			t.Fatalf("labels = %v, missing %q", b.Labels, label)
		}
	}
	for _, label := range absent {
		if slices.Contains(b.Labels, label) {
			t.Fatalf("labels = %v, still carry %q", b.Labels, label)
		}
	}
}
