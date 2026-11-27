package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/inputadmission"
)

// The hook's claim path is the third door that can hand a row to an agent
// (#8832), so it refuses the same rows the ready arm and the controller's
// demand pass refuse: withholdInputGatedCandidates judges every served
// candidate through the input-admission seam and drops the withheld ones —
// whatever query served them, since a work_query is admission-blind — naming
// each refusal's facts on stderr and writing nothing. The demand/claim
// divergence classifier reads the same seam, so a controller-counted row the
// hook could not claim because its input is not ready classifies as pull
// working, not divergence.

// hookInputAdmissionFixture is one store holding a graph.v2 workflow — a root
// with gc.input_convoy_id, an open routed step under it — whose input convoy
// tracks one input member.
type hookInputAdmissionFixture struct {
	store    beads.Store
	input    beads.Bead
	convoyID string
	root     beads.Bead
	step     beads.Bead
}

// newHookInputAdmissionFixture builds the workflow over one store. A non-zero
// input overrides the input member's shape (a deferral is authored at create
// time); setup mutates the store after the workflow exists.
func newHookInputAdmissionFixture(t *testing.T, input beads.Bead, setup func(t *testing.T, store beads.Store, input beads.Bead)) hookInputAdmissionFixture {
	t.Helper()
	store := beads.NewMemStore()
	f := hookInputAdmissionFixture{store: store}
	if input.ID == "" && input.Title == "" {
		input = beads.Bead{Title: "input", Type: "task"}
	}
	var err error
	if f.input, err = store.Create(input); err != nil {
		t.Fatal(err)
	}
	conv, err := store.Create(beads.Bead{Title: "input convoy", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	f.convoyID = conv.ID
	if err := store.DepAdd(conv.ID, f.input.ID, "tracks"); err != nil {
		t.Fatalf("tracking the input in the convoy: %v", err)
	}
	if f.root, err = store.Create(beads.Bead{Title: "workflow root", Type: "molecule", Metadata: map[string]string{
		beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
		beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		beadmeta.InputConvoyIDMetadataKey:   conv.ID,
	}}); err != nil {
		t.Fatal(err)
	}
	if f.step, err = store.Create(beads.Bead{Title: "open step", Type: "task", Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey: f.root.ID,
		beadmeta.RoutedToMetadataKey:   "worker",
	}}); err != nil {
		t.Fatal(err)
	}
	if setup != nil {
		setup(t, store, f.input)
	}
	return f
}

// ops are the seams over the fixture's own store, the shape
// hookInputAdmissionWithBdStore has over the bd store at dir.
func (f hookInputAdmissionFixture) ops() hookClaimOps {
	return hookClaimOps{
		InputAdmission: func(ctx context.Context, _ string, _ []string, candidates []beads.Bead) (map[string]inputadmission.Withhold, error) {
			adm := inputadmission.New([]inputadmission.Source{{
				Label: "city",
				Live:  beads.LiveFor(f.store),
				Work:  beads.WorkStore{Store: f.store},
			}}, time.Now())
			return adm.Judge(ctx, 0, candidates)
		},
	}
}

func hookAdmissionOptions() hookClaimOptions {
	return hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
	}
}

// TestWithholdInputGatedCandidatesRefusesUnreadyInput pins the rule shared
// with the ready arm: every authored readiness fact an input can carry — a
// canonical dispatch hold, a raw blocked status, a future deferral, an
// unsatisfied ready-blocking prerequisite — withholds the step, with the
// verdict's reason, input, root and convoy named on stderr, and nothing
// written. A clean or terminal input, and a row that is not subject (existing
// ownership, plain work), stays served; a failed judgment fails the gate; a
// nil seam keeps every row.
func TestWithholdInputGatedCandidatesRefusesUnreadyInput(t *testing.T) {
	future := time.Now().Add(time.Hour)
	for _, tt := range []struct {
		name     string
		deferred bool
		setup    func(t *testing.T, store beads.Store, input beads.Bead)
		wantKept int
		wantLine []string
	}{
		{
			name: "held input",
			setup: func(t *testing.T, store beads.Store, input beads.Bead) {
				if err := store.Update(input.ID, beads.UpdateOpts{Labels: []string{"hold:external"}}); err != nil {
					t.Fatalf("holding input: %v", err)
				}
			},
			wantLine: []string{"carries a dispatch hold"},
		},
		{
			name: "blocked input",
			setup: func(t *testing.T, store beads.Store, input beads.Bead) {
				status := "blocked"
				if err := store.Update(input.ID, beads.UpdateOpts{Status: &status}); err != nil {
					t.Fatalf("blocking input: %v", err)
				}
			},
			wantLine: []string{"is blocked"},
		},
		{
			name:     "deferred input",
			deferred: true,
			wantLine: []string{"is deferred"},
		},
		{
			name: "prerequisite input",
			setup: func(t *testing.T, store beads.Store, input beads.Bead) {
				target, err := store.Create(beads.Bead{Title: "prerequisite target", Type: "task"})
				if err != nil {
					t.Fatal(err)
				}
				if err := store.DepAdd(input.ID, target.ID, "blocks"); err != nil {
					t.Fatalf("adding the blocks edge: %v", err)
				}
			},
			wantLine: []string{"waits on prerequisite"},
		},
		{name: "clean input", wantKept: 1},
		{
			name: "terminal input",
			setup: func(t *testing.T, store beads.Store, input beads.Bead) {
				if err := store.Close(input.ID); err != nil {
					t.Fatalf("closing input: %v", err)
				}
			},
			wantKept: 1,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			input := beads.Bead{}
			if tt.deferred {
				input = beads.Bead{Title: "input", Type: "task", DeferUntil: &future}
			}
			f := newHookInputAdmissionFixture(t, input, tt.setup)
			var stderr bytes.Buffer
			kept, err := withholdInputGatedCandidates([]beads.Bead{f.step}, hookAdmissionOptions(), f.ops(), "/work", &stderr)
			if err != nil {
				t.Fatalf("withholdInputGatedCandidates: %v", err)
			}
			if len(kept) != tt.wantKept {
				t.Fatalf("kept %d candidates (stderr=%q), want %d", len(kept), stderr.String(), tt.wantKept)
			}
			if tt.wantKept == 0 {
				for _, want := range append([]string{f.step.ID, f.input.ID, "root " + f.root.ID, "input convoy " + f.convoyID}, tt.wantLine...) {
					if !strings.Contains(stderr.String(), want) {
						t.Fatalf("stderr = %q, want it to name %q", stderr.String(), want)
					}
				}
			}
		})
	}
}

// Existing ownership is never judged, and a row outside a workflow has no
// input to judge: both stay served, so adoption and plain work survive the
// gate.
func TestWithholdInputGatedCandidatesKeepsUnsubjectRows(t *testing.T) {
	f := newHookInputAdmissionFixture(t, beads.Bead{}, func(t *testing.T, store beads.Store, input beads.Bead) {
		if err := store.Update(input.ID, beads.UpdateOpts{Labels: []string{"hold:external"}}); err != nil {
			t.Fatalf("holding input: %v", err)
		}
	})
	inProgress, assignee := "in_progress", "worker-1"
	if err := f.store.Update(f.step.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &assignee}); err != nil {
		t.Fatal(err)
	}
	plain, err := f.store.Create(beads.Bead{Title: "plain work", Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"}})
	if err != nil {
		t.Fatal(err)
	}
	owned, err := f.store.Get(f.step.ID)
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	kept, err := withholdInputGatedCandidates([]beads.Bead{owned, plain}, hookAdmissionOptions(), f.ops(), "/work", &stderr)
	if err != nil {
		t.Fatalf("withholdInputGatedCandidates: %v", err)
	}
	if len(kept) != 2 {
		t.Fatalf("kept %d candidates (stderr=%q), want both: the in_progress row is existing ownership and the plain row has no input", len(kept), stderr.String())
	}
}

// A judgment that fails fails the gate — a row served unaudited is how the
// three doors disagree — and a nil seam keeps every row.
func TestWithholdInputGatedCandidatesFailsClosed(t *testing.T) {
	f := newHookInputAdmissionFixture(t, beads.Bead{}, nil)
	ops := hookClaimOps{
		InputAdmission: func(context.Context, string, []string, []beads.Bead) (map[string]inputadmission.Withhold, error) {
			return nil, errors.New("store down")
		},
	}
	var stderr bytes.Buffer
	if _, err := withholdInputGatedCandidates([]beads.Bead{f.step}, hookAdmissionOptions(), ops, "/work", &stderr); err == nil {
		t.Fatal("a failed judgment kept the candidates, want the gate to fail")
	}
	if !strings.Contains(stderr.String(), "checking input admission") {
		t.Fatalf("stderr = %q, want the judgment failure named", stderr.String())
	}

	kept, err := withholdInputGatedCandidates([]beads.Bead{f.step}, hookAdmissionOptions(), hookClaimOps{}, "/work", &stderr)
	if err != nil || len(kept) != 1 {
		t.Fatalf("nil seam: kept=%d err=%v, want every row kept", len(kept), err)
	}
}

// The claim path applies the gate after done-workflow retirement and before
// any tier claims: a withheld candidate is refused without a claim attempt
// and drains as no-work, and a judgment that fails fails the hook.
func TestDoHookClaimRefusesInputWithheldCandidates(t *testing.T) {
	runner := func(string, string) (string, error) {
		return `[{"id":"work-1","status":"open","metadata":{"gc.routed_to":"worker","gc.root_bead_id":"root-1"}}]`, nil
	}
	opts := hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"worker"},
		DrainAck:     true,
		JSON:         true,
	}
	t.Run("withheld drains as no work", func(t *testing.T) {
		var attempts int
		drained := false
		ops := hookClaimOps{
			Runner: runner,
			Claim: func(context.Context, string, []string, string, string) (beads.Bead, bool, error) {
				attempts++
				return beads.Bead{}, false, nil
			},
			DrainAck: func(io.Writer) error {
				drained = true
				return nil
			},
			InputAdmission: func(context.Context, string, []string, []beads.Bead) (map[string]inputadmission.Withhold, error) {
				return map[string]inputadmission.Withhold{"work-1": {Reason: inputadmission.ReasonHeld, RootID: "root-1", ConvoyID: "conv-1", InputID: "in-1"}}, nil
			},
		}
		var stdout, stderr bytes.Buffer
		if code := doHookClaim("query", "/rig", opts, ops, &stdout, &stderr); code != 0 {
			t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
		}
		if attempts != 0 {
			t.Fatalf("claim attempts = %d, want none for a withheld candidate", attempts)
		}
		if !drained {
			t.Fatal("the withheld refusal did not drain as no work")
		}
		for _, want := range []string{"work-1", "carries a dispatch hold", "in-1", "root root-1", "input convoy conv-1"} {
			if !strings.Contains(stderr.String(), want) {
				t.Fatalf("stderr = %q, want it to name %q", stderr.String(), want)
			}
		}
	})
	t.Run("judgment failure fails the hook", func(t *testing.T) {
		ops := hookClaimOps{
			Runner: runner,
			InputAdmission: func(context.Context, string, []string, []beads.Bead) (map[string]inputadmission.Withhold, error) {
				return nil, errors.New("convoy read failed")
			},
		}
		var stdout, stderr bytes.Buffer
		if code := doHookClaim("query", "/rig", opts, ops, &stdout, &stderr); code != 1 {
			t.Fatalf("doHookClaim = %d, want 1; stderr=%s", code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "checking input admission") {
			t.Fatalf("stderr = %q, want the judgment failure named", stderr.String())
		}
	})
}

// The split-city seam judges across the two legs a relocated workflow spans:
// the root and step in the binding, the held input in the work store. The
// gate withholds the binding-resident step and writes nothing in either leg;
// a clean input keeps it served.
func TestClassRoutedHookWithholdsUnreadyInput(t *testing.T) {
	for _, tc := range []struct {
		name     string
		hold     bool
		wantKept int
	}{
		{name: "held work-side input withholds the binding step", hold: true},
		{name: "clean input keeps the step served", wantKept: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDoneWorkflowSplitFixture(t, false)
			if tc.hold {
				if err := f.work.Update(f.source.ID, beads.UpdateOpts{Labels: []string{"hold:external"}}); err != nil {
					t.Fatalf("holding input: %v", err)
				}
			}
			ops := classRoutedHookClaimOps(f.baseOps(t), f.route)
			var stderr bytes.Buffer
			kept, err := withholdInputGatedCandidates([]beads.Bead{f.step}, hookAdmissionOptions(), ops, "/work", &stderr)
			if err != nil {
				t.Fatalf("withholdInputGatedCandidates: %v", err)
			}
			if len(kept) != tc.wantKept {
				t.Fatalf("kept %d candidates (stderr=%q), want %d", len(kept), stderr.String(), tc.wantKept)
			}
			if tc.hold {
				for _, want := range []string{f.step.ID, f.source.ID, "root " + f.root.ID, "input convoy " + f.convoyID} {
					if !strings.Contains(stderr.String(), want) {
						t.Fatalf("stderr = %q, want it to name %q", stderr.String(), want)
					}
				}
			}
			f.assertOpen(t, f.root.ID, f.step.ID) // the gate writes nothing
		})
	}
}

// A controller-counted row the hook could not claim because its input is not
// ready is correct pull, the same bucket a held row lands in — and a judgment
// that fails is a failed read of the trigger, never either verdict.
func TestDivergenceTreatsWithheldInputAsBenign(t *testing.T) {
	routed := map[string]string{beadmeta.RoutedToMetadataKey: "rig/worker"}
	bead := beads.Bead{ID: "wb-1", Status: "open", Type: "task", Metadata: routed}
	for _, tt := range []struct {
		name      string
		withheld  map[string]inputadmission.Withhold
		err       error
		wantClass string
		wantStat  string
	}{
		{
			name:      "withheld is benign",
			withheld:  map[string]inputadmission.Withhold{"wb-1": {Reason: inputadmission.ReasonHeld, RootID: "r", ConvoyID: "c", InputID: "i"}},
			wantClass: events.DemandClaimBenign,
			wantStat:  "open",
		},
		{
			name:      "judgment failure is unknown",
			err:       errors.New("convoy read failed"),
			wantClass: events.DemandClaimUnknown,
			wantStat:  demandTriggerUnreadable,
		},
		{
			name:      "admissible is divergence",
			withheld:  map[string]inputadmission.Withhold{},
			wantClass: events.DemandClaimDivergence,
			wantStat:  "open",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ops := demandDivergenceOpsForBlockedBead(bead, nil, false, nil)
			ops.InputAdmission = func(context.Context, string, []string, []beads.Bead) (map[string]inputadmission.Withhold, error) {
				return tt.withheld, tt.err
			}
			status, class := classifyDemandTrigger(bead.ID, "/rig", divergenceOptions(), ops)
			if class != tt.wantClass || status != tt.wantStat {
				t.Fatalf("classification = (%q, %q), want (%q, %q)", status, class, tt.wantStat, tt.wantClass)
			}
		})
	}
}

// admissionOps are full claim ops over the fixture's own store: the retire
// pair and the admission gate both read it live, so a doHookClaim driven over
// these seams exercises the real ordering — retirement first, admission
// second — against the same rows a unified city would serve.
func (f hookInputAdmissionFixture) admissionOps(claim hookClaimFunc) hookClaimOps {
	ops := f.ops()
	ops.Claim = claim
	ops.InputVerdict = func(ctx context.Context, _ string, _ []string, candidate beads.Bead, ids []string, prove workflowInputAbsenceProver) (workflowInputVerdict, error) {
		return workflowInputDone(ctx, beadstest.Work(f.store), candidate, ids, prove)
	}
	ops.RetireWorkflow = func(ctx context.Context, _ string, _ []string, verdict workflowInputVerdict) error {
		if verdict.State == workflowInputPurged {
			return failPurgedWorkflow(ctx, f.store, verdict.RootID)
		}
		return skipDoneWorkflow(f.store, verdict.RootID)
	}
	return ops
}

// serveRunner fabricates a work query that answers exactly rows.
func serveRunner(rows ...beads.Bead) func(string, string) (string, error) {
	output, err := json.Marshal(rows)
	if err != nil {
		panic(err)
	}
	return func(string, string) (string, error) { return string(output), nil }
}

// TestDoHookClaimServesUnsubjectRowsThroughRealJudge covers AC-7's hook arms:
// through the real judge over the fixture's own store, a control-kind row is
// claimed and an in_progress row owned by this identity is adopted exactly as
// before the gate — control kinds are dispatcher bookkeeping and in_progress is
// existing ownership, neither is judged — while the same workflow's open step
// stays refused while its input is held.
func TestDoHookClaimServesUnsubjectRowsThroughRealJudge(t *testing.T) {
	hold := func(t *testing.T, store beads.Store, input beads.Bead) {
		if err := store.Update(input.ID, beads.UpdateOpts{Labels: []string{"hold:external"}}); err != nil {
			t.Fatalf("holding input: %v", err)
		}
	}
	opts := hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		JSON:               true,
	}
	t.Run("control-kind row claimed", func(t *testing.T) {
		f := newHookInputAdmissionFixture(t, beads.Bead{}, hold)
		control, err := f.store.Create(beads.Bead{Title: "control row", Type: "task", Metadata: map[string]string{
			beadmeta.KindMetadataKey:       beadmeta.KindDrain,
			beadmeta.RootBeadIDMetadataKey: f.root.ID,
			beadmeta.RoutedToMetadataKey:   "worker",
		}})
		if err != nil {
			t.Fatal(err)
		}
		var claimed []string
		ops := f.admissionOps(func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, bool, error) {
			claimed = append(claimed, beadID)
			return beads.Bead{ID: beadID, Status: "in_progress", Assignee: "worker-1"}, true, nil
		})
		ops.Runner = serveRunner(control, f.step)
		var stdout, stderr bytes.Buffer
		if code := doHookClaim("query", "/rig", opts, ops, &stdout, &stderr); code != 0 {
			t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
		}
		if len(claimed) != 1 || claimed[0] != control.ID {
			t.Fatalf("claimed %v, want only the control row %s", claimed, control.ID)
		}
		if !strings.Contains(stderr.String(), f.step.ID) {
			t.Fatalf("stderr = %q, want the withheld open step named", stderr.String())
		}
	})
	t.Run("owned in_progress row adopted", func(t *testing.T) {
		f := newHookInputAdmissionFixture(t, beads.Bead{}, hold)
		inProgress, assignee := "in_progress", "worker-1"
		if err := f.store.Update(f.step.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &assignee}); err != nil {
			t.Fatal(err)
		}
		ops := f.admissionOps(func(context.Context, string, []string, string, string) (beads.Bead, bool, error) {
			t.Fatal("an owned in_progress row is adopted without a claim")
			return beads.Bead{}, false, nil
		})
		owned, err := f.store.Get(f.step.ID) // the live row a work query would serve
		if err != nil {
			t.Fatal(err)
		}
		ops.Runner = serveRunner(owned)
		var stdout, stderr bytes.Buffer
		if code := doHookClaim("query", "/rig", opts, ops, &stdout, &stderr); code != 0 {
			t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "existing_assignment") || !strings.Contains(stdout.String(), f.step.ID) {
			t.Fatalf("stdout = %q, want the owned row adopted as existing_assignment", stdout.String())
		}
		if strings.Contains(stderr.String(), "skipped "+f.step.ID) {
			t.Fatalf("stderr = %q, want the owned row never refused", stderr.String())
		}
	})
}

// TestDoHookClaimRetiresTerminalInputBeforeAdmissionGate covers AC-7's
// retirement arm: an all-terminal input retires the workflow — root and open
// steps closed as skipped — before the admission gate runs, exactly the
// pre-gate behavior; the gate sees no candidate and the claim path never
// attempts one.
func TestDoHookClaimRetiresTerminalInputBeforeAdmissionGate(t *testing.T) {
	f := newHookInputAdmissionFixture(t, beads.Bead{}, nil)
	if err := f.store.Close(f.input.ID); err != nil {
		t.Fatalf("closing the input: %v", err)
	}
	var claimed []string
	ops := f.admissionOps(func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, bool, error) {
		claimed = append(claimed, beadID)
		return beads.Bead{ID: beadID, Status: "in_progress", Assignee: "worker-1"}, true, nil
	})
	ops.Runner = serveRunner(f.step)
	opts := hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		DrainAck:           true,
		JSON:               true,
	}
	ops.DrainAck = func(io.Writer) error { return nil }
	var stdout, stderr bytes.Buffer
	if code := doHookClaim("query", "/rig", opts, ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if len(claimed) != 0 {
		t.Fatalf("claimed %v, want retirement to close the workflow before any claim", claimed)
	}
	if !strings.Contains(stderr.String(), "input convoy work is already terminal") {
		t.Fatalf("stderr = %q, want the retirement line", stderr.String())
	}
	if strings.Contains(stderr.String(), "input admission") {
		t.Fatalf("stderr = %q, want no admission line over a retired workflow", stderr.String())
	}
	for _, id := range []string{f.root.ID, f.step.ID} {
		got, err := f.store.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "closed" || got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeSkipped {
			t.Fatalf("%s: status=%q outcome=%q, want closed/%s", id, got.Status, got.Metadata[beadmeta.OutcomeMetadataKey], beadmeta.OutcomeSkipped)
		}
	}
}
