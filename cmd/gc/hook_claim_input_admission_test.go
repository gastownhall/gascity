package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/inputadmission"
	"github.com/gastownhall/gascity/internal/readywork"
)

// admissionFixture is a single-store city holding one workflow whose input is
// parked on hold:external, the steps that workflow materialized, and a free
// routed bead no input gates.
type admissionFixture struct {
	store *beads.MemStore
	input beads.Bead
	root  beads.Bead
	steps []beads.Bead
	free  beads.Bead
}

// newAdmissionFixture seeds the fixture; assignee, when set, is the seat every
// withheld step is already assigned to (open, not yet started).
func newAdmissionFixture(t *testing.T, steps int, assignee string) admissionFixture {
	t.Helper()
	store := beads.NewMemStore()
	input, err := store.Create(beads.Bead{Title: "input", Labels: []string{"hold:external"}})
	if err != nil {
		t.Fatalf("create input: %v", err)
	}
	conv, err := store.Create(beads.Bead{Title: "input convoy", Type: "convoy"})
	if err != nil {
		t.Fatalf("create convoy: %v", err)
	}
	if err := convoy.TrackItemIn(convoy.MemberClasses{Convoy: beadstest.Work(store)}, conv.ID, input.ID); err != nil {
		t.Fatalf("track: %v", err)
	}
	root, err := store.Create(beads.Bead{Title: "mol", Metadata: map[string]string{
		beadmeta.KindMetadataKey:          beadmeta.KindWorkflow,
		beadmeta.InputConvoyIDMetadataKey: conv.ID,
		beadmeta.RoutedToMetadataKey:      "worker",
	}})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	f := admissionFixture{store: store, input: input, root: root}
	for range steps {
		step, err := store.Create(beads.Bead{Title: "step", Assignee: assignee, Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: root.ID,
			beadmeta.RoutedToMetadataKey:   "worker",
		}})
		if err != nil {
			t.Fatalf("create step: %v", err)
		}
		f.steps = append(f.steps, step)
	}
	f.free, err = store.Create(beads.Bead{Title: "free work", Metadata: map[string]string{
		beadmeta.RoutedToMetadataKey: "worker",
	}})
	if err != nil {
		t.Fatalf("create free bead: %v", err)
	}
	return f
}

// output is rows as the work query prints them.
func (f admissionFixture) output(t *testing.T, rows ...beads.Bead) string {
	t.Helper()
	out, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(out)
}

// ops is a claim path over the fixture's store: input admission judged live
// against it, workflows and the rows a federated re-ask keeps read from it,
// every claim recorded in claimed.
func (f admissionFixture) ops(t *testing.T, claimed *[]string) hookClaimOps {
	t.Helper()
	return hookClaimOps{
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			*claimed = append(*claimed, beadID)
			return beads.Bead{ID: beadID, Assignee: assignee, Status: "in_progress"}, true, nil
		},
		WorkflowStore: func(context.Context, string, []string, string, string) beads.Store { return f.store },
		InputVerdict: func(ctx context.Context, _ string, _ []string, candidate beads.Bead, identityCandidates []string, _ workflowInputAbsenceProver) (workflowInputVerdict, error) {
			return workflowInputDone(ctx, beadstest.Work(f.store), candidate, identityCandidates, nil)
		},
		RetireWorkflow: func(_ context.Context, _ string, _ []string, verdict workflowInputVerdict) error {
			t.Fatalf("unexpected retirement of %s", verdict.RootID)
			return nil
		},
		InputAdmission: func(ctx context.Context, _ string, _ []string, candidates []beads.Bead) (inputadmission.Verdicts, error) {
			return inputadmission.New([]inputadmission.Source{{Label: "work", Reader: f.store}}, time.Now()).Verdicts(ctx, 0, candidates)
		},
		ListContinuation: func(context.Context, string, []string, string, string) ([]beads.Bead, error) {
			return nil, nil
		},
		AssignContinuation: func(context.Context, string, []string, string, string) error { return nil },
		ResolveWorkBranch:  func(hookClaimWorkTree) string { return "" },
		StampWorkMeta: func(context.Context, string, []string, string, string, map[string]string) error {
			return nil
		},
		PublishRunMap: func(string, string, ...string) error { return nil },
		DrainAck:      func(io.Writer) error { return nil },
	}
}

// requireUntouched fails unless every withheld step still has the status,
// assignee and metadata it was created with.
func (f admissionFixture) requireUntouched(t *testing.T) {
	t.Helper()
	for _, want := range f.steps {
		got, err := f.store.Get(want.ID)
		if err != nil {
			t.Fatalf("get %s: %v", want.ID, err)
		}
		if got.Status != want.Status || got.Assignee != want.Assignee || !maps.Equal(got.Metadata, want.Metadata) {
			t.Fatalf("withheld step %s changed: status %q assignee %q metadata %v, want %q %q %v",
				want.ID, got.Status, got.Assignee, got.Metadata, want.Status, want.Assignee, want.Metadata)
		}
	}
}

// TestHookClaimSkipsInputWithheldCandidate pins that a routed step whose
// workflow input is held is never claimed: the hook names it once on stderr,
// leaves it as it was, and claims the admissible row behind it.
func TestHookClaimSkipsInputWithheldCandidate(t *testing.T) {
	f := newAdmissionFixture(t, 1, "")
	var claimed []string
	ops := f.ops(t, &claimed)
	ops.Runner = func(string, string) (string, error) { return f.output(t, f.steps[0], f.free), nil }

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"worker"},
		JSON:         true,
	}, ops, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("doHookClaim = %d, want 0 (stderr=%q)", code, stderr.String())
	}
	if len(claimed) != 1 || claimed[0] != f.free.ID {
		t.Fatalf("claimed %v, want only the admissible %s (withheld %s)", claimed, f.free.ID, f.steps[0].ID)
	}
	line := "withheld " + f.steps[0].ID
	if strings.Count(stderr.String(), line) != 1 || !strings.Contains(stderr.String(), f.input.ID) {
		t.Fatalf("stderr = %q, want one %q line naming input %s", stderr.String(), line, f.input.ID)
	}
	f.requireUntouched(t)
}

// TestHookClaimWithheldAssignedHeadFallsThroughToRoutedTier pins AC-6's
// assigned head: the single-store tier answers with the seat's own open step,
// whose input is held, and stops there (--limit=1), so the hook re-asks the
// federated form, whose tiers admit before they bound, and claims the routed
// row it returns.
func TestHookClaimWithheldAssignedHeadFallsThroughToRoutedTier(t *testing.T) {
	f := newAdmissionFixture(t, 1, "worker-1")
	var claimed []string
	ops := f.ops(t, &claimed)
	var ran []string
	ops.Runner = func(command, _ string) (string, error) {
		ran = append(ran, command)
		if command == "federated" {
			return f.output(t, f.free), nil
		}
		return f.output(t, f.steps[0]), nil
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("single", "/rig", hookClaimOptions{
		Assignee:               "worker-1",
		RouteTargets:           []string{"worker"},
		JSON:                   true,
		AdmissionFallbackQuery: "federated",
	}, ops, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("doHookClaim = %d, want 0 (stderr=%q)", code, stderr.String())
	}
	if len(claimed) != 1 || claimed[0] != f.free.ID {
		t.Fatalf("claimed %v, want only %s from the federated re-read (ran %v)", claimed, f.free.ID, ran)
	}
	if strings.Contains(stdout.String(), f.steps[0].ID) {
		t.Fatalf("stdout = %q, handed back the withheld head %s", stdout.String(), f.steps[0].ID)
	}
	f.requireUntouched(t)
}

// TestHookClaimFederatedFallbackOnWithheldRoutedWindow pins AC-6's routed
// window through the production store loop: every row the single-store query
// returns is withheld, so the loop's runner re-asks with the federated form
// and the deeper admissible row is claimed.
func TestHookClaimFederatedFallbackOnWithheldRoutedWindow(t *testing.T) {
	f := newAdmissionFixture(t, 3, "")
	var claimed []string
	ops := f.ops(t, &claimed)
	run := func(command, _ string, _ []string) (string, error) {
		if command == "federated" {
			return f.output(t, f.free), nil
		}
		return f.output(t, f.steps...), nil
	}

	var stdout, stderr bytes.Buffer
	code := claimHookWorkWithRunner("single", "city", nil, []hookStore{{dir: "city"}}, hookClaimOptions{
		Assignee:               "worker-1",
		RouteTargets:           []string{"worker"},
		JSON:                   true,
		AdmissionFallbackQuery: "federated",
	}, ops, run, func(string, error) {}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("claimHookWorkWithRunner = %d, want 0 (stdout=%q stderr=%q)", code, stdout.String(), stderr.String())
	}
	if len(claimed) != 1 || claimed[0] != f.free.ID {
		t.Fatalf("claimed %v, want only %s", claimed, f.free.ID)
	}
	f.requireUntouched(t)
}

// TestHookClaimInputAdmissionReadErrorFailsLoud pins AC-8's hook half: a
// candidate whose input could not be judged is never claimed, and the hook
// fails with the judge's error, which names the bead and store.
func TestHookClaimInputAdmissionReadErrorFailsLoud(t *testing.T) {
	f := newAdmissionFixture(t, 1, "")
	var claimed []string
	ops := f.ops(t, &claimed)
	ops.Runner = func(string, string) (string, error) { return f.output(t, f.steps[0], f.free), nil }
	readErr := errors.New("judging input admission of " + f.steps[0].ID + ": work store: connection refused")
	ops.InputAdmission = func(context.Context, string, []string, []beads.Bead) (inputadmission.Verdicts, error) {
		return inputadmission.Verdicts{}, readErr
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"worker"},
		JSON:         true,
		DrainAck:     true,
	}, ops, &stdout, &stderr)

	if code == 0 {
		t.Fatalf("doHookClaim = 0, want a failure (stdout=%q)", stdout.String())
	}
	if len(claimed) != 0 {
		t.Fatalf("claimed %v after the admission read failed", claimed)
	}
	if !strings.Contains(stderr.String(), readErr.Error()) {
		t.Fatalf("stderr = %q, want the admission error %q", stderr.String(), readErr)
	}
	if strings.Contains(stdout.String(), "no_work") {
		t.Fatalf("stdout = %q, laundered an admission failure into a no_work drain", stdout.String())
	}
}

// TestClassRoutedHookWithholdsUnreadyInput pins the split-city seam: the
// override judges across the two legs a relocated workflow spans — the root
// and step in the binding, the held input in the work store — so the gate
// withholds the binding-resident step and writes nothing in either leg, and
// a clean input keeps the step served.
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
			base := f.baseOps(t)
			base.InputAdmission = func(context.Context, string, []string, []beads.Bead) (inputadmission.Verdicts, error) {
				t.Fatal("the split override judges across both legs, never through the base seam")
				return inputadmission.Verdicts{}, nil
			}
			ops := classRoutedHookClaimOps(base, f.route)
			var stderr bytes.Buffer
			kept, admission, err := withholdInputGatedCandidates([]beads.Bead{f.step}, hookClaimOptions{
				Assignee:     "worker-1",
				RouteTargets: []string{"worker"},
			}, ops, "/work", &stderr)
			if err != nil {
				t.Fatalf("withholdInputGatedCandidates: %v", err)
			}
			if len(kept) != tc.wantKept || admission.withheld != tc.hold || admission.deferred {
				t.Fatalf("kept %d candidates, admission=%+v (stderr=%q), want %d, withheld=%v, nothing deferred", len(kept), admission, stderr.String(), tc.wantKept, tc.hold)
			}
			if tc.hold {
				for _, want := range []string{"withheld " + f.step.ID, "input " + f.source.ID, "convoy " + f.convoyID, "workflow root " + f.root.ID} {
					if !strings.Contains(stderr.String(), want) {
						t.Fatalf("stderr = %q, want it to name %q", stderr.String(), want)
					}
				}
			}
			f.assertOpen(t, f.root.ID, f.step.ID) // the gate writes nothing
		})
	}
}

// TestDivergenceTreatsWithheldInputAsBenign pins the classifier's read of the
// claim path's own seam: a controller-counted row the hook could not claim
// because its input is not ready is correct pull, the bucket a held row lands
// in, and a judgment that fails is a failed read of the trigger, never either
// verdict. A row the seam leaves unresolved is no excuse: the hook takes the
// federated reader's verdict on it, the one the controller counted it by, so
// failing to claim it is a divergence.
func TestDivergenceTreatsWithheldInputAsBenign(t *testing.T) {
	routed := map[string]string{beadmeta.RoutedToMetadataKey: "rig/worker"}
	bead := beads.Bead{ID: "wb-1", Status: "open", Type: "task", Metadata: routed}
	for _, tt := range []struct {
		name      string
		verdicts  inputadmission.Verdicts
		err       error
		wantClass string
		wantStat  string
	}{
		{
			name:      "withheld is benign",
			verdicts:  inputadmission.Verdicts{Withheld: map[string]inputadmission.Withhold{"wb-1": {Reason: inputadmission.ReasonHeld, RootID: "r", ConvoyID: "c", InputID: "i"}}},
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
			verdicts:  inputadmission.Verdicts{Withheld: map[string]inputadmission.Withhold{}},
			wantClass: events.DemandClaimDivergence,
			wantStat:  "open",
		},
		{
			name:      "unresolved is divergence",
			verdicts:  inputadmission.Verdicts{Unresolved: map[string]inputadmission.Unresolved{"wb-1": {RootID: "r", ConvoyID: "c", InputID: "i", TargetID: "mc-1"}}},
			wantClass: events.DemandClaimDivergence,
			wantStat:  "open",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ops := demandDivergenceOpsForBlockedBead(bead, nil, false, nil)
			ops.InputAdmission = func(context.Context, string, []string, []beads.Bead) (inputadmission.Verdicts, error) {
				return tt.verdicts, tt.err
			}
			status, class := classifyDemandTrigger(bead.ID, "/rig", divergenceOptions(), ops)
			if class != tt.wantClass || status != tt.wantStat {
				t.Fatalf("classification = (%q, %q), want (%q, %q)", status, class, tt.wantStat, tt.wantClass)
			}
		})
	}
}

// TestDoHookClaimServesUnsubjectRowsThroughRealJudge pins the hook's
// non-subject arms through the real judge over the fixture's own store: a
// control-kind row of the held workflow is claimed and an in_progress row owned
// by this identity is adopted exactly as before the gate — control kinds are
// dispatcher bookkeeping and in_progress is existing ownership, neither is
// judged — while the same workflow's open step stays withheld.
func TestDoHookClaimServesUnsubjectRowsThroughRealJudge(t *testing.T) {
	opts := hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		JSON:               true,
	}
	t.Run("control-kind row claimed", func(t *testing.T) {
		f := newAdmissionFixture(t, 1, "")
		control, err := f.store.Create(beads.Bead{Title: "control row", Type: "task", Metadata: map[string]string{
			beadmeta.KindMetadataKey:       beadmeta.KindDrain,
			beadmeta.RootBeadIDMetadataKey: f.root.ID,
			beadmeta.RoutedToMetadataKey:   "worker",
		}})
		if err != nil {
			t.Fatal(err)
		}
		var claimed []string
		ops := f.ops(t, &claimed)
		ops.Runner = func(string, string) (string, error) { return f.output(t, f.steps[0], control), nil }
		var stdout, stderr bytes.Buffer
		if code := doHookClaim("query", "/rig", opts, ops, &stdout, &stderr); code != 0 {
			t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
		}
		if len(claimed) != 1 || claimed[0] != control.ID {
			t.Fatalf("claimed %v, want only the control row %s", claimed, control.ID)
		}
		if !strings.Contains(stderr.String(), "withheld "+f.steps[0].ID) {
			t.Fatalf("stderr = %q, want the withheld open step named", stderr.String())
		}
		f.requireUntouched(t)
	})
	t.Run("owned in_progress row adopted", func(t *testing.T) {
		f := newAdmissionFixture(t, 1, "")
		inProgress, assignee := "in_progress", "worker-1"
		if err := f.store.Update(f.steps[0].ID, beads.UpdateOpts{Status: &inProgress, Assignee: &assignee}); err != nil {
			t.Fatal(err)
		}
		owned, err := f.store.Get(f.steps[0].ID) // the live row a work query would serve
		if err != nil {
			t.Fatal(err)
		}
		var claimed []string
		ops := f.ops(t, &claimed)
		ops.Runner = func(string, string) (string, error) { return f.output(t, owned), nil }
		var stdout, stderr bytes.Buffer
		if code := doHookClaim("query", "/rig", opts, ops, &stdout, &stderr); code != 0 {
			t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
		}
		if len(claimed) != 0 {
			t.Fatalf("claimed %v, want the owned in_progress row adopted without a claim", claimed)
		}
		if !strings.Contains(stdout.String(), "existing_assignment") || !strings.Contains(stdout.String(), owned.ID) {
			t.Fatalf("stdout = %q, want the owned row adopted as existing_assignment", stdout.String())
		}
		if strings.Contains(stderr.String(), "withheld "+owned.ID) {
			t.Fatalf("stderr = %q, want the owned row never withheld", stderr.String())
		}
	})
}

// TestDoHookClaimRetiresTerminalInputBeforeAdmissionGate pins the retirement
// arm's order: an all-terminal input retires the workflow — root and open step
// closed as skipped — before the admission gate runs, exactly the pre-gate
// behavior, so the gate sees no candidate and no claim is attempted.
func TestDoHookClaimRetiresTerminalInputBeforeAdmissionGate(t *testing.T) {
	f := newAdmissionFixture(t, 1, "")
	if err := f.store.Close(f.input.ID); err != nil {
		t.Fatalf("closing the input: %v", err)
	}
	var claimed []string
	ops := f.ops(t, &claimed)
	ops.RetireWorkflow = func(ctx context.Context, _ string, _ []string, verdict workflowInputVerdict) error {
		if verdict.State == workflowInputPurged {
			return failPurgedWorkflow(ctx, f.store, verdict.RootID)
		}
		return skipDoneWorkflow(f.store, verdict.RootID)
	}
	judged := false
	judge := ops.InputAdmission
	ops.InputAdmission = func(ctx context.Context, dir string, env []string, candidates []beads.Bead) (inputadmission.Verdicts, error) {
		judged = true
		return judge(ctx, dir, env, candidates)
	}
	ops.Runner = func(string, string) (string, error) { return f.output(t, f.steps[0]), nil }
	var stdout, stderr bytes.Buffer
	if code := doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		DrainAck:           true,
		JSON:               true,
	}, ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if len(claimed) != 0 {
		t.Fatalf("claimed %v, want retirement to close the workflow before any claim", claimed)
	}
	if judged {
		t.Fatal("the admission gate judged a candidate of a retired workflow")
	}
	if !strings.Contains(stderr.String(), "input convoy work is already terminal") {
		t.Fatalf("stderr = %q, want the retirement line", stderr.String())
	}
	for _, id := range []string{f.root.ID, f.steps[0].ID} {
		got, err := f.store.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "closed" || got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeSkipped {
			t.Fatalf("%s: status=%q outcome=%q, want closed/%s", id, got.Status, got.Metadata[beadmeta.OutcomeMetadataKey], beadmeta.OutcomeSkipped)
		}
	}
}

// crossStorePrerequisiteFixture is a rig workflow whose input waits on a
// prerequisite: the input, its convoy, the root and an open step routed to
// rig/worker in the rig store, and the prerequisite in home — the city store,
// or the rig store itself.
type crossStorePrerequisiteFixture struct {
	rig, city, home *beads.MemStore
	step, prereq    beads.Bead
}

// newCrossStorePrerequisiteFixture seeds it. The input's blocks edge names the
// prerequisite plainly, or as "external:<id>" when external.
func newCrossStorePrerequisiteFixture(t *testing.T, inOwnStore, external bool) crossStorePrerequisiteFixture {
	t.Helper()
	rig := beads.NewMemStore()
	rig.IDPrefix = "ga"
	city := beads.NewMemStore()
	city.IDPrefix = "mc"
	input := mustAdmissionCreate(t, rig, beads.Bead{Title: "input"})
	conv := mustAdmissionCreate(t, rig, beads.Bead{Title: "input convoy", Type: "convoy"})
	if err := rig.DepAdd(conv.ID, input.ID, "tracks"); err != nil {
		t.Fatal(err)
	}
	root := mustAdmissionCreate(t, rig, beads.Bead{Title: "root", Type: "molecule", Metadata: map[string]string{beadmeta.InputConvoyIDMetadataKey: conv.ID}})
	step := mustAdmissionCreate(t, rig, beads.Bead{Title: "step", Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey: root.ID,
		beadmeta.RoutedToMetadataKey:   "rig/worker",
	}})
	home := city
	if inOwnStore {
		home = rig
	}
	prereq := mustAdmissionCreate(t, home, beads.Bead{Title: "prerequisite"})
	target := prereq.ID
	if external {
		target = "external:" + prereq.ID
	}
	if err := rig.DepAdd(input.ID, target, "blocks"); err != nil {
		t.Fatal(err)
	}
	return crossStorePrerequisiteFixture{rig: rig, city: city, home: home, step: step, prereq: prereq}
}

// rigJudge is the hook's default seam for a claim from the rig store: one
// judge over that store alone (hookInputAdmissionWithBdStore).
func (f crossStorePrerequisiteFixture) rigJudge(ctx context.Context, _ string, _ []string, candidates []beads.Bead) (inputadmission.Verdicts, error) {
	return inputadmission.New([]inputadmission.Source{{Label: "rig", Reader: f.rig}}, time.Now()).Verdicts(ctx, 0, candidates)
}

// TestHookClaimDefersPrerequisitesItsStoreCannotSeeToTheFederatedReader pins
// the hook half of the prerequisite rule (#8832 review). The hook judges over
// the store it claims from, so an input prerequisite only another store holds
// (plain or "external:", open or closed) is unresolved there, and the hook
// takes the federated reader's verdict on it instead of its own. A claim
// whose query has a federated form defers the step to that form. An answer
// that is already the federated reader's keeps it, because that reader judged
// every store. A custom query has no federated verdict to take, so it keeps
// the step and says so. A prerequisite the hook's own store holds open
// withholds the step whatever the query.
func TestHookClaimDefersPrerequisitesItsStoreCannotSeeToTheFederatedReader(t *testing.T) {
	single := hookClaimOptions{AdmissionFallbackQuery: "federated"}
	federated := hookClaimOptions{}
	custom := hookClaimOptions{AdmissionBlindQuery: true}
	for _, tc := range []struct {
		name       string
		inOwnStore bool
		closed     bool
		external   bool
		opts       hookClaimOptions
		want       string // what the claim does with the step: deferred, kept or withheld
		stderr     string // a fragment of the line naming it; "" for none
	}{
		{name: "open prerequisite in another store, single-store query", opts: single, want: "deferred", stderr: "deferred"},
		{name: "open external: prerequisite in another store, single-store query", external: true, opts: single, want: "deferred", stderr: "deferred"},
		{name: "closed prerequisite in another store, single-store query", closed: true, opts: single, want: "deferred", stderr: "deferred"},
		{name: "closed external: prerequisite in another store, single-store query", closed: true, external: true, opts: single, want: "deferred", stderr: "deferred"},
		{name: "open prerequisite in another store, federated answer", opts: federated, want: "kept"},
		{name: "open prerequisite in another store, custom query", opts: custom, want: "kept", stderr: "custom work_query"},
		{name: "open prerequisite in the hook's own store, single-store query", inOwnStore: true, opts: single, want: "withheld", stderr: "withheld"},
		{name: "open prerequisite in the hook's own store, federated answer", inOwnStore: true, opts: federated, want: "withheld", stderr: "withheld"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCrossStorePrerequisiteFixture(t, tc.inOwnStore, tc.external)
			if tc.closed {
				if err := f.home.Close(f.prereq.ID); err != nil {
					t.Fatal(err)
				}
			}
			opts := tc.opts
			opts.Assignee = "worker-1"
			opts.RouteTargets = []string{"rig/worker"}
			var stderr bytes.Buffer
			kept, admission, err := withholdInputGatedCandidates([]beads.Bead{f.step}, opts, hookClaimOps{InputAdmission: f.rigJudge}, "/rig", &stderr)
			if err != nil {
				t.Fatalf("withholdInputGatedCandidates: %v", err)
			}
			got := "kept"
			switch {
			case admission.withheld && admission.deferred:
				t.Fatalf("admission = %+v: one step both withheld and deferred", admission)
			case admission.withheld:
				got = "withheld"
			case admission.deferred:
				got = "deferred"
			}
			if got != tc.want || (len(kept) == 1) != (tc.want == "kept") {
				t.Fatalf("step %s, %d kept (stderr=%q), want %s", got, len(kept), stderr.String(), tc.want)
			}
			if (tc.stderr == "" && stderr.Len() != 0) || !strings.Contains(stderr.String(), tc.stderr) {
				t.Fatalf("stderr = %q, want it to name the step with %q", stderr.String(), tc.stderr)
			}
			if tc.stderr != "" && !strings.Contains(stderr.String(), f.step.ID) {
				t.Fatalf("stderr = %q, want it to name %s", stderr.String(), f.step.ID)
			}
			// The federated reader judges over both stores: it withholds
			// exactly the open prerequisites, wherever they live, so the
			// verdict a deferred step waits for is the one demand counts by.
			fed, err := inputadmission.New([]inputadmission.Source{
				{Label: "rig", Reader: f.rig}, {Label: "city", Reader: f.city},
			}, time.Now()).Judge(context.Background(), 0, []beads.Bead{f.step})
			if err != nil {
				t.Fatal(err)
			}
			if _, fedHeld := fed[f.step.ID]; fedHeld == tc.closed {
				t.Fatalf("federated verdict withheld=%t, want %t", fedHeld, !tc.closed)
			}
		})
	}
}

// TestHookClaimTakesTheFederatedVerdictOnACrossStorePrerequisite is the
// claim-path regression for #8832's cross-store prerequisite (review
// iteration 2), through the production store loop. A rig seat's single-store
// query returns a routed step whose input waits on a prerequisite in the city
// store. The claim defers the step to the federated form of its query and
// claims only from that answer, the federated reader's (readywork.Read over
// both stores, which gc ready runs). While the prerequisite is open, no claim
// is written and the step stays as it was; once it closes, the next poll
// claims the step.
func TestHookClaimTakesTheFederatedVerdictOnACrossStorePrerequisite(t *testing.T) {
	f := newCrossStorePrerequisiteFixture(t, false, false)
	// "single" stands in for bd ready in the rig store, which reads that store
	// alone; "federated" is the reader the federated form runs.
	legs := map[string][]readywork.Leg{
		"single":    {{Label: "rig", Store: f.rig}},
		"federated": {{Label: "rig", Store: f.rig}, {Label: "city", Store: f.city}},
	}
	query := readywork.Query{MetadataFields: []string{beadmeta.RoutedToMetadataKey + "=rig/worker"}, Unassigned: true}
	var ran []string
	run := func(command, _ string, _ []string) (string, error) {
		ran = append(ran, command)
		rows, err := readywork.Read(legs[command], query)
		if err != nil {
			return "", err
		}
		out, err := json.Marshal(rows)
		return string(out), err
	}
	var claimed []string
	ops := admissionFixture{store: f.rig}.ops(t, &claimed)
	ops.InputAdmission = f.rigJudge
	ops.Claim = func(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, bool, error) {
		claimed = append(claimed, id)
		inProgress := "in_progress"
		if err := f.rig.Update(id, beads.UpdateOpts{Status: &inProgress, Assignee: &assignee}); err != nil {
			return beads.Bead{}, false, err
		}
		b, err := f.rig.Get(id)
		return b, true, err
	}
	ops.ReadWorkMeta = func(_ context.Context, _ string, _ []string, id, _ string) (beads.Bead, error) { return f.rig.Get(id) }
	ops.WorkflowStore = func(context.Context, string, []string, string, string) beads.Store { return f.rig }
	ops.StampSessionClaim = func(string, string) error { return nil }
	ops.EmitExecutionStepStarted = func(beads.Bead, string, []string, string) {}
	ops.EnqueueContinuationNudge = func(string, string, io.Writer) {}
	ops.ResolveSessionWorkDir = func(string) string { return "" }
	poll := func() (stdout, stderr string) {
		t.Helper()
		ran = nil
		var out, errOut bytes.Buffer
		code := claimHookWorkWithRunner("single", "rig", nil, []hookStore{{dir: "rig"}}, hookClaimOptions{
			Assignee:               "worker-1",
			RouteTargets:           []string{"rig/worker"},
			JSON:                   true,
			DrainAck:               true,
			AdmissionFallbackQuery: "federated",
		}, ops, run, func(string, error) {}, &out, &errOut)
		if code != 0 {
			t.Fatalf("claimHookWorkWithRunner = %d, want 0 (stdout=%q stderr=%q)", code, out.String(), errOut.String())
		}
		return out.String(), errOut.String()
	}

	stdout, stderr := poll()
	if len(claimed) != 0 {
		t.Fatalf("claimed %v while the city prerequisite %s is open (ran %v, stderr=%q)", claimed, f.prereq.ID, ran, stderr)
	}
	if got, err := f.rig.Get(f.step.ID); err != nil || got.Status != "open" || got.Assignee != "" {
		t.Fatalf("step %s = %+v (%v), want it open and unassigned while its prerequisite is open", f.step.ID, got, err)
	}
	if !strings.Contains(stderr, "deferred "+f.step.ID) || !strings.Contains(stdout, hookClaimReasonNoWork) || !slices.Contains(ran, "federated") {
		t.Fatalf("ran %v, stdout=%q stderr=%q, want the step deferred to the federated form and a no_work drain", ran, stdout, stderr)
	}

	if err := f.city.Close(f.prereq.ID); err != nil {
		t.Fatal(err)
	}
	stdout, stderr = poll()
	if len(claimed) != 1 || claimed[0] != f.step.ID {
		t.Fatalf("claimed %v once the prerequisite closed, want only %s (ran %v, stdout=%q stderr=%q)", claimed, f.step.ID, ran, stdout, stderr)
	}
	if got, err := f.rig.Get(f.step.ID); err != nil || got.Status != "in_progress" || got.Assignee != "worker-1" {
		t.Fatalf("step %s = %+v (%v), want it claimed by worker-1", f.step.ID, got, err)
	}
	if !strings.Contains(stdout, f.step.ID) {
		t.Fatalf("stdout = %q, want the claimed step %s handed out", stdout, f.step.ID)
	}
}

// TestHookClaimReAsksPastAdmissibleSurvivorsOfADeferral pins the half of the
// re-ask rule a single-candidate answer never reaches: a single-store answer
// that defers one candidate to the federated reader re-asks the federated
// form even though an admissible candidate survived beside it, and claims
// from that form's answer rather than from the survivor.
func TestHookClaimReAsksPastAdmissibleSurvivorsOfADeferral(t *testing.T) {
	f := newCrossStorePrerequisiteFixture(t, false, false)
	routed := map[string]string{beadmeta.RoutedToMetadataKey: "rig/worker"}
	survivor := mustAdmissionCreate(t, f.rig, beads.Bead{Title: "survivor", Metadata: routed})
	federatedRow := mustAdmissionCreate(t, f.rig, beads.Bead{Title: "federated row", Metadata: routed})
	answers := map[string][]beads.Bead{
		// The rig store's single-store answer: the step, whose prerequisite
		// only the city store holds, ahead of an admissible row.
		"single": {f.step, survivor},
		// The federated form's answer, judged over both stores and bounded
		// after admission. Its row is the rig store's, so the leg claims it.
		"federated": {federatedRow},
	}
	var ran, claimed []string
	run := func(command, _ string, _ []string) (string, error) {
		ran = append(ran, command)
		out, err := json.Marshal(answers[command])
		return string(out), err
	}
	ops := admissionFixture{store: f.rig}.ops(t, &claimed)
	ops.InputAdmission = f.rigJudge

	var stdout, stderr bytes.Buffer
	code := claimHookWorkWithRunner("single", "rig", nil, []hookStore{{dir: "rig"}}, hookClaimOptions{
		Assignee:               "worker-1",
		RouteTargets:           []string{"rig/worker"},
		JSON:                   true,
		AdmissionFallbackQuery: "federated",
	}, ops, run, func(string, error) {}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("claimHookWorkWithRunner = %d, want 0 (stdout=%q stderr=%q)", code, stdout.String(), stderr.String())
	}
	if !slices.Contains(ran, "federated") || !strings.Contains(stderr.String(), "deferred "+f.step.ID) {
		t.Fatalf("ran %v, stderr=%q, want the deferred %s to re-ask the federated form although %s survived", ran, stderr.String(), f.step.ID, survivor.ID)
	}
	if !slices.Equal(claimed, []string{federatedRow.ID}) {
		t.Fatalf("claimed %v, want only %s from the federated answer, not the survivor %s", claimed, federatedRow.ID, survivor.ID)
	}
}

// crossLegReAskFixture is a city with no class binding and the two hook legs
// hookWorkQueryStores gives a rig-scoped pool, the rig store first and the
// city store second. The rig store holds a workflow step routed to
// rig/worker, ahead of all other work (priority 0), whose input the rig
// leg's judge withholds (held) or cannot resolve (its prerequisite is open in
// the city store). The city store holds claimable rows routed to rig/worker.
type crossLegReAskFixture struct {
	stores   map[string]*beads.MemStore // by leg dir: "rig", "city"
	step     beads.Bead
	prereq   beads.Bead // the prerequisite of the step's input, unless held
	cityRows []string
}

// newCrossLegReAskFixture seeds it; held parks the step's input on
// hold:external instead of giving it a city-store prerequisite.
func newCrossLegReAskFixture(t *testing.T, held bool) crossLegReAskFixture {
	t.Helper()
	rig, city := beads.NewMemStore(), beads.NewMemStore()
	rig.IDPrefix, city.IDPrefix = "ga", "mc"
	f := crossLegReAskFixture{stores: map[string]*beads.MemStore{"rig": rig, "city": city}}
	input := beads.Bead{Title: "input"}
	if held {
		input.Labels = []string{"hold:external"}
	}
	in := mustAdmissionCreate(t, rig, input)
	conv := mustAdmissionCreate(t, rig, beads.Bead{Title: "input convoy", Type: "convoy"})
	if err := rig.DepAdd(conv.ID, in.ID, "tracks"); err != nil {
		t.Fatal(err)
	}
	root := mustAdmissionCreate(t, rig, beads.Bead{Title: "root", Type: "molecule", Metadata: map[string]string{beadmeta.InputConvoyIDMetadataKey: conv.ID}})
	first, later := 0, 2
	f.step = mustAdmissionCreate(t, rig, beads.Bead{Title: "step", Priority: &first, Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey: root.ID,
		beadmeta.RoutedToMetadataKey:   "rig/worker",
	}})
	if !held {
		f.prereq = mustAdmissionCreate(t, city, beads.Bead{Title: "prerequisite"})
		if err := rig.DepAdd(in.ID, f.prereq.ID, "blocks"); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 3 {
		f.cityRows = append(f.cityRows, mustAdmissionCreate(t, city, beads.Bead{Title: fmt.Sprintf("city work %d", i), Priority: &later, Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey: "rig/worker",
		}}).ID)
	}
	return f
}

// run answers the work query in one leg, recording "<leg>:<command>" in ran.
// "single" stands in for bd ready in that leg's store: its open, unassigned
// rows routed to rig/worker, in ready order, with no input admission.
// "federated" is the reader the federated form runs, readywork.Read over both
// stores, which judges input over both.
func (f crossLegReAskFixture) run(ran *[]string) hookStoreRunner {
	return func(command, dir string, _ []string) (string, error) {
		*ran = append(*ran, dir+":"+command)
		if command == "federated" {
			rows, err := readywork.Read([]readywork.Leg{{Label: "rig", Store: f.stores["rig"]}, {Label: "city", Store: f.stores["city"]}},
				readywork.Query{MetadataFields: []string{beadmeta.RoutedToMetadataKey + "=rig/worker"}, Unassigned: true})
			if err != nil {
				return "", err
			}
			out, err := json.Marshal(rows)
			return string(out), err
		}
		open, err := f.stores[dir].List(beads.ListQuery{Status: "open"})
		if err != nil {
			return "", err
		}
		var rows []beads.Bead
		for _, b := range open {
			if b.Assignee == "" && b.Metadata[beadmeta.RoutedToMetadataKey] == "rig/worker" {
				rows = append(rows, b)
			}
		}
		beads.SortBeadsReadyOrder(rows)
		out, err := json.Marshal(rows)
		return string(out), err
	}
}

// ops is a claim path over both legs as the production seams read them: a
// leg's admission judge, workflow store and readback read its own store
// alone. A claim writes "<leg>:<id>" to claimed; in a leg whose store lacks
// the id it fails not found, as BdStore.Claim does there, and is recorded in
// wrongLeg. Every bead.claim_failed is recorded in failed.
func (f crossLegReAskFixture) ops(t *testing.T, claimed, wrongLeg, failed *[]string) hookClaimOps {
	t.Helper()
	ops := admissionFixture{store: f.stores["rig"]}.ops(t, new([]string))
	ops.InputVerdict = func(ctx context.Context, dir string, _ []string, candidate beads.Bead, identityCandidates []string, _ workflowInputAbsenceProver) (workflowInputVerdict, error) {
		return workflowInputDone(ctx, beadstest.Work(f.stores[dir]), candidate, identityCandidates, nil)
	}
	ops.InputAdmission = func(ctx context.Context, dir string, _ []string, candidates []beads.Bead) (inputadmission.Verdicts, error) {
		return inputadmission.New([]inputadmission.Source{{Label: dir, Reader: f.stores[dir]}}, time.Now()).Verdicts(ctx, 0, candidates)
	}
	ops.WorkflowStore = func(_ context.Context, dir string, _ []string, _, _ string) beads.Store { return f.stores[dir] }
	ops.Claim = func(_ context.Context, dir string, _ []string, id, assignee string) (beads.Bead, bool, error) {
		store := f.stores[dir]
		if _, err := store.Get(id); err != nil {
			*wrongLeg = append(*wrongLeg, dir+":"+id)
			return beads.Bead{}, false, fmt.Errorf("claiming bead %q: %w", id, beads.ErrNotFound)
		}
		inProgress := "in_progress"
		if err := store.Update(id, beads.UpdateOpts{Status: &inProgress, Assignee: &assignee}); err != nil {
			return beads.Bead{}, false, err
		}
		*claimed = append(*claimed, dir+":"+id)
		b, err := store.Get(id)
		return b, true, err
	}
	ops.EmitClaimFailed = func(id string, _ hookClaimOptions, err error) { *failed = append(*failed, id+": "+err.Error()) }
	ops.ReadWorkMeta = func(_ context.Context, dir string, _ []string, id, _ string) (beads.Bead, error) {
		return f.stores[dir].Get(id)
	}
	ops.StampSessionClaim = func(string, string) error { return nil }
	ops.EmitExecutionStepStarted = func(beads.Bead, string, []string, string) {}
	ops.EnqueueContinuationNudge = func(string, string, io.Writer) {}
	ops.ResolveSessionWorkDir = func(string) string { return "" }
	return ops
}

// TestHookClaimReAskClaimsOnlyTheRowsItsLegHolds is the regression for the
// cross-leg re-ask (#9552 review, iteration 3). A rig seat's hook on a city
// with no class binding runs its store loop over the rig leg, then the city
// leg. The rig leg's screen re-asks the federated form, for a step it defers
// to the federated reader or for a step that is every candidate it has and is
// withheld. That form's answer spans the city, and claiming its city rows
// through the rig leg's store failed each one not found: a bead.claim_failed
// the claim-loop guard counts against a bead that is claimable. The re-ask
// keeps only the rows its leg's store holds, so each seat claims a city row
// through the city leg, no claim fails, and the rig step is left as it was
// until a deferred step's prerequisite closes; then the rig leg claims it.
func TestHookClaimReAskClaimsOnlyTheRowsItsLegHolds(t *testing.T) {
	for _, tc := range []struct {
		name string
		held bool
		line string // how the rig leg's screen names the step
	}{
		{name: "rig step deferred to the federated reader", line: "deferred"},
		{name: "every rig candidate withheld", held: true, line: "withheld"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCrossLegReAskFixture(t, tc.held)
			var ran, claimed, wrongLeg, failed []string
			ops := f.ops(t, &claimed, &wrongLeg, &failed)
			poll := func(seat int) {
				t.Helper()
				ran = nil
				var stdout, stderr bytes.Buffer
				code := claimHookWorkWithRunner("single", "rig", nil, []hookStore{{dir: "rig"}, {dir: "city"}}, hookClaimOptions{
					Assignee:               fmt.Sprintf("worker-%d", seat),
					RouteTargets:           []string{"rig/worker"},
					JSON:                   true,
					AdmissionFallbackQuery: "federated",
				}, ops, f.run(&ran), func(string, error) {}, &stdout, &stderr)
				if code != 0 {
					t.Fatalf("seat %d: claimHookWorkWithRunner = %d, want 0 (stdout=%q stderr=%q)", seat, code, stdout.String(), stderr.String())
				}
				if !slices.Contains(ran, "rig:federated") || !strings.Contains(stderr.String(), tc.line+" "+f.step.ID) {
					t.Fatalf("seat %d: ran %v, stderr=%q, want the rig leg to name %s %s and re-ask the federated form", seat, ran, stderr.String(), tc.line, f.step.ID)
				}
			}

			for seat := 1; seat <= len(f.cityRows); seat++ {
				poll(seat)
			}
			if len(wrongLeg) != 0 || len(failed) != 0 {
				t.Fatalf("claims in a leg whose store lacks the id %v, claim_failed %v; want none", wrongLeg, failed)
			}
			want := make([]string, 0, len(f.cityRows))
			for _, id := range f.cityRows {
				want = append(want, "city:"+id)
			}
			if got := slices.Sorted(slices.Values(claimed)); !slices.Equal(got, want) {
				t.Fatalf("claimed %v, want each seat to claim one city row through the city leg: %v", claimed, want)
			}
			if got, err := f.stores["rig"].Get(f.step.ID); err != nil || got.Status != "open" || got.Assignee != "" {
				t.Fatalf("rig step %s = %+v (%v), want it open and unassigned", f.step.ID, got, err)
			}
			if tc.held {
				return
			}

			if err := f.stores["city"].Close(f.prereq.ID); err != nil {
				t.Fatal(err)
			}
			poll(len(f.cityRows) + 1)
			if len(wrongLeg) != 0 || len(failed) != 0 || claimed[len(claimed)-1] != "rig:"+f.step.ID {
				t.Fatalf("after the prerequisite closed: claimed %v, wrong-leg claims %v, claim_failed %v; want the rig leg to claim %s", claimed, wrongLeg, failed, f.step.ID)
			}
		})
	}
}

// issueTierBatchStore answers its exact batch read from the issue tier only,
// as bd's batch read (`bd show`) may: a wisp it holds is missing from the
// batch and found only by Get. failBatch and failGet fail those reads.
type issueTierBatchStore struct {
	*beads.MemStore
	failBatch, failGet error
}

func (s *issueTierBatchStore) GetExactBatch(ids []string) (map[string]beads.Bead, []string, error) {
	if s.failBatch != nil {
		return nil, nil, s.failBatch
	}
	found := make(map[string]beads.Bead, len(ids))
	var unresolved []string
	for _, id := range ids {
		if b, err := s.MemStore.Get(id); err == nil && !b.Ephemeral {
			found[id] = b
			continue
		}
		unresolved = append(unresolved, id)
	}
	return found, unresolved, nil
}

func (s *issueTierBatchStore) Get(id string) (beads.Bead, error) {
	if s.failGet != nil {
		return beads.Bead{}, s.failGet
	}
	return s.MemStore.Get(id)
}

// TestKeepHookRowsTheLegHolds pins how a federated re-ask decides what its
// leg may claim: the rows the leg's store holds, in answer order, a wisp
// included when the store's batch read misses it, and none of the rows
// another store holds. A read that fails is an error naming the leg.
func TestKeepHookRowsTheLegHolds(t *testing.T) {
	rig, city := beads.NewMemStore(), beads.NewMemStore()
	rig.IDPrefix, city.IDPrefix = "ga", "mc"
	cityRow := mustAdmissionCreate(t, city, beads.Bead{Title: "city row"})
	rigRow := mustAdmissionCreate(t, rig, beads.Bead{Title: "rig row"})
	cityWisp := mustAdmissionCreate(t, city, beads.Bead{Title: "city wisp", Ephemeral: true})
	rigWisp := mustAdmissionCreate(t, rig, beads.Bead{Title: "rig wisp", Ephemeral: true})
	keep := func(store beads.Store) ([]beads.Bead, error) {
		ops := hookClaimOps{WorkflowStore: func(context.Context, string, []string, string, string) beads.Store { return store }}
		return keepHookRowsTheLegHolds([]beads.Bead{cityRow, rigRow, cityWisp, rigWisp}, hookClaimOptions{}, ops, "/rig")
	}

	kept, err := keep(&issueTierBatchStore{MemStore: rig})
	if err != nil {
		t.Fatalf("keepHookRowsTheLegHolds: %v", err)
	}
	var got []string
	for _, row := range kept {
		got = append(got, row.ID)
	}
	if want := []string{rigRow.ID, rigWisp.ID}; !slices.Equal(got, want) {
		t.Fatalf("kept %v, want the rig store's rows %v, its wisp included", got, want)
	}

	readErr := errors.New("ledger unavailable")
	for name, store := range map[string]*issueTierBatchStore{
		"batch read fails": {MemStore: rig, failBatch: readErr},
		"wisp read fails":  {MemStore: rig, failGet: readErr},
	} {
		if kept, err := keep(store); !errors.Is(err, readErr) || !strings.Contains(err.Error(), "/rig") {
			t.Errorf("%s: kept %v, err %v; want the read error, naming the store at /rig", name, kept, err)
		}
	}
}

// TestHookClaimReAskFailsLoudWhenItsLegCannotBeRead pins the claim half of a
// failed residence read on the agent's own leg: a re-ask whose leg cannot say
// which federated rows it holds claims only what its own screen admitted
// (nothing here) and fails naming the read error, without the
// store-unavailable token, rather than claiming rows it may not hold or
// draining as if no work were ready.
func TestHookClaimReAskFailsLoudWhenItsLegCannotBeRead(t *testing.T) {
	f := newAdmissionFixture(t, 1, "")
	var claimed []string
	ops := f.ops(t, &claimed)
	readErr := errors.New("ledger unavailable")
	ops.WorkflowStore = func(context.Context, string, []string, string, string) beads.Store {
		return &issueTierBatchStore{MemStore: f.store, failBatch: readErr}
	}
	run := func(command, _ string, _ []string) (string, error) {
		if command == "federated" {
			return f.output(t, f.free), nil
		}
		return f.output(t, f.steps...), nil
	}

	var stdout, stderr bytes.Buffer
	code := claimHookWorkWithRunner("single", "city", nil, []hookStore{{dir: "city"}}, hookClaimOptions{
		Assignee:               "worker-1",
		RouteTargets:           []string{"worker"},
		JSON:                   true,
		DrainAck:               true,
		AdmissionFallbackQuery: "federated",
	}, ops, run, func(string, error) {}, &stdout, &stderr)

	if code == 0 {
		t.Fatalf("claimHookWorkWithRunner = 0, want a failure (stdout=%q stderr=%q)", stdout.String(), stderr.String())
	}
	if len(claimed) != 0 {
		t.Fatalf("claimed %v although the leg's residence read failed", claimed)
	}
	if !strings.Contains(stderr.String(), readErr.Error()) || strings.Contains(stderr.String(), hookStoreUnavailableToken) {
		t.Fatalf("stderr = %q, want the read error %q without the store-unavailable token", stderr.String(), readErr)
	}
	if strings.Contains(stdout.String(), hookClaimReasonNoWork) {
		t.Fatalf("stdout = %q, laundered a failed read into a no_work drain", stdout.String())
	}
	f.requireUntouched(t)
}

// mustAdmissionCreate creates b in store, a task unless b says otherwise.
func mustAdmissionCreate(t *testing.T, store beads.Store, b beads.Bead) beads.Bead {
	t.Helper()
	if b.Type == "" {
		b.Type = "task"
	}
	created, err := store.Create(b)
	if err != nil {
		t.Fatalf("creating %q: %v", b.Title, err)
	}
	return created
}

// errFederatedReadDown is the federated form failing as gc ready does when a
// store of the city it reads refuses connections.
var errFederatedReadDown = errors.New(`running work query "federated": exit status 1: gc ready: rig "beta" store: dial tcp 127.0.0.1:3311: connect: connection refused`)

// federatedReadFails is run with the federated form's read failing
// (errFederatedReadDown), recorded in ran as "<leg>:federated".
func federatedReadFails(run hookStoreRunner, ran *[]string) hookStoreRunner {
	return func(command, dir string, env []string) (string, error) {
		if command == "federated" {
			*ran = append(*ran, dir+":"+command)
			return "", errFederatedReadDown
		}
		return run(command, dir, env)
	}
}

// requireReAskFailureReported fails unless the failed re-ask from leg was
// named on stderr as a read of the federated form, without the
// store-unavailable token, and recorded as exactly one work-query-failure
// event naming leg.
func requireReAskFailureReported(t *testing.T, stderr string, emitted []error, leg string) {
	t.Helper()
	if strings.Contains(stderr, hookStoreUnavailableToken) {
		t.Fatalf("stderr = %q: a failed federated re-ask is a read elsewhere, never this session's own store failing", stderr)
	}
	if !strings.Contains(stderr, "federated work query re-asked from store "+leg) || !strings.Contains(stderr, "connection refused") {
		t.Fatalf("stderr = %q, want the failed re-ask from %s named with its cause", stderr, leg)
	}
	if len(emitted) != 1 {
		t.Fatalf("work-query-failure events %v, want exactly one, for the failed re-ask", emitted)
	}
	if reason, _ := classifyWorkQueryFailure(emitted[0]); !strings.Contains(reason, "federated work query re-asked from store "+leg) {
		t.Fatalf("work-query-failure event reason %q, want it to name the re-ask from %s", reason, leg)
	}
}

// TestHookClaimFailedReAskClaimsFromTheOtherLegs is the regression for a
// failed federated re-ask (#9552 review, iteration 4, probe P2). A rig seat's
// store loop on a city with no class binding selects the rig leg, whose head
// is a step its screen withholds or defers, and the rig leg's re-ask of the
// federated form fails: gc ready cannot read some store of the city. That is
// a failed read elsewhere, not the agent's own store failing, so it ends
// nothing. The rig leg claims what its own screen admitted (nothing here),
// the loop moves on, and the seat claims a city row through the city leg. The
// failure is named on stderr, without the store-unavailable token, and
// recorded as one work-query-failure event naming the rig leg.
func TestHookClaimFailedReAskClaimsFromTheOtherLegs(t *testing.T) {
	for _, tc := range []struct {
		name string
		held bool
		line string // how the rig leg's screen names the step
	}{
		{name: "rig step deferred to the federated reader", line: "deferred"},
		{name: "every rig candidate withheld", held: true, line: "withheld"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCrossLegReAskFixture(t, tc.held)
			var ran, claimed, wrongLeg, failed []string
			var emitted []error
			var stdout, stderr bytes.Buffer
			code := claimHookWorkWithRunner("single", "rig", nil, []hookStore{{dir: "rig"}, {dir: "city"}}, hookClaimOptions{
				Assignee:               "worker-1",
				RouteTargets:           []string{"rig/worker"},
				JSON:                   true,
				DrainAck:               true,
				AdmissionFallbackQuery: "federated",
			}, f.ops(t, &claimed, &wrongLeg, &failed), federatedReadFails(f.run(&ran), &ran), func(_ string, err error) { emitted = append(emitted, err) }, &stdout, &stderr)

			if code != 0 {
				t.Fatalf("claimHookWorkWithRunner = %d, want 0 (stdout=%q stderr=%q)", code, stdout.String(), stderr.String())
			}
			if !slices.Contains(ran, "rig:federated") || !strings.Contains(stderr.String(), tc.line+" "+f.step.ID) {
				t.Fatalf("ran %v, stderr=%q, want the rig leg to name %s %s and re-ask the federated form", ran, stderr.String(), tc.line, f.step.ID)
			}
			if len(claimed) != 1 || !slices.Contains(f.cityRows, strings.TrimPrefix(claimed[0], "city:")) || !strings.HasPrefix(claimed[0], "city:") {
				t.Fatalf("claimed %v, want one city row through the city leg (stdout=%q stderr=%q)", claimed, stdout.String(), stderr.String())
			}
			if len(wrongLeg) != 0 || len(failed) != 0 {
				t.Fatalf("claims in a leg whose store lacks the id %v, claim_failed %v; want none", wrongLeg, failed)
			}
			requireReAskFailureReported(t, stderr.String(), emitted, "rig")
			if got, err := f.stores["rig"].Get(f.step.ID); err != nil || got.Status != "open" || got.Assignee != "" {
				t.Fatalf("rig step %s = %+v (%v), want it open and unassigned", f.step.ID, got, err)
			}
		})
	}
}

// TestHookClaimFailedReAskClaimsWhatTheLegsScreenAdmitted is the
// single-store half (#9552 review, iteration 4, probe P3): the leg's answer
// defers a step to the federated reader ahead of a candidate its own judge
// admitted, and the re-ask fails. The admitted candidate is claimed; the
// deferred step stays refused.
func TestHookClaimFailedReAskClaimsWhatTheLegsScreenAdmitted(t *testing.T) {
	f := newCrossStorePrerequisiteFixture(t, false, false)
	survivor := mustAdmissionCreate(t, f.rig, beads.Bead{Title: "survivor", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "rig/worker"}})
	var ran, claimed []string
	run := func(command, dir string, _ []string) (string, error) {
		ran = append(ran, dir+":"+command)
		out, err := json.Marshal([]beads.Bead{f.step, survivor})
		return string(out), err
	}
	ops := admissionFixture{store: f.rig}.ops(t, &claimed)
	ops.InputAdmission = f.rigJudge
	var emitted []error
	var stdout, stderr bytes.Buffer
	code := claimHookWorkWithRunner("single", "rig", nil, []hookStore{{dir: "rig"}}, hookClaimOptions{
		Assignee:               "worker-1",
		RouteTargets:           []string{"rig/worker"},
		JSON:                   true,
		DrainAck:               true,
		AdmissionFallbackQuery: "federated",
	}, ops, federatedReadFails(run, &ran), func(_ string, err error) { emitted = append(emitted, err) }, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("claimHookWorkWithRunner = %d, want 0 (stdout=%q stderr=%q)", code, stdout.String(), stderr.String())
	}
	if !slices.Contains(ran, "rig:federated") || !strings.Contains(stderr.String(), "deferred "+f.step.ID) {
		t.Fatalf("ran %v, stderr=%q, want the deferred %s to re-ask the federated form", ran, stderr.String(), f.step.ID)
	}
	if !slices.Equal(claimed, []string{survivor.ID}) {
		t.Fatalf("claimed %v, want only the admitted %s, never the deferred %s", claimed, survivor.ID, f.step.ID)
	}
	requireReAskFailureReported(t, stderr.String(), emitted, "rig")
}

// TestHookClaimFailedReAskWithNothingClaimableDoesNotDrain: when the failed
// re-ask leaves nothing claimable anywhere, "no work" is not known (the
// re-ask was asked to settle rows input admission withheld or deferred), so
// the claim writes no drain result and acknowledges no drain, as for a failed
// secondary store with no work anywhere, and still prints no
// store-unavailable token.
func TestHookClaimFailedReAskWithNothingClaimableDoesNotDrain(t *testing.T) {
	f := newCrossLegReAskFixture(t, true)
	for _, id := range f.cityRows {
		if err := f.stores["city"].Close(id); err != nil {
			t.Fatal(err)
		}
	}
	var ran, claimed, wrongLeg, failed []string
	ops := f.ops(t, &claimed, &wrongLeg, &failed)
	acked := false
	ops.DrainAck = func(io.Writer) error { acked = true; return nil }
	var emitted []error
	var stdout, stderr bytes.Buffer
	code := claimHookWorkWithRunner("single", "rig", nil, []hookStore{{dir: "rig"}, {dir: "city"}}, hookClaimOptions{
		Assignee:               "worker-1",
		RouteTargets:           []string{"rig/worker"},
		JSON:                   true,
		DrainAck:               true,
		AdmissionFallbackQuery: "federated",
	}, ops, federatedReadFails(f.run(&ran), &ran), func(_ string, err error) { emitted = append(emitted, err) }, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("claimHookWorkWithRunner = %d, want 1: a claim whose re-ask failed and found nothing is a failed query (stdout=%q stderr=%q)", code, stdout.String(), stderr.String())
	}
	if stdout.Len() != 0 || acked {
		t.Fatalf("stdout = %q, drain acknowledged = %t; want no drain result: the withheld rig step's federated answer was never read", stdout.String(), acked)
	}
	if len(claimed) != 0 || len(wrongLeg) != 0 || len(failed) != 0 {
		t.Fatalf("claimed %v, wrong-leg claims %v, claim_failed %v; want none", claimed, wrongLeg, failed)
	}
	if reAsked := slices.Index(ran, "rig:federated"); reAsked < 0 || !slices.Contains(ran[reAsked:], "city:single") {
		t.Fatalf("ran %v, want the city leg read again after the rig leg's re-ask failed", ran)
	}
	requireReAskFailureReported(t, stderr.String(), emitted, "rig")
}

// TestHookClaimFailedResidenceReadOnAnotherLegClaimsElsewhere applies the
// rule to the residence read: the re-ask runs on a leg that is not the
// agent's own store, the federated form answers, but which of its rows that
// leg's store holds cannot be read. That leg claims what its own screen
// admitted (nothing), and the seat claims a row through the agent's own leg.
func TestHookClaimFailedResidenceReadOnAnotherLegClaimsElsewhere(t *testing.T) {
	f := newCrossLegReAskFixture(t, true)
	var ran, claimed, wrongLeg, failed []string
	ops := f.ops(t, &claimed, &wrongLeg, &failed)
	readErr := errors.New("rig ledger unavailable: connection refused")
	ops.WorkflowStore = func(_ context.Context, dir string, _ []string, _, _ string) beads.Store {
		if dir == "rig" {
			return &issueTierBatchStore{MemStore: f.stores["rig"], failBatch: readErr}
		}
		return f.stores[dir]
	}
	var emitted []error
	var stdout, stderr bytes.Buffer
	// The city store is the agent's own (first) leg; the rig leg's step, the
	// highest-priority row anywhere, has it selected first.
	code := claimHookWorkWithRunner("single", "city", nil, []hookStore{{dir: "city"}, {dir: "rig"}}, hookClaimOptions{
		Assignee:               "worker-1",
		RouteTargets:           []string{"rig/worker"},
		JSON:                   true,
		DrainAck:               true,
		AdmissionFallbackQuery: "federated",
	}, ops, f.run(&ran), func(_ string, err error) { emitted = append(emitted, err) }, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("claimHookWorkWithRunner = %d, want 0 (stdout=%q stderr=%q)", code, stdout.String(), stderr.String())
	}
	if !slices.Contains(ran, "rig:federated") {
		t.Fatalf("ran %v, want the rig leg selected first and its re-ask run", ran)
	}
	if len(claimed) != 1 || !strings.HasPrefix(claimed[0], "city:") || len(wrongLeg) != 0 || len(failed) != 0 {
		t.Fatalf("claimed %v, wrong-leg claims %v, claim_failed %v; want one city row through the city leg", claimed, wrongLeg, failed)
	}
	if strings.Contains(stderr.String(), hookStoreUnavailableToken) || !strings.Contains(stderr.String(), readErr.Error()) {
		t.Fatalf("stderr = %q, want the failed residence read named, without the store-unavailable token", stderr.String())
	}
	if len(emitted) != 1 {
		t.Fatalf("work-query-failure events %v, want one, for the rig leg's failed re-ask", emitted)
	}
}
