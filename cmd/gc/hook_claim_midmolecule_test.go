package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// Coverage for ci-soai behavior 6 (the cross-wired pour): a pool slot
// mid-molecule on one workflow root claimed steps of unrelated roots because
// the eligible-candidate loop matched route+assignee only (2026-11-02: the
// session executing gk-dg6uh claimed gk-ln00k/gk-pvugm steps and its own
// machinery braked them post-hoc). The claim-side veto refuses a foreign-root
// step while this session's own molecule is live, and stays inert otherwise —
// settled molecule, no session identity, resolution failure, plain beads.

const (
	midMoleculeSessionID  = "ci-wisp-bqkp4i"
	midMoleculePoolRoute  = "gk/gastown.polecat"
	midMoleculeRootLiveID = "gk-dg6uh" // open root with open work: mid-flight
	midMoleculeRootDeadID = "gk-ln00k" // deferred root, deferred steps: settled
	midMoleculeRootGoneID = "gk-f4gr"  // closed root, closed steps: settled
)

// midMoleculeStepBead renders a step stamped to the session under a root.
func midMoleculeStepBead(id, root, status string) beads.Bead {
	return beads.Bead{
		ID:     id,
		Title:  "step " + id,
		Type:   "task",
		Status: status,
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey:     root,
			beadmeta.SessionIDMetadataKey:      midMoleculeSessionID,
			beadmeta.SessionIDCamelMetadataKey: midMoleculeSessionID,
		},
	}
}

// midMoleculeRootBead renders a molecule root with the given status.
func midMoleculeRootBead(id, status string) beads.Bead {
	return beads.Bead{ID: id, Title: "root " + id, Type: "molecule", Status: status}
}

// midMoleculeStore is the 2026-11-02 incident shape: root gk-dg6uh is open
// with an in_progress step on this session and an unclaimed open step (steps
// of one root spread across slots); gk-ln00k and its steps were deferred by
// the cross-wired-pour playbook; gk-f4gr settled closed.
func midMoleculeStore(t *testing.T) *beads.MemStore {
	t.Helper()
	mem := beads.NewMemStore()
	// Keep fixture ids: the steps' gc.root_bead_id stamps name them.
	mem.HonorExplicitIDs = true
	for _, bead := range []beads.Bead{
		midMoleculeRootBead(midMoleculeRootLiveID, "open"),
		midMoleculeStepBead("gk-stjig", midMoleculeRootLiveID, "open"),
		{ID: "gk-open-next", Title: "step gk-open-next", Type: "task", Status: "open",
			Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: midMoleculeRootLiveID}},
		midMoleculeRootBead(midMoleculeRootDeadID, "open"),
		midMoleculeStepBead("gk-ljxig", midMoleculeRootDeadID, "open"),
		midMoleculeRootBead(midMoleculeRootGoneID, "open"),
		midMoleculeStepBead("gk-oldstep", midMoleculeRootGoneID, "open"),
	} {
		if _, err := mem.Create(bead); err != nil {
			t.Fatalf("seed %s: %v", bead.ID, err)
		}
	}
	// Create forces status "open" (a fresh bead has no transition history), so
	// park and settle the fixture rows through Update, the transition the
	// playbook itself uses.
	for _, park := range []struct{ id, status string }{
		{"gk-stjig", "in_progress"},
		{midMoleculeRootDeadID, "deferred"},
		{"gk-ljxig", "deferred"},
		{midMoleculeRootGoneID, "closed"},
		{"gk-oldstep", "closed"},
	} {
		status := park.status
		if err := mem.Update(park.id, beads.UpdateOpts{Status: &status}); err != nil {
			t.Fatalf("park %s as %s: %v", park.id, status, err)
		}
	}
	return mem
}

func TestHookClaimMoleculeRootsLiveBrakedAndSettled(t *testing.T) {
	live, err := hookClaimMoleculeRoots(midMoleculeStore(t), midMoleculeSessionID)
	if err != nil {
		t.Fatalf("resolve live roots: %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("live roots = %v, want only {%s}: a deferred root (braked steps) and a closed root are not mid-flight", live, midMoleculeRootLiveID)
	}
	if _, ok := live[midMoleculeRootLiveID]; !ok {
		t.Fatalf("live roots %v missing the executing root %s", live, midMoleculeRootLiveID)
	}
}

func TestHookClaimMoleculeRootsCamelKeyStamp(t *testing.T) {
	// A writer that stamps only the camelCase session variant must still be
	// seen as mid-flight: both keys are read wherever a session link resolves.
	mem := beads.NewMemStore()
	mem.HonorExplicitIDs = true
	if _, err := mem.Create(midMoleculeRootBead(midMoleculeRootLiveID, "open")); err != nil {
		t.Fatalf("seed root: %v", err)
	}
	step := midMoleculeStepBead("gk-camel", midMoleculeRootLiveID, "open")
	delete(step.Metadata, beadmeta.SessionIDMetadataKey) // camel variant only
	if _, err := mem.Create(step); err != nil {
		t.Fatalf("seed step: %v", err)
	}
	inProgress := "in_progress"
	if err := mem.Update("gk-camel", beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("start step: %v", err)
	}
	live, err := hookClaimMoleculeRoots(mem, midMoleculeSessionID)
	if err != nil {
		t.Fatalf("resolve live roots: %v", err)
	}
	if _, ok := live[midMoleculeRootLiveID]; !ok {
		t.Fatalf("camelCase gc.sessionId stamp not resolved: live = %v", live)
	}
}

func TestHookClaimMoleculeRootsEmptySessionID(t *testing.T) {
	live, err := hookClaimMoleculeRoots(midMoleculeStore(t), "")
	if err != nil {
		t.Fatalf("resolve with empty session: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("empty session id resolved live roots %v; want none", live)
	}
}

func TestHookClaimForeignMoleculeStep(t *testing.T) {
	live := map[string]struct{}{midMoleculeRootLiveID: {}}
	foreign := beads.Bead{ID: "gk-ljxig", Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey: midMoleculeRootDeadID,
	}}
	own := beads.Bead{ID: "gk-stjig", Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey: midMoleculeRootLiveID,
	}}
	plain := beads.Bead{ID: "gk-plain"}
	for _, tc := range []struct {
		name      string
		candidate beads.Bead
		liveRoots map[string]struct{}
		want      bool
	}{
		{"foreign step while mid-molecule", foreign, live, true},
		{"own-root step is never foreign", own, live, false},
		{"settled molecule vetoes nothing", foreign, nil, false},
		{"empty live set vetoes nothing", foreign, map[string]struct{}{}, false},
		{"plain beads are never foreign", plain, live, false},
		{"blank root stamp is never foreign", beads.Bead{ID: "x", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: " "}}, live, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hookClaimForeignMoleculeStep(tc.candidate, tc.liveRoots); got != tc.want {
				t.Fatalf("hookClaimForeignMoleculeStep(%s, %v) = %v, want %v", tc.candidate.ID, tc.liveRoots, got, tc.want)
			}
		})
	}
}

// midMoleculeStepRow renders one work-query row carrying the pool route and a
// root stamp, the shape the pour leaves in the pool lane.
func midMoleculeStepRow(id, root string) string {
	return `{"id":"` + id + `","status":"open","issue_type":"task","assignee":"","metadata":{"gc.routed_to":"` + midMoleculePoolRoute + `","gc.root_bead_id":"` + root + `"}}`
}

func midMoleculeClaimOptions(sessionID string) hookClaimOptions {
	return hookClaimOptions{
		Assignee:     midMoleculePoolRoute,
		SessionID:    sessionID,
		RouteTargets: hookClaimRouteTargets(midMoleculePoolRoute),
		JSON:         true,
	}
}

// midMoleculeClaimOps stubs the resolver and records every claim CAS.
func midMoleculeClaimOps(t *testing.T, live map[string]struct{}, resolveErr error, claimed *[]string) hookClaimOps {
	t.Helper()
	return hookClaimOps{
		Runner: func(string, string) (string, error) {
			return `[` +
				midMoleculeStepRow("gk-ljxig", midMoleculeRootDeadID) + `,` +
				midMoleculeStepRow("gk-stjig", midMoleculeRootLiveID) +
				`]`, nil
		},
		SessionMoleculeRoots: func(context.Context, string, []string, string) (map[string]struct{}, error) {
			return live, resolveErr
		},
		Claim: func(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, bool, error) {
			*claimed = append(*claimed, id)
			return beads.Bead{ID: id, Status: "in_progress", Assignee: assignee, Type: "task"}, true, nil
		},
	}
}

// TestHookClaimMidMoleculeVetoSkipsForeignStepClaims is the incident
// regression: with the seat mid-molecule on gk-dg6uh, the foreign-root step
// is skipped and the seat's own next step is claimed in the same pass.
func TestHookClaimMidMoleculeVetoSkipsForeignStepClaims(t *testing.T) {
	var claimed []string
	ops := midMoleculeClaimOps(t, map[string]struct{}{midMoleculeRootLiveID: {}}, nil, &claimed)
	var stdout, stderr bytes.Buffer
	code := doHookClaim("bd ready --json", "/tmp/work", midMoleculeClaimOptions(midMoleculeSessionID), ops, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("hook exit code = %d, want 0\nstderr: %s", code, stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s", err, stdout.String())
	}
	if len(claimed) != 1 || claimed[0] != "gk-stjig" {
		t.Fatalf("claims = %v, want exactly the seat's own next step gk-stjig (cross-wire regression: claimed a foreign root's step)", claimed)
	}
	if result.Action != "work" || result.BeadID != "gk-stjig" {
		t.Fatalf("result = action=%q bead=%q; want the own-root step served as work", result.Action, result.BeadID)
	}
	if !strings.Contains(stderr.String(), "gk-ljxig") || !strings.Contains(stderr.String(), "mid-molecule") {
		t.Fatalf("vetoed candidate not named on stderr (the incident was invisible for two cycles):\n%s", stderr.String())
	}
}

func TestHookClaimMidMoleculeVetoInertWhenMoleculeSettled(t *testing.T) {
	// All of the seat's steps closed: the empty live set must leave the first
	// candidate claimable — this is the free-session path.
	var claimed []string
	ops := midMoleculeClaimOps(t, map[string]struct{}{}, nil, &claimed)
	var stdout, stderr bytes.Buffer
	doHookClaim("bd ready --json", "/tmp/work", midMoleculeClaimOptions(midMoleculeSessionID), ops, &stdout, &stderr)

	if len(claimed) != 1 || claimed[0] != "gk-ljxig" {
		t.Fatalf("claims = %v, want the first ready step gk-ljxig claimed once the seat's molecule settled", claimed)
	}
	if strings.Contains(stderr.String(), "mid-molecule") {
		t.Fatalf("settled seat produced a veto note:\n%s", stderr.String())
	}
}

func TestHookClaimMidMoleculeVetoNeedsSessionIdentity(t *testing.T) {
	// A claimant without a session bead id (named/manual seat) never consults
	// the resolver and is never vetoed.
	var claimed []string
	ops := hookClaimOps{
		Runner: midMoleculeClaimOps(t, nil, nil, &claimed).Runner,
		SessionMoleculeRoots: func(context.Context, string, []string, string) (map[string]struct{}, error) {
			t.Fatalf("resolver consulted without a session id")
			return nil, nil
		},
		Claim: func(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, bool, error) {
			claimed = append(claimed, id)
			return beads.Bead{ID: id, Status: "in_progress", Assignee: assignee, Type: "task"}, true, nil
		},
	}
	var stdout, stderr bytes.Buffer
	doHookClaim("bd ready --json", "/tmp/work", midMoleculeClaimOptions(""), ops, &stdout, &stderr)

	if len(claimed) != 1 || claimed[0] != "gk-ljxig" {
		t.Fatalf("claims = %v, want the first step claimed by a session-less claimant", claimed)
	}
}

func TestHookClaimMidMoleculeVetoFailsOpenOnStoreError(t *testing.T) {
	// Resolution failure must not stall the pool: the veto goes inert and the
	// claim proceeds (the session's own cross-wire detector is the backstop).
	var claimed []string
	ops := midMoleculeClaimOps(t, nil, context.DeadlineExceeded, &claimed)
	var stdout, stderr bytes.Buffer
	doHookClaim("bd ready --json", "/tmp/work", midMoleculeClaimOptions(midMoleculeSessionID), ops, &stdout, &stderr)

	if len(claimed) != 1 || claimed[0] != "gk-ljxig" {
		t.Fatalf("claims = %v, want fail-open claim of the first step on resolver error", claimed)
	}
	if !strings.Contains(stderr.String(), "veto inert") {
		t.Fatalf("resolution failure not surfaced on stderr:\n%s", stderr.String())
	}
}

func TestHookClaimMidMoleculeVetoBlocksStaleReclaim(t *testing.T) {
	// The veto sits before the reclaim attempt: a mid-molecule seat must not
	// free (reclaim) a foreign step away from another session either.
	var claimed []string
	ops := midMoleculeClaimOps(t, map[string]struct{}{midMoleculeRootLiveID: {}}, nil, &claimed)
	claimOpts := midMoleculeClaimOptions(midMoleculeSessionID)
	claimOpts.AutoReclaimStaleClaims = true
	ops.ReclaimStale = func(context.Context, string, []string, string) (bool, string, error) {
		t.Fatalf("stale reclaim ran for a foreign step while the seat is mid-molecule")
		return false, "", nil
	}
	ops.Runner = func(string, string) (string, error) {
		// The foreign step carries a stale assignee: otherwise reclaim-eligible.
		return `[{"id":"gk-ljxig","status":"open","issue_type":"task","assignee":"gk/gastown.stale","metadata":{"gc.routed_to":"` + midMoleculePoolRoute + `","gc.root_bead_id":"` + midMoleculeRootDeadID + `"}}]`, nil
	}
	var stdout, stderr bytes.Buffer
	doHookClaim("bd ready --json", "/tmp/work", claimOpts, ops, &stdout, &stderr)

	if len(claimed) != 0 {
		t.Fatalf("claims = %v, want none: the mid-molecule seat must skip the foreign step, not reclaim it", claimed)
	}
	if !strings.Contains(stderr.String(), "mid-molecule") {
		t.Fatalf("vetoed reclaim candidate not named on stderr:\n%s", stderr.String())
	}
}
