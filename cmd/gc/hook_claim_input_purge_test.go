package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// purgedInputWorkflow is a graph.v2 workflow root whose input convoy id names
// convoyID, with an open step, an in_progress step and a step that already
// finished with gc.outcome=pass, all in store.
type purgedInputWorkflow struct {
	root, open, active, finished beads.Bead
}

func seedPurgedInputWorkflow(t *testing.T, store beads.Store, convoyID string) purgedInputWorkflow {
	t.Helper()
	var w purgedInputWorkflow
	var err error
	w.root, err = store.Create(beads.Bead{Title: "workflow root", Type: "task", Metadata: map[string]string{
		beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
		beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		beadmeta.InputConvoyIDMetadataKey:   convoyID,
		beadmeta.RoutedToMetadataKey:        "worker",
	}})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	step := func(title string) beads.Bead {
		b, err := store.Create(beads.Bead{Title: title, Type: "task", Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: w.root.ID,
			beadmeta.RoutedToMetadataKey:   "worker",
		}})
		if err != nil {
			t.Fatalf("create %s: %v", title, err)
		}
		return b
	}
	w.open = step("open step")
	w.active = step("active step")
	inProgress := "in_progress"
	holder := "worker-old"
	if err := store.Update(w.active.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &holder}); err != nil {
		t.Fatalf("start active step: %v", err)
	}
	w.finished = step("finished step")
	if _, err := store.CloseAll([]string{w.finished.ID}, map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass}); err != nil {
		t.Fatalf("finish step: %v", err)
	}
	if w.root, err = store.Get(w.root.ID); err != nil {
		t.Fatalf("reload root: %v", err)
	}
	return w
}

// assertPurgeRetired checks that every id closed with the purge disposition.
func assertPurgeRetired(t *testing.T, store beads.Store, ids ...string) {
	t.Helper()
	for _, id := range ids {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		want := map[string]string{
			beadmeta.OutcomeMetadataKey:       beadmeta.OutcomeFail,
			beadmeta.FailureClassMetadataKey:  beadmeta.FailureClassHard,
			beadmeta.FailureReasonMetadataKey: workflowInputPurgedFailureReason,
			"close_reason":                    sourceworkflow.HookInputPurgedWorkflowCloseReason,
		}
		if got.Status != "closed" {
			t.Errorf("%s status = %q, want closed", id, got.Status)
		}
		for k, v := range want {
			if got.Metadata[k] != v {
				t.Errorf("%s %s = %q, want %q", id, k, got.Metadata[k], v)
			}
		}
	}
}

// assertFinishedOutcomeKept checks that a step that finished before the
// retirement kept its recorded outcome.
func assertFinishedOutcomeKept(t *testing.T, store beads.Store, id string) {
	t.Helper()
	got, err := store.Get(id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	if got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass || got.Metadata[beadmeta.FailureReasonMetadataKey] != "" {
		t.Errorf("finished step %s metadata = %v; its recorded outcome must survive the retirement", id, got.Metadata)
	}
}

// recordingProver proves absent exactly the ids in absent and records each
// call.
type recordingProver struct {
	absent map[string]bool
	calls  [][]string
}

func (p *recordingProver) prove(_ context.Context, ids []string) (bool, error) {
	p.calls = append(p.calls, slices.Clone(ids))
	for _, id := range ids {
		if !p.absent[id] {
			return false, nil
		}
	}
	return true, nil
}

// A graph.v2 workflow whose input convoy is proven absent reads as purged,
// naming the convoy as the missing input.
func TestWorkflowInputVerdictPurgedWhenConvoyProvenAbsent(t *testing.T) {
	store := beads.NewMemStore()
	w := seedPurgedInputWorkflow(t, store, "gc-gone")
	prover := &recordingProver{absent: map[string]bool{"gc-gone": true}}

	for _, candidate := range []beads.Bead{w.root, w.open} {
		verdict, err := workflowInputDone(context.Background(), store, candidate, nil, prover.prove)
		if err != nil {
			t.Fatalf("workflowInputDone(%s): %v", candidate.ID, err)
		}
		if verdict.RootID != w.root.ID || verdict.State != workflowInputPurged || !slices.Equal(verdict.MissingIDs, []string{"gc-gone"}) {
			t.Fatalf("workflowInputDone(%s) = %+v, want purged root %s missing [gc-gone]", candidate.ID, verdict, w.root.ID)
		}
	}
}

// The purge retirement closes the root and every nonterminal step with the
// hard-failure disposition and leaves a finished step's outcome alone.
func TestFailPurgedWorkflowStampsHardFailureAndKeepsTerminalOutcomes(t *testing.T) {
	store := beads.NewMemStore()
	w := seedPurgedInputWorkflow(t, store, "gc-gone")

	if err := failPurgedWorkflow(context.Background(), store, w.root.ID); err != nil {
		t.Fatalf("failPurgedWorkflow: %v", err)
	}
	assertPurgeRetired(t, store, w.root.ID, w.open.ID, w.active.ID)
	assertFinishedOutcomeKept(t, store, w.finished.ID)
}

// purgeProofCity is a file-store city with one rig (ops, prefix OP) whose
// stores the city-wide absence prover reads, and its loaded config.
func purgeProofCity(t *testing.T, extraRigs string, rigDirs ...string) (string, *config.City) {
	t.Helper()
	cityDir := convoyCheckRigCity(t, `
[[rigs]]
name = "ops"
path = "ops"
prefix = "OP"
`+extraRigs, append([]string{"ops"}, rigDirs...)...)
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("loading city config: %v", err)
	}
	return cityDir, cfg
}

func openPurgeProofStore(t *testing.T, cityDir, rel string) beads.Store {
	t.Helper()
	store, err := openScopeLocalFileStoreForCity(filepath.Join(cityDir, rel), cityDir)
	if err != nil {
		t.Fatalf("opening %s store: %v", rel, err)
	}
	return store
}

// purgeClaimOps are claim ops whose input seams run the production evaluator
// and retirement over store, with the city-wide absence prover of cityDir —
// the way hookCity.claimOps wires it over the agent's bd store.
func purgeClaimOps(t *testing.T, cityDir string, cfg *config.City, store beads.Store, candidates []beads.Bead) (hookClaimOps, *[]string) {
	t.Helper()
	output, err := json.Marshal(candidates)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var claimed []string
	city := newHookCity(cityDir, cfg)
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) { return string(output), nil },
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			claimed = append(claimed, beadID)
			return beads.Bead{ID: beadID, Assignee: assignee, Status: "in_progress"}, true, nil
		},
		InputVerdict: func(ctx context.Context, _ string, _ []string, candidate beads.Bead, ids []string, prove workflowInputAbsenceProver) (workflowInputVerdict, error) {
			return workflowInputDone(ctx, store, candidate, ids, prove)
		},
		RetireWorkflow: func(ctx context.Context, _ string, _ []string, verdict workflowInputVerdict) error {
			if verdict.State == workflowInputPurged {
				return failPurgedWorkflow(ctx, store, verdict.RootID)
			}
			return skipDoneWorkflow(store, verdict.RootID)
		},
		ProveInputAbsent: city.claimOps().ProveInputAbsent,
		DrainAck:         func(io.Writer) error { return nil },
	}
	return ops, &claimed
}

// TestDoHookClaimRetiresWorkflowWhoseInputWasPurged: a served step of a
// graph.v2 workflow whose input convoy no store in the city holds is retired
// before any claim — root and open steps closed fail/hard/input_purged, the
// finished step's outcome kept — and the hook drains instead of handing the
// step out again.
func TestDoHookClaimRetiresWorkflowWhoseInputWasPurged(t *testing.T) {
	cityDir, cfg := purgeProofCity(t, "")
	store := openPurgeProofStore(t, cityDir, ".")
	w := seedPurgedInputWorkflow(t, store, "OP-purged")
	ops, claimed := purgeClaimOps(t, cityDir, cfg, store, []beads.Bead{w.open})

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", cityDir, hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		DrainAck:           true,
		JSON:               true,
	}, ops, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("doHookClaim = %d, want 0 (stderr=%q)", code, stderr.String())
	}
	if len(*claimed) != 0 {
		t.Fatalf("claimed %v; the workflow's input no longer exists anywhere", *claimed)
	}
	if !strings.Contains(stdout.String(), `"reason":"no_work"`) {
		t.Fatalf("stdout = %q, want no_work drain", stdout.String())
	}
	assertPurgeRetired(t, store, w.root.ID, w.open.ID, w.active.ID)
	assertFinishedOutcomeKept(t, store, w.finished.ID)
	if !strings.Contains(stderr.String(), w.root.ID) || !strings.Contains(stderr.String(), "OP-purged") {
		t.Fatalf("stderr = %q, want the retired root and the missing input named", stderr.String())
	}
}

// runPurgeClaim runs one `gc hook --claim --drain-ack` over candidates with
// purgeClaimOps and returns the beads it claimed and its stderr.
func runPurgeClaim(t *testing.T, cityDir string, cfg *config.City, store beads.Store, candidates ...beads.Bead) ([]string, string) {
	t.Helper()
	ops, claimed := purgeClaimOps(t, cityDir, cfg, store, candidates)
	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", cityDir, hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		DrainAck:           true,
		JSON:               true,
	}, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim = %d, want 0 (stderr=%q)", code, stderr.String())
	}
	return *claimed, stderr.String()
}

// TestWorkflowInputPurgedEveryTrackedMemberAbsent: the input convoy is still
// present, but every bead it tracks is gone from every store of the city — its
// membership is all placeholders — so the workflow is retired with the purge
// disposition, naming each missing member.
func TestWorkflowInputPurgedEveryTrackedMemberAbsent(t *testing.T) {
	cityDir, cfg := purgeProofCity(t, "")
	store := openPurgeProofStore(t, cityDir, ".")
	input, err := store.Create(beads.Bead{Title: "input convoy", Type: "convoy"})
	if err != nil {
		t.Fatalf("create convoy: %v", err)
	}
	for _, id := range []string{"OP-gone1", "OP-gone2"} {
		if err := store.DepAdd(input.ID, id, "tracks"); err != nil {
			t.Fatalf("track %s: %v", id, err)
		}
	}
	w := seedPurgedInputWorkflow(t, store, input.ID)

	claimed, stderr := runPurgeClaim(t, cityDir, cfg, store, w.open)

	if len(claimed) != 0 {
		t.Fatalf("claimed %v; every bead the workflow's input tracks is gone", claimed)
	}
	assertPurgeRetired(t, store, w.root.ID, w.open.ID, w.active.ID)
	assertFinishedOutcomeKept(t, store, w.finished.ID)
	if !strings.Contains(stderr, "OP-gone1, OP-gone2") {
		t.Fatalf("stderr = %q, want both missing members named", stderr)
	}
}
