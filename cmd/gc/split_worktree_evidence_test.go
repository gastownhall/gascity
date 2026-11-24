package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/worktree"
)

// seedEvidenceStep creates a routed graph step in store whose logical owner is
// ownerRef (gc.root_store_ref), provisions its worktree the way
// `gc worktree ensure --store-ref <ownerRef>` does, and publishes gc.work_dir
// plus the nine ownership keys on the step, as a provisioner stamps them.
func seedEvidenceStep(t *testing.T, store beads.Store, target, ownerRef string) (beads.Bead, string) {
	t.Helper()
	step, err := store.Create(beads.Bead{
		Title:  "implement",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey:     target,
			beadmeta.RootBeadIDMetadataKey:   "root-1",
			beadmeta.RootStoreRefMetadataKey: ownerRef,
		},
	})
	if err != nil {
		t.Fatalf("seed step: %v", err)
	}
	repo, base := worktreeTestRepo(t)
	root := t.TempDir()
	path := filepath.Join(root, step.ID)
	spec := worktree.Spec{
		RepoDir: repo, Root: root, Path: path, Branch: "wt/task/" + step.ID, Base: base,
		BeadID: step.ID, StoreRef: ownerRef, Creator: "test", Owner: "gc-sling",
		Generation: "g1", Lifecycle: worktree.LifecycleActive,
	}
	report, err := worktree.Ensure(spec)
	if err != nil {
		t.Fatalf("worktree.Ensure: %v", err)
	}
	if err := store.SetMetadataBatch(step.ID, map[string]string{
		beadmeta.WorkDirMetadataKey:            path,
		beadmeta.WorktreeRepoMetadataKey:       repo,
		beadmeta.WorktreeRootMetadataKey:       root,
		beadmeta.WorkBranchMetadataKey:         spec.Branch,
		beadmeta.WorktreeBaseRefMetadataKey:    base,
		beadmeta.WorktreeBaseSHAMetadataKey:    report.Provenance.BaseSHA,
		beadmeta.WorktreeCreatorMetadataKey:    spec.Creator,
		beadmeta.WorktreeOwnerMetadataKey:      spec.Owner,
		beadmeta.WorktreeGenerationMetadataKey: spec.Generation,
		beadmeta.WorktreeLifecycleMetadataKey:  spec.Lifecycle,
	}); err != nil {
		t.Fatalf("stamp evidence: %v", err)
	}
	return step, path
}

// bindStepFromDemand is what a cold pool does with one demanded step: the
// demand pass's merge, the fresh-session request computePoolDesiredStates
// builds for the step, then the evidence guard and worktree.Verify
// (verifiedPoolTriggerWorkDir) before any session is bound.
func bindStepFromDemand(t *testing.T, e legAgreementEnv, template, stepID string) (SessionRequest, string, error) {
	t.Helper()
	_, counts, demand, _, errs := poolDemandForEnv(t, e)
	if len(errs) != 0 {
		t.Fatalf("pool demand errors: %v", errs)
	}
	d := demand[template]
	if msg := d.WorktreeErrors[stepID]; msg != "" {
		t.Fatalf("demand recorded worktree error for %s: %s", stepID, msg)
	}
	if d.WorktreeSpecs[stepID] == nil {
		t.Fatalf("demand for %s carries no worktree spec; counted %v", template, d.WorkBeadIDs)
	}
	merged := mergeCollectedDemand(e.cfg, collectedDemand{DefaultProbed: true, DefaultCounts: counts, DefaultDemand: demand})
	var req SessionRequest
	found := false
	for _, state := range computePoolDesiredStates(e.cfg, nil, nil, merged.ScaleCheckCounts, merged.ScaleCheckDemand, nil) {
		for _, r := range state.Requests {
			if r.Template == template && r.WorkBeadID == stepID {
				req, found = r, true
			}
		}
	}
	if !found {
		t.Fatalf("no fresh pool request for %s on %s", stepID, template)
	}
	agent := &e.cfg.Agents[0]
	workDir, err := verifiedPoolTriggerWorkDir(nil, agent, template+"-1", req)
	return req, workDir, err
}

// ga-8tpvb: on a split city a rig-rooted graph step lives in the class
// binding, and the rig pool's demand reads it through a city leg (key "city").
// Its worktree evidence names its owner, gc.root_store_ref=rig:alpha. The
// binding serves every scope, so refusing that pair starves the step forever.
func TestSplitCityBindingStepWorktreeEvidenceBindsRigPool(t *testing.T) {
	e := newLegAgreementEnv(t, true, "alpha")
	e.cfg.Agents = []config.Agent{{Name: "worker", Dir: "alpha", StartCommand: "true", MaxActiveSessions: intPtr(5)}}
	step, path := seedEvidenceStep(t, e.binding, "alpha/worker", "rig:alpha")

	req, workDir, err := bindStepFromDemand(t, e, "alpha/worker", step.ID)
	if err != nil {
		t.Fatalf("binding-resident step %s (request store %q, evidence store %q) refused: %v",
			step.ID, req.WorkStoreRef, req.WorktreeSpec.StoreRef, err)
	}
	if workDir != path {
		t.Fatalf("work dir = %q, want the verified worktree %q", workDir, path)
	}
}

// Control: the same step in the rig's own store (a single-store city) binds.
func TestSplitCityControlRigStoreStepWorktreeEvidenceBinds(t *testing.T) {
	e := newLegAgreementEnv(t, false, "alpha")
	e.cfg.Agents = []config.Agent{{Name: "worker", Dir: "alpha", StartCommand: "true", MaxActiveSessions: intPtr(5)}}
	step, path := seedEvidenceStep(t, e.rigs["alpha"], "alpha/worker", "rig:alpha")

	_, workDir, err := bindStepFromDemand(t, e, "alpha/worker", step.ID)
	if err != nil || workDir != path {
		t.Fatalf("rig-store step: work dir %q err %v, want %q", workDir, err, path)
	}
}

// Control: the guard stays closed across rigs: a step read from rig
// bravo's own store whose evidence names rig:alpha is refused.
func TestSplitCityCrossRigWorktreeEvidenceStillRefused(t *testing.T) {
	e := newLegAgreementEnv(t, true, "alpha", "bravo")
	e.cfg.Agents = []config.Agent{{Name: "worker", Dir: "bravo", StartCommand: "true", MaxActiveSessions: intPtr(5)}}
	step, _ := seedEvidenceStep(t, e.rigs["bravo"], "bravo/worker", "rig:alpha")

	_, _, err := bindStepFromDemand(t, e, "bravo/worker", step.ID)
	if err == nil || !strings.Contains(err.Error(), "does not match request store") {
		t.Fatalf("cross-rig evidence err = %v, want a store mismatch refusal", err)
	}
}

// Control: on a split city a row read from the city WORK LEDGER is
// scope-bound, so rig-owned evidence on it is still refused; only the binding,
// which serves every scope, defers to the row's own gc.root_store_ref.
func TestSplitCityLedgerRowWithRigWorktreeEvidenceStillRefused(t *testing.T) {
	e := newLegAgreementEnv(t, true, "alpha")
	e.cfg.Agents = []config.Agent{{Name: "worker", Dir: "alpha", StartCommand: "true", MaxActiveSessions: intPtr(5)}}
	step, _ := seedEvidenceStep(t, e.work, "alpha/worker", "rig:alpha")

	req, _, err := bindStepFromDemand(t, e, "alpha/worker", step.ID)
	if req.WorkBindingResident {
		t.Fatalf("work-ledger row %s marked binding-resident", step.ID)
	}
	if err == nil || !strings.Contains(err.Error(), "does not match request store") {
		t.Fatalf("ledger row with rig evidence err = %v, want a store mismatch refusal", err)
	}
}

// The cold-wake probe of a custom scale_check rig pool reads the binding too;
// its target must carry the class-binding mark like the default probe's.
func TestColdWakeProbeMarksBindingTarget(t *testing.T) {
	e := newLegAgreementEnv(t, true, "alpha")
	e.cfg.Agents = []config.Agent{
		{Name: "worker", Dir: "alpha", StartCommand: "true", MaxActiveSessions: intPtr(5)},
		{Name: "custom", Dir: "alpha", StartCommand: "true", MaxActiveSessions: intPtr(5), ScaleCheck: "echo 0"},
	}
	targets, _, _, _, _ := poolDemandForEnv(t, e)
	marked := map[string]int{}
	for _, tg := range targets.defaultScaleTargets {
		if tg.store == e.binding {
			if !tg.classBinding {
				t.Errorf("binding target for %s not marked classBinding", tg.template)
			}
			marked[tg.template]++
		} else if tg.classBinding {
			t.Errorf("non-binding target %+v marked classBinding", tg)
		}
	}
	if marked["alpha/worker"] == 0 || marked["alpha/custom"] == 0 {
		t.Fatalf("binding targets by template = %v, want the default probe and the cold-wake probe", marked)
	}
}

// Control: a city-rooted step in the binding, served to a city pool,
// binds as it always did.
func TestSplitCityCityRootedBindingStepWorktreeEvidenceBinds(t *testing.T) {
	e := newLegAgreementEnv(t, true)
	e.cfg.Agents = []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(5)}}
	step, path := seedEvidenceStep(t, e.binding, "worker", "city:test-city")

	_, workDir, err := bindStepFromDemand(t, e, "worker", step.ID)
	if err != nil || workDir != path {
		t.Fatalf("city-rooted binding step: work dir %q err %v, want %q", workDir, err, path)
	}
}

// assignedStepSession seeds the session row whose claim holds the step: an
// open session in the store the step lives in, resolved to the template.
func assignedStepSession(t *testing.T, store beads.Store, template string) beads.Bead {
	t.Helper()
	sess, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":             template,
			"agent_name":           template,
			"session_name":         template,
			"state":                "active",
			poolManagedMetadataKey: boolMetadata(true),
		},
	})
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return sess
}

// bindStepFromAssignedWork is what a warm pool does with one assigned step
// claimed by a session: the assigned-work filter with the census ref of the
// leg the step lives in, the resume request computePoolDesiredStatesParkedAt
// builds for it from that ref, then the evidence guard and worktree.Verify
// (verifiedPoolTriggerWorkDir) before any session is bound.
func bindStepFromAssignedWork(t *testing.T, e legAgreementEnv, template string, step beads.Bead, stepStore beads.Store) (SessionRequest, string, error) {
	t.Helper()
	legs, err := censusStoreCandidates(e.cityPath, e.cfg, e.leading(), e.rigs, nil, censusRefBare)
	if err != nil {
		t.Fatalf("censusStoreCandidates: %v", err)
	}
	found, ref := false, ""
	for _, leg := range legs {
		if leg.store == stepStore {
			found, ref = true, leg.ref
		}
	}
	if !found {
		t.Fatalf("the census has no leg for the step's store")
	}
	sessions := sessionInfosFromBeads([]beads.Bead{sessionBead(step.Assignee, "open")})
	kept, keptRefs := filterAssignedWorkBeadsForPoolDemand(e.cfg, e.cityPath, e.leading(), sessions, []beads.Bead{step}, []string{ref})
	if len(kept) != 1 {
		t.Fatalf("assigned-work filter kept %d steps, want the one (census ref %q)", len(kept), ref)
	}
	states := computePoolDesiredStatesParkedAt(e.cfg, kept, nil, keptRefs, sessions, nil, nil, time.Time{}, nil, nil)
	var req SessionRequest
	matched := false
	for _, state := range states {
		for _, r := range state.Requests {
			if r.Template == template && r.WorkBeadID == step.ID {
				req, matched = r, true
			}
		}
	}
	if !matched {
		t.Fatalf("no resume request for assigned step %s on %s", step.ID, template)
	}
	agent := &e.cfg.Agents[0]
	workDir, err := verifiedPoolTriggerWorkDir(nil, agent, template+"-1", req)
	return req, workDir, err
}

// assignedEvidenceStep seeds a step claimed by a session in store, with
// worktree evidence owned by ownerRef, and returns it assigned to the
// session and its verified path (gastownhall/gascity#5753's resume-path
// shape). The step is re-read after stamping so it carries its evidence.
func assignedEvidenceStep(t *testing.T, store beads.Store, target, ownerRef string) (beads.Bead, string) {
	t.Helper()
	seeded, path := seedEvidenceStep(t, store, target, ownerRef)
	sess := assignedStepSession(t, store, target)
	step, err := store.Get(seeded.ID)
	if err != nil {
		t.Fatalf("re-read stamped step %s: %v", seeded.ID, err)
	}
	step.Assignee = sess.ID
	return step, path
}

// ga-3v2ckw / gastownhall/gascity#5753, resume path: the store guard stays
// closed across rigs for claimed work too. A step read from rig bravo's own
// store whose evidence names rig:alpha is refused, not silently bound to
// bravo's pool directory.
func TestSplitCityResumeCrossRigWorktreeEvidenceStillRefused(t *testing.T) {
	e := newLegAgreementEnv(t, true, "alpha", "bravo")
	e.cfg.Agents = []config.Agent{{Name: "worker", Dir: "bravo", StartCommand: "true", MaxActiveSessions: intPtr(5)}}
	step, _ := assignedEvidenceStep(t, e.rigs["bravo"], "bravo/worker", "rig:alpha")

	req, _, err := bindStepFromAssignedWork(t, e, "bravo/worker", step, e.rigs["bravo"])
	if req.WorkStoreRef != "rig:bravo" || req.WorkBindingResident {
		t.Fatalf("resume request provenance = store %q resident %v, want the bravo leg rig:bravo", req.WorkStoreRef, req.WorkBindingResident)
	}
	if err == nil || !strings.Contains(err.Error(), "does not match request store") {
		t.Fatalf("cross-rig resume evidence err = %v, want a store mismatch refusal", err)
	}
}

// Control: on a split city a claimed row read from the city WORK LEDGER is
// scope-bound, so rig-owned evidence on it is refused even for the
// city-scoped agent that can see the ledger; only the binding defers to the
// row's own gc.root_store_ref.
func TestSplitCityResumeLedgerRowWithRigWorktreeEvidenceStillRefused(t *testing.T) {
	e := newLegAgreementEnv(t, true, "alpha")
	e.cfg.Agents = []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(5)}}
	step, _ := assignedEvidenceStep(t, e.work, "worker", "rig:alpha")

	req, _, err := bindStepFromAssignedWork(t, e, "worker", step, e.work)
	if req.WorkStoreRef != "city" || req.WorkBindingResident {
		t.Fatalf("resume request provenance = store %q resident %v, want the city leg", req.WorkStoreRef, req.WorkBindingResident)
	}
	if err == nil || !strings.Contains(err.Error(), "does not match request store") {
		t.Fatalf("ledger resume row with rig evidence err = %v, want a store mismatch refusal", err)
	}
}

// The positive leg: a binding-resident step claimed by a session, whose
// evidence names its owner rig, binds the rig pool to the verified worktree.
// The binding serves every scope, so its class ref marks the request
// binding-resident and the store guard defers to the row's own
// gc.root_store_ref — refusing it would starve the step forever.
func TestSplitCityResumeBindingStepWorktreeEvidenceBindsRigPool(t *testing.T) {
	e := newLegAgreementEnv(t, true, "alpha")
	e.cfg.Agents = []config.Agent{{Name: "worker", Dir: "alpha", StartCommand: "true", MaxActiveSessions: intPtr(5)}}
	step, path := assignedEvidenceStep(t, e.binding, "alpha/worker", "rig:alpha")

	req, workDir, err := bindStepFromAssignedWork(t, e, "alpha/worker", step, e.binding)
	if !req.WorkBindingResident {
		t.Fatalf("binding-resident step %s: request store %q not marked binding-resident", step.ID, req.WorkStoreRef)
	}
	if err != nil {
		t.Fatalf("binding-resident step %s (request store %q, evidence store %q) refused: %v",
			step.ID, req.WorkStoreRef, req.WorktreeSpec.StoreRef, err)
	}
	if workDir != path {
		t.Fatalf("work dir = %q, want the verified worktree %q", workDir, path)
	}
}
