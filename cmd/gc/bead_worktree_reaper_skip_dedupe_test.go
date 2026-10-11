package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// skipReasonsFor returns, in order, the reasons carried by every
// bead.worktree.reap_skipped event the fake recorded for worktreePath.
//
// Paths are matched canonically, not by raw string equality. The reaper
// discovers worktrees through `git worktree list --porcelain`, and git reports
// a fully symlink-resolved path — on macOS the /private/var spelling of the
// /var path these tests build from t.TempDir(). Both spellings name the same
// worktree, so comparing them raw found nothing on that host.
func skipReasonsFor(t *testing.T, fake *events.Fake, worktreePath string) []string {
	t.Helper()
	want := canonicalTestPath(worktreePath)
	var reasons []string
	for _, e := range fake.Events {
		if e.Type != events.BeadWorktreeReapSkipped {
			continue
		}
		var p events.BeadWorktreeReapSkippedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("unmarshal reap_skipped payload: %v", err)
		}
		if canonicalTestPath(p.Path) == want {
			reasons = append(reasons, p.Reason)
		}
	}
	return reasons
}

// countStderrProtecting returns how many "protecting" lines the reaper wrote
// for worktreePath, so the log sink can be asserted on the same edge as the
// event sink.
// Paths are compared canonically for the same reason as skipReasonsFor: the
// logged path is git's symlink-resolved spelling, not the one the test built.
func countStderrProtecting(stderr string, worktreePath string) int {
	want := canonicalTestPath(worktreePath)
	n := 0
	for _, line := range strings.Split(stderr, "\n") {
		_, rest, found := strings.Cut(line, "protecting ")
		if !found {
			continue
		}
		logged, _, _ := strings.Cut(rest, " (bead ")
		if canonicalTestPath(logged) == want {
			n++
		}
	}
	return n
}

// TestReapSkipTracker_SuppressesUnchangedRepeats is the canonical failing test
// for ga-4rse: a worktree whose skip reason has not changed since the previous
// pass must not re-emit an identical bead.worktree.reap_skipped event. The
// reaper sweeps every ~12s over worktrees that stay protected for hours, and
// re-emitting each one per sweep made this single event 95% of all city
// telemetry (~500 events/min, ~260 MB/day of events.jsonl).
func TestReapSkipTracker_SuppressesUnchangedRepeats(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-dedup01")
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-dedup01", Status: "closed"}}, nil)
	cfg := reapTestConfig(rigRoot)
	injectLiveness(t, liveWorktreeState{scanned: true, cwds: []string{canonicalTestPath(wt)}})

	fake := events.NewFake()
	skips := newReapSkipTracker()

	var stderr bytes.Buffer
	for pass := 1; pass <= 3; pass++ {
		report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{reapTestRigName: store}, nil, false, fake, skips, &stderr)
		// Suppression is an emission concern only — the report stays complete
		// on every pass so callers still see everything that was protected.
		if len(report.Protected) != 1 {
			t.Fatalf("pass %d: Protected = %+v, want exactly 1 entry regardless of suppression", pass, report.Protected)
		}
		if len(report.Reaped) != 0 {
			t.Fatalf("pass %d: Reaped = %+v, want 0 for a live worktree", pass, report.Reaped)
		}
	}

	reasons := skipReasonsFor(t, fake, wt)
	if len(reasons) != 1 {
		t.Fatalf("emitted %d reap_skipped events across 3 identical passes, want exactly 1: %q", len(reasons), reasons)
	}
	if !strings.Contains(reasons[0], "live") {
		t.Errorf("reason = %q, want it to name the liveness protection", reasons[0])
	}
	if n := countStderrProtecting(stderr.String(), wt); n != 1 {
		t.Errorf("wrote %d 'protecting' log lines across 3 identical passes, want exactly 1\nstderr:\n%s", n, stderr.String())
	}
}

// TestReapSkipTracker_ReemitsWhenReasonChanges proves suppression is
// edge-triggered rather than once-forever: when the reason a worktree is
// protected changes, that transition is real news and must be surfaced.
func TestReapSkipTracker_ReemitsWhenReasonChanges(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-chg0001")
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-chg0001", Status: "closed"}}, nil)
	cfg := reapTestConfig(rigRoot)

	fake := events.NewFake()
	skips := newReapSkipTracker()
	var stderr bytes.Buffer

	// Pass 1 + 2: protected because a live process sits in the tree.
	injectLiveness(t, liveWorktreeState{scanned: true, cwds: []string{canonicalTestPath(wt)}})
	reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{reapTestRigName: store}, nil, false, fake, skips, &stderr)
	reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{reapTestRigName: store}, nil, false, fake, skips, &stderr)

	// Pass 3: the process is gone, but the tree now has uncommitted work — a
	// different reason, and therefore a transition worth reporting.
	injectLiveness(t, liveWorktreeState{scanned: true})
	if err := os.WriteFile(filepath.Join(wt, "dirty.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatalf("write uncommitted file: %v", err)
	}
	reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{reapTestRigName: store}, nil, false, fake, skips, &stderr)

	reasons := skipReasonsFor(t, fake, wt)
	if len(reasons) != 2 {
		t.Fatalf("emitted %d reap_skipped events, want 2 (initial + reason change): %q", len(reasons), reasons)
	}
	if !strings.Contains(reasons[0], "live") {
		t.Errorf("first reason = %q, want the liveness protection", reasons[0])
	}
	if !strings.Contains(reasons[1], "uncommitted=true") {
		t.Errorf("second reason = %q, want the changed git-state protection", reasons[1])
	}
}

// TestReapSkipTracker_ReemitsAfterWorktreeLeavesTheSweep proves the tracker
// forgets paths that drop out of a pass, so a worktree that stops being a
// candidate and later comes back re-announces itself instead of staying
// permanently silent.
func TestReapSkipTracker_ReemitsAfterWorktreeLeavesTheSweep(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-gap0001")
	closedStore := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-gap0001", Status: "closed"}}, nil)
	// While the bead is open the worktree is not a reap candidate at all, so
	// the reaper never evaluates it and the tracker must not keep its entry.
	openStore := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-gap0001", Status: "open"}}, nil)
	cfg := reapTestConfig(rigRoot)
	injectLiveness(t, liveWorktreeState{scanned: true, cwds: []string{canonicalTestPath(wt)}})

	fake := events.NewFake()
	skips := newReapSkipTracker()
	var stderr bytes.Buffer

	reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{reapTestRigName: closedStore}, nil, false, fake, skips, &stderr)
	reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{reapTestRigName: openStore}, nil, false, fake, skips, &stderr)
	reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{reapTestRigName: closedStore}, nil, false, fake, skips, &stderr)

	reasons := skipReasonsFor(t, fake, wt)
	if len(reasons) != 2 {
		t.Fatalf("emitted %d reap_skipped events, want 2 (before and after the gap): %q", len(reasons), reasons)
	}
}

// TestReapSkipTracker_NilTrackerEmitsEveryPass proves a nil tracker keeps the
// unsuppressed behavior, so one-shot callers with no cross-pass state to carry
// (tests, a single CLI sweep) still see every skip.
func TestReapSkipTracker_NilTrackerEmitsEveryPass(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-nilt001")
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-nilt001", Status: "closed"}}, nil)
	cfg := reapTestConfig(rigRoot)
	injectLiveness(t, liveWorktreeState{scanned: true, cwds: []string{canonicalTestPath(wt)}})

	fake := events.NewFake()
	var stderr bytes.Buffer
	for pass := 1; pass <= 3; pass++ {
		reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{reapTestRigName: store}, nil, false, fake, nil, &stderr)
	}

	if reasons := skipReasonsFor(t, fake, wt); len(reasons) != 3 {
		t.Fatalf("emitted %d reap_skipped events with a nil tracker, want 3 (one per pass): %q", len(reasons), reasons)
	}
}

// TestReapSkipTracker_SuppressesDryRunWouldReapRepeats proves the dry-run
// would-reap notice — which rides the same event type — is deduped too. A city
// left in dry-run re-announces the same reapable trees forever otherwise.
func TestReapSkipTracker_SuppressesDryRunWouldReapRepeats(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-dry0001")
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-dry0001", Status: "closed"}}, nil)
	cfg := reapTestConfig(rigRoot)
	injectLiveness(t, liveWorktreeState{scanned: true})

	fake := events.NewFake()
	skips := newReapSkipTracker()
	var stderr bytes.Buffer

	for pass := 1; pass <= 3; pass++ {
		report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{reapTestRigName: store}, nil, true, fake, skips, &stderr)
		if len(report.Reaped) != 1 {
			t.Fatalf("pass %d: Reaped = %+v, want the would-reap entry reported every pass", pass, report.Reaped)
		}
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("dry-run removed worktree %s: %v", wt, err)
	}

	reasons := skipReasonsFor(t, fake, wt)
	if len(reasons) != 1 {
		t.Fatalf("emitted %d would-reap events across 3 identical dry-run passes, want exactly 1: %q", len(reasons), reasons)
	}
}

// TestReapSkipTracker_TracksPathsIndependently proves suppression is keyed per
// worktree: one tree's steady state does not silence another tree's news.
func TestReapSkipTracker_TracksPathsIndependently(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	stable := addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-stab001")
	changing := addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-mover01")
	store := beads.NewMemStoreFrom(1, []beads.Bead{
		{ID: "ga-stab001", Status: "closed"},
		{ID: "ga-mover01", Status: "closed"},
	}, nil)
	cfg := reapTestConfig(rigRoot)

	fake := events.NewFake()
	skips := newReapSkipTracker()
	var stderr bytes.Buffer

	live := []string{canonicalTestPath(stable), canonicalTestPath(changing)}
	injectLiveness(t, liveWorktreeState{scanned: true, cwds: live})
	reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{reapTestRigName: store}, nil, false, fake, skips, &stderr)

	// Only `changing` transitions; `stable` holds its liveness protection.
	injectLiveness(t, liveWorktreeState{scanned: true, cwds: []string{canonicalTestPath(stable)}})
	if err := os.WriteFile(filepath.Join(changing, "dirty.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatalf("write uncommitted file: %v", err)
	}
	reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{reapTestRigName: store}, nil, false, fake, skips, &stderr)

	if reasons := skipReasonsFor(t, fake, stable); len(reasons) != 1 {
		t.Errorf("stable worktree emitted %d events, want 1: %q", len(reasons), reasons)
	}
	if reasons := skipReasonsFor(t, fake, changing); len(reasons) != 2 {
		t.Errorf("changing worktree emitted %d events, want 2: %q", len(reasons), reasons)
	}
}

// TestReapSkipTracker_SuppressesQuarantineRepeats pins that the freshness
// class deduplicates like every other skip class. The reason string is the
// tracker's key, so any per-sweep-varying component in it (an elapsed age at
// second resolution, say) silently defeats suppression for that whole class.
//
// The worktree is aged forward between passes — still well inside the default
// quarantine window — because that is what real sweeps see: the controller
// ticks every ~12s and the tree's elapsed age is different every time. Three
// passes run back-to-back share one wall-clock second and would agree on any
// age reading, so they cannot tell a stable reason from a volatile one.
func TestReapSkipTracker_SuppressesQuarantineRepeats(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktreeWithAge(t, rigRoot, cityPath, "builder", "ga-quar001", 0)
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-quar001", Status: "closed"}}, nil)
	cfg := reapTestConfig(rigRoot) // default quarantine window
	injectLiveness(t, liveWorktreeState{scanned: true})

	fake := events.NewFake()
	skips := newReapSkipTracker()
	var stderr bytes.Buffer

	for pass := 1; pass <= 3; pass++ {
		backdateWorktreeGitFile(t, wt, time.Duration(pass)*time.Minute)
		report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{reapTestRigName: store}, nil, false, fake, skips, &stderr)
		if len(report.Protected) != 1 {
			t.Fatalf("pass %d: Protected = %+v, want exactly 1 quarantine entry regardless of suppression", pass, report.Protected)
		}
	}

	reasons := skipReasonsFor(t, fake, wt)
	if len(reasons) != 1 {
		t.Fatalf("emitted %d quarantine reap_skipped events across 3 passes, want exactly 1: %q", len(reasons), reasons)
	}
	if !strings.Contains(reasons[0], "quarantine") {
		t.Errorf("reason = %q, want it to name the quarantine protection", reasons[0])
	}
}
