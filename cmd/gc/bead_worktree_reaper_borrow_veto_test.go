package main

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// reapBorrowVetoCountingStore wraps a beads.Store and counts List calls, so
// tests can assert the borrow-veto scan issues one batched query per rig per
// tick (FR-3) rather than one query per candidate.
type reapBorrowVetoCountingStore struct {
	beads.Store
	calls int
}

func (s *reapBorrowVetoCountingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.calls++
	return s.Store.List(q)
}

// reapBorrowVetoErrorStore wraps a beads.Store and makes every List call
// fail, so tests can assert the borrow-veto scan fails closed (NFR-1) on a
// query error.
type reapBorrowVetoErrorStore struct {
	beads.Store
	err error
}

func (s *reapBorrowVetoErrorStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, s.err
}

// TestReapClosedBeadWorktrees_ProtectsViaCrossMoleculeBorrowVeto is the
// canonical FR-1/FR-2 test: a worktree's nominally-owning bead is closed, but
// an unrelated bead in a different molecule still carries gc.work_dir
// metadata pointing at the same path and is not terminal. The worktree must
// be protected, and per FR-7 the reason must name the referencing bead.
func TestReapClosedBeadWorktrees_ProtectsViaCrossMoleculeBorrowVeto(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-owner01")
	store := beads.NewMemStoreFrom(1, []beads.Bead{
		{ID: "ga-owner01", Status: "closed"},
		{ID: "ga-other02", Status: "open", Metadata: map[string]string{beadmeta.WorkDirMetadataKey: wt}},
	}, nil)
	cfg := reapTestConfig(rigRoot)
	injectLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{"mrig": store}, nil, false, events.Discard, &stderr)

	if len(report.Reaped) != 0 {
		t.Fatalf("Reaped = %+v, want 0 when an unrelated open bead still references the path\nstderr:\n%s", report.Reaped, stderr.String())
	}
	if len(report.Protected) != 1 {
		t.Fatalf("Protected = %+v, want exactly 1 borrow-veto entry", report.Protected)
	}
	if !strings.Contains(report.Protected[0].Reason, "ga-other02") {
		t.Errorf("Reason = %q, want it to name the referencing bead ga-other02", report.Protected[0].Reason)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("borrow-veto-protected worktree %s was removed: %v", wt, err)
	}
}

// TestReapClosedBeadWorktrees_ProtectsViaLegacyWorkDirKey proves FR-1's
// legacy-key fallback: a referencing bead using the deprecated "work_dir" key
// (instead of canonical "gc.work_dir") still vetoes the reap.
func TestReapClosedBeadWorktrees_ProtectsViaLegacyWorkDirKey(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-owner03")
	store := beads.NewMemStoreFrom(1, []beads.Bead{
		{ID: "ga-owner03", Status: "closed"},
		{ID: "ga-legacy04", Status: "in_progress", Metadata: map[string]string{beadmeta.LegacyWorkDirMetadataKey: wt}},
	}, nil)
	cfg := reapTestConfig(rigRoot)
	injectLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{"mrig": store}, nil, false, events.Discard, &stderr)

	if len(report.Reaped) != 0 {
		t.Fatalf("Reaped = %+v, want 0 when a legacy work_dir reference exists\nstderr:\n%s", report.Reaped, stderr.String())
	}
	if len(report.Protected) != 1 || !strings.Contains(report.Protected[0].Reason, "ga-legacy04") {
		t.Fatalf("Protected = %+v, want a legacy-key borrow-veto entry naming ga-legacy04", report.Protected)
	}
}

// TestReapClosedBeadWorktrees_TerminalReferenceDoesNotVeto proves FR-2 uses
// convoycore.IsTerminalStatus, not a bare "!= closed" check: a referencing
// bead in "tombstone" status is terminal and must not block the reap.
func TestReapClosedBeadWorktrees_TerminalReferenceDoesNotVeto(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-owner05")
	store := beads.NewMemStoreFrom(1, []beads.Bead{
		{ID: "ga-owner05", Status: "closed"},
		{ID: "ga-tomb06", Status: "tombstone", Metadata: map[string]string{beadmeta.WorkDirMetadataKey: wt}},
	}, nil)
	cfg := reapTestConfig(rigRoot)
	injectLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{"mrig": store}, nil, false, events.Discard, &stderr)

	if len(report.Reaped) != 1 || report.Reaped[0].BeadID != "ga-owner05" {
		t.Fatalf("Reaped = %+v, want ga-owner05 reaped: a tombstoned reference must not veto\nstderr:\n%s", report.Reaped, stderr.String())
	}
}

// TestReapClosedBeadWorktrees_FailsClosedOnBorrowVetoQueryError proves NFR-1:
// a query error during the borrow-veto scan protects every candidate in that
// rig's tick, not just the one being evaluated when the error surfaced.
func TestReapClosedBeadWorktrees_FailsClosedOnBorrowVetoQueryError(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt1 := addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-errA001")
	wt2 := addClosedWorktree(t, rigRoot, cityPath, "builder-2", "ga-errB002")
	base := beads.NewMemStoreFrom(1, []beads.Bead{
		{ID: "ga-errA001", Status: "closed"},
		{ID: "ga-errB002", Status: "closed"},
	}, nil)
	store := &reapBorrowVetoErrorStore{Store: base, err: errors.New("store unreachable")}
	cfg := reapTestConfig(rigRoot)
	injectLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{"mrig": store}, nil, false, events.Discard, &stderr)

	if len(report.Reaped) != 0 {
		t.Fatalf("Reaped = %+v, want 0 when the borrow-veto query errors (fail closed)\nstderr:\n%s", report.Reaped, stderr.String())
	}
	if len(report.Protected) != 2 {
		t.Fatalf("Protected = %+v, want both candidates protected on query error", report.Protected)
	}
	for _, wt := range []string{wt1, wt2} {
		if _, err := os.Stat(wt); err != nil {
			t.Errorf("worktree %s was removed despite a borrow-veto query error: %v", wt, err)
		}
	}
}

// TestReapClosedBeadWorktrees_BorrowVetoScanIsBatchedPerRig proves FR-3: with
// multiple reap-eligible candidates in the same rig and tick, the borrow-veto
// scan issues exactly one List call for that rig, not one per candidate.
func TestReapClosedBeadWorktrees_BorrowVetoScanIsBatchedPerRig(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-batchA1")
	addClosedWorktree(t, rigRoot, cityPath, "builder-2", "ga-batchB2")
	addClosedWorktree(t, rigRoot, cityPath, "builder-3", "ga-batchC3")
	base := beads.NewMemStoreFrom(1, []beads.Bead{
		{ID: "ga-batchA1", Status: "closed"},
		{ID: "ga-batchB2", Status: "closed"},
		{ID: "ga-batchC3", Status: "closed"},
	}, nil)
	store := &reapBorrowVetoCountingStore{Store: base}
	cfg := reapTestConfig(rigRoot)
	injectLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{"mrig": store}, nil, false, events.Discard, &stderr)

	if len(report.Reaped) != 3 {
		t.Fatalf("Reaped = %+v, want all 3 candidates reaped\nstderr:\n%s", report.Reaped, stderr.String())
	}
	if store.calls != 1 {
		t.Errorf("List calls = %d, want exactly 1 batched call for 3 candidates in one rig", store.calls)
	}
}
