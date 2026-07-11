package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// updateCountingStore counts Update calls so a test can assert the legacy-bound
// re-home is idempotent (no write once the bead already carries canonical
// identity).
type updateCountingStore struct {
	beads.Store
	updates int
}

func (u *updateCountingStore) Update(id string, opts beads.UpdateOpts) error {
	u.updates++
	return u.Store.Update(id, opts)
}

func legacyBoundRecoveryConfig() *config.City {
	// Unbound pool agent "rig-A/planner"; its legacy bound form is
	// "rig-A/gc.planner" (binding name "gc" was removed by the migration).
	return &config.City{Agents: []config.Agent{poolAgent("planner", "rig-A", intPtr(5), 0)}}
}

// TestCanonicalizeLegacyBoundAssignedWorkRehomesToCanonical proves the
// agent-side half of the migration recovery loop: work pre-assigned to the
// legacy bound identity of a now-unbound pool agent has its Assignee and
// gc.routed_to rewritten to the current canonical identity, so the canonical
// session the awake/scale accounting wakes for it can surface and claim it.
func TestCanonicalizeLegacyBoundAssignedWorkRehomesToCanonical(t *testing.T) {
	cfg := legacyBoundRecoveryConfig()
	const legacy = "rig-A/gc.planner"
	const canonical = "rig-A/planner"

	for _, status := range []string{"in_progress", "open"} {
		t.Run(status, func(t *testing.T) {
			wb := workBead("wb-1", legacy, legacy, status, 5)
			mem := beads.NewMemStoreFrom(0, []beads.Bead{wb}, nil)
			store := &updateCountingStore{Store: mem}

			canonicalizeLegacyBoundAssignedWork(cfg, []beads.Bead{wb}, []beads.Store{store}, newSessionBeadSnapshot(nil), io.Discard)

			got, err := mem.Get("wb-1")
			if err != nil {
				t.Fatalf("Get(wb-1): %v", err)
			}
			if got.Assignee != canonical {
				t.Errorf("assignee = %q, want %q (re-homed to canonical)", got.Assignee, canonical)
			}
			if routed := got.Metadata["gc.routed_to"]; routed != canonical {
				t.Errorf("gc.routed_to = %q, want %q (re-homed to canonical)", routed, canonical)
			}

			// Idempotent: a second pass over the now-canonical bead writes nothing.
			rehomed, _ := mem.Get("wb-1")
			store.updates = 0
			canonicalizeLegacyBoundAssignedWork(cfg, []beads.Bead{rehomed}, []beads.Store{store}, newSessionBeadSnapshot(nil), io.Discard)
			if store.updates != 0 {
				t.Errorf("second pass wrote %d times, want 0 (re-home must be idempotent)", store.updates)
			}
		})
	}
}

// TestCanonicalizeLegacyBoundAssignedWorkSkipsNonMigration covers the cases the
// re-home must leave untouched: a live session still owns the legacy assignment
// (resume tier), a real per-session assignee, already-canonical work, and work
// that is neither in_progress nor open.
func TestCanonicalizeLegacyBoundAssignedWorkSkipsNonMigration(t *testing.T) {
	cfg := legacyBoundRecoveryConfig()
	const legacy = "rig-A/gc.planner"

	t.Run("live session owns legacy assignment", func(t *testing.T) {
		wb := workBead("wb-live", legacy, legacy, "in_progress", 5)
		mem := beads.NewMemStoreFrom(0, []beads.Bead{wb}, nil)
		store := &updateCountingStore{Store: mem}
		// A still-running session whose identity is the legacy assignee: the
		// resume tier handles it, so re-homing would strand its work_query.
		live := beads.Bead{
			ID: "sess-legacy", Type: sessionBeadType, Status: "open",
			Metadata: map[string]string{"session_name": legacy, "template": legacy, "state": "active"},
		}
		canonicalizeLegacyBoundAssignedWork(cfg, []beads.Bead{wb}, []beads.Store{store}, newSessionBeadSnapshot([]beads.Bead{live}), io.Discard)
		if store.updates != 0 {
			t.Fatalf("re-homed work owned by a live legacy session, got %d writes (want 0)", store.updates)
		}
		got, _ := mem.Get("wb-live")
		if got.Assignee != legacy {
			t.Errorf("assignee = %q, want %q (left for resume tier)", got.Assignee, legacy)
		}
	})

	cases := map[string]beads.Bead{
		// Real per-session assignment: a session name is not equivalent to any
		// pool template, so it is not migration work.
		"per-session assignee":  workBead("wb-sess", "rig-A/planner", "planner-gc-7", "in_progress", 5),
		"already canonical":     workBead("wb-canon", "rig-A/planner", "rig-A/planner", "in_progress", 5),
		"unconfigured assignee": workBead("wb-ext", "other/thing", "other/thing", "in_progress", 5),
		"closed":                workBead("wb-closed", legacy, legacy, "closed", 5),
	}
	for name, wb := range cases {
		t.Run(name, func(t *testing.T) {
			mem := beads.NewMemStoreFrom(0, []beads.Bead{wb}, nil)
			store := &updateCountingStore{Store: mem}
			canonicalizeLegacyBoundAssignedWork(cfg, []beads.Bead{wb}, []beads.Store{store}, newSessionBeadSnapshot(nil), io.Discard)
			if store.updates != 0 {
				t.Errorf("%s: expected no re-home, got %d writes", name, store.updates)
			}
		})
	}
}

// TestCanonicalizeLegacyBoundAssignedWorkSkipsDegradedSessionSnapshot proves the
// major review finding is closed: re-homing must not run on an incomplete
// session snapshot. A re-home moves an assignee away from its owner, so — unlike
// the benign stampRunSessionIdentity pass — a nil or load-errored snapshot that
// can omit a live legacy owner must skip the whole pass. Otherwise a transient
// session-bead load failure rewrites in-flight work away from the running legacy
// session that still owns it under the legacy identity.
func TestCanonicalizeLegacyBoundAssignedWorkSkipsDegradedSessionSnapshot(t *testing.T) {
	cfg := legacyBoundRecoveryConfig()
	const legacy = "rig-A/gc.planner"

	// In both cases a live legacy session still owns the work under its legacy
	// identity, but it is absent from the snapshot the reconciler can see. A
	// complete snapshot would carry that session and the live-session guard would
	// protect the work; a degraded snapshot must not be trusted to prove the
	// owner is gone.
	cases := map[string]*sessionBeadSnapshot{
		// loadSessionBeadSnapshot / loadSessionBeadSnapshotWithPartial both return
		// a nil snapshot when the session-bead query fails.
		"nil snapshot (session query failed)": nil,
		// A fail-soft load keeps partial data but tags LoadError; the live owner
		// may be one of the entries the degraded query dropped.
		"load-errored snapshot": newSessionBeadSnapshotWithError(errors.New("session list timeout")),
	}
	for name, snapshot := range cases {
		t.Run(name, func(t *testing.T) {
			wb := workBead("wb-degraded", legacy, legacy, "in_progress", 5)
			mem := beads.NewMemStoreFrom(0, []beads.Bead{wb}, nil)
			store := &updateCountingStore{Store: mem}

			canonicalizeLegacyBoundAssignedWork(cfg, []beads.Bead{wb}, []beads.Store{store}, snapshot, io.Discard)

			if store.updates != 0 {
				t.Fatalf("re-homed work on an incomplete session snapshot, got %d writes (want 0)", store.updates)
			}
			got, err := mem.Get("wb-degraded")
			if err != nil {
				t.Fatalf("Get(wb-degraded): %v", err)
			}
			if got.Assignee != legacy {
				t.Errorf("assignee = %q, want %q (left untouched on degraded snapshot)", got.Assignee, legacy)
			}
			if routed := got.Metadata["gc.routed_to"]; routed != legacy {
				t.Errorf("gc.routed_to = %q, want %q (left untouched on degraded snapshot)", routed, legacy)
			}
		})
	}
}

// TestCanonicalizeLegacyBoundAssignedWorkWokenSessionClaimsRehomedWork is the
// end-to-end proof requested by review: after the legacy-bound work is
// re-homed, the canonical pool session the reconciler wakes for it actually
// surfaces and claims the triggering bead through gc hook --claim (rather than
// only emitting a wake request that lands on a session that cannot consume the
// work).
func TestCanonicalizeLegacyBoundAssignedWorkWokenSessionClaimsRehomedWork(t *testing.T) {
	cfg := legacyBoundRecoveryConfig()
	const legacy = "rig-A/gc.planner"
	const canonical = "rig-A/planner"

	// 1. In-progress work persisted under the legacy bound assignee, no live
	//    session (the wake-known-identity case).
	wb := workBead("wb-claim", legacy, legacy, "in_progress", 5)
	mem := beads.NewMemStoreFrom(0, []beads.Bead{wb}, nil)

	// 2. The reconciler re-homes it to the canonical identity.
	canonicalizeLegacyBoundAssignedWork(cfg, []beads.Bead{wb}, []beads.Store{mem}, newSessionBeadSnapshot(nil), io.Discard)
	rehomed, err := mem.Get("wb-claim")
	if err != nil {
		t.Fatalf("Get(wb-claim): %v", err)
	}
	if rehomed.Assignee != canonical {
		t.Fatalf("precondition: assignee = %q, want %q", rehomed.Assignee, canonical)
	}

	// 3. The woken canonical session runs gc hook --claim. Its work_query (tier
	//    1: in_progress assigned to one of the session's identities) now
	//    surfaces the bead because the assignee is the canonical template, which
	//    the claim path always carries as resolvedAgentName.
	workQueryOutput, err := json.Marshal([]beads.Bead{rehomed})
	if err != nil {
		t.Fatalf("marshal work query output: %v", err)
	}
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) { return string(workQueryOutput), nil },
		Claim: func(context.Context, string, []string, string, string) (beads.Bead, bool, error) {
			t.Fatal("claim must not run: in-progress work already assigned to the canonical identity is an existing assignment")
			return beads.Bead{}, false, nil
		},
	}
	opts := hookClaimOptions{
		Assignee:           "planner-gc-1",
		IdentityCandidates: []string{"planner-gc-1", canonical},
		RouteTargets:       []string{canonical},
		JSON:               true,
	}

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("work-query", "/tmp/work", opts, ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s", err, stdout.String())
	}
	if result.Action != "work" || result.BeadID != "wb-claim" {
		t.Fatalf("woken canonical session did not claim the re-homed work: %+v", result)
	}
	if result.Reason != "existing_assignment" {
		t.Errorf("claim reason = %q, want existing_assignment", result.Reason)
	}
}

// TestCanonicalizeLegacyBoundUnassignedRoutedWorkRehomesRoute proves the
// demand/claim half of the migration recovery loop (review finding BF-1): open,
// unassigned work routed to the legacy bound identity of a now-unbound pool agent
// has its gc.routed_to rewritten to the current canonical identity, so the
// canonical pool-demand probe, worker work_query, and claim predicate — all of
// which match gc.routed_to by raw string — can see and claim it.
func TestCanonicalizeLegacyBoundUnassignedRoutedWorkRehomesRoute(t *testing.T) {
	cfg := legacyBoundRecoveryConfig()
	const legacy = "rig-A/gc.planner"
	const canonical = "rig-A/planner"

	wb := workBead("wb-1", legacy, "", "open", 5)
	// An unrelated metadata key must survive the route rewrite (the Update must
	// merge, not replace, the metadata map).
	wb.Metadata["gc.root_bead_id"] = "root-7"
	mem := beads.NewMemStoreFrom(0, []beads.Bead{wb}, nil)
	store := &updateCountingStore{Store: mem}

	canonicalizeLegacyBoundUnassignedRoutedWork(cfg, []beads.Bead{wb}, []beads.Store{store}, io.Discard)

	got, err := mem.Get("wb-1")
	if err != nil {
		t.Fatalf("Get(wb-1): %v", err)
	}
	if routed := got.Metadata["gc.routed_to"]; routed != canonical {
		t.Errorf("gc.routed_to = %q, want %q (re-homed to canonical)", routed, canonical)
	}
	if got.Assignee != "" {
		t.Errorf("assignee = %q, want empty (unassigned work stays unassigned)", got.Assignee)
	}
	if root := got.Metadata["gc.root_bead_id"]; root != "root-7" {
		t.Errorf("gc.root_bead_id = %q, want root-7 (unrelated metadata must be preserved)", root)
	}

	// Idempotent: a second pass over the now-canonical bead writes nothing.
	rehomed, _ := mem.Get("wb-1")
	store.updates = 0
	canonicalizeLegacyBoundUnassignedRoutedWork(cfg, []beads.Bead{rehomed}, []beads.Store{store}, io.Discard)
	if store.updates != 0 {
		t.Errorf("second pass wrote %d times, want 0 (re-home must be idempotent)", store.updates)
	}
}

// TestCanonicalizeLegacyBoundUnassignedRoutedWorkSkipsNonMigration covers the
// shapes the unassigned-route re-home must leave untouched: assigned work (the
// assignee-keyed pass owns it), non-open status, an already-canonical route, an
// empty route, and a dotted route that resolves to no configured agent.
func TestCanonicalizeLegacyBoundUnassignedRoutedWorkSkipsNonMigration(t *testing.T) {
	cfg := legacyBoundRecoveryConfig()
	const legacy = "rig-A/gc.planner"
	const canonical = "rig-A/planner"

	cases := map[string]beads.Bead{
		// Assigned work is re-homed by canonicalizeLegacyBoundAssignedWork, which
		// carries the live-session guard; this pass must not touch it.
		"assigned legacy route":      workBead("wb-assigned", legacy, "rig-A/gc.planner", "open", 5),
		"in_progress unassigned":     workBead("wb-ip", legacy, "", "in_progress", 5),
		"closed unassigned":          workBead("wb-closed", legacy, "", "closed", 5),
		"already canonical":          workBead("wb-canon", canonical, "", "open", 5),
		"empty route":                workBead("wb-noroute", "", "", "open", 5),
		"dotted route, no agent":     workBead("wb-ext", "other/gc.thing", "", "open", 5),
		"unqualified base no rehome": workBead("wb-bare", "planner", "", "open", 5),
	}
	for name, wb := range cases {
		t.Run(name, func(t *testing.T) {
			mem := beads.NewMemStoreFrom(0, []beads.Bead{wb}, nil)
			store := &updateCountingStore{Store: mem}
			canonicalizeLegacyBoundUnassignedRoutedWork(cfg, []beads.Bead{wb}, []beads.Store{store}, io.Discard)
			if store.updates != 0 {
				t.Errorf("%s: expected no re-home, got %d writes", name, store.updates)
			}
		})
	}
}

// TestCanonicalizeLegacyBoundUnassignedRoutedWorkCanonicalWorkerClaims is the
// claim-visibility proof requested by review: before the route is canonicalized
// the canonical worker's claim predicate rejects the legacy-routed bead, and
// after the re-home the woken canonical pool session claims it through
// gc hook --claim as a fresh open claim.
func TestCanonicalizeLegacyBoundUnassignedRoutedWorkCanonicalWorkerClaims(t *testing.T) {
	cfg := legacyBoundRecoveryConfig()
	const legacy = "rig-A/gc.planner"
	const canonical = "rig-A/planner"

	wb := workBead("wb-claim", legacy, "", "open", 5)
	mem := beads.NewMemStoreFrom(0, []beads.Bead{wb}, nil)

	// Before the re-home: the canonical worker's claim predicate matches
	// gc.routed_to by raw string, so the legacy route is invisible to it.
	if hookClaimMatchesRoute(wb, []string{canonical}) {
		t.Fatal("precondition: legacy-routed bead must not match the canonical route before re-home")
	}

	canonicalizeLegacyBoundUnassignedRoutedWork(cfg, []beads.Bead{wb}, []beads.Store{mem}, io.Discard)
	rehomed, err := mem.Get("wb-claim")
	if err != nil {
		t.Fatalf("Get(wb-claim): %v", err)
	}
	if rehomed.Metadata["gc.routed_to"] != canonical {
		t.Fatalf("precondition: route = %q, want %q", rehomed.Metadata["gc.routed_to"], canonical)
	}
	if !hookClaimMatchesRoute(rehomed, []string{canonical}) {
		t.Fatal("re-homed bead must match the canonical route")
	}

	// The woken canonical session runs gc hook --claim. Its work_query surfaces
	// the open, unassigned, canonically-routed bead, and the claim predicate now
	// accepts it, so it is claimed as a fresh open claim.
	workQueryOutput, err := json.Marshal([]beads.Bead{rehomed})
	if err != nil {
		t.Fatalf("marshal work query output: %v", err)
	}
	claimInvoked := false
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) { return string(workQueryOutput), nil },
		Claim: func(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, bool, error) {
			claimInvoked = true
			claimed := rehomed
			claimed.ID = id
			claimed.Status = "in_progress"
			claimed.Assignee = assignee
			return claimed, true, nil
		},
	}
	opts := hookClaimOptions{
		Assignee:           "planner-gc-1",
		IdentityCandidates: []string{"planner-gc-1", canonical},
		RouteTargets:       []string{canonical},
		JSON:               true,
	}

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("work-query", "/tmp/work", opts, ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !claimInvoked {
		t.Fatal("claim must run: an open, unassigned, canonically-routed bead is a fresh claim")
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s", err, stdout.String())
	}
	if result.Action != "work" || result.BeadID != "wb-claim" {
		t.Fatalf("woken canonical session did not claim the re-homed work: %+v", result)
	}
	if result.Reason != "claimed" {
		t.Errorf("claim reason = %q, want claimed", result.Reason)
	}
}

// TestRetainScaleCheckPartialPoolDesiredNormalizesLegacyBoundTemplate proves a
// transient scale_check partial failure still retains pool session beads
// persisted under a legacy bound template. partialTemplates is keyed by the
// current canonical name, so the retain pass must normalize each bead's stored
// template before the membership check.
func TestRetainScaleCheckPartialPoolDesiredNormalizesLegacyBoundTemplate(t *testing.T) {
	cfg := legacyBoundRecoveryConfig()
	const canonical = "rig-A/planner"

	// Adopted pool session bead persisted under the removed binding.
	legacySession := beads.Bead{
		ID: "adopted-1", Type: sessionBeadType, Status: "open",
		Metadata: map[string]string{
			"template":             "rig-A/gc.planner",
			"session_name":         "planner-legacy-1",
			"state":                "active",
			poolManagedMetadataKey: boolMetadata(true),
		},
	}
	snapshot := newSessionBeadSnapshot([]beads.Bead{legacySession})
	// The partial set is keyed canonically (as the scale_check paths produce it).
	partial := map[string]bool{canonical: true}

	got := retainScaleCheckPartialPoolDesired(cfg, nil, snapshot, partial)
	if got[canonical] < 1 {
		t.Fatalf("retained[%s] = %d, want >= 1 (legacy-bound session retained through partial scale_check)", canonical, got[canonical])
	}

	// Without normalization the raw legacy template would miss the canonical
	// partial key: prove a nil cfg (no normalization possible) cannot retain it,
	// which is exactly the pre-fix behavior the canonical path must avoid.
	if raw := retainScaleCheckPartialPoolDesired(nil, nil, snapshot, partial); raw[canonical] != 0 {
		t.Fatalf("retained[%s] = %d with nil cfg, want 0 (legacy template only matches after normalization)", canonical, raw[canonical])
	}
}

// TestRetainScaleCheckPartialPoolDesired_InFlightCreatingBeadRetained confirms that
// scaleCheckPartialSessionRetainable retains creating beads that hold an active
// pending_create_claim lease, while stale creates (lease cleared/expired) are dropped.
// This is acceptance criterion #4 from ga-4qbgqf.1: after the retainable narrowing
// that removes "start-pending" and "creating" from the explicit case list,
// in-flight creates with an active lease still count as retained capacity.
func TestRetainScaleCheckPartialPoolDesired_InFlightCreatingBeadRetained(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "echo",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(3),
		}},
	}
	const template = "worker"
	partial := map[string]bool{template: true}

	// In-flight create: pending_create_claim=true (active lease) → must be retained.
	inFlightBead := beads.Bead{
		ID: "creating-inflight", Type: sessionBeadType, Status: "open",
		Metadata: map[string]string{
			"template":             template,
			"session_name":         "worker-1",
			"state":                "creating",
			"pending_create_claim": boolMetadata(true),
			poolManagedMetadataKey: boolMetadata(true),
		},
	}
	snapshot := newSessionBeadSnapshot([]beads.Bead{inFlightBead})
	got := retainScaleCheckPartialPoolDesired(cfg, nil, snapshot, partial)
	if got[template] < 1 {
		t.Fatalf("in-flight creating bead not retained: retained[worker]=%d, want >= 1", got[template])
	}

	// Stale create: no pending_create_claim (lease expired/cleared) → must not be retained.
	staleBead := beads.Bead{
		ID: "creating-stale", Type: sessionBeadType, Status: "open",
		Metadata: map[string]string{
			"template":             template,
			"session_name":         "worker-2",
			"state":                "creating",
			poolManagedMetadataKey: boolMetadata(true),
		},
	}
	staleSnapshot := newSessionBeadSnapshot([]beads.Bead{staleBead})
	staleGot := retainScaleCheckPartialPoolDesired(cfg, nil, staleSnapshot, partial)
	if staleGot[template] != 0 {
		t.Fatalf("stale creating bead incorrectly retained: retained[worker]=%d, want 0", staleGot[template])
	}
}
