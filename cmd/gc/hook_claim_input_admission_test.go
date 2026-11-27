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
