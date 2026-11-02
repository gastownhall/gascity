package dispatch

import (
	"errors"
	"maps"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// scopedFanoutFixture is a fanout control in scope "body" over a closed source
// step, with the scope body blocked by the fanout. sourceMeta decides which
// terminal branch the fanout takes (failed source, or no items).
type scopedFanoutFixture struct {
	store  bdClosePolicyStore
	body   beads.Bead
	fanout beads.Bead
}

func newScopedFanoutFixture(t *testing.T, sourceMeta map[string]string) scopedFanoutFixture {
	t.Helper()
	store := newBdClosePolicyStore()
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{Title: "workflow", Type: "task", Metadata: map[string]string{
		beadmeta.KindMetadataKey: beadmeta.KindWorkflow,
	}})
	body := mustCreateWorkflowBead(t, store, beads.Bead{Title: "body", Type: "task", Metadata: map[string]string{
		beadmeta.KindMetadataKey:       beadmeta.KindScope,
		beadmeta.RootBeadIDMetadataKey: workflow.ID,
		beadmeta.StepRefMetadataKey:    "body",
		beadmeta.ScopeRoleMetadataKey:  beadmeta.ScopeRoleBody,
	}})
	meta := map[string]string{
		beadmeta.RootBeadIDMetadataKey: workflow.ID,
		beadmeta.StepRefMetadataKey:    "demo.survey",
		beadmeta.ScopeRefMetadataKey:   "body",
		beadmeta.ScopeRoleMetadataKey:  "member",
	}
	maps.Copy(meta, sourceMeta)
	source := mustCreateWorkflowBead(t, store, beads.Bead{Title: "survey", Type: "task", Status: "closed", Metadata: meta})
	fanout := mustCreateWorkflowBead(t, store, beads.Bead{Title: "fanout", Type: "task", Metadata: map[string]string{
		beadmeta.KindMetadataKey:       beadmeta.KindFanout,
		beadmeta.RootBeadIDMetadataKey: workflow.ID,
		beadmeta.ScopeRefMetadataKey:   "body",
		beadmeta.ScopeRoleMetadataKey:  beadmeta.ScopeRoleControl,
		beadmeta.ControlForMetadataKey: "demo.survey",
		beadmeta.ForEachMetadataKey:    "output.items",
		beadmeta.BondMetadataKey:       "expansion-review",
	}})
	mustDepAdd(t, store, fanout.ID, source.ID, "blocks")
	mustDepAdd(t, store, body.ID, fanout.ID, "blocks")
	store.closeOrder = nil // setup closes are not under test
	return scopedFanoutFixture{store: store, body: body, fanout: fanout}
}

var (
	emptyFanoutSource  = map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass, beadmeta.OutputJSONMetadataKey: `{"items":[]}`}
	failedFanoutSource = map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomeFail}
)

// TestFanoutSettlesScopeBeforeClosing pins the ga-l6kddq write order: the
// fanout settles its enclosing scope while it is still open and closes last,
// because once it is closed nothing re-drives the scope. The body is blocked by
// the open fanout, so its close must be the forced close bd allows.
func TestFanoutSettlesScopeBeforeClosing(t *testing.T) {
	for _, tc := range []struct {
		name        string
		source      map[string]string
		wantOutcome string
		wantAction  string
	}{
		{name: "empty", source: emptyFanoutSource, wantOutcome: beadmeta.OutcomePass, wantAction: "fanout-empty"},
		{name: "source-failed", source: failedFanoutSource, wantOutcome: beadmeta.OutcomeFail, wantAction: "fanout-fail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newScopedFanoutFixture(t, tc.source)
			result, err := ProcessControl(f.store, f.fanout, ProcessOptions{})
			if err != nil {
				t.Fatalf("ProcessControl: %v", err)
			}
			if result.Action != tc.wantAction {
				t.Fatalf("Action = %q, want %q", result.Action, tc.wantAction)
			}
			if got, want := f.store.closeOrder, []string{f.body.ID, f.fanout.ID}; !slices.Equal(got, want) {
				t.Fatalf("close order = %v, want %v (scope body before the fanout)", got, want)
			}
			for _, id := range []string{f.body.ID, f.fanout.ID} {
				b := mustGetBead(t, f.store, id)
				if b.Status != "closed" || b.Metadata[beadmeta.OutcomeMetadataKey] != tc.wantOutcome {
					t.Fatalf("%s = %s/%q, want closed/%s", id, b.Status, b.Metadata[beadmeta.OutcomeMetadataKey], tc.wantOutcome)
				}
			}
		})
	}
}

// TestFanoutRerunAdoptsSettledScope covers the interrupted settle: the scope
// body was settled but the fanout's own close was lost. The re-run closes the
// fanout and reports scope-settled without touching the scope again. The open
// sibling member would be skipped by a second scope abort, so it staying open
// proves the scope was not re-evaluated.
func TestFanoutRerunAdoptsSettledScope(t *testing.T) {
	for _, tc := range []struct {
		name        string
		source      map[string]string
		bodyOutcome string
	}{
		{name: "empty", source: emptyFanoutSource, bodyOutcome: beadmeta.OutcomePass},
		{name: "source-failed", source: failedFanoutSource, bodyOutcome: beadmeta.OutcomeFail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newScopedFanoutFixture(t, tc.source)
			sibling := mustCreateWorkflowBead(t, f.store, beads.Bead{Title: "sibling", Type: "task", Metadata: map[string]string{
				beadmeta.RootBeadIDMetadataKey: f.fanout.Metadata[beadmeta.RootBeadIDMetadataKey],
				beadmeta.StepRefMetadataKey:    "demo.sibling",
				beadmeta.ScopeRefMetadataKey:   "body",
				beadmeta.ScopeRoleMetadataKey:  "member",
			}})
			if err := forceCloseScopeBody(f.store, f.body.ID, map[string]string{beadmeta.OutcomeMetadataKey: tc.bodyOutcome}); err != nil {
				t.Fatalf("settling body: %v", err)
			}
			bodyBefore := mustGetBead(t, f.store, f.body.ID)

			result, err := ProcessControl(f.store, f.fanout, ProcessOptions{})
			if err != nil {
				t.Fatalf("ProcessControl: %v", err)
			}
			if result.Action != scopeSettledAction {
				t.Fatalf("Action = %q, want %q", result.Action, scopeSettledAction)
			}
			if fanout := mustGetBead(t, f.store, f.fanout.ID); fanout.Status != "closed" {
				t.Fatalf("fanout status = %s, want closed", fanout.Status)
			}
			if got := mustGetBead(t, f.store, f.body.ID); !maps.Equal(got.Metadata, bodyBefore.Metadata) || got.Status != "closed" {
				t.Fatalf("body changed on re-run: %s %v, want %s %v", got.Status, got.Metadata, bodyBefore.Status, bodyBefore.Metadata)
			}
			if got := mustGetBead(t, f.store, sibling.ID); got.Status != "open" {
				t.Fatalf("sibling status = %s, want open (scope must not be re-aborted)", got.Status)
			}
		})
	}
}

// TestDrainFinishesRecordedClose covers a drain whose terminal state and
// outcome are recorded while it is still open: the next pass settles the scope
// and closes the drain with the recorded outcome instead of leaving it open.
func TestDrainFinishesRecordedClose(t *testing.T) {
	store := newBdClosePolicyStore()
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{Title: "workflow", Type: "task", Metadata: map[string]string{
		beadmeta.KindMetadataKey: beadmeta.KindWorkflow,
	}})
	body := mustCreateWorkflowBead(t, store, beads.Bead{Title: "body", Type: "task", Metadata: map[string]string{
		beadmeta.KindMetadataKey:       beadmeta.KindScope,
		beadmeta.RootBeadIDMetadataKey: workflow.ID,
		beadmeta.StepRefMetadataKey:    "body",
		beadmeta.ScopeRoleMetadataKey:  beadmeta.ScopeRoleBody,
	}})
	drain := mustCreateWorkflowBead(t, store, beads.Bead{Title: "drain", Type: "task", Metadata: map[string]string{
		beadmeta.KindMetadataKey:       beadmeta.KindDrain,
		beadmeta.RootBeadIDMetadataKey: workflow.ID,
		beadmeta.ScopeRefMetadataKey:   "body",
		beadmeta.ScopeRoleMetadataKey:  beadmeta.ScopeRoleControl,
		beadmeta.DrainStateMetadataKey: beadmeta.DrainStateFailed,
		beadmeta.OutcomeMetadataKey:    beadmeta.OutcomeFail,
	}})
	mustDepAdd(t, store, body.ID, drain.ID, "blocks")

	result, err := ProcessControl(store, drain, ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl: %v", err)
	}
	if result.Action != "drain-failed" {
		t.Fatalf("Action = %q, want drain-failed", result.Action)
	}
	if got, want := store.closeOrder, []string{body.ID, drain.ID}; !slices.Equal(got, want) {
		t.Fatalf("close order = %v, want %v", got, want)
	}
	for _, id := range []string{body.ID, drain.ID} {
		if b := mustGetBead(t, store, id); b.Status != "closed" || b.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeFail {
			t.Fatalf("%s = %s/%q, want closed/fail", id, b.Status, b.Metadata[beadmeta.OutcomeMetadataKey])
		}
	}
}

func TestDrainRecordedCloseWithoutOutcomeIsMalformed(t *testing.T) {
	store := beads.NewMemStore()
	drain := mustCreateWorkflowBead(t, store, beads.Bead{Title: "drain", Type: "task", Metadata: map[string]string{
		beadmeta.KindMetadataKey:       beadmeta.KindDrain,
		beadmeta.DrainStateMetadataKey: beadmeta.DrainStateSucceeded,
	}})
	_, err := ProcessControl(store, drain, ProcessOptions{})
	if !errors.Is(err, ErrControlGraphMalformed) {
		t.Fatalf("ProcessControl error = %v, want ErrControlGraphMalformed", err)
	}
	if got := mustGetBead(t, store, drain.ID).Status; got != "open" {
		t.Fatalf("drain status = %s, want open", got)
	}
}
