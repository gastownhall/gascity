package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/inputadmission"
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
// against it, every claim recorded in claimed.
func (f admissionFixture) ops(t *testing.T, claimed *[]string) hookClaimOps {
	t.Helper()
	return hookClaimOps{
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			*claimed = append(*claimed, beadID)
			return beads.Bead{ID: beadID, Assignee: assignee, Status: "in_progress"}, true, nil
		},
		InputVerdict: func(ctx context.Context, _ string, _ []string, candidate beads.Bead, identityCandidates []string, _ workflowInputAbsenceProver) (workflowInputVerdict, error) {
			return workflowInputDone(ctx, beadstest.Work(f.store), candidate, identityCandidates, nil)
		},
		RetireWorkflow: func(_ context.Context, _ string, _ []string, verdict workflowInputVerdict) error {
			t.Fatalf("unexpected retirement of %s", verdict.RootID)
			return nil
		},
		InputAdmission: func(ctx context.Context, _ string, _ []string, candidates []beads.Bead) (map[string]inputadmission.Withhold, error) {
			return inputadmission.New([]inputadmission.Source{{Label: "work", Reader: f.store}}, time.Now()).Judge(ctx, 0, candidates)
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
	ops.InputAdmission = func(context.Context, string, []string, []beads.Bead) (map[string]inputadmission.Withhold, error) {
		return nil, readErr
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
			base.InputAdmission = func(context.Context, string, []string, []beads.Bead) (map[string]inputadmission.Withhold, error) {
				t.Fatal("the split override judges across both legs, never through the base seam")
				return nil, nil
			}
			ops := classRoutedHookClaimOps(base, f.route)
			var stderr bytes.Buffer
			kept, withheld, err := withholdInputGatedCandidates([]beads.Bead{f.step}, hookClaimOptions{
				Assignee:     "worker-1",
				RouteTargets: []string{"worker"},
			}, ops, "/work", &stderr)
			if err != nil {
				t.Fatalf("withholdInputGatedCandidates: %v", err)
			}
			if len(kept) != tc.wantKept || withheld != tc.hold {
				t.Fatalf("kept %d candidates, withheld=%v (stderr=%q), want %d, %v", len(kept), withheld, stderr.String(), tc.wantKept, tc.hold)
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
// verdict.
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
	ops.InputAdmission = func(ctx context.Context, dir string, env []string, candidates []beads.Bead) (map[string]inputadmission.Withhold, error) {
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
