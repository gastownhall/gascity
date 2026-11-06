package main

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

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/graphv2"
	"github.com/gastownhall/gascity/internal/storeref"
)

// purgeFixtureCityConfig is the city the purge fixtures' frame is cut over:
// its HQ namespace is the MemStore's minted "gc" prefix.
func purgeFixtureCityConfig() *config.City {
	return &config.City{Workspace: config.Workspace{Prefix: "gc"}}
}

// purgeFrameOverWork is the purge frame the claim path builds for the rigless
// fixture city, over its work store already open.
func purgeFrameOverWork(t *testing.T, work beads.Store) workflowInputPurgeFrame {
	t.Helper()
	frame, err := workflowInputPurgeFrameOver(newWaitDependencyPlanReader(assembleResidencyTopology(purgeFixtureCityConfig(), work, nil, nil, nil), false))
	if err != nil {
		t.Fatalf("workflowInputPurgeFrameOver: %v", err)
	}
	return frame
}

// seedInputWorkflow builds a graph.v2 workflow in store: a root naming
// convoyID as its input convoy, an open step and an in-progress step under it,
// and a step that already closed with gc.outcome=pass.
func seedInputWorkflow(t *testing.T, store beads.Store, convoyID string) (root, open, inProgress, finished beads.Bead) {
	t.Helper()
	root, err := store.Create(beads.Bead{Title: "mol", Metadata: map[string]string{
		beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
		beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		beadmeta.InputConvoyIDMetadataKey:   convoyID,
		beadmeta.RoutedToMetadataKey:        "worker",
	}})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	step := func(title string) beads.Bead {
		b, err := store.Create(beads.Bead{Title: title, Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: root.ID,
			beadmeta.RoutedToMetadataKey:   "worker",
		}})
		if err != nil {
			t.Fatalf("create step %s: %v", title, err)
		}
		return b
	}
	open = step("open step")
	inProgress = step("in-progress step")
	if err := store.Update(inProgress.ID, beads.UpdateOpts{Status: strPtr("in_progress"), Assignee: strPtr("worker-2")}); err != nil {
		t.Fatalf("start step: %v", err)
	}
	finished = step("finished step")
	if err := store.SetMetadata(finished.ID, beadmeta.OutcomeMetadataKey, beadmeta.OutcomePass); err != nil {
		t.Fatalf("stamp finished step: %v", err)
	}
	if err := store.Close(finished.ID); err != nil {
		t.Fatalf("close finished step: %v", err)
	}
	return root, open, inProgress, finished
}

// seedSyntheticInputWorkflow builds a workflow over the single-item synthetic
// input convoy a graph.v2 invocation pours for target.
func seedSyntheticInputWorkflow(t *testing.T, store beads.Store) (target, conv, root, open beads.Bead) {
	t.Helper()
	target, err := store.Create(beads.Bead{Title: "the work", Type: "task"})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	conv, err = graphv2.CreateSingleItemInputConvoy(beadstest.Work(store), target)
	if err != nil {
		t.Fatalf("pour input convoy: %v", err)
	}
	root, open, _, _ = seedInputWorkflow(t, store, conv.ID)
	return target, conv, root, open
}

// proveOver is the purge probe the claim path installs, over frame.
func proveOver(frame workflowInputPurgeFrame) hookInputPurgedFunc {
	return func(verdict workflowInputVerdict) ([]string, error) {
		return proveWorkflowInputPurged(frame, verdict)
	}
}

// purgeClaimOps are the claim ops of a single-store city over store: the real
// verdict and retirement, the purge probe inputPurged, and stub claim-time
// writes that record which candidate was claimed.
func purgeClaimOps(t *testing.T, store beads.Store, inputPurged hookInputPurgedFunc, candidates []beads.Bead, claimed *string) hookClaimOps {
	t.Helper()
	output, err := json.Marshal(candidates)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return hookClaimOps{
		Runner: func(string, string) (string, error) { return string(output), nil },
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			*claimed = beadID
			return beads.Bead{ID: beadID, Assignee: assignee, Status: "in_progress"}, true, nil
		},
		InputVerdict: func(_ context.Context, _ string, _ []string, candidate beads.Bead, identityCandidates []string) (workflowInputVerdict, error) {
			return workflowInputDoneAcross(store, beadstest.Work(store), candidate, identityCandidates)
		},
		InputPurged: inputPurged,
		RetirePurgedWorkflow: func(ctx context.Context, _ string, _ []string, rootID string) error {
			return retirePurgedWorkflow(ctx, store, rootID)
		},
		SkipDoneWorkflow: func(_ context.Context, _ string, _ []string, rootID string) error {
			t.Fatalf("unexpected done-skip of %s", rootID)
			return nil
		},
		ListContinuation: func(_ context.Context, _ string, _ []string, _, _ string) ([]beads.Bead, error) {
			return nil, nil
		},
		AssignContinuation: func(_ context.Context, _ string, _ []string, _, _ string) error { return nil },
		ResolveWorkBranch:  func(hookClaimWorkTree) string { return "" },
		StampWorkMeta: func(_ context.Context, _ string, _ []string, _, _ string, _ map[string]string) error {
			return nil
		},
		PublishRunMap: func(_, _ string, _ ...string) error { return nil },
		DrainAck:      func(io.Writer) error { return nil },
	}
}

// runPurgeClaim runs one `gc hook --claim` for worker-1 over ops, requires its
// exit code (1 is "no work"), and returns its stderr.
func runPurgeClaim(t *testing.T, ops hookClaimOps, wantCode int) string {
	t.Helper()
	var out, errOut bytes.Buffer
	code := doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"worker"},
		JSON:         true,
	}, ops, &out, &errOut)
	if code != wantCode {
		t.Fatalf("doHookClaim = %d, want %d (stderr=%q)", code, wantCode, errOut.String())
	}
	return errOut.String()
}

func assertRetiredAsInputPurged(t *testing.T, store beads.Store, ids ...string) {
	t.Helper()
	for _, id := range ids {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if got.Status != "closed" ||
			got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeFail ||
			got.Metadata[beadmeta.FailureClassMetadataKey] != beadmeta.FailureClassHard ||
			got.Metadata[beadmeta.FailureReasonMetadataKey] != beadmeta.FailureReasonInputPurged ||
			got.Metadata["close_reason"] != hookRetirePurgedWorkflowCloseReason {
			t.Fatalf("%s = %s %v; want closed fail/hard/input_purged", id, got.Status, got.Metadata)
		}
	}
}

func assertNotRetired(t *testing.T, store beads.Store, wantStatus map[string]string) {
	t.Helper()
	for id, want := range wantStatus {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if got.Status != want || got.Metadata[beadmeta.FailureReasonMetadataKey] != "" {
			t.Fatalf("%s = %s %v; want %s and untouched", id, got.Status, got.Metadata, want)
		}
	}
}

// TestDoHookClaimRetiresWorkflowWhoseInputConvoyWasPurged is #8729: a purged
// input convoy reads as an empty membership, which the done-check fails open
// on, so every seat was re-served the workflow's steps forever. Once the
// convoy is proved absent over the whole city frame, the claim retires the
// root and its non-terminal steps as fail/hard/input_purged, serves none of
// them, and goes on to claim the next candidate.
func TestDoHookClaimRetiresWorkflowWhoseInputConvoyWasPurged(t *testing.T) {
	store := beads.NewMemStore()
	conv, err := store.Create(beads.Bead{Title: "input convoy", Type: "convoy"})
	if err != nil {
		t.Fatalf("create convoy: %v", err)
	}
	convoyID := conv.ID
	root, open, inProgress, finished := seedInputWorkflow(t, store, convoyID)
	other, err := store.Create(beads.Bead{Title: "unrelated work", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"}})
	if err != nil {
		t.Fatalf("create other: %v", err)
	}
	if err := store.Delete(convoyID); err != nil {
		t.Fatalf("purge convoy: %v", err)
	}
	frame := purgeFrameOverWork(t, store)

	claimed := ""
	ops := purgeClaimOps(t, store, proveOver(frame), []beads.Bead{open, other}, &claimed)
	stderr := runPurgeClaim(t, ops, 0)

	if claimed != other.ID {
		t.Fatalf("claimed = %q, want the next candidate %s (stderr=%q)", claimed, other.ID, stderr)
	}
	assertRetiredAsInputPurged(t, store, root.ID, open.ID, inProgress.ID)
	if got, err := store.Get(finished.ID); err != nil || got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
		t.Fatalf("finished step = %v %v; want its pass outcome kept", got.Metadata, err)
	}
	if !strings.Contains(stderr, "input is purged") || !strings.Contains(stderr, convoyID) {
		t.Fatalf("stderr = %q; want the retirement and its evidence logged", stderr)
	}
}

// A purge of the tracked work, not the convoy, leaves the synthetic convoy
// tracking a dangling edge: membership is one unresolved placeholder. The
// placeholder and the target the convoy recorded are the evidence, and both
// proved absent retire the workflow.
func TestDoHookClaimRetiresWorkflowWhoseSyntheticInputTargetWasPurged(t *testing.T) {
	store := beads.NewMemStore()
	target, _, root, open := seedSyntheticInputWorkflow(t, store)
	if err := store.Delete(target.ID); err != nil {
		t.Fatalf("purge target: %v", err)
	}
	frame := purgeFrameOverWork(t, store)

	claimed := ""
	stderr := runPurgeClaim(t, purgeClaimOps(t, store, proveOver(frame), []beads.Bead{open}, &claimed), 1)

	if claimed != "" {
		t.Fatalf("claimed %s, a step of a workflow whose input is purged (stderr=%q)", claimed, stderr)
	}
	assertRetiredAsInputPurged(t, store, root.ID, open.ID)
	if !strings.Contains(stderr, target.ID) {
		t.Fatalf("stderr = %q; want the purged target named as evidence", stderr)
	}
}

// A purge that cascaded the tracks edge away leaves the synthetic convoy with
// no members at all. The target the convoy recorded in its metadata still
// names what it tracked, and that target proved absent retires the workflow.
func TestDoHookClaimRetiresWorkflowWhoseRecordedInputTargetWasPurgedWithItsEdge(t *testing.T) {
	store := beads.NewMemStore()
	target, conv, root, open := seedSyntheticInputWorkflow(t, store)
	if err := store.DepRemove(conv.ID, target.ID); err != nil {
		t.Fatalf("cascade the tracks edge: %v", err)
	}
	if err := store.Delete(target.ID); err != nil {
		t.Fatalf("purge target: %v", err)
	}
	frame := purgeFrameOverWork(t, store)

	claimed := ""
	stderr := runPurgeClaim(t, purgeClaimOps(t, store, proveOver(frame), []beads.Bead{open}, &claimed), 1)

	if claimed != "" {
		t.Fatalf("claimed %s, a step of a workflow whose input is purged (stderr=%q)", claimed, stderr)
	}
	assertRetiredAsInputPurged(t, store, root.ID, open.ID)
	if !strings.Contains(stderr, target.ID) {
		t.Fatalf("stderr = %q; want the recorded target named as evidence", stderr)
	}
}

// TestDoHookClaimServesWorkflowWhoseInputPurgeIsNotProved: an input the
// done-check cannot judge is retired only on proof. Evidence that is
// incomplete (the frame leaves part of the city out), erroneous (a read
// failed) or contradictory (the convoy or its target still exists somewhere)
// writes nothing, says why on stderr, and serves the candidate exactly as
// before the probe existed.
func TestDoHookClaimServesWorkflowWhoseInputPurgeIsNotProved(t *testing.T) {
	rigCity := func(rigPrefix string) *config.City {
		cfg := purgeFixtureCityConfig()
		cfg.Rigs = []config.Rig{{Name: "rig1", Prefix: rigPrefix}}
		return cfg
	}
	missingConvoy := func(t *testing.T, store *beads.MemStore) (root, open beads.Bead) {
		t.Helper()
		conv, err := store.Create(beads.Bead{Title: "input convoy", Type: "convoy"})
		if err != nil {
			t.Fatal(err)
		}
		root, open, _, _ = seedInputWorkflow(t, store, conv.ID)
		if err := store.Delete(conv.ID); err != nil {
			t.Fatal(err)
		}
		return root, open
	}
	frameOver := func(t *testing.T, topo storeref.Topology, narrowed bool) hookInputPurgedFunc {
		t.Helper()
		frame, err := workflowInputPurgeFrameOver(newWaitDependencyPlanReader(topo, narrowed))
		if err != nil {
			return func(workflowInputVerdict) ([]string, error) { return nil, err }
		}
		return proveOver(frame)
	}
	for _, tc := range []struct {
		name string
		// seed builds the workflow in the work store and returns the probe
		// the claim runs.
		seed     func(t *testing.T, store *beads.MemStore) (root, open beads.Bead, probe hookInputPurgedFunc)
		wantDiag string
	}{
		{
			name: "a suspended rig is out of frame",
			seed: func(t *testing.T, store *beads.MemStore) (beads.Bead, beads.Bead, hookInputPurgedFunc) {
				root, open := missingConvoy(t, store)
				return root, open, frameOver(t, assembleResidencyTopology(purgeFixtureCityConfig(), store, nil, nil, nil), true)
			},
			wantDiag: "a suspended rig is out of frame",
		},
		{
			name: "a rig store declares a prefix the city did not configure",
			seed: func(t *testing.T, store *beads.MemStore) (beads.Bead, beads.Bead, hookInputPurgedFunc) {
				root, open := missingConvoy(t, store)
				rig := waitPrefixedStore{Store: beads.NewMemStore(), prefix: "xx"}
				return root, open, frameOver(t, assembleResidencyTopology(rigCity("rg"), store, map[string]beads.Store{"rig1": rig}, nil, nil), false)
			},
			wantDiag: "does not cover the city",
		},
		{
			name: "the residency topology is refused",
			seed: func(t *testing.T, store *beads.MemStore) (beads.Bead, beads.Bead, hookInputPurgedFunc) {
				root, open := missingConvoy(t, store)
				return root, open, frameOver(t, assembleResidencyTopology(purgeFixtureCityConfig(), store, nil, nil, errors.New("binding route refused")), false)
			},
			wantDiag: "binding route refused",
		},
		{
			name: "the input convoy id is in no namespace the frame holds",
			seed: func(t *testing.T, store *beads.MemStore) (beads.Bead, beads.Bead, hookInputPurgedFunc) {
				root, open, _, _ := seedInputWorkflow(t, store, "zz-404")
				return root, open, frameOver(t, assembleResidencyTopology(purgeFixtureCityConfig(), store, nil, nil, nil), false)
			},
			wantDiag: "zz-404 is in no namespace",
		},
		{
			name: "reading the input convoy fails",
			seed: func(t *testing.T, store *beads.MemStore) (beads.Bead, beads.Bead, hookInputPurgedFunc) {
				root, open := missingConvoy(t, store)
				failing := waitDependencyGetErrorStore{Store: store, prefix: "gc", err: errors.New("dolt server unavailable")}
				return root, open, frameOver(t, assembleResidencyTopology(purgeFixtureCityConfig(), failing, nil, nil, nil), false)
			},
			wantDiag: "dolt server unavailable",
		},
		{
			name: "the input convoy still exists and is not synthetic",
			seed: func(t *testing.T, store *beads.MemStore) (beads.Bead, beads.Bead, hookInputPurgedFunc) {
				conv, err := store.Create(beads.Bead{Title: "operator convoy", Type: "convoy"})
				if err != nil {
					t.Fatal(err)
				}
				root, open, _, _ := seedInputWorkflow(t, store, conv.ID)
				return root, open, frameOver(t, assembleResidencyTopology(purgeFixtureCityConfig(), store, nil, nil, nil), false)
			},
			wantDiag: "is not synthetic",
		},
		{
			name: "a synthetic input convoy tracks and records nothing",
			seed: func(t *testing.T, store *beads.MemStore) (beads.Bead, beads.Bead, hookInputPurgedFunc) {
				conv, err := store.Create(beads.Bead{Title: "input convoy", Type: "convoy", Metadata: map[string]string{beadmeta.SyntheticMetadataKey: "true"}})
				if err != nil {
					t.Fatal(err)
				}
				root, open, _, _ := seedInputWorkflow(t, store, conv.ID)
				return root, open, frameOver(t, assembleResidencyTopology(purgeFixtureCityConfig(), store, nil, nil, nil), false)
			},
			wantDiag: "neither tracks nor records a target",
		},
		{
			name: "the recorded target still exists after its edge went",
			seed: func(t *testing.T, store *beads.MemStore) (beads.Bead, beads.Bead, hookInputPurgedFunc) {
				target, conv, root, open := seedSyntheticInputWorkflow(t, store)
				if err := store.DepRemove(conv.ID, target.ID); err != nil {
					t.Fatal(err)
				}
				return root, open, frameOver(t, assembleResidencyTopology(purgeFixtureCityConfig(), store, nil, nil, nil), false)
			},
			wantDiag: "still exists",
		},
		{
			name: "the tracked target lives in a rig the work store cannot see",
			seed: func(t *testing.T, store *beads.MemStore) (beads.Bead, beads.Bead, hookInputPurgedFunc) {
				rig := waitPrefixedStore{Store: &beads.MemStore{IDPrefix: "rg"}, prefix: "rg"}
				target, err := rig.Create(beads.Bead{Title: "rig work", Type: "task"})
				if err != nil {
					t.Fatal(err)
				}
				conv, err := store.Create(beads.Bead{Title: "input convoy", Type: "convoy", Metadata: map[string]string{
					beadmeta.SyntheticMetadataKey:     "true",
					beadmeta.InputTargetIDMetadataKey: target.ID,
				}})
				if err != nil {
					t.Fatal(err)
				}
				if err := store.DepAdd(conv.ID, target.ID, "tracks"); err != nil {
					t.Fatal(err)
				}
				root, open, _, _ := seedInputWorkflow(t, store, conv.ID)
				return root, open, frameOver(t, assembleResidencyTopology(rigCity("rg"), store, map[string]beads.Store{"rig1": rig}, nil, nil), false)
			},
			wantDiag: "still exists",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			root, open, probe := tc.seed(t, store)

			claimed := ""
			stderr := runPurgeClaim(t, purgeClaimOps(t, store, probe, []beads.Bead{open}, &claimed), 0)

			if claimed != open.ID {
				t.Fatalf("claimed = %q, want %s served (stderr=%q)", claimed, open.ID, stderr)
			}
			assertNotRetired(t, store, map[string]string{root.ID: "open", open.ID: "open"})
			if !strings.Contains(stderr, "purge is not proved") || !strings.Contains(stderr, tc.wantDiag) {
				t.Fatalf("stderr = %q; want the unproved purge diagnosed with %q", stderr, tc.wantDiag)
			}
		})
	}
}

// TestHookCityInputPurgeFrameRefusesACityWithARigOutOfFrame: the claim's
// frame is the whole city or nothing. A suspended rig and an unbound rig have
// no store in the frame, so their namespaces cannot prove an absence, and the
// frame is refused with both named rather than narrowed to the rest.
func TestHookCityInputPurgeFrameRefusesACityWithARigOutOfFrame(t *testing.T) {
	cityPath := t.TempDir()
	work := beads.NewMemStore()
	old := hookCityStoreOpener
	hookCityStoreOpener = func(string, *config.City) (beads.Store, error) { return work, nil }
	t.Cleanup(func() { hookCityStoreOpener = old })

	cfg := purgeFixtureCityConfig()
	cfg.Rigs = []config.Rig{
		{Name: "parked", Path: filepath.Join(cityPath, "parked"), Prefix: "pk", SuspendedOnStart: true},
		{Name: "loose", Prefix: "lo"},
	}
	_, err := newHookCity(cityPath, cfg).inputPurgeFrame()
	if !errors.Is(err, errWorkflowInputPurgeUnproven) || !strings.Contains(err.Error(), "parked, loose") {
		t.Fatalf("inputPurgeFrame() err = %v; want the frame refused naming parked and loose", err)
	}
}

// TestDoHookClaimNeverProbesPurgeForAResolvedInput: the purge probe is asked
// only about an input the done-check cannot judge. An input whose member
// resolves keeps the decisions it had before — served while the member is
// ours and open, skipped as done once it is terminal or handed off to another
// lane — and no purge frame is opened for it.
func TestDoHookClaimNeverProbesPurgeForAResolvedInput(t *testing.T) {
	for _, tc := range []struct {
		name string
		// member moves the tracked target into the state under test.
		member      func(t *testing.T, store beads.Store, target beads.Bead)
		wantSkipped bool
	}{
		{
			name:   "an open member is served",
			member: func(*testing.T, beads.Store, beads.Bead) {},
		},
		{
			name: "a closed member is done",
			member: func(t *testing.T, store beads.Store, target beads.Bead) {
				if err := store.Close(target.ID); err != nil {
					t.Fatal(err)
				}
			},
			wantSkipped: true,
		},
		{
			name: "a member handed off to another lane is done",
			member: func(t *testing.T, store beads.Store, target beads.Bead) {
				if err := store.Update(target.ID, beads.UpdateOpts{Status: strPtr("in_progress"), Assignee: strPtr("reviewer-1")}); err != nil {
					t.Fatal(err)
				}
			},
			wantSkipped: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			target, _, root, open := seedSyntheticInputWorkflow(t, store)
			tc.member(t, store, target)

			claimed := ""
			probe := func(verdict workflowInputVerdict) ([]string, error) {
				t.Fatalf("purge probed for the resolved input of %s", verdict.RootID)
				return nil, nil
			}
			ops := purgeClaimOps(t, store, probe, []beads.Bead{open}, &claimed)
			skipped := ""
			ops.SkipDoneWorkflow = func(ctx context.Context, _ string, _ []string, rootID string) error {
				skipped = rootID
				return skipDoneWorkflow(ctx, store, rootID)
			}
			wantCode, wantClaimed, wantSkipped := 0, open.ID, ""
			if tc.wantSkipped {
				wantCode, wantClaimed, wantSkipped = 1, "", root.ID
			}
			stderr := runPurgeClaim(t, ops, wantCode)

			if claimed != wantClaimed || skipped != wantSkipped {
				t.Fatalf("claimed %q, skipped %q; want claimed %q, skipped %q (stderr=%q)", claimed, skipped, wantClaimed, wantSkipped, stderr)
			}
			if strings.Contains(stderr, "purge") {
				t.Fatalf("stderr = %q; a resolved input must not reach the purge probe", stderr)
			}
			if !tc.wantSkipped {
				assertNotRetired(t, store, map[string]string{root.ID: "open", open.ID: "open"})
			}
		})
	}
}

// TestClassRoutedHookRetiresABindingResidentWorkflowWhoseInputWasPurged: the
// retirement of a purged input lands in the store that holds the workflow. A
// binding-resident root and its open step close as fail/hard/input_purged in
// the binding, and a step that had already finished keeps its disposition.
func TestClassRoutedHookRetiresABindingResidentWorkflowWhoseInputWasPurged(t *testing.T) {
	f := newDoneWorkflowSplitFixture(t, false)
	// A purge takes the convoy's tracks edge with it; the store's Delete
	// does not cascade, so the edge goes first.
	if err := f.work.DepRemove(f.convoyID, f.source.ID); err != nil {
		t.Fatalf("purging the input convoy's tracks edge: %v", err)
	}
	if err := f.work.Delete(f.convoyID); err != nil {
		t.Fatalf("purging the input convoy: %v", err)
	}
	base := f.baseOps(t)
	base.InputPurged = proveOver(purgeFrameOverWork(t, f.work))
	base.RetirePurgedWorkflow = func(ctx context.Context, _ string, _ []string, rootID string) error {
		return retirePurgedWorkflow(ctx, f.work, rootID)
	}
	ops := classRoutedHookClaimOps(base, f.route)

	kept, stderr := f.retire(t, ops, f.step)
	if len(kept) != 0 {
		t.Fatalf("candidate %s kept (stderr=%q); its workflow %s has a purged input convoy and must be retired", f.step.ID, stderr, f.root.ID)
	}
	assertRetiredAsInputPurged(t, f.binding, f.root.ID, f.step.ID)
	done, err := f.binding.Get(f.done.ID)
	if err != nil {
		t.Fatalf("reading %s from the binding: %v", f.done.ID, err)
	}
	if done.Status != "closed" || done.Metadata[beadmeta.FailureReasonMetadataKey] != "" || done.Metadata[beadmeta.OutcomeMetadataKey] != "" {
		t.Fatalf("finished step %s = %s %v; it closed before the purge and keeps its disposition", f.done.ID, done.Status, done.Metadata)
	}
}

// seedInputStampedStep adds an open step under root that carries the input
// convoy id beside its root id, as a formula's step metadata may stamp it.
func seedInputStampedStep(t *testing.T, store beads.Store, root beads.Bead) beads.Bead {
	t.Helper()
	step, err := store.Create(beads.Bead{Title: "stamped step", Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey:    root.ID,
		beadmeta.InputConvoyIDMetadataKey: root.Metadata[beadmeta.InputConvoyIDMetadataKey],
		beadmeta.RoutedToMetadataKey:      "worker",
	}})
	if err != nil {
		t.Fatalf("create stamped step: %v", err)
	}
	return step
}

// A workflow whose input convoy is proved absent reads as unjudged on its
// root, and the probe proves it purged naming the convoy, for the root and
// for any of its steps, including one that carries the input convoy id
// itself.
func TestWorkflowInputVerdictPurgedWhenConvoyProvenAbsent(t *testing.T) {
	store := beads.NewMemStore()
	conv, err := store.Create(beads.Bead{Title: "input convoy", Type: "convoy"})
	if err != nil {
		t.Fatalf("create convoy: %v", err)
	}
	root, open, _, _ := seedInputWorkflow(t, store, conv.ID)
	stamped := seedInputStampedStep(t, store, root)
	if err := store.Delete(conv.ID); err != nil {
		t.Fatalf("purge convoy: %v", err)
	}
	frame := purgeFrameOverWork(t, store)

	for _, candidate := range []beads.Bead{root, open, stamped} {
		verdict, err := workflowInputDoneAcross(store, beadstest.Work(store), candidate, nil)
		if err != nil {
			t.Fatalf("workflowInputDoneAcross(%s): %v", candidate.ID, err)
		}
		if verdict.RootID != root.ID || verdict.ConvoyID != conv.ID || !verdict.Unjudged || verdict.Done {
			t.Fatalf("workflowInputDoneAcross(%s) = %+v, want unjudged root %s convoy %s", candidate.ID, verdict, root.ID, conv.ID)
		}
		evidence, err := proveWorkflowInputPurged(frame, verdict)
		if err != nil || len(evidence) != 1 || evidence[0] != conv.ID {
			t.Fatalf("proveWorkflowInputPurged(%s) = %v, %v; want [%s]", candidate.ID, evidence, err, conv.ID)
		}
	}
}

// TestDoHookClaimRetiresWorkflowWhoseInputWasPurged: a served step of a
// workflow whose input convoy no store in the city holds is retired before
// any claim — root and open steps closed fail/hard/input_purged, the finished
// step's outcome kept. A step that carries the input convoy id itself retires
// the same workflow through its root.
func TestDoHookClaimRetiresWorkflowWhoseInputWasPurged(t *testing.T) {
	for _, stamped := range []bool{false, true} {
		t.Run(fmt.Sprintf("step stamped with the input id=%t", stamped), func(t *testing.T) {
			store := beads.NewMemStore()
			conv, err := store.Create(beads.Bead{Title: "input convoy", Type: "convoy"})
			if err != nil {
				t.Fatalf("create convoy: %v", err)
			}
			root, open, inProgress, finished := seedInputWorkflow(t, store, conv.ID)
			candidate := open
			if stamped {
				candidate = seedInputStampedStep(t, store, root)
			}
			if err := store.Delete(conv.ID); err != nil {
				t.Fatalf("purge convoy: %v", err)
			}

			claimed := ""
			stderr := runPurgeClaim(t, purgeClaimOps(t, store, proveOver(purgeFrameOverWork(t, store)), []beads.Bead{candidate}, &claimed), 1)

			if claimed != "" {
				t.Fatalf("claimed %s; the workflow's input no longer exists anywhere (stderr=%q)", claimed, stderr)
			}
			assertRetiredAsInputPurged(t, store, root.ID, open.ID, inProgress.ID, candidate.ID)
			if got, err := store.Get(finished.ID); err != nil || got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
				t.Fatalf("finished step = %v %v; want its pass outcome kept", got.Metadata, err)
			}
			if !strings.Contains(stderr, root.ID) || !strings.Contains(stderr, conv.ID) {
				t.Fatalf("stderr = %q, want the retired root and the missing input named", stderr)
			}
		})
	}
}

// TestDoHookClaimRetiresPurgedWorkflowHeldAssignment: a step the claiming
// seat already holds is retired with its workflow once the input is proved
// purged, instead of being handed back to the seat to resume.
func TestDoHookClaimRetiresPurgedWorkflowHeldAssignment(t *testing.T) {
	store := beads.NewMemStore()
	conv, err := store.Create(beads.Bead{Title: "input convoy", Type: "convoy"})
	if err != nil {
		t.Fatalf("create convoy: %v", err)
	}
	root, open, _, _ := seedInputWorkflow(t, store, conv.ID)
	if err := store.Update(open.ID, beads.UpdateOpts{Status: strPtr("in_progress"), Assignee: strPtr("worker-1")}); err != nil {
		t.Fatalf("hold step: %v", err)
	}
	held, err := store.Get(open.ID)
	if err != nil {
		t.Fatalf("get held step: %v", err)
	}
	if err := store.Delete(conv.ID); err != nil {
		t.Fatalf("purge convoy: %v", err)
	}

	claimed := ""
	stderr := runPurgeClaim(t, purgeClaimOps(t, store, proveOver(purgeFrameOverWork(t, store)), []beads.Bead{held}, &claimed), 1)

	if claimed != "" {
		t.Fatalf("claimed %s; the held step's workflow input is purged (stderr=%q)", claimed, stderr)
	}
	assertRetiredAsInputPurged(t, store, root.ID, held.ID)
}

// TestDoHookClaimRetiresWorkflowWhoseInputIsPurged is the #8729 triage
// reproduction over the claim's purge seams and an explicit whole-city frame:
// a workflow over a synthetic input convoy is retired at claim time when the
// convoy, or the single work bead it tracks, has been purged, instead of being
// served to a seat that can only burn its claim cycle discovering the spec is
// gone.
func TestDoHookClaimRetiresWorkflowWhoseInputIsPurged(t *testing.T) {
	for _, tc := range []struct {
		name  string
		purge func(t *testing.T, store *beads.MemStore, target, conv beads.Bead)
	}{
		{name: "convoy purged", purge: func(t *testing.T, store *beads.MemStore, target, conv beads.Bead) {
			// A purge takes the convoy's tracks edge with it; the store's
			// Delete does not cascade, so the edge goes first.
			if err := store.DepRemove(conv.ID, target.ID); err != nil {
				t.Fatalf("purge the tracks edge: %v", err)
			}
			if err := store.Delete(conv.ID); err != nil {
				t.Fatalf("purge convoy: %v", err)
			}
		}},
		{name: "tracked member purged", purge: func(t *testing.T, store *beads.MemStore, target, _ beads.Bead) {
			if err := store.Delete(target.ID); err != nil {
				t.Fatalf("purge member: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			target, conv, root, open := seedSyntheticInputWorkflow(t, store)
			tc.purge(t, store, target, conv)

			claimed := ""
			stderr := runPurgeClaim(t, purgeClaimOps(t, store, proveOver(purgeFrameOverWork(t, store)), []beads.Bead{open}, &claimed), 1)

			if claimed != "" {
				t.Errorf("claimed step %s of a workflow whose input is purged (stderr=%q)", claimed, stderr)
			}
			assertRetiredAsInputPurged(t, store, root.ID, open.ID)
		})
	}
}

// TestDoHookClaimServesWorkflowWhoseOrdinaryInputConvoyLostItsTrackedWork is
// the triage reproduction's negative half: an ordinary (non-synthetic) input
// convoy that still exists but tracks only a purged bead is not proof — such
// a convoy may track work no store in the frame names — so the claim writes
// nothing, says why, and serves the step.
func TestDoHookClaimServesWorkflowWhoseOrdinaryInputConvoyLostItsTrackedWork(t *testing.T) {
	store, root, step := seedDoneWorkflow(t, "open")
	members, err := convoy.MembersIn(convoy.MemberClasses{Convoy: beadstest.Work(store)}, root.Metadata[beadmeta.InputConvoyIDMetadataKey], true)
	if err != nil || len(members) != 1 {
		t.Fatalf("members = %v, %v; want one", members, err)
	}
	if err := store.Delete(members[0].ID); err != nil {
		t.Fatalf("purge member: %v", err)
	}

	claimed := ""
	ops := purgeClaimOps(t, store, proveOver(purgeFrameOverWork(t, store)), []beads.Bead{step}, &claimed)
	var out, errOut bytes.Buffer
	if code := doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"rig/worker"},
		JSON:         true,
	}, ops, &out, &errOut); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0 (stderr=%q)", code, errOut.String())
	}

	if claimed != step.ID {
		t.Fatalf("claimed = %q, want %s served (stderr=%q)", claimed, step.ID, errOut.String())
	}
	assertNotRetired(t, store, map[string]string{root.ID: "open", step.ID: "open"})
	if !strings.Contains(errOut.String(), "not synthetic") {
		t.Fatalf("stderr = %q; want the failed proof diagnosed", errOut.String())
	}
}

// purgeStaleCacheStore is a store whose Get answers from a cache that holds no
// row — the miss a caching store gives for a closed or evicted bead — while
// its live handle reads the store itself, or fails with liveErr.
type purgeStaleCacheStore struct {
	beads.Store
	liveErr error
}

func (s purgeStaleCacheStore) Get(string) (beads.Bead, error) { return beads.Bead{}, beads.ErrNotFound }

func (s purgeStaleCacheStore) Handles() beads.StoreHandles {
	h := beads.HandlesFor(s.Store)
	if s.liveErr != nil {
		h.Live = purgeFailingLiveReader{LiveReader: h.Live, err: s.liveErr}
	}
	return beads.StoreHandles{Cached: h.Cached, Live: h.Live, Writer: s.Store}
}

// purgeFailingLiveReader is a live handle whose Get fails with err.
type purgeFailingLiveReader struct {
	beads.LiveReader
	err error
}

func (r purgeFailingLiveReader) Get(string) (beads.Bead, error) { return beads.Bead{}, r.err }

// purgeFailingGetFor fails Get for the ids in fail and reads every other id.
type purgeFailingGetFor struct {
	beads.Store
	fail map[string]bool
}

func (s purgeFailingGetFor) Get(id string) (beads.Bead, error) {
	if s.fail[id] {
		return beads.Bead{}, fmt.Errorf("store unavailable for %s", id)
	}
	return s.Store.Get(id)
}

// TestWorkflowInputPurgedErrorsOnFailingEvidenceRead: an evidence read that
// fails is a failed proof, never absence — whether the by-id read fails, or
// the by-id read misses and the live confirmation of that miss fails.
func TestWorkflowInputPurgedErrorsOnFailingEvidenceRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		work func(store beads.Store, target beads.Bead) beads.Store
		want string
	}{
		{
			name: "the by-id read of a recorded target fails",
			work: func(store beads.Store, target beads.Bead) beads.Store {
				return purgeFailingGetFor{Store: store, fail: map[string]bool{target.ID: true}}
			},
			want: "store unavailable for",
		},
		{
			name: "the live confirmation of a miss fails",
			work: func(store beads.Store, _ beads.Bead) beads.Store {
				return purgeStaleCacheStore{Store: store, liveErr: errors.New("live read refused")}
			},
			want: "live read refused",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			target, conv, root, _ := seedSyntheticInputWorkflow(t, store)
			if err := store.DepRemove(conv.ID, target.ID); err != nil {
				t.Fatalf("cascade the tracks edge: %v", err)
			}
			verdict := workflowInputVerdict{RootID: root.ID, ConvoyID: conv.ID, Unjudged: true}

			evidence, err := proveWorkflowInputPurged(purgeFrameOverWork(t, tc.work(store, target)), verdict)
			if !errors.Is(err, errWorkflowInputPurgeUnproven) || errors.Is(err, beads.ErrNotFound) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("proveWorkflowInputPurged = %v, %v; want an unproved purge naming %q, never an absence", evidence, err, tc.want)
			}
		})
	}
}

// TestDoHookClaimServesWhenPurgeProbeErrors: a probe that cannot run is a
// diagnosed failed proof, not a purge: the candidate is served untouched and
// the retirement never fires.
func TestDoHookClaimServesWhenPurgeProbeErrors(t *testing.T) {
	store := beads.NewMemStore()
	conv, err := store.Create(beads.Bead{Title: "input convoy", Type: "convoy"})
	if err != nil {
		t.Fatalf("create convoy: %v", err)
	}
	root, open, _, _ := seedInputWorkflow(t, store, conv.ID)
	if err := store.Delete(conv.ID); err != nil {
		t.Fatalf("purge convoy: %v", err)
	}

	claimed := ""
	probe := func(workflowInputVerdict) ([]string, error) {
		return nil, fmt.Errorf("%w: frame unavailable", errWorkflowInputPurgeUnproven)
	}
	ops := purgeClaimOps(t, store, probe, []beads.Bead{open}, &claimed)
	ops.RetirePurgedWorkflow = func(_ context.Context, _ string, _ []string, rootID string) error {
		t.Fatalf("RetirePurgedWorkflow(%s) called after the probe errored", rootID)
		return nil
	}
	stderr := runPurgeClaim(t, ops, 0)

	if claimed != open.ID {
		t.Fatalf("claimed = %q, want %s: a failed proof serves the candidate (stderr=%q)", claimed, open.ID, stderr)
	}
	assertNotRetired(t, store, map[string]string{root.ID: "open", open.ID: "open"})
	if !strings.Contains(stderr, "purge is not proved") || !strings.Contains(stderr, "frame unavailable") {
		t.Fatalf("stderr = %q; want the failed proof diagnosed", stderr)
	}
}

// TestDoHookClaimServesWhenPurgeRetirementFails: a retirement that does not
// land leaves the candidate served and the failure on stderr: the close is
// the proof's consequence, never a leap past it.
func TestDoHookClaimServesWhenPurgeRetirementFails(t *testing.T) {
	store := beads.NewMemStore()
	conv, err := store.Create(beads.Bead{Title: "input convoy", Type: "convoy"})
	if err != nil {
		t.Fatalf("create convoy: %v", err)
	}
	root, open, _, _ := seedInputWorkflow(t, store, conv.ID)
	if err := store.Delete(conv.ID); err != nil {
		t.Fatalf("purge convoy: %v", err)
	}

	claimed := ""
	ops := purgeClaimOps(t, store, proveOver(purgeFrameOverWork(t, store)), []beads.Bead{open}, &claimed)
	ops.RetirePurgedWorkflow = func(context.Context, string, []string, string) error {
		return errors.New("store unavailable for close")
	}
	stderr := runPurgeClaim(t, ops, 0)

	if claimed != open.ID {
		t.Fatalf("claimed = %q, want %s: a failed retirement serves the candidate (stderr=%q)", claimed, open.ID, stderr)
	}
	assertNotRetired(t, store, map[string]string{root.ID: "open", open.ID: "open"})
	if !strings.Contains(stderr, "retiring workflow "+root.ID) || !strings.Contains(stderr, "store unavailable for close") {
		t.Fatalf("stderr = %q; want the failed retirement diagnosed", stderr)
	}
}

// stubHookCityStore makes the hook city's store opener hand back store and
// counts its opens.
func stubHookCityStore(t *testing.T, store beads.Store) *int {
	t.Helper()
	opens := 0
	old := hookCityStoreOpener
	hookCityStoreOpener = func(string, *config.City) (beads.Store, error) {
		opens++
		return store, nil
	}
	t.Cleanup(func() { hookCityStoreOpener = old })
	return &opens
}

// TestHookCityInputPurgeFrameOpensOncePerInvocation: the claim's real purge
// probe (hookCity.claimOps) opens the city frame lazily — not at all until an
// unjudged input needs it — and then once for the whole invocation, however
// many workflows it proves purged.
func TestHookCityInputPurgeFrameOpensOncePerInvocation(t *testing.T) {
	store := beads.NewMemStore()
	opens := stubHookCityStore(t, store)
	city := newHookCity(t.TempDir(), purgeFixtureCityConfig())

	var roots, steps []beads.Bead
	for range 2 {
		target, _, root, open := seedSyntheticInputWorkflow(t, store)
		if err := store.Delete(target.ID); err != nil {
			t.Fatalf("purge target: %v", err)
		}
		roots, steps = append(roots, root), append(steps, open)
	}
	ops := purgeClaimOps(t, store, city.claimOps().InputPurged, steps, new(string))
	if *opens != 0 {
		t.Fatalf("city store opened %d times before any claim; the purge frame opens lazily", *opens)
	}
	stderr := runPurgeClaim(t, ops, 1)

	assertRetiredAsInputPurged(t, store, roots[0].ID, steps[0].ID, roots[1].ID, steps[1].ID)
	if *opens != 1 {
		t.Fatalf("city store opened %d times for two purge proofs, want once per invocation (stderr=%q)", *opens, stderr)
	}
}

// TestHookCityInputPurgeProofReadsEvidenceLive: the claim's real purge probe
// judges a recorded target by its live row. A caching store's Get misses a
// closed or evicted bead, and an ephemeral one lives in the wisps tier; each
// still exists, so the purge is not proved and the step is served, while a
// target that is really gone retires the workflow over the same frame.
func TestHookCityInputPurgeProofReadsEvidenceLive(t *testing.T) {
	for _, tc := range []struct {
		name string
		// target moves the recorded target into the state under test.
		target      func(t *testing.T, store *beads.MemStore, target beads.Bead) beads.Bead
		wantRetired bool
	}{
		{
			name:   "an open target the cache misses",
			target: func(_ *testing.T, _ *beads.MemStore, target beads.Bead) beads.Bead { return target },
		},
		{
			name: "a closed target",
			target: func(t *testing.T, store *beads.MemStore, target beads.Bead) beads.Bead {
				if err := store.Close(target.ID); err != nil {
					t.Fatal(err)
				}
				return target
			},
		},
		{
			name: "an ephemeral target",
			target: func(t *testing.T, store *beads.MemStore, _ beads.Bead) beads.Bead {
				wisp, err := store.Create(beads.Bead{Title: "wisp work", Type: "task", Ephemeral: true})
				if err != nil {
					t.Fatal(err)
				}
				return wisp
			},
		},
		{
			name: "a purged target",
			target: func(t *testing.T, store *beads.MemStore, target beads.Bead) beads.Bead {
				if err := store.Delete(target.ID); err != nil {
					t.Fatal(err)
				}
				return target
			},
			wantRetired: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			target, conv, root, open := seedSyntheticInputWorkflow(t, store)
			if err := store.DepRemove(conv.ID, target.ID); err != nil {
				t.Fatalf("cascade the tracks edge: %v", err)
			}
			recorded := tc.target(t, store, target)
			if err := store.SetMetadata(conv.ID, beadmeta.InputTargetIDMetadataKey, recorded.ID); err != nil {
				t.Fatalf("record target: %v", err)
			}
			stubHookCityStore(t, purgeStaleCacheStore{Store: store})
			city := newHookCity(t.TempDir(), purgeFixtureCityConfig())

			claimed := ""
			wantCode := 0
			if tc.wantRetired {
				wantCode = 1
			}
			stderr := runPurgeClaim(t, purgeClaimOps(t, store, city.claimOps().InputPurged, []beads.Bead{open}, &claimed), wantCode)

			if tc.wantRetired {
				if claimed != "" {
					t.Fatalf("claimed %s, a step of a workflow whose input is purged (stderr=%q)", claimed, stderr)
				}
				assertRetiredAsInputPurged(t, store, root.ID, open.ID)
				return
			}
			if claimed != open.ID {
				t.Fatalf("claimed = %q, want %s served: %s still exists (stderr=%q)", claimed, open.ID, recorded.ID, stderr)
			}
			assertNotRetired(t, store, map[string]string{root.ID: "open", open.ID: "open"})
			if !strings.Contains(stderr, "purge is not proved") || !strings.Contains(stderr, recorded.ID+" of convoy "+conv.ID+" still exists") {
				t.Fatalf("stderr = %q; want the surviving target diagnosed", stderr)
			}
		})
	}
}

// TestHookCityInputPurgeRefusalServesTheCandidate: a frame the claim's real
// probe refuses — a suspended rig, an unbound rig, or a rig store that cannot
// be opened — writes nothing, names the refusal on stderr, and serves the
// step.
func TestHookCityInputPurgeRefusalServesTheCandidate(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rig      func(cityPath string) config.Rig
		openErr  error
		wantDiag string
	}{
		{
			name: "a suspended rig",
			rig: func(cityPath string) config.Rig {
				return config.Rig{Name: "parked", Path: filepath.Join(cityPath, "parked"), Prefix: "pk", SuspendedOnStart: true}
			},
			wantDiag: "rigs parked are suspended or unbound",
		},
		{
			name:     "an unbound rig",
			rig:      func(string) config.Rig { return config.Rig{Name: "loose", Prefix: "lo"} },
			wantDiag: "rigs loose are suspended or unbound",
		},
		{
			name: "an unreadable rig store",
			rig: func(cityPath string) config.Rig {
				return config.Rig{Name: "broken", Path: filepath.Join(cityPath, "broken"), Prefix: "bk"}
			},
			openErr:  errors.New("rig dolt unreachable"),
			wantDiag: "opening the rig stores",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			conv, err := store.Create(beads.Bead{Title: "input convoy", Type: "convoy"})
			if err != nil {
				t.Fatalf("create convoy: %v", err)
			}
			root, open, _, _ := seedInputWorkflow(t, store, conv.ID)
			if err := store.Delete(conv.ID); err != nil {
				t.Fatalf("purge convoy: %v", err)
			}
			stubHookCityStore(t, store)
			if tc.openErr != nil {
				prev := readyRigStoreOpener
				readyRigStoreOpener = func(*config.City) rigStoreOpener {
					return func(string, string) (beads.Store, error) { return nil, tc.openErr }
				}
				t.Cleanup(func() { readyRigStoreOpener = prev })
			}
			cityPath := t.TempDir()
			cfg := purgeFixtureCityConfig()
			cfg.Rigs = []config.Rig{tc.rig(cityPath)}

			claimed := ""
			stderr := runPurgeClaim(t, purgeClaimOps(t, store, newHookCity(cityPath, cfg).claimOps().InputPurged, []beads.Bead{open}, &claimed), 0)

			if claimed != open.ID {
				t.Fatalf("claimed = %q, want %s served (stderr=%q)", claimed, open.ID, stderr)
			}
			assertNotRetired(t, store, map[string]string{root.ID: "open", open.ID: "open"})
			if !strings.Contains(stderr, "purge is not proved") || !strings.Contains(stderr, tc.wantDiag) {
				t.Fatalf("stderr = %q; want the refused frame diagnosed with %q", stderr, tc.wantDiag)
			}
		})
	}
}
