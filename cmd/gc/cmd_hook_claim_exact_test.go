package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/molecule"
)

// exactClaimFixture is a work store holding graph-step beads, the observed
// rows the work query serves for them, and hook seams that claim through the
// store's revision fence.
type exactClaimFixture struct {
	t        *testing.T
	store    *beads.MemStore
	observed []beads.Bead
	rejected []string
	failed   []string
	claims   []string
}

func newExactClaimFixture(t *testing.T) *exactClaimFixture {
	t.Helper()
	store := beads.NewMemStore()
	store.HonorExplicitIDs = true
	if _, err := store.Create(beads.Bead{ID: "root-1", Title: "root", Status: "open"}); err != nil {
		t.Fatalf("create root: %v", err)
	}
	return &exactClaimFixture{t: t, store: store}
}

// graphStepMeta is the metadata of an unbound pool graph step under root-1.
func graphStepMeta(step string) map[string]string {
	return map[string]string{
		beadmeta.RoutedToMetadataKey:   "worker",
		beadmeta.RootBeadIDMetadataKey: "root-1",
		beadmeta.StepIDMetadataKey:     step,
	}
}

// add stores bead as the canonical row and observed as the work query's
// projection of it.
func (f *exactClaimFixture) add(stored, observed beads.Bead) {
	f.t.Helper()
	if _, err := f.store.Create(stored); err != nil {
		f.t.Fatalf("create %s: %v", stored.ID, err)
	}
	f.observed = append(f.observed, observed)
}

func (f *exactClaimFixture) writers() molecule.ClaimExactWriters {
	return molecule.ClaimExactWriters{Revision: f.store}
}

func (f *exactClaimFixture) ops() hookClaimOps {
	f.t.Helper()
	rows, err := json.Marshal(f.observed)
	if err != nil {
		f.t.Fatalf("marshal candidates: %v", err)
	}
	return hookClaimOps{
		Runner: func(string, string) (string, error) { return string(rows), nil },
		Claim: func(_ context.Context, _ string, _ []string, id, _ string) (beads.Bead, bool, error) {
			f.claims = append(f.claims, id)
			return beads.Bead{}, false, nil
		},
		ClaimExact: func(_ context.Context, _ string, _ []string, actor string, req molecule.ClaimExactRequest) (beads.Bead, hookClaimVerdict, error) {
			return hookClaimExactThroughStore(f.store, f.writers(), actor, req)
		},
		EmitClaimRejected: func(id, _, _ string) { f.rejected = append(f.rejected, id) },
		EmitClaimFailed:   func(id string, _ hookClaimOptions, _ error) { f.failed = append(f.failed, id) },
		ResolveWorkBranch: func(hookClaimWorkTree) string { return "" },
		StampWorkMeta:     noopStampWorkMeta,
		ReadWorkMeta: func(_ context.Context, _ string, _ []string, id, _ string) (beads.Bead, error) {
			return f.store.Get(id)
		},
		PublishRunMap:     noopPublishRunMap,
		StampSessionClaim: noopStampSessionClaim,
		WorkflowStore:     func(context.Context, string, []string, string, string) beads.Store { return f.store },
		DrainPending:      func(string) (bool, error) { return false, nil },
		InputVerdict: func(context.Context, string, []string, beads.Bead, []string, workflowInputAbsenceProver) (workflowInputVerdict, error) {
			return workflowInputVerdict{State: workflowInputPending}, nil
		},
	}
}

func (f *exactClaimFixture) get(id string) beads.Bead {
	f.t.Helper()
	b, err := f.store.Get(id)
	if err != nil {
		f.t.Fatalf("get %s: %v", id, err)
	}
	return b
}

// assertUntouched fails unless id is still open, unassigned, and carries
// neither a claim generation nor a session back-reference.
func (f *exactClaimFixture) assertUntouched(id string) {
	f.t.Helper()
	b := f.get(id)
	if b.Status != "open" || b.Assignee != "" {
		f.t.Fatalf("%s = status %q assignee %q, want open and unassigned", id, b.Status, b.Assignee)
	}
	for _, key := range []string{beadmeta.ClaimGenerationMetadataKey, beadmeta.SessionIDMetadataKey, beadmeta.SessionNameMetadataKey} {
		if v, ok := b.Metadata[key]; ok {
			f.t.Fatalf("%s carries %s=%q, want it unwritten", id, key, v)
		}
	}
}

// TestHookClaimExactFencesObservedStepID: the eligible tier observed work-1 as
// step build, but its step moved to test before the claim. The claim writes
// nothing to it, reports a rejection rather than claims_errored, and claims the
// next candidate instead.
func TestHookClaimExactFencesObservedStepID(t *testing.T) {
	f := newExactClaimFixture(t)
	f.add(
		beads.Bead{ID: "work-1", Title: "moved", Status: "open", Metadata: graphStepMeta("test")},
		beads.Bead{ID: "work-1", Status: "open", Metadata: graphStepMeta("build")},
	)
	f.add(
		beads.Bead{ID: "work-2", Title: "next", Status: "open", Metadata: graphStepMeta("build")},
		beads.Bead{ID: "work-2", Status: "open", Metadata: graphStepMeta("build")},
	)

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", poolClaimOpts(), f.ops(), &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	f.assertUntouched("work-1")
	if !strings.Contains(stderr.String(), "exact claim of work-1 rejected") {
		t.Fatalf("stderr = %q, want the rejection of work-1 named", stderr.String())
	}
	if len(f.failed) != 0 || strings.Contains(stdout.String(), "claims_errored") {
		t.Fatalf("claim failures = %v stdout = %s, want no claims_errored", f.failed, stdout.String())
	}
	if len(f.claims) != 0 {
		t.Fatalf("--claim ran for graph steps %v, want none", f.claims)
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode result %q: %v", stdout.String(), err)
	}
	if result.Action != "work" || result.BeadID != "work-2" {
		t.Fatalf("result = %+v, want work on work-2", result)
	}
	if got := f.get("work-2"); got.Status != "in_progress" || got.Assignee != "gc__role-mc-sess1" {
		t.Fatalf("work-2 = status %q assignee %q, want in_progress owned by this session", got.Status, got.Assignee)
	}
}

// TestHookClaimExactFencesObservedAssignee: a graph step observed open and
// assigned to this session (ready tier), or open and unassigned (eligible
// tier), went to another worker before the claim ran. Nothing is written and
// the other worker keeps the bead. A stale lease this invocation reclaimed is
// claimed from the open, unassigned row the reclaim left.
func TestHookClaimExactFencesObservedAssignee(t *testing.T) {
	const self, other = "gc__role-mc-sess1", "other-worker"
	for _, tc := range []struct {
		name             string
		observedAssignee string
	}{
		{name: "ready tier", observedAssignee: self},
		{name: "eligible tier", observedAssignee: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newExactClaimFixture(t)
			f.add(
				beads.Bead{ID: "work-1", Title: "taken", Status: "open", Assignee: other, Metadata: graphStepMeta("build")},
				beads.Bead{ID: "work-1", Status: "open", Assignee: tc.observedAssignee, Metadata: graphStepMeta("build")},
			)

			var stdout, stderr bytes.Buffer
			doHookClaim("bd ready --json", "/tmp/work", poolClaimOpts(), f.ops(), &stdout, &stderr)

			if len(f.claims) != 0 {
				t.Fatalf("--claim ran for graph steps %v, want none", f.claims)
			}
			if strings.Contains(stdout.String(), `"action":"work"`) {
				t.Fatalf("handed out work: %s", stdout.String())
			}
			got := f.get("work-1")
			if got.Status != "open" || got.Assignee != other {
				t.Fatalf("work-1 = status %q assignee %q, want open and still %s's", got.Status, got.Assignee, other)
			}
			if v, ok := got.Metadata[beadmeta.ClaimGenerationMetadataKey]; ok {
				t.Fatalf("work-1 carries %s=%q, want it unwritten", beadmeta.ClaimGenerationMetadataKey, v)
			}
		})
	}

	t.Run("reclaimed lease", func(t *testing.T) {
		f := newExactClaimFixture(t)
		f.add(
			beads.Bead{ID: "work-1", Title: "stale", Status: "in_progress", Assignee: "stale-worker", Metadata: graphStepMeta("build")},
			beads.Bead{ID: "work-1", Status: "in_progress", Assignee: "stale-worker", Metadata: graphStepMeta("build")},
		)
		ops := f.ops()
		ops.ReclaimStale = func(_ context.Context, _ string, _ []string, id string) (bool, string, error) {
			open, none := "open", ""
			return true, "stale-worker", f.store.Update(id, beads.UpdateOpts{Status: &open, Assignee: &none})
		}
		ops.EmitHookClaimReclaimedStale = func(string, string, string) {}
		opts := poolClaimOpts()
		opts.AutoReclaimStaleClaims = true

		var stdout, stderr bytes.Buffer
		if code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr); code != 0 {
			t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
		}
		if got := f.get("work-1"); got.Status != "in_progress" || got.Assignee != self {
			t.Fatalf("work-1 = status %q assignee %q, want in_progress owned by this session; stderr=%s", got.Status, got.Assignee, stderr.String())
		}
	})
}
