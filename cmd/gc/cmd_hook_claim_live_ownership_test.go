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
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
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

// TestHookClaimLiveOwnershipIdentityAndRecovery pins the holder rule — which
// in_progress member counts as another session's, and which exemptions keep a
// root claimable — and that the fence lifts the moment the member's state
// changes.
func TestHookClaimLiveOwnershipIdentityAndRecovery(t *testing.T) {
	for _, tc := range []struct {
		name         string
		assignee     string
		provenance   map[string]string
		wantWithhold bool
	}{
		// An assignee is the authoritative holder; provenance does not
		// overrule it.
		{name: "foreign assignee", assignee: "seat-x", wantWithhold: true},
		{name: "foreign assignee beats own provenance", assignee: "seat-x", provenance: map[string]string{beadmeta.SessionNameMetadataKey: "worker-1"}, wantWithhold: true},
		{name: "own assignee"},
		// Provenance is a set: any alias resolving to this session exempts,
		// the way a renamed session's stale name sits beside its durable id.
		{name: "own provenance name", provenance: map[string]string{beadmeta.SessionNameMetadataKey: "worker-1"}},
		{name: "own provenance id", provenance: map[string]string{beadmeta.SessionIDMetadataKey: "worker-1"}},
		{name: "own id exempts a mixed set", provenance: map[string]string{
			beadmeta.SessionNameMetadataKey: "seat-x",
			beadmeta.SessionIDMetadataKey:   "worker-1",
		}},
		// Nonempty provenance matching nobody withholds: the seat that died
		// mid-step left its stamps behind, and the row is its work to finish
		// or release, not a fresh root for another seat to run whole.
		{name: "dead-seat provenance", provenance: map[string]string{beadmeta.SessionNameMetadataKey: "seat-dead"}, wantWithhold: true},
		{name: "foreign provenance id", provenance: map[string]string{beadmeta.SessionIDMetadataKey: "seat-x"}, wantWithhold: true},
		// Nothing evidences foreign ownership: witholding here would strand
		// every memberless root the fallback exists to serve.
		{name: "no holder fields"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHookLiveOwnershipFixture(t)
			root := f.root("run", nil)
			f.step(root.ID, "in_progress", tc.assignee, tc.provenance)
			live := f.task("live work")
			f.served = []string{root.ID, live.ID}

			got := f.run()

			if tc.wantWithhold {
				if got.action != "work" || got.beadID != live.ID {
					t.Fatalf("handed out action=%q bead %q, want the live task %s behind the held root; stderr=%s", got.action, got.beadID, live.ID, got.stderr)
				}
				f.wantNeverClaimed(root.ID)
				f.wantUnchanged(root.ID)
				return
			}
			if got.action != "work" || got.beadID != root.ID {
				t.Fatalf("handed out action=%q bead %q, want the root %s itself; stderr=%s", got.action, got.beadID, root.ID, got.stderr)
			}
		})
	}

	t.Run("open and closed siblings never withhold", func(t *testing.T) {
		f := newHookLiveOwnershipFixture(t)
		root := f.root("run", map[string]string{"gc.note": "siblings"})
		f.step(root.ID, "open", "", nil)
		done := f.step(root.ID, "", "seat-x", nil)
		if err := f.store.Close(done.ID); err != nil {
			t.Fatal(err)
		}
		f.served = []string{root.ID}

		if got := f.run(); got.action != "work" || got.beadID != root.ID {
			t.Fatalf("handed out action=%q bead %q, want the root %s; only in_progress members hold; stderr=%s", got.action, got.beadID, root.ID, got.stderr)
		}
	})

	t.Run("mixed holders withhold", func(t *testing.T) {
		f := newHookLiveOwnershipFixture(t)
		root := f.root("run", nil)
		f.step(root.ID, "in_progress", "worker-1", nil)
		f.step(root.ID, "in_progress", "seat-x", nil)
		live := f.task("standby work")
		f.served = []string{root.ID, live.ID}

		if got := f.run(); got.beadID != live.ID {
			t.Fatalf("handed out %q, want %s; one foreign in_progress member holds the whole root; stderr=%s", got.beadID, live.ID, got.stderr)
		}
		f.wantNeverClaimed(root.ID)
	})

	t.Run("a memberless root stays claimable", func(t *testing.T) {
		f := newHookLiveOwnershipFixture(t)
		root := f.root("run", nil)
		f.served = []string{root.ID}

		if got := f.run(); got.action != "work" || got.beadID != root.ID {
			t.Fatalf("handed out action=%q bead %q, want the memberless root %s; stderr=%s", got.action, got.beadID, root.ID, got.stderr)
		}
	})

	// A root naming ITSELF as its root (the self-link some tools stamp) is
	// not "linked" for the stop gate and must not be its own holder either.
	t.Run("a self-link does not wedge or hold the root", func(t *testing.T) {
		f := newHookLiveOwnershipFixture(t)
		root := f.root("run", nil)
		if err := f.store.SetMetadata(root.ID, beadmeta.RootBeadIDMetadataKey, root.ID); err != nil {
			t.Fatal(err)
		}
		f.served = []string{root.ID}

		if got := f.run(); got.action != "work" || got.beadID != root.ID {
			t.Fatalf("handed out action=%q bead %q, want the self-linked root %s; stderr=%s", got.action, got.beadID, root.ID, got.stderr)
		}
	})

	t.Run("member status folds case and space", func(t *testing.T) {
		f := newHookLiveOwnershipFixture(t)
		root := f.root("run", nil)
		f.step(root.ID, " In_Progress ", "seat-x", nil)
		live := f.task("live work")
		f.served = []string{root.ID, live.ID}

		if got := f.run(); got.beadID != live.ID {
			t.Fatalf("handed out %q, want %s; a padded status is still in_progress; stderr=%s", got.beadID, live.ID, got.stderr)
		}
		f.wantNeverClaimed(root.ID)
	})

	t.Run("the fence lifts when the holder closes", func(t *testing.T) {
		f := newHookLiveOwnershipFixture(t)
		root := f.root("run", nil)
		holder := f.step(root.ID, "in_progress", "seat-x", nil)
		live := f.task("live work")
		f.served = []string{root.ID, live.ID}

		if first := f.run(); first.beadID != live.ID {
			t.Fatalf("first poll handed out %q, want %s; stderr=%s", first.beadID, live.ID, first.stderr)
		}
		if err := f.store.Close(holder.ID); err != nil {
			t.Fatal(err)
		}
		// A fresh seat: the first one still holds the live task it was given,
		// and its own adoption tier would hand that back before the root.
		second := f.runAs("worker-2")
		if second.action != "work" || second.beadID != root.ID {
			t.Fatalf("second poll handed out action=%q bead %q, want the root %s once every holder settled; stderr=%s", second.action, second.beadID, root.ID, second.stderr)
		}
	})

	t.Run("the fence lifts when the holder releases", func(t *testing.T) {
		f := newHookLiveOwnershipFixture(t)
		root := f.root("run", nil)
		holder := f.step(root.ID, "in_progress", "seat-x", nil)
		live := f.task("live work")
		f.served = []string{root.ID, live.ID}

		if first := f.run(); first.beadID != live.ID {
			t.Fatalf("first poll handed out %q, want %s; stderr=%s", first.beadID, live.ID, first.stderr)
		}
		status, none := "open", ""
		if err := f.store.Update(holder.ID, beads.UpdateOpts{Status: &status, Assignee: &none}); err != nil {
			t.Fatal(err)
		}
		// A fresh seat: the first one still holds the live task it was given,
		// and its own adoption tier would hand that back before the root.
		second := f.runAs("worker-2")
		if second.action != "work" || second.beadID != root.ID {
			t.Fatalf("second poll handed out action=%q bead %q, want the root %s once the holder released; stderr=%s", second.action, second.beadID, root.ID, second.stderr)
		}
	})
}

// TestHookClaimLiveOwnershipScopeAndReads pins the fence's scope and cost:
// only a standalone, not-yet-expanded workflow root pays the member listing —
// exactly one — and every other candidate shape, including the tiers that
// resume a session's own work, pays nothing.
func TestHookClaimLiveOwnershipScopeAndReads(t *testing.T) {
	t.Run("a plain task performs no member read", func(t *testing.T) {
		f := newHookLiveOwnershipFixture(t)
		live := f.task("plain work")
		f.served = []string{live.ID}

		if got := f.run(); got.beadID != live.ID {
			t.Fatalf("handed out %q, want %s; stderr=%s", got.beadID, live.ID, got.stderr)
		}
		if f.lists != 0 {
			t.Fatalf("member listings = %d, want 0 for a plain task", f.lists)
		}
	})

	t.Run("a linked step performs no member read", func(t *testing.T) {
		f := newHookLiveOwnershipFixture(t)
		root := f.root("run", nil)
		step := f.step(root.ID, "open", "", nil)
		f.served = []string{step.ID}

		if got := f.run(); got.beadID != step.ID {
			t.Fatalf("handed out %q, want the step %s; stderr=%s", got.beadID, step.ID, got.stderr)
		}
		if f.lists != 0 {
			t.Fatalf("member listings = %d, want 0 for a step: its ancestry gates walk up, not down", f.lists)
		}
	})

	t.Run("a drain-control link performs no member read", func(t *testing.T) {
		f := newHookLiveOwnershipFixture(t)
		run := f.root("implement", nil)
		drain := f.create(beads.Bead{Title: "drain", Type: "task", Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: run.ID,
			beadmeta.KindMetadataKey:       "drain",
		}})
		item := f.root("do-work", map[string]string{beadmeta.DrainControlIDMetadataKey: drain.ID})
		f.served = []string{item.ID}

		if got := f.run(); got.beadID != item.ID {
			t.Fatalf("handed out %q, want the drain-linked root %s; stderr=%s", got.beadID, item.ID, got.stderr)
		}
		if f.lists != 0 {
			t.Fatalf("member listings = %d, want 0 for a drain-linked root: the stop gate already reads its ancestry", f.lists)
		}
	})

	t.Run("an expanded root performs no member read", func(t *testing.T) {
		f := newHookLiveOwnershipFixture(t)
		expanded := f.root("run", map[string]string{beadmeta.WorkflowExpandedMetadataKey: "true"})
		f.served = []string{expanded.ID}

		got := f.run()

		if got.action != "drain" {
			t.Fatalf("action=%q bead %q, want a drain; an expanded root is not claimable at all; stderr=%s", got.action, got.beadID, got.stderr)
		}
		if f.lists != 0 {
			t.Fatalf("member listings = %d, want 0 for an expanded root: it is out of scope before any read", f.lists)
		}
	})

	t.Run("a standalone root performs exactly one member listing", func(t *testing.T) {
		f := newHookLiveOwnershipFixture(t)
		root := f.root("run", nil)
		f.step(root.ID, "in_progress", "worker-1", nil)
		f.served = []string{root.ID}

		if got := f.run(); got.action != "work" || got.beadID != root.ID {
			t.Fatalf("handed out action=%q bead %q, want the root %s (its only holder is this session); stderr=%s", got.action, got.beadID, root.ID, got.stderr)
		}
		if f.lists != 1 {
			t.Fatalf("member listings = %d, want exactly 1 for a standalone root", f.lists)
		}
	})

	t.Run("a ready assignment is untouched by the fence", func(t *testing.T) {
		f := newHookLiveOwnershipFixture(t)
		assigned := f.task("assigned work")
		assignee := "worker-1"
		if err := f.store.Update(assigned.ID, beads.UpdateOpts{Assignee: &assignee}); err != nil {
			t.Fatal(err)
		}
		f.served = []string{assigned.ID}

		if got := f.run(); got.beadID != assigned.ID {
			t.Fatalf("handed out %q, want the assignment %s; stderr=%s", got.beadID, assigned.ID, got.stderr)
		}
		if f.lists != 0 {
			t.Fatalf("member listings = %d, want 0: the ready tier resumes a session's own assignment and pays no fence read", f.lists)
		}
	})

	t.Run("an existing assignment is untouched by the fence", func(t *testing.T) {
		f := newHookLiveOwnershipFixture(t)
		adopted := f.task("work in flight")
		status, assignee := "in_progress", "worker-1"
		if err := f.store.Update(adopted.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
			t.Fatal(err)
		}
		f.served = []string{adopted.ID}

		if got := f.run(); got.beadID != adopted.ID {
			t.Fatalf("handed out %q, want the in-flight assignment %s; stderr=%s", got.beadID, adopted.ID, got.stderr)
		}
		if f.lists != 0 {
			t.Fatalf("member listings = %d, want 0: the existing-assignment tier resumes this session's own work", f.lists)
		}
	})
}

// workflowMembersCountedBinding counts the Lists and probes the routed member
// listing makes against the binding, and can fault either, while forwarding
// the compare-and-swap claim newHookClaimClassRoute requires (embedding the
// Store interface alone hides it — the same trap claimCapableCountedStore
// exists for).
type workflowMembersCountedBinding struct {
	beads.Store
	lists   *int
	claim   func(id, assignee string) (beads.Bead, bool, error)
	getErr  func(id string) error
	listErr error
}

func (s *workflowMembersCountedBinding) List(q beads.ListQuery) ([]beads.Bead, error) {
	*s.lists++
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.Store.List(q)
}

func (s *workflowMembersCountedBinding) Get(id string) (beads.Bead, error) {
	if s.getErr != nil {
		if err := s.getErr(id); err != nil {
			return beads.Bead{}, err
		}
	}
	return s.Store.Get(id)
}

func (s *workflowMembersCountedBinding) Claim(id, assignee string) (beads.Bead, bool, error) {
	return s.claim(id, assignee)
}

// workflowMembersSplitFixture is the split-city shape of the fence's member
// read: a graph.v2 workflow root and its steps live in the relocated class
// binding (a real SQLite leaf), while the ordinary work stays in a real
// FileStore. The unrouted member listing reads the work leg, which answers an
// empty success, and a root held through binding-resident steps sails through
// the fence — the wrong-store empty the routed seam exists to close.
type workflowMembersSplitFixture struct {
	t        *testing.T
	work     beads.Store
	leaf     beads.Store
	binding  *workflowMembersCountedBinding
	route    *hookClaimClassRoute
	claims   []string
	failures []hookClaimFailureNote
	lists    struct {
		work    int
		binding int
	}
	reads       int
	workReadErr error
}

func newWorkflowMembersSplitFixture(t *testing.T) *workflowMembersSplitFixture {
	t.Helper()
	work, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), ".gc", "beads.json"), beads.WithFileStoreIDPrefix("gc"))
	if err != nil {
		t.Fatalf("opening the file work store: %v", err)
	}
	leaf := newSplitEnvClassLeaf(t)
	f := &workflowMembersSplitFixture{t: t, work: work, leaf: leaf}
	f.binding = &workflowMembersCountedBinding{
		Store: leaf,
		lists: &f.lists.binding,
		claim: func(id, assignee string) (beads.Bead, bool, error) {
			claimer, ok := leaf.(interface {
				Claim(id, assignee string) (beads.Bead, bool, error)
			})
			if !ok {
				return beads.Bead{}, false, fmt.Errorf("the split leaf %T has no compare-and-swap claim", leaf)
			}
			return claimer.Claim(id, assignee)
		},
	}
	f.route = newClaimRouteFor(t, f.binding)
	return f
}

func (f *workflowMembersSplitFixture) bindingRoot(title string) beads.Bead {
	f.t.Helper()
	root, err := f.leaf.Create(beads.Bead{Title: title, Type: "molecule", Metadata: map[string]string{
		beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
		beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		beadmeta.RoutedToMetadataKey:        "worker",
	}})
	if err != nil {
		f.t.Fatalf("creating the binding root %q: %v", title, err)
	}
	return root
}

// bindingStep creates a member step of rootID that is in_progress under
// assignee — the shape that holds its root.
func (f *workflowMembersSplitFixture) bindingStep(rootID, assignee string) beads.Bead {
	f.t.Helper()
	step, err := f.leaf.Create(beads.Bead{Title: "member step", Type: "task", Assignee: assignee, Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey: rootID,
	}})
	if err != nil {
		f.t.Fatalf("creating the binding member of %s: %v", rootID, err)
	}
	inProgress := "in_progress"
	if err := f.leaf.Update(step.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		f.t.Fatalf("binding step %s to in_progress: %v", step.ID, err)
	}
	return step
}

func (f *workflowMembersSplitFixture) workTask(title string) beads.Bead {
	f.t.Helper()
	task, err := f.work.Create(beads.Bead{Title: title, Type: "task", Metadata: map[string]string{
		beadmeta.RoutedToMetadataKey: "worker",
	}})
	if err != nil {
		f.t.Fatalf("creating the work task %q: %v", title, err)
	}
	return task
}

// baseOps are the work-scope seams: the FileStore, the way the default seams
// run against the agent's bd store.
func (f *workflowMembersSplitFixture) baseOps() hookClaimOps {
	return hookClaimOps{
		Claim: func(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, bool, error) {
			f.claims = append(f.claims, id)
			current, err := f.work.Get(id)
			if err != nil {
				return beads.Bead{}, false, err
			}
			if current.Status == "closed" || (current.Assignee != "" && current.Assignee != assignee) {
				return current, false, nil
			}
			status := "in_progress"
			if err := f.work.Update(id, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
				return beads.Bead{}, false, err
			}
			claimed, err := f.work.Get(id)
			return claimed, true, err
		},
		Release: func(_ context.Context, _ string, _ []string, id, assignee string) (bool, error) {
			current, err := f.work.Get(id)
			if err != nil || current.Status != "in_progress" || current.Assignee != assignee {
				return false, err
			}
			status, none := "open", ""
			return true, f.work.Update(id, beads.UpdateOpts{Status: &status, Assignee: &none})
		},
		ReadWorkMeta: func(_ context.Context, _ string, _ []string, id, _ string) (beads.Bead, error) {
			f.reads++
			if f.workReadErr != nil {
				return beads.Bead{}, f.workReadErr
			}
			return f.work.Get(id)
		},
		WorkflowStore: func(context.Context, string, []string, string, string) beads.Store {
			return f.work
		},
		ListWorkflowMembers: func(_ context.Context, _ string, _ []string, rootID string) ([]beads.Bead, error) {
			f.lists.work++
			return f.work.List(beads.ListQuery{
				Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: rootID},
				TierMode: beads.TierBoth,
			})
		},
		EmitClaimFailed: func(beadID string, _ hookClaimOptions, err error) {
			f.failures = append(f.failures, hookClaimFailureNote{beadID: beadID, err: err})
		},
		StoreRef:                 func(string) string { return "" }, // the tier driven directly skips applyDefaults
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

func (f *workflowMembersSplitFixture) routedOps() hookClaimOps {
	return classRoutedHookClaimOps(f.baseOps(), f.route)
}

// TestClassRoutedWorkflowMembersFollowRootResidence pins the member listing on
// a split city: a graph.v2 root and its steps live in the relocated binding,
// so the listing must follow the root's residence by the claim-time read rule
// — the work leg first, the binding only on its proven absence — and a
// binding-held root must be withheld before any claim CAS runs.
func TestClassRoutedWorkflowMembersFollowRootResidence(t *testing.T) {
	listIDs := func(rows []beads.Bead) []string {
		ids := make([]string, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, row.ID)
		}
		return ids
	}

	t.Run("a binding-resident root lists its binding members", func(t *testing.T) {
		f := newWorkflowMembersSplitFixture(t)
		root := f.bindingRoot("run")
		held := f.bindingStep(root.ID, "seat-x")
		ops := f.routedOps()

		members, err := ops.ListWorkflowMembers(context.Background(), "/work", nil, root.ID)
		if err != nil {
			t.Fatalf("routed member listing of %s: %v", root.ID, err)
		}
		if got := strings.Join(listIDs(members), ","); !strings.Contains(","+got+",", ","+held.ID+",") {
			t.Fatalf("members = %v, want the binding member %s", listIDs(members), held.ID)
		}
		if f.lists.binding != 1 || f.lists.work != 0 {
			t.Fatalf("listings: binding=%d work=%d, want the binding's members read from the binding (1/0)", f.lists.binding, f.lists.work)
		}
		if f.reads != 0 {
			t.Fatalf("work reads = %d, want 0: the binding-minted id is reserved, so no work read can answer it", f.reads)
		}
	})

	t.Run("a work-resident root keeps the work leg", func(t *testing.T) {
		f := newWorkflowMembersSplitFixture(t)
		root := f.workTask("run")
		if err := f.work.SetMetadata(root.ID, beadmeta.KindMetadataKey, beadmeta.KindWorkflow); err != nil {
			t.Fatal(err)
		}
		held := f.workTask("member step")
		if err := f.work.SetMetadata(held.ID, beadmeta.RootBeadIDMetadataKey, root.ID); err != nil {
			t.Fatal(err)
		}
		status, holder := "in_progress", "seat-x"
		if err := f.work.Update(held.ID, beads.UpdateOpts{Status: &status, Assignee: &holder}); err != nil {
			t.Fatal(err)
		}
		ops := f.routedOps()

		members, err := ops.ListWorkflowMembers(context.Background(), "/work", nil, root.ID)
		if err != nil {
			t.Fatalf("routed member listing of %s: %v", root.ID, err)
		}
		if got := strings.Join(listIDs(members), ","); !strings.Contains(","+got+",", ","+held.ID+",") {
			t.Fatalf("members = %v, want the work member %s", listIDs(members), held.ID)
		}
		if f.lists.work != 1 || f.lists.binding != 0 {
			t.Fatalf("listings: work=%d binding=%d, want the work root's members read from the work leg (1/0)", f.lists.work, f.lists.binding)
		}
		if f.reads != 1 {
			t.Fatalf("work reads = %d, want 1: the work leg decides before any binding probe", f.reads)
		}
	})

	t.Run("co-residence keeps the work answer", func(t *testing.T) {
		f := newWorkflowMembersSplitFixture(t)
		root := f.workTask("run")
		if err := f.work.SetMetadata(root.ID, beadmeta.KindMetadataKey, beadmeta.KindWorkflow); err != nil {
			t.Fatal(err)
		}
		own := f.workTask("own member")
		if err := f.work.SetMetadata(own.ID, beadmeta.RootBeadIDMetadataKey, root.ID); err != nil {
			t.Fatal(err)
		}
		// The migration's copy of the root, and a foreign holder, live only
		// in the binding.
		foreigner, err := f.leaf.Create(beads.Bead{Title: "binding member", Type: "task", Assignee: "seat-x", Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: root.ID,
		}})
		if err != nil {
			t.Fatal(err)
		}
		inProgress := "in_progress"
		if err := f.leaf.Update(foreigner.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
			t.Fatal(err)
		}
		ops := f.routedOps()

		members, err := ops.ListWorkflowMembers(context.Background(), "/work", nil, root.ID)
		if err != nil {
			t.Fatalf("routed member listing of %s: %v", root.ID, err)
		}
		for _, row := range members {
			if row.ID == foreigner.ID {
				t.Fatalf("members = %v: the work store holds the root, so its answer is the answer; the binding's copy is not", listIDs(members))
			}
		}
		if f.lists.work != 1 || f.lists.binding != 0 {
			t.Fatalf("listings: work=%d binding=%d, want the work leg alone (1/0)", f.lists.work, f.lists.binding)
		}
	})

	t.Run("an unknown root keeps the work leg without an error", func(t *testing.T) {
		f := newWorkflowMembersSplitFixture(t)
		ops := f.routedOps()

		members, err := ops.ListWorkflowMembers(context.Background(), "/work", nil, "gcg--9223372036855500000")
		if err != nil {
			t.Fatalf("routed member listing of an unknown root: %v", err)
		}
		if len(members) != 0 {
			t.Fatalf("members = %v, want none", listIDs(members))
		}
		if f.lists.work != 1 {
			t.Fatalf("work listings = %d, want 1: the base seam keeps the leg after the probes proved absence", f.lists.work)
		}
	})

	t.Run("an operational work read keeps the work leg", func(t *testing.T) {
		f := newWorkflowMembersSplitFixture(t)
		f.workReadErr = errors.New("work leg unreadable")
		ops := f.routedOps()

		if _, err := ops.ListWorkflowMembers(context.Background(), "/work", nil, "gc-work-9"); err != nil {
			t.Fatalf("routed member listing: %v; an operational error is not proven absence and must not escalate", err)
		}
		if f.lists.work != 1 || f.lists.binding != 0 {
			t.Fatalf("listings: work=%d binding=%d, want the work leg retained (1/0)", f.lists.work, f.lists.binding)
		}
	})

	t.Run("a binding probe failure surfaces", func(t *testing.T) {
		f := newWorkflowMembersSplitFixture(t)
		f.binding.getErr = func(string) error { return errors.New("binding unreadable") }
		ops := f.routedOps()

		if _, err := ops.ListWorkflowMembers(context.Background(), "/work", nil, "gcg--9223372036855500001"); err == nil {
			t.Fatal("routed member listing of a reserved id succeeded; the binding probe's failure must surface, not read as absence")
		}
	})

	t.Run("a binding listing failure surfaces", func(t *testing.T) {
		f := newWorkflowMembersSplitFixture(t)
		root := f.bindingRoot("run")
		f.bindingStep(root.ID, "seat-x")
		f.binding.listErr = errors.New("binding list failed")
		ops := f.routedOps()

		if _, err := ops.ListWorkflowMembers(context.Background(), "/work", nil, root.ID); err == nil {
			t.Fatal("routed member listing succeeded; the binding listing's failure must surface")
		}
	})

	// End to end: the whole eligible tier on a split city, candidates served
	// in order — the binding-held root first, live work behind it. The root
	// must be withheld WITHOUT a claim CAS, and the live work claimed.
	t.Run("the claim flow withholds a binding-held root without a CAS", func(t *testing.T) {
		f := newWorkflowMembersSplitFixture(t)
		root := f.bindingRoot("do-work run")
		f.bindingStep(root.ID, "seat-x")
		f.bindingStep(root.ID, "seat-y")
		live := f.workTask("live work")
		rootRow, err := f.leaf.Get(root.ID)
		if err != nil {
			t.Fatal(err)
		}
		ops := f.routedOps()

		var stdout, stderr bytes.Buffer
		res := claimFirstEligibleHookCandidate([]beads.Bead{rootRow, live}, hookClaimOptions{
			Assignee:           "worker-1",
			IdentityCandidates: []string{"worker-1"},
			RouteTargets:       []string{"worker"},
		}, ops, "/work", &stdout, &stderr)

		if !res.terminal || res.code != 0 {
			t.Fatalf("tier result = terminal:%v code:%d, want the live claim delivered; stderr=%s", res.terminal, res.code, stderr.String())
		}
		if len(f.claims) != 1 || f.claims[0] != live.ID {
			t.Fatalf("claim CAS ran on %v, want only the live task %s; the held root must be withheld before any CAS", f.claims, live.ID)
		}
		got, err := f.leaf.Get(root.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "open" || got.Assignee != "" {
			t.Fatalf("root = status %q assignee %q, want the served row untouched in the binding", got.Status, got.Assignee)
		}
		if !strings.Contains(stderr.String(), "withholding workflow root "+root.ID) {
			t.Fatalf("stderr=%q, want the withhold named for %s", stderr.String(), root.ID)
		}
		if len(f.failures) != 1 || f.failures[0].beadID != root.ID {
			t.Fatalf("claim failures = %+v, want one for the withheld root %s", f.failures, root.ID)
		}
	})
}

// TestHookClaimLiveOwnershipUnavailable pins the fail-open trade: the member
// read is one more pre-claim read on every routed root, and a store that
// cannot answer it must not idle the seat — the same trade the stop gate
// makes. The unavailability is named on stderr, and the claim attempt
// proceeds exactly as before.
func TestHookClaimLiveOwnershipUnavailable(t *testing.T) {
	claimDirectly := func(t *testing.T, ops hookClaimOps, candidate beads.Bead) (hookClaimResult, string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		res := claimFirstEligibleHookCandidate([]beads.Bead{candidate}, hookClaimOptions{
			Assignee:           "worker-1",
			IdentityCandidates: []string{"worker-1"},
			RouteTargets:       []string{"worker"},
		}, ops, "/work", &stdout, &stderr)
		return res, stderr.String()
	}

	t.Run("a missing seam proceeds with the claim", func(t *testing.T) {
		f := newHookLiveOwnershipFixture(t)
		root := f.root("run", nil)
		f.step(root.ID, "in_progress", "seat-x", nil)
		ops := f.ops()
		ops.ListWorkflowMembers = nil

		res, stderr := claimDirectly(t, ops, root)

		if !res.terminal || res.code != 0 {
			t.Fatalf("tier result = terminal:%v code:%d, want the root claim delivered; stderr=%s", res.terminal, res.code, stderr)
		}
		if len(f.claims) != 1 || f.claims[0] != root.ID {
			t.Fatalf("claims = %v, want the claim attempt to proceed on %s exactly as before", f.claims, root.ID)
		}
		if !strings.Contains(stderr, "foreign workflow hold check unavailable for "+root.ID) {
			t.Fatalf("stderr=%q, want the unavailability named for %s", stderr, root.ID)
		}
	})

	t.Run("a failing member read proceeds with the claim", func(t *testing.T) {
		f := newHookLiveOwnershipFixture(t)
		root := f.root("run", nil)
		f.step(root.ID, "in_progress", "seat-x", nil)
		ops := f.ops()
		ops.ListWorkflowMembers = func(context.Context, string, []string, string) ([]beads.Bead, error) {
			return nil, errors.New("member read timed out")
		}

		res, stderr := claimDirectly(t, ops, root)

		if !res.terminal || res.code != 0 {
			t.Fatalf("tier result = terminal:%v code:%d, want the root claim delivered; stderr=%s", res.terminal, res.code, stderr)
		}
		if len(f.claims) != 1 || f.claims[0] != root.ID {
			t.Fatalf("claims = %v, want the claim attempt to proceed on %s exactly as before", f.claims, root.ID)
		}
		if !strings.Contains(stderr, "foreign workflow hold check unavailable for "+root.ID) || !strings.Contains(stderr, "member read timed out") {
			t.Fatalf("stderr=%q, want the unavailability and its reason named", stderr)
		}
	})

	t.Run("a non-root candidate never pays the read", func(t *testing.T) {
		f := newHookLiveOwnershipFixture(t)
		live := f.task("plain work")
		ops := f.ops()
		ops.ListWorkflowMembers = nil // scope precedes the seam check

		res, stderr := claimDirectly(t, ops, live)

		if !res.terminal || res.code != 0 || len(f.claims) != 1 || f.claims[0] != live.ID {
			t.Fatalf("tier result = terminal:%v code:%d claims=%v, want the plain claim delivered; stderr=%s", res.terminal, res.code, f.claims, stderr)
		}
		if strings.Contains(stderr, "foreign workflow hold check unavailable") {
			t.Fatalf("stderr=%q: an out-of-scope candidate must short-circuit before the seam is ever read", stderr)
		}
	})
}

// TestHookClaimLiveOwnershipLoopReporting pins the withhold's observability
// contract: distinct seats withheld by the same root report IDENTICAL failure
// text (member ids are the incident's evidence, not the loop's signature), the
// invocation drains no_work rather than claims_errored, the root is never
// mutated, and the resulting events are exactly what the claim-loop guard
// needs — a hold at the third seat, immune to member events, lifted by the
// root's own change, and one probe admitted when the hold runs out.
func TestHookClaimLiveOwnershipLoopReporting(t *testing.T) {
	f := newHookLiveOwnershipFixture(t)
	root := f.root("do-work run", nil)
	f.served = []string{root.ID}

	var texts []string
	var members []string
	for seat := 1; seat <= claimLoopTripFailures; seat++ {
		members = append(members, f.step(root.ID, "in_progress", fmt.Sprintf("seat-holder-%d", seat), nil).ID)
		run := f.runAs(fmt.Sprintf("worker-%d", seat),
			"GC_TEMPLATE=worker-pool",
			fmt.Sprintf("GC_SESSION_ID=gcg-loop-%d", seat))
		if run.action != "drain" || run.reason != "no_work" {
			t.Fatalf("seat %d: action=%q reason=%q, want drain no_work; a withheld root is not a claim error; stderr=%s", seat, run.action, run.reason, run.stderr)
		}
		f.wantUnchanged(root.ID)
		if len(f.failures) != seat {
			t.Fatalf("seat %d: claim failures = %d, want one per withheld poll", seat, len(f.failures))
		}
		note := f.failures[seat-1]
		if note.beadID != root.ID {
			t.Fatalf("seat %d: failure subject %q, want the root %s", seat, note.beadID, root.ID)
		}
		texts = append(texts, note.err.Error())
	}
	if len(f.claims) != 0 {
		t.Fatalf("claims = %v; the root must never be claimed whole", f.claims)
	}
	for _, text := range texts[1:] {
		if text != texts[0] {
			t.Fatalf("withheld failure text varies by member set:\n  %q\n  %q\nthe claim-loop guard compares these texts; member ids must stay out of them", texts[0], text)
		}
	}

	// The guard leg: the same three seats' events, through the real guard.
	held := func(h *claimLoopHarness) bool {
		t.Helper()
		counts := map[string]int{"worker-pool": 1}
		demand := map[string]scaleCheckDemand{"worker-pool": {Count: 1, WorkBeadIDs: []string{root.ID}}}
		gotCounts, gotDemand := h.guard.withhold(counts, demand)
		return gotCounts["worker-pool"] == 0 && len(gotDemand["worker-pool"].WorkBeadIDs) == 0
	}
	trip := func(h *claimLoopHarness) {
		t.Helper()
		for seat := 1; seat <= claimLoopTripFailures; seat++ {
			h.failAs(fmt.Sprintf("gcg-loop-%d", seat), "worker-pool", root.ID, texts[0])
			h.advance(time.Minute)
		}
	}

	h := newClaimLoopHarness(t)
	trip(h)
	if !held(h) {
		t.Fatal("repeated withholds did not hold the root out of its template's default routed demand")
	}
	h.guard.observe(events.Event{Type: events.BeadUpdated, Subject: members[0], Ts: h.now})
	if !held(h) {
		t.Fatal("a member's own bead event lifted the hold; only the root's changes may")
	}
	h.guard.observe(events.Event{Type: events.BeadUpdated, Subject: root.ID, Ts: h.now})
	if held(h) {
		t.Fatal("a change to the root did not lift the hold")
	}

	probe := newClaimLoopHarness(t)
	trip(probe)
	if !held(probe) {
		t.Fatal("not held after the trip")
	}
	probe.advance(claimLoopHoldBase)
	if held(probe) {
		t.Fatal("a hold that ran out did not admit one probe")
	}
}
