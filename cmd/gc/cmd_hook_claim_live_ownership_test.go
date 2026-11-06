package main

// The live-ownership fence of ga-35b8a4. `gc hook --claim` handed a graph.v2
// workflow root out WHOLE while two other sessions still held its steps
// in_progress: the root row was legitimately open and unassigned, and every
// ancestry gate the claim path had walks UP from a candidate, so nothing ever
// looked DOWN at the root's members. The seat that took the root ran the whole
// workflow beside the seats already running its steps.
//
// The fence added to the eligible tier is one read: before claiming a
// standalone, not-yet-expanded workflow root, list its member steps, and
// withhold the root — untouched, named on stderr, reported as a claim failure —
// while any in_progress member is held by another session. The tiers that
// resume a session's OWN work (ready assignment, existing assignment,
// adoption) are unchanged, and out-of-scope candidates never pay the read.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// hookClaimFailureNote is one bead.claim_failed emission a fixture captured:
// the withheld bead and the error text the claim-loop guard would compare.
type hookClaimFailureNote struct {
	beadID string
	err    error
}

// hookLiveOwnershipFixture drives the full `gc hook --claim` flow over one
// MemStore holding the served candidates, the roots under test and the member
// steps that hold them, so the tiers, the claim CAS and the foreign-workflow
// hold all see one ledger. served lists the ids the work query returns, in
// order; closed rows drop out of it as they would from bd ready.
type hookLiveOwnershipFixture struct {
	t        *testing.T
	store    *beads.MemStore
	served   []string
	claims   []string
	lists    int
	failures []hookClaimFailureNote
}

func newHookLiveOwnershipFixture(t *testing.T) *hookLiveOwnershipFixture {
	t.Helper()
	return &hookLiveOwnershipFixture{t: t, store: beads.NewMemStore()}
}

func (f *hookLiveOwnershipFixture) create(b beads.Bead) beads.Bead {
	f.t.Helper()
	created, err := f.store.Create(b)
	if err != nil {
		f.t.Fatalf("create %q: %v", b.Title, err)
	}
	return created
}

// root creates the claimable workflow root of the incident: open, unassigned,
// routed to this session's lane, not yet expanded — the row the eligible tier
// handed out whole while other sessions still held its steps.
func (f *hookLiveOwnershipFixture) root(title string, extra map[string]string) beads.Bead {
	f.t.Helper()
	meta := map[string]string{
		beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
		beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		beadmeta.RoutedToMetadataKey:        "worker",
	}
	for k, v := range extra {
		meta[k] = v
	}
	return f.create(beads.Bead{Title: title, Type: "molecule", Metadata: meta})
}

// step creates a member step of rootID carrying the in-flight state the fence
// reads: its status, and its holder recorded the way a step's execution
// stamps record it — an assignee, or the session provenance of the seat that
// started it.
func (f *hookLiveOwnershipFixture) step(rootID, status, assignee string, provenance map[string]string) beads.Bead {
	f.t.Helper()
	meta := map[string]string{
		beadmeta.RootBeadIDMetadataKey: rootID,
		beadmeta.RoutedToMetadataKey:   "worker",
	}
	for k, v := range provenance {
		meta[k] = v
	}
	created := f.create(beads.Bead{Title: "member step", Type: "task", Assignee: assignee, Metadata: meta})
	if status != "" && status != "open" {
		if err := f.store.Update(created.ID, beads.UpdateOpts{Status: &status}); err != nil {
			f.t.Fatalf("step %s to %s: %v", created.ID, status, err)
		}
	}
	return created
}

// task creates the plain routed work behind a held root: what a seat handed
// the root instead must run.
func (f *hookLiveOwnershipFixture) task(title string) beads.Bead {
	f.t.Helper()
	return f.create(beads.Bead{Title: title, Type: "task", Metadata: map[string]string{
		beadmeta.RoutedToMetadataKey: "worker",
	}})
}

func (f *hookLiveOwnershipFixture) get(id string) beads.Bead {
	f.t.Helper()
	b, err := f.store.Get(id)
	if err != nil {
		f.t.Fatalf("get %s: %v", id, err)
	}
	return b
}

func (f *hookLiveOwnershipFixture) ops() hookClaimOps {
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
				Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: rootID, "gc.continuation_group": group},
			})
		},
		AssignContinuation: func(_ context.Context, _ string, _ []string, id, assignee string) error {
			return f.store.Update(id, beads.UpdateOpts{Assignee: &assignee})
		},
		WorkflowStore: func(context.Context, string, []string, string, string) beads.Store {
			return f.store
		},
		ListWorkflowMembers: func(_ context.Context, _ string, _ []string, rootID string) ([]beads.Bead, error) {
			f.lists++
			return f.store.List(beads.ListQuery{
				Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: rootID},
				TierMode: beads.TierBoth,
			})
		},
		DrainPending: func(string) (bool, error) { return false, nil },
		ReadWorkStarted: func(string) (string, error) {
			return "", nil // this seat holds no started claim: the drain may be acknowledged
		},
		StoreRef: func(string) string { return "" }, // the tier driven directly skips applyDefaults
		EmitClaimFailed: func(beadID string, _ hookClaimOptions, err error) {
			f.failures = append(f.failures, hookClaimFailureNote{beadID: beadID, err: err})
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

// hookClaimOutcome is the parsed JSON result line of one hook invocation.
type hookClaimOutcome struct {
	action string
	reason string
	beadID string
	stderr string
}

// runAs invokes one `gc hook --claim` as assignee (a fresh seat per call when
// the caller varies it) and returns the last JSON result line: a work claim or
// a drain, both terminal.
func (f *hookLiveOwnershipFixture) runAs(assignee string, env ...string) hookClaimOutcome {
	f.t.Helper()
	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", f.t.TempDir(), hookClaimOptions{
		Assignee:           assignee,
		IdentityCandidates: []string{assignee},
		RouteTargets:       []string{"worker"},
		Env:                env,
		DrainAck:           true,
		JSON:               true,
	}, f.ops(), &stdout, &stderr)
	if code != 0 {
		f.t.Fatalf("doHookClaim = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var out hookClaimOutcome
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		var result hookClaimJSONResult
		if err := json.Unmarshal([]byte(line), &result); err != nil {
			f.t.Fatalf("decode claim result %q: %v", line, err)
		}
		if result.Action != "" {
			out.action, out.reason, out.beadID = result.Action, result.Reason, result.BeadID
		}
	}
	out.stderr = stderr.String()
	return out
}

func (f *hookLiveOwnershipFixture) run() hookClaimOutcome {
	f.t.Helper()
	return f.runAs("worker-1")
}

func (f *hookLiveOwnershipFixture) wantNeverClaimed(id string) {
	f.t.Helper()
	for _, claimed := range f.claims {
		if claimed == id {
			f.t.Fatalf("claim CAS ran on %s (claims=%v); a root held by other sessions must not be claimed whole", id, f.claims)
		}
	}
}

// wantUnchanged asserts a withheld row is exactly as it was served: still
// open, unassigned, unsettled. A withheld root is neither closed nor stamped —
// closing is the stop gate's residue business, and this root is not stopped,
// only spoken for.
func (f *hookLiveOwnershipFixture) wantUnchanged(id string) {
	f.t.Helper()
	got := f.get(id)
	if got.Status != "open" || got.Assignee != "" || got.Metadata[beadmeta.OutcomeMetadataKey] != "" {
		f.t.Fatalf("%s = status %q assignee %q outcome %q, want the served row untouched", id, got.Status, got.Assignee, got.Metadata[beadmeta.OutcomeMetadataKey])
	}
}

// TestHookClaimLiveOwnershipWithholdsAndMovesOn is the incident itself: an
// open, unassigned, route-matched workflow root is withheld untouched while
// two other sessions hold its steps in_progress, the withhold is named on
// stderr with the members that caused it, and the hook moves on to the live
// work behind the root.
func TestHookClaimLiveOwnershipWithholdsAndMovesOn(t *testing.T) {
	f := newHookLiveOwnershipFixture(t)
	root := f.root("do-work run", nil)
	held1 := f.step(root.ID, "in_progress", "seat-x", nil)
	held2 := f.step(root.ID, "in_progress", "seat-y", nil)
	live := f.task("live work")
	f.served = []string{root.ID, live.ID}

	got := f.run()

	if got.action != "work" || got.beadID != live.ID {
		t.Fatalf("handed out action=%q bead %q, want the live task %s; stderr=%s", got.action, got.beadID, live.ID, got.stderr)
	}
	f.wantNeverClaimed(root.ID)
	f.wantUnchanged(root.ID)
	if !strings.Contains(got.stderr, "withholding workflow root "+root.ID) {
		t.Fatalf("stderr=%q, want the withhold named for %s", got.stderr, root.ID)
	}
	for _, id := range []string{held1.ID, held2.ID} {
		if !strings.Contains(got.stderr, id) {
			t.Fatalf("stderr=%q, want the withheld member %s named", got.stderr, id)
		}
	}
	if len(f.failures) != 1 || f.failures[0].beadID != root.ID {
		t.Fatalf("claim failures = %+v, want exactly one for the withheld root %s", f.failures, root.ID)
	}
}
