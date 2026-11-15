package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// hookWorkflowStopFixture drives `gc hook --claim` against a MemStore that
// holds the candidates AND their workflow ancestry, so the claim tiers, the
// claim CAS and the stop gate all see one ledger. served lists the ids the work
// query returns, in order; closed rows drop out of it like they would from bd
// ready.
type hookWorkflowStopFixture struct {
	t      *testing.T
	store  *beads.MemStore
	served []string
	claims []string
	// afterClaim runs inside the claim CAS, after it commits: the window in
	// which a workflow root can settle under a claim already in flight.
	afterClaim func(id string)
}

func newHookWorkflowStopFixture(t *testing.T) *hookWorkflowStopFixture {
	t.Helper()
	return &hookWorkflowStopFixture{t: t, store: beads.NewMemStore()}
}

func (f *hookWorkflowStopFixture) create(b beads.Bead) beads.Bead {
	f.t.Helper()
	created, err := f.store.Create(b)
	if err != nil {
		f.t.Fatalf("create %q: %v", b.Title, err)
	}
	return created
}

// root creates a graph.v2 workflow root, linked to a drain control when
// drainControlID is set (the shape a drain item workflow root carries).
func (f *hookWorkflowStopFixture) root(title, drainControlID string) beads.Bead {
	meta := map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"}
	if drainControlID != "" {
		meta["gc.drain_control_id"] = drainControlID
	}
	return f.create(beads.Bead{Title: title, Type: "molecule", Metadata: meta})
}

// step creates a routed workflow member of rootID.
func (f *hookWorkflowStopFixture) step(title, rootID string, extra map[string]string) beads.Bead {
	meta := map[string]string{"gc.root_bead_id": rootID, "gc.routed_to": "worker"}
	for k, v := range extra {
		meta[k] = v
	}
	return f.create(beads.Bead{Title: title, Type: "task", Metadata: meta})
}

func (f *hookWorkflowStopFixture) closeAs(id, outcome string, extra map[string]string) {
	f.t.Helper()
	meta := map[string]string{"gc.outcome": outcome}
	for k, v := range extra {
		meta[k] = v
	}
	if _, err := f.store.CloseAll([]string{id}, meta); err != nil {
		f.t.Fatalf("close %s: %v", id, err)
	}
}

func (f *hookWorkflowStopFixture) get(id string) beads.Bead {
	f.t.Helper()
	b, err := f.store.Get(id)
	if err != nil {
		f.t.Fatalf("get %s: %v", id, err)
	}
	return b
}

func (f *hookWorkflowStopFixture) ops() hookClaimOps {
	return hookClaimOps{
		Runner: func(string, string) (string, error) {
			rows := []beads.Bead{}
			for _, id := range f.served {
				b, err := f.store.Get(id)
				if err != nil {
					return "", err
				}
				if b.Status == "closed" {
					continue
				}
				rows = append(rows, b)
			}
			out, err := json.Marshal(rows)
			return string(out), err
		},
		Claim: func(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, bool, error) {
			f.claims = append(f.claims, id)
			current, err := f.store.Get(id)
			if err != nil {
				return beads.Bead{}, false, err
			}
			if current.Status == "closed" || (current.Assignee != "" && current.Assignee != assignee) {
				return current, false, nil
			}
			status := "in_progress"
			if err := f.store.Update(id, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
				return beads.Bead{}, false, err
			}
			if f.afterClaim != nil {
				f.afterClaim(id)
			}
			claimed, err := f.store.Get(id)
			return claimed, true, err
		},
		Release: func(_ context.Context, _ string, _ []string, id, assignee string) (bool, error) {
			current, err := f.store.Get(id)
			if err != nil || current.Status != "in_progress" || current.Assignee != assignee {
				return false, err
			}
			status, none := "open", ""
			return true, f.store.Update(id, beads.UpdateOpts{Status: &status, Assignee: &none})
		},
		ReadWorkMeta: func(_ context.Context, _ string, _ []string, id, _ string) (beads.Bead, error) {
			return f.store.Get(id)
		},
		ListContinuation: func(_ context.Context, _ string, _ []string, rootID, group string) ([]beads.Bead, error) {
			return f.store.List(beads.ListQuery{
				Status:   "open",
				Metadata: map[string]string{"gc.root_bead_id": rootID, "gc.continuation_group": group},
			})
		},
		AssignContinuation: func(_ context.Context, _ string, _ []string, id, assignee string) error {
			return f.store.Update(id, beads.UpdateOpts{Assignee: &assignee})
		},
		WorkflowStore: func(context.Context, string, []string, string, string) beads.Store {
			return f.store
		},
		StampWorkMeta:            func(context.Context, string, []string, string, string, map[string]string) error { return nil },
		StampSessionClaim:        func(string, string) error { return nil },
		PublishRunMap:            func(string, string, ...string) error { return nil },
		EmitExecutionStepStarted: func(beads.Bead, string, []string, string) {},
		EmitClaimRejected:        func(string, string, string) {},
		EnqueueContinuationNudge: func(string, string, io.Writer) {},
		ResolveWorkBranch:        func(hookClaimWorkTree) string { return "" },
		ResolveSessionWorkDir:    func(string) string { return "" },
		DrainAck:                 func(io.Writer) error { return nil },
	}
}

// run invokes one `gc hook --claim` and returns the bead it handed out (empty
// when it handed out nothing).
func (f *hookWorkflowStopFixture) run() (string, string) {
	f.t.Helper()
	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", f.t.TempDir(), hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		DrainAck:           true,
		JSON:               true,
	}, f.ops(), &stdout, &stderr)
	if code != 0 {
		f.t.Fatalf("doHookClaim = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		var result hookClaimJSONResult
		if err := json.Unmarshal([]byte(line), &result); err != nil {
			f.t.Fatalf("decode claim result %q: %v", line, err)
		}
		if result.Action == "work" {
			return result.BeadID, stderr.String()
		}
	}
	return "", stderr.String()
}

func (f *hookWorkflowStopFixture) wantClosed(id, outcome string) {
	f.t.Helper()
	got := f.get(id)
	if got.Status != "closed" || got.Metadata["gc.outcome"] != outcome {
		f.t.Fatalf("%s = status %q outcome %q, want closed %s so no later poll offers it again", id, got.Status, got.Metadata["gc.outcome"], outcome)
	}
}

func (f *hookWorkflowStopFixture) wantNeverClaimed(id string) {
	f.t.Helper()
	for _, claimed := range f.claims {
		if claimed == id {
			f.t.Fatalf("claim CAS ran on %s (claims=%v); a step under a stopped workflow must not be claimed", id, f.claims)
		}
	}
}

// TestHookClaimSkipsStepUnderFailedRoot is the first half of the live failure:
// a do-work root closed gc.outcome=fail and a worker still claimed one of its
// open steps minutes later, because nothing on the claim path read the root.
// The step must not be claimed through any tier, must be closed skipped (the
// finalizer sweep's own residue outcome) so it stops being offered, and the
// hook must move on to live work behind it.
func TestHookClaimSkipsStepUnderFailedRoot(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   string
		assignee string
	}{
		{name: "routed", status: "open"},
		{name: "ready assignment", status: "open", assignee: "worker-1"},
		{name: "existing assignment", status: "in_progress", assignee: "worker-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHookWorkflowStopFixture(t)
			failed := f.root("do-work", "")
			stranded := f.step("implement", failed.ID, nil)
			if tc.status != "open" || tc.assignee != "" {
				status, assignee := tc.status, tc.assignee
				if err := f.store.Update(stranded.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
					t.Fatal(err)
				}
			}
			f.closeAs(failed.ID, "fail", nil)
			live := f.root("other run", "")
			next := f.step("next", live.ID, nil)
			f.served = []string{stranded.ID, next.ID}

			got, stderr := f.run()

			if got != next.ID {
				t.Fatalf("handed out %q, want the live step %s; stderr=%s", got, next.ID, stderr)
			}
			f.wantNeverClaimed(stranded.ID)
			f.wantClosed(stranded.ID, "skipped")
		})
	}
}

// TestHookClaimSkipsStepUnderCanceledParentRun is the second half: the parent
// implement run was canceled, but the nested do-work item root it drained into
// stayed open, so its steps carried a live gc.root_bead_id and were claimed six
// minutes after the cancel. The gate must follow the item root's
// gc.drain_control_id up to the run and close the step as canceled.
func TestHookClaimSkipsStepUnderCanceledParentRun(t *testing.T) {
	f := newHookWorkflowStopFixture(t)
	run := f.root("implement", "")
	drain := f.create(beads.Bead{Title: "drain", Type: "task", Metadata: map[string]string{
		"gc.root_bead_id": run.ID,
		"gc.kind":         "drain",
	}})
	item := f.root("do-work", drain.ID)
	stranded := f.step("implement", item.ID, nil)
	f.closeAs(run.ID, "canceled", map[string]string{"gc.cancel_requested": "true"})
	f.served = []string{stranded.ID}

	got, stderr := f.run()

	if got != "" {
		t.Fatalf("handed out %q, want nothing; stderr=%s", got, stderr)
	}
	f.wantNeverClaimed(stranded.ID)
	f.wantClosed(stranded.ID, "canceled")
}

// TestHookClaimSkipsStepsUnderRunMarkedCanceling covers the window a run
// cancel opens with its first write: the run root is still open and carries
// only gc.cancel_requested, while the cancel interrupts the run's sessions and
// closes its beads. A worker polling in that window, an interrupted one
// included, must not claim the run's open steps or those of a drain item
// workflow whose own root is still open.
func TestHookClaimSkipsStepsUnderRunMarkedCanceling(t *testing.T) {
	f := newHookWorkflowStopFixture(t)
	run := f.root("implement", "")
	runStep := f.step("review", run.ID, nil)
	drain := f.create(beads.Bead{Title: "drain", Type: "task", Metadata: map[string]string{
		"gc.root_bead_id": run.ID,
		"gc.kind":         "drain",
	}})
	item := f.root("do-work", drain.ID)
	itemStep := f.step("implement", item.ID, nil)
	if err := f.store.SetMetadata(run.ID, "gc.cancel_requested", "true"); err != nil {
		t.Fatal(err)
	}
	f.served = []string{runStep.ID, itemStep.ID}

	got, stderr := f.run()

	if got != "" {
		t.Fatalf("handed out %q, want nothing; stderr=%s", got, stderr)
	}
	for _, id := range []string{runStep.ID, itemStep.ID} {
		f.wantNeverClaimed(id)
		f.wantClosed(id, "canceled")
	}
	if root := f.get(run.ID); root.Status == "closed" {
		t.Fatalf("run root closed by the claim gate; closing it is the cancel's job")
	}
}

// TestHookClaimStillServesTeardownTailUnderFailedRoot pins the exemption: the
// teardown tail runs after its root settles by contract (it may branch on
// ROOT_OUTCOME and releases the workflow's resources), so a settled root must
// not stop it. Retry expansion strips gc.scope_role from the first attempt, so
// an attempt linked to a teardown step only by gc.step_id is exempt too.
func TestHookClaimStillServesTeardownTailUnderFailedRoot(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta map[string]string
	}{
		{name: "teardown step", meta: map[string]string{"gc.scope_role": "teardown", "gc.step_id": "cleanup"}},
		{name: "teardown attempt", meta: map[string]string{"gc.step_id": "cleanup", "gc.attempt": "1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHookWorkflowStopFixture(t)
			failed := f.root("do-work", "")
			f.step("cleanup", failed.ID, map[string]string{"gc.scope_role": "teardown", "gc.step_id": "cleanup", "gc.routed_to": "nobody"})
			teardown := f.step("cleanup run", failed.ID, tc.meta)
			f.closeAs(failed.ID, "fail", nil)
			f.served = []string{teardown.ID}

			got, stderr := f.run()

			if got != teardown.ID {
				t.Fatalf("handed out %q, want teardown %s; stderr=%s", got, teardown.ID, stderr)
			}
		})
	}
}

// TestHookClaimUnderLiveWorkflowIsUnchanged: a live chain (open step, open item
// root, open run) claims exactly as before and closes nothing.
func TestHookClaimUnderLiveWorkflowIsUnchanged(t *testing.T) {
	f := newHookWorkflowStopFixture(t)
	run := f.root("implement", "")
	drain := f.create(beads.Bead{Title: "drain", Type: "task", Metadata: map[string]string{
		"gc.root_bead_id": run.ID,
		"gc.kind":         "drain",
	}})
	item := f.root("do-work", drain.ID)
	step := f.step("implement", item.ID, nil)
	f.served = []string{step.ID}

	got, stderr := f.run()

	if got != step.ID {
		t.Fatalf("handed out %q, want %s; stderr=%s", got, step.ID, stderr)
	}
	if claimed := f.get(step.ID); claimed.Status != "in_progress" || claimed.Assignee != "worker-1" {
		t.Fatalf("step = status %q assignee %q, want in_progress worker-1", claimed.Status, claimed.Assignee)
	}
	for _, id := range []string{run.ID, drain.ID, item.ID} {
		if b := f.get(id); b.Status == "closed" {
			t.Fatalf("%s closed by a live claim", id)
		}
	}
}

// TestHookClaimRechecksWorkflowAfterClaim covers the race window: the root
// settles while the claim CAS is in flight. The claim this invocation just won
// must not be handed to the worker; it is closed as the stop requires.
func TestHookClaimRechecksWorkflowAfterClaim(t *testing.T) {
	f := newHookWorkflowStopFixture(t)
	root := f.root("do-work", "")
	step := f.step("implement", root.ID, nil)
	f.served = []string{step.ID}
	f.afterClaim = func(string) { f.closeAs(root.ID, "fail", nil) }

	got, stderr := f.run()

	if got != "" {
		t.Fatalf("handed out %q, want nothing once the root settled under the claim; stderr=%s", got, stderr)
	}
	f.wantClosed(step.ID, "skipped")
}

// TestHookClaimDoesNotPreassignSiblingsUnderStoppedWorkflow: a teardown step
// is still served under a failed root, but the continuation siblings it shares
// a group with are ordinary work under that same stopped root and must not be
// pinned to the session.
func TestHookClaimDoesNotPreassignSiblingsUnderStoppedWorkflow(t *testing.T) {
	f := newHookWorkflowStopFixture(t)
	failed := f.root("do-work", "")
	teardown := f.step("cleanup", failed.ID, map[string]string{
		"gc.scope_role":         "teardown",
		"gc.step_id":            "cleanup",
		"gc.continuation_group": "lane",
	})
	sibling := f.step("implement", failed.ID, map[string]string{"gc.continuation_group": "lane"})
	f.closeAs(failed.ID, "fail", nil)
	f.served = []string{teardown.ID}

	got, stderr := f.run()

	if got != teardown.ID {
		t.Fatalf("handed out %q, want teardown %s; stderr=%s", got, teardown.ID, stderr)
	}
	if pinned := f.get(sibling.ID); pinned.Assignee != "" {
		t.Fatalf("sibling %s pinned to %q under a failed root", sibling.ID, pinned.Assignee)
	}
}

// slowWorkflowStore charges every ancestry read the stop gate makes against the
// invocation clock, the way a bd store at ~1s per call does.
type slowWorkflowStore struct {
	beads.Store
	charge func()
}

func (s slowWorkflowStore) Get(id string) (beads.Bead, error) {
	s.charge()
	return s.Store.Get(id)
}

// TestHookClaimWorkflowRecheckDoesNotSpendTheLandingWindow pins what the
// straddle fence measures: when the claim CAS returned, not how long the reads
// after it took. A CAS that lands inside the claim window plus its reserved
// mutation budget is a kept claim even when the post-CAS workflow re-check, on a
// slow store, carries the invocation past that line; one that lands past it is
// still unwound.
func TestHookClaimWorkflowRecheckDoesNotSpendTheLandingWindow(t *testing.T) {
	const window = time.Minute
	for _, tier := range []struct {
		name     string
		assignee string
	}{
		{name: "routed"},
		{name: "ready assignment", assignee: "worker-1"},
	} {
		for _, tc := range []struct {
			name    string
			landing time.Duration // invocation age at which the claim CAS returns
			kept    bool
		}{
			{name: "landed inside reserve", landing: window + hookClaimMutationTimeout - time.Second, kept: true},
			{name: "landed past reserve", landing: window + hookClaimMutationTimeout + time.Second},
		} {
			t.Run(tier.name+"/"+tc.name, func(t *testing.T) {
				f := newHookWorkflowStopFixture(t)
				// A live nested chain, so the re-check walks several ancestry reads.
				run := f.root("implement", "")
				drain := f.create(beads.Bead{Title: "drain", Type: "task", Metadata: map[string]string{
					"gc.root_bead_id": run.ID,
					"gc.kind":         "drain",
				}})
				item := f.root("do-work", drain.ID)
				step := f.step("implement", item.ID, nil)
				if tier.assignee != "" {
					assignee := tier.assignee
					if err := f.store.Update(step.ID, beads.UpdateOpts{Assignee: &assignee}); err != nil {
						t.Fatal(err)
					}
				}
				f.served = []string{step.ID}

				invokedAt := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
				clock := invokedAt
				landed := false
				ops := f.ops()
				ops.Now = func() time.Time { return clock }
				ops.InvokedAt = invokedAt
				ops.ClaimWindow = window
				baseClaim := ops.Claim
				ops.Claim = func(ctx context.Context, dir string, env []string, id, assignee string) (beads.Bead, bool, error) {
					claimed, ok, err := baseClaim(ctx, dir, env, id, assignee)
					clock = invokedAt.Add(tc.landing)
					landed = true
					return claimed, ok, err
				}
				ops.WorkflowStore = func(context.Context, string, []string, string, string) beads.Store {
					return slowWorkflowStore{Store: f.store, charge: func() {
						if landed {
							clock = clock.Add(5 * time.Second)
						}
					}}
				}

				var stdout, stderr bytes.Buffer
				code := doHookClaim("query", t.TempDir(), hookClaimOptions{
					Assignee:           "worker-1",
					IdentityCandidates: []string{"worker-1"},
					RouteTargets:       []string{"worker"},
					JSON:               true,
				}, ops, &stdout, &stderr)

				got := f.get(step.ID)
				if tc.kept {
					if age := clock.Sub(invokedAt); age <= window+hookClaimMutationTimeout {
						t.Fatalf("invocation age after the re-check = %s, want it carried past window + reserve", age)
					}
					if code != 0 || !strings.Contains(stdout.String(), step.ID) || strings.Contains(stderr.String(), "claim window") {
						t.Fatalf("claim = %d, stdout=%q, want the claim that landed inside window + reserve delivered; stderr=%s", code, stdout.String(), stderr.String())
					}
					if got.Status != "in_progress" || got.Assignee != "worker-1" {
						t.Fatalf("step = status %q assignee %q, want the kept claim in_progress worker-1", got.Status, got.Assignee)
					}
					return
				}
				if code != 1 || !strings.Contains(stderr.String(), "claim window") {
					t.Fatalf("claim = %d, stderr=%s, want the straddle unwind", code, stderr.String())
				}
				if got.Status != "open" || got.Assignee != "" {
					t.Fatalf("step = status %q assignee %q, want the straddling claim released", got.Status, got.Assignee)
				}
			})
		}
	}
}
