package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
	"github.com/gastownhall/gascity/internal/suspensionstate"
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

// openPurgeProofStore opens the city store of a purgeProofCity.
func openPurgeProofStore(t *testing.T, cityDir string) beads.Store {
	t.Helper()
	store, err := openScopeLocalFileStoreForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("opening city store: %v", err)
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
		ListContinuation: func(_ context.Context, _ string, _ []string, _, _ string) ([]beads.Bead, error) {
			return nil, nil
		},
		AssignContinuation: func(_ context.Context, _ string, _ []string, _, _ string) error { return nil },
		ResolveWorkBranch:  func(hookClaimWorkTree) string { return "" },
		StampWorkMeta: func(_ context.Context, _ string, _ []string, _, _ string, _ map[string]string) error {
			return nil
		},
		PublishRunMap: func(_, _ string, _ ...string) error { return nil },
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
	store := openPurgeProofStore(t, cityDir)
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
	store := openPurgeProofStore(t, cityDir)
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

// TestHookInputPurgeLeavesSuspendedRigUnreadAndUnproven: a suspended rig's
// store is one the absence proof will not open — suspension is quiescence —
// so input it could still hold is never proven absent: the workflow is not
// retired and its step is claimed as before.
func TestHookInputPurgeLeavesSuspendedRigUnreadAndUnproven(t *testing.T) {
	cityDir, cfg := purgeProofCity(t, `
[[rigs]]
name = "dormant"
path = "dormant"
prefix = "DM"
`, "dormant")
	suspended := true
	if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, cityDir, "dormant", &suspended); err != nil {
		t.Fatal(err)
	}
	store := openPurgeProofStore(t, cityDir)
	w := seedPurgedInputWorkflow(t, store, "OP-purged")

	claimed, _ := runPurgeClaim(t, cityDir, cfg, store, w.open)

	if !slices.Equal(claimed, []string{w.open.ID}) {
		t.Fatalf("claimed %v, want [%s]: a suspended rig's store was not read, so the input is not proven absent", claimed, w.open.ID)
	}
	for _, id := range []string{w.root.ID, w.open.ID} {
		if got, err := store.Get(id); err != nil || convoy.IsTerminalStatus(got.Status) {
			t.Fatalf("%s = %+v, %v; want it left open", id, got, err)
		}
	}
}

// Input the evaluator cannot settle never reads as purged: the verdict stays
// pending, the prover is consulted only for input that misses everywhere it
// can read, and a proof failure is returned as an error naming the input.
func TestWorkflowInputVerdictWithoutCompleteAbsenceProofStaysPending(t *testing.T) {
	proofErr := errors.New("store unreachable")
	cases := []struct {
		name      string
		seed      func(t *testing.T, store beads.Store) string // returns the input convoy id
		contract  string
		prove     func(p *recordingProver) workflowInputAbsenceProver
		wantCalls int
		wantErr   string
	}{{
		name:     "nil prover",
		seed:     func(*testing.T, beads.Store) string { return "gc-gone" },
		contract: beadmeta.FormulaContractGraphV2,
		prove:    func(*recordingProver) workflowInputAbsenceProver { return nil },
	}, {
		name:      "prover finds the convoy elsewhere",
		seed:      func(*testing.T, beads.Store) string { return "gc-gone" },
		contract:  beadmeta.FormulaContractGraphV2,
		prove:     func(p *recordingProver) workflowInputAbsenceProver { p.absent = nil; return p.prove },
		wantCalls: 1,
	}, {
		name:     "prover fails",
		seed:     func(*testing.T, beads.Store) string { return "gc-gone" },
		contract: beadmeta.FormulaContractGraphV2,
		prove: func(p *recordingProver) workflowInputAbsenceProver {
			return func(ctx context.Context, ids []string) (bool, error) {
				_, _ = p.prove(ctx, ids)
				return false, proofErr
			}
		},
		wantCalls: 1,
		wantErr:   "proving workflow input gc-gone absent: store unreachable",
	}, {
		name:  "not a graph.v2 workflow",
		seed:  func(*testing.T, beads.Store) string { return "gc-gone" },
		prove: func(p *recordingProver) workflowInputAbsenceProver { return p.prove },
	}, {
		name: "present convoy with no members",
		seed: func(t *testing.T, store beads.Store) string {
			input, err := store.Create(beads.Bead{Title: "input", Type: "convoy"})
			if err != nil {
				t.Fatal(err)
			}
			return input.ID
		},
		contract: beadmeta.FormulaContractGraphV2,
		prove:    func(p *recordingProver) workflowInputAbsenceProver { return p.prove },
	}, {
		name: "one member resolved and closed, one missing",
		seed: func(t *testing.T, store beads.Store) string {
			input, err := store.Create(beads.Bead{Title: "input", Type: "convoy"})
			if err != nil {
				t.Fatal(err)
			}
			done, err := store.Create(beads.Bead{Title: "done work"})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Close(done.ID); err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{done.ID, "gc-gone"} {
				if err := store.DepAdd(input.ID, id, "tracks"); err != nil {
					t.Fatal(err)
				}
			}
			return input.ID
		},
		contract: beadmeta.FormulaContractGraphV2,
		prove:    func(p *recordingProver) workflowInputAbsenceProver { return p.prove },
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			w := seedPurgedInputWorkflow(t, store, tc.seed(t, store))
			if tc.contract == "" {
				contract := ""
				if err := store.Update(w.root.ID, beads.UpdateOpts{Metadata: map[string]string{beadmeta.FormulaContractMetadataKey: contract}}); err != nil {
					t.Fatal(err)
				}
			}
			prover := &recordingProver{absent: map[string]bool{"gc-gone": true}}

			verdict, err := workflowInputDone(context.Background(), store, w.open, nil, tc.prove(prover))

			if tc.wantErr != "" {
				if err == nil || !errors.Is(err, proofErr) || err.Error() != tc.wantErr {
					t.Fatalf("err = %v, want %q wrapping the proof failure", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("workflowInputDone: %v", err)
			}
			if verdict.State != workflowInputPending || verdict.RootID != w.root.ID {
				t.Fatalf("verdict = %+v, want pending for root %s", verdict, w.root.ID)
			}
			if len(prover.calls) != tc.wantCalls {
				t.Fatalf("prover calls = %v, want %d", prover.calls, tc.wantCalls)
			}
		})
	}
}

// A proof failure is reported on stderr and keeps the candidate claimable.
func TestDoHookClaimReportsInputProofFailureAndKeepsCandidate(t *testing.T) {
	cityDir, cfg := purgeProofCity(t, "")
	store := openPurgeProofStore(t, cityDir)
	w := seedPurgedInputWorkflow(t, store, "OP-purged")
	ops, claimed := purgeClaimOps(t, cityDir, cfg, store, []beads.Bead{w.open})
	ops.ProveInputAbsent = func(context.Context, []string) (bool, error) {
		return false, errors.New("store unreachable")
	}

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("query", cityDir, hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		JSON:               true,
	}, ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0 (stderr=%q)", code, stderr.String())
	}
	if !slices.Equal(*claimed, []string{w.open.ID}) {
		t.Fatalf("claimed %v, want [%s]", *claimed, w.open.ID)
	}
	want := "gc hook --claim: checking input convoy for " + w.open.ID + ": proving workflow input OP-purged absent: store unreachable"
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
}

// The city-wide proof stops at a canceled context instead of reading on.
func TestProveInputAbsentAcrossCityHonorsCancellation(t *testing.T) {
	cityDir, cfg := purgeProofCity(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	absent, err := proveInputAbsentAcrossCity(ctx, cityDir, cfg, []string{"OP-purged"})

	if absent || !errors.Is(err, context.Canceled) {
		t.Fatalf("proveInputAbsentAcrossCity = %v, %v; want not proven, context.Canceled", absent, err)
	}
}

// membershipCountingStore counts the membership listings of one convoy: the
// legacy parent-child List and the tracks DepList.
type membershipCountingStore struct {
	beads.Store
	convoyID         string
	childLists, deps int
}

func (s *membershipCountingStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.ParentID == s.convoyID {
		s.childLists++
	}
	return s.Store.List(query)
}

func (s *membershipCountingStore) DepList(id, direction string) ([]beads.Dep, error) {
	if id == s.convoyID {
		s.deps++
	}
	return s.Store.DepList(id, direction)
}

// Present input whose tracked work is all terminal keeps the skipped
// retirement: one membership listing, no absence proof, and the root and open
// steps closed with the skipped disposition and no failure stamp.
func TestHookDoneWorkflowRetirementListsMembershipOnceAndNeverProves(t *testing.T) {
	mem := beads.NewMemStore()
	source, err := mem.Create(beads.Bead{Title: "source work", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.Close(source.ID); err != nil {
		t.Fatal(err)
	}
	conv, err := mem.Create(beads.Bead{Title: "input convoy", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	if err := convoy.TrackItem(mem, conv.ID, source.ID); err != nil {
		t.Fatal(err)
	}
	w := seedPurgedInputWorkflow(t, mem, conv.ID)
	store := &membershipCountingStore{Store: mem, convoyID: conv.ID}
	prover := &recordingProver{absent: map[string]bool{conv.ID: true, source.ID: true}}
	ops := hookClaimOps{
		InputVerdict: func(ctx context.Context, _ string, _ []string, candidate beads.Bead, ids []string, prove workflowInputAbsenceProver) (workflowInputVerdict, error) {
			return workflowInputDone(ctx, store, candidate, ids, prove)
		},
		RetireWorkflow: func(ctx context.Context, _ string, _ []string, verdict workflowInputVerdict) error {
			if verdict.State == workflowInputPurged {
				return failPurgedWorkflow(ctx, store, verdict.RootID)
			}
			return skipDoneWorkflow(store, verdict.RootID)
		},
		ProveInputAbsent: prover.prove,
	}
	opts := hookClaimOptions{Assignee: "worker-1", IdentityCandidates: []string{"worker-1"}, RouteTargets: []string{"worker"}}

	var stderr bytes.Buffer
	kept := retireDoneWorkflowCandidates([]beads.Bead{w.open}, opts, ops, "/work", &stderr)
	if len(kept) != 0 {
		t.Fatalf("candidate %s kept (stderr=%q); its input work is terminal", w.open.ID, stderr.String())
	}
	if len(prover.calls) != 0 {
		t.Errorf("prover calls = %v; present input must not be scanned for absence", prover.calls)
	}
	if store.childLists != 1 || store.deps != 1 {
		t.Errorf("membership listings of %s: %d child lists, %d dep lists; want one of each", conv.ID, store.childLists, store.deps)
	}
	for _, id := range []string{w.root.ID, w.open.ID, w.active.ID} {
		got, err := mem.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "closed" || got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeSkipped ||
			got.Metadata["close_reason"] != sourceworkflow.HookSkipDoneWorkflowCloseReason ||
			got.Metadata[beadmeta.FailureReasonMetadataKey] != "" || got.Metadata[beadmeta.FailureClassMetadataKey] != "" {
			t.Errorf("%s: status=%q metadata=%v, want closed as skipped with no failure stamp", id, got.Status, got.Metadata)
		}
	}
	assertFinishedOutcomeKept(t, mem, w.finished.ID)
}

// partialCloseStore closes only the first id of a CloseAll and then fails, for
// as long as fail is set.
type partialCloseStore struct {
	beads.Store
	fail bool
}

func (s *partialCloseStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	if !s.fail {
		return s.Store.CloseAll(ids, metadata)
	}
	n, err := s.Store.CloseAll(ids[:1], metadata)
	if err != nil {
		return n, err
	}
	return n, errors.New("injected close failure")
}

// A purge retirement whose write fails keeps the candidate and logs the
// workflow, the missing input and the error; the retry after the store
// recovers retires the rest and keeps every outcome already recorded,
// including the one the failed write left and one a worker recorded since.
func TestHookInputPurgeRetirementFailure(t *testing.T) {
	mem := beads.NewMemStore()
	w := seedPurgedInputWorkflow(t, mem, "gc-gone")
	store := &partialCloseStore{Store: mem, fail: true}
	prover := &recordingProver{absent: map[string]bool{"gc-gone": true}}
	ops := hookClaimOps{
		InputVerdict: func(ctx context.Context, _ string, _ []string, candidate beads.Bead, ids []string, prove workflowInputAbsenceProver) (workflowInputVerdict, error) {
			return workflowInputDone(ctx, store, candidate, ids, prove)
		},
		RetireWorkflow: func(ctx context.Context, _ string, _ []string, verdict workflowInputVerdict) error {
			if verdict.State == workflowInputPurged {
				return failPurgedWorkflow(ctx, store, verdict.RootID)
			}
			return skipDoneWorkflow(store, verdict.RootID)
		},
		ProveInputAbsent: prover.prove,
	}
	opts := hookClaimOptions{Assignee: "worker-1", IdentityCandidates: []string{"worker-1"}, RouteTargets: []string{"worker"}}

	var stderr bytes.Buffer
	kept := retireDoneWorkflowCandidates([]beads.Bead{w.open}, opts, ops, "/work", &stderr)
	if len(kept) != 1 || kept[0].ID != w.open.ID {
		t.Fatalf("kept = %v after a failed retirement write, want [%s]", kept, w.open.ID)
	}
	for _, want := range []string{w.root.ID, "gc-gone", "injected close failure"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr = %q, want it to name %q", stderr.String(), want)
		}
	}
	if root, err := mem.Get(w.root.ID); err != nil || root.Status == "closed" {
		t.Fatalf("root %s: status=%q err=%v; a failed write must leave it open for the retry", w.root.ID, root.Status, err)
	}
	if _, err := mem.CloseAll([]string{w.active.ID}, map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass}); err != nil {
		t.Fatalf("finishing %s: %v", w.active.ID, err)
	}

	store.fail = false
	stderr.Reset()
	if kept := retireDoneWorkflowCandidates([]beads.Bead{w.open}, opts, ops, "/work", &stderr); len(kept) != 0 {
		t.Fatalf("candidate %s kept on retry (stderr=%q), want retired", w.open.ID, stderr.String())
	}
	assertPurgeRetired(t, mem, w.root.ID, w.open.ID)
	assertFinishedOutcomeKept(t, mem, w.finished.ID)
	assertFinishedOutcomeKept(t, mem, w.active.ID)
}
