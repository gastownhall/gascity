package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/graphv2"
	"github.com/gastownhall/gascity/internal/storeref"
)

// purgeFixtureCityConfig is the city the purge fixtures' frame is cut over:
// its HQ namespace is the MemStore's minted "gc" prefix.
func purgeFixtureCityConfig() *config.City {
	return &config.City{Workspace: config.Workspace{Prefix: "gc"}}
}

// purgeFrameOverStores is the purge frame the claim path builds, over a work
// store and rig stores already open.
func purgeFrameOverStores(t *testing.T, cfg *config.City, work beads.Store, rigs map[string]beads.Store) workflowInputPurgeFrame {
	t.Helper()
	frame, err := workflowInputPurgeFrameOver(newWaitDependencyPlanReader(assembleResidencyTopology(cfg, work, rigs, nil, nil), false))
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
	conv, err = graphv2.CreateSingleItemInputConvoy(store, target)
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
			return workflowInputDoneAcross(store, store, candidate, identityCandidates)
		},
		InputPurged: inputPurged,
		RetirePurgedWorkflow: func(_ context.Context, _ string, _ []string, rootID string) error {
			return retirePurgedWorkflow(store, rootID)
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
	frame := purgeFrameOverStores(t, purgeFixtureCityConfig(), store, nil)

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
	frame := purgeFrameOverStores(t, purgeFixtureCityConfig(), store, nil)

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
	frame := purgeFrameOverStores(t, purgeFixtureCityConfig(), store, nil)

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
			ops.SkipDoneWorkflow = func(_ context.Context, _ string, _ []string, rootID string) error {
				skipped = rootID
				return skipDoneWorkflow(store, rootID)
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
