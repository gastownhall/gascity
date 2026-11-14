package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/pathutil"
)

// rigWorktreeTestMinAge is the min-age the subset-B tests configure. Fixtures
// are backdated past it unless a test is exercising the quarantine itself.
const rigWorktreeTestMinAge = 24 * time.Hour

// addRigRootWorktree adds a worktree directly under <rigRoot>/worktrees/<name>,
// the layout subset B (agent-improvised deploy-*/review-*/mayor-*) uses. A
// detached worktree is checked out at HEAD with no branch; an attached one gets
// branch "wt-<name>". The .git pointer file is backdated by age (0 leaves the
// real mtime).
func addRigRootWorktree(t *testing.T, rigRoot, name string, detached bool, age time.Duration) string {
	t.Helper()
	wtPath := filepath.Join(rigRoot, "worktrees", name)
	if err := os.MkdirAll(filepath.Dir(wtPath), 0o755); err != nil {
		t.Fatalf("mkdir worktrees dir: %v", err)
	}
	if detached {
		mustGit(t, rigRoot, "worktree", "add", "--detach", wtPath, "HEAD")
	} else {
		mustGit(t, rigRoot, "worktree", "add", "-b", "wt-"+name, wtPath)
	}
	if age > 0 {
		backdateWorktreeGitFile(t, wtPath, age)
	}
	return wtPath
}

// rigWorktreeConfig is reapTestConfig with the subset-B min-age pinned, so the
// tests do not depend on the default value.
func rigWorktreeConfig(rigRoot string) *config.City {
	cfg := reapTestConfig(rigRoot)
	minutes := int(rigWorktreeTestMinAge / time.Minute)
	cfg.Daemon.AutoReapRigWorktreesMinAgeMinutes = &minutes
	return cfg
}

// rigReapRecorder collects every event the reaper emits.
type rigReapRecorder struct{ events []events.Event }

func (r *rigReapRecorder) Record(e events.Event) { r.events = append(r.events, e) }

func (r *rigReapRecorder) ofType(typ string) []events.Event {
	var out []events.Event
	for _, e := range r.events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func runDetachedReap(t *testing.T, cfg *config.City, sessionDirs []string, dryRun bool, rec events.Recorder, skips *reapSkipTracker) (reapReport, string) {
	t.Helper()
	var stderr bytes.Buffer
	report := reapDetachedRigWorktrees(cfg, nil, sessionDirs, dryRun, rec, skips, &stderr)
	return report, stderr.String()
}

// TestReapDetachedRigWorktrees_ReapsIdleDetachedWorktree is the happy path: a
// clean, old, detached worktree under <rig>/worktrees is removed, with no
// owning bead anywhere in sight (eligibility is unconditional).
func TestReapDetachedRigWorktrees_ReapsIdleDetachedWorktree(t *testing.T) {
	_, rigRoot := initReapRig(t)
	wt := addRigRootWorktree(t, rigRoot, "deploy-abc", true, 48*time.Hour)
	injectLiveness(t, liveWorktreeState{scanned: true})
	rec := &rigReapRecorder{}

	report, stderr := runDetachedReap(t, rigWorktreeConfig(rigRoot), nil, false, rec, nil)

	if len(report.Reaped) != 1 || filepath.Base(report.Reaped[0].Path) != "deploy-abc" {
		t.Fatalf("Reaped = %+v, want exactly deploy-abc\nstderr:\n%s", report.Reaped, stderr)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree %s still present after reap (stat err=%v)", wt, err)
	}
	reaped := rec.ofType(events.BeadWorktreeReaped)
	if len(reaped) != 1 {
		t.Fatalf("bead.worktree.reaped events = %d, want 1", len(reaped))
	}
	var payload events.BeadWorktreeReapedPayload
	if err := json.Unmarshal(reaped[0].Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.BeadID != "" || payload.Rig != reapTestRigName || payload.Path != wt {
		t.Fatalf("payload = %+v, want empty bead id, rig %q, path %q", payload, reapTestRigName, wt)
	}
}

// TestReapDetachedRigWorktrees_SkipsBranchAttachedWorktree guards the
// double-jurisdiction boundary: a branch-attached entry under <rig>/worktrees
// belongs to subset A (ga-4wem3l) and this reaper must never touch it, however
// old and idle it is.
func TestReapDetachedRigWorktrees_SkipsBranchAttachedWorktree(t *testing.T) {
	_, rigRoot := initReapRig(t)
	wt := addRigRootWorktree(t, rigRoot, "ga-attached01", false, 48*time.Hour)
	injectLiveness(t, liveWorktreeState{scanned: true})

	report, stderr := runDetachedReap(t, rigWorktreeConfig(rigRoot), nil, false, events.Discard, nil)

	if len(report.Reaped) != 0 || len(report.Protected) != 0 {
		t.Fatalf("report = %+v, want branch-attached worktree entirely out of scope\nstderr:\n%s", report, stderr)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("branch-attached worktree %s was removed: %v", wt, err)
	}
}

// TestReapDetachedRigWorktrees_IgnoresWorktreesOutsideRigWorktreesDir proves
// scope: a detached worktree elsewhere (here, the per-bead .gc/worktrees tree,
// which reapClosedBeadWorktrees owns) is not this reaper's to remove, and
// neither is a nested entry below <rig>/worktrees/<name>/.
func TestReapDetachedRigWorktrees_IgnoresWorktreesOutsideRigWorktreesDir(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	outside := filepath.Join(cityPath, ".gc", "worktrees", reapTestRigName, "builder", "deploy-outside")
	if err := os.MkdirAll(filepath.Dir(outside), 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, rigRoot, "worktree", "add", "--detach", outside, "HEAD")
	backdateWorktreeGitFile(t, outside, 48*time.Hour)
	nested := filepath.Join(rigRoot, "worktrees", "group", "deploy-nested")
	if err := os.MkdirAll(filepath.Dir(nested), 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, rigRoot, "worktree", "add", "--detach", nested, "HEAD")
	backdateWorktreeGitFile(t, nested, 48*time.Hour)
	injectLiveness(t, liveWorktreeState{scanned: true})

	report, stderr := runDetachedReap(t, rigWorktreeConfig(rigRoot), nil, false, events.Discard, nil)

	if len(report.Reaped) != 0 || len(report.Protected) != 0 {
		t.Fatalf("report = %+v, want out-of-scope worktrees ignored\nstderr:\n%s", report, stderr)
	}
	for _, p := range []string{outside, nested} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("out-of-scope worktree %s was removed: %v", p, err)
		}
	}
}

// TestReapDetachedRigWorktrees_ProtectsTooYoung proves the independent min-age
// quarantine, which is the primary safety net given eligibility is otherwise
// unconditional.
func TestReapDetachedRigWorktrees_ProtectsTooYoung(t *testing.T) {
	_, rigRoot := initReapRig(t)
	wt := addRigRootWorktree(t, rigRoot, "review-young", true, time.Hour) // < 24h
	injectLiveness(t, liveWorktreeState{scanned: true})

	report, _ := runDetachedReap(t, rigWorktreeConfig(rigRoot), nil, false, events.Discard, nil)

	if len(report.Reaped) != 0 {
		t.Fatalf("Reaped = %+v, want 0 for a worktree inside the quarantine", report.Reaped)
	}
	if len(report.Protected) != 1 || !strings.Contains(report.Protected[0].Reason, "too young") {
		t.Fatalf("Protected = %+v, want 1 too-young entry", report.Protected)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("young worktree %s was removed: %v", wt, err)
	}
}

// TestReapDetachedRigWorktrees_MinAgeIsIndependentOfClosedBeadKnob proves the
// subset-B quarantine is its own knob: shrinking the closed-bead reaper's
// min-age must not shrink this one.
func TestReapDetachedRigWorktrees_MinAgeIsIndependentOfClosedBeadKnob(t *testing.T) {
	_, rigRoot := initReapRig(t)
	wt := addRigRootWorktree(t, rigRoot, "mayor-young", true, time.Hour)
	injectLiveness(t, liveWorktreeState{scanned: true})
	cfg := rigWorktreeConfig(rigRoot)
	zero := 0
	cfg.Daemon.AutoReapClosedBeadWorktreesMinAgeMinutes = &zero

	report, _ := runDetachedReap(t, cfg, nil, false, events.Discard, nil)

	if len(report.Reaped) != 0 {
		t.Fatalf("Reaped = %+v, want 0: the closed-bead min-age knob must not govern subset B", report.Reaped)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree %s was removed: %v", wt, err)
	}
}

// TestReapDetachedRigWorktrees_ProtectsLiveProcess proves the reused liveness
// gate: a live process cwd inside the worktree vetoes the reap.
func TestReapDetachedRigWorktrees_ProtectsLiveProcess(t *testing.T) {
	_, rigRoot := initReapRig(t)
	wt := addRigRootWorktree(t, rigRoot, "deploy-live", true, 48*time.Hour)
	injectLiveness(t, liveWorktreeState{scanned: true, cwds: []string{pathutil.NormalizePathForCompare(wt)}})

	report, _ := runDetachedReap(t, rigWorktreeConfig(rigRoot), nil, false, events.Discard, nil)

	if len(report.Reaped) != 0 {
		t.Fatalf("Reaped = %+v, want 0 for a live worktree", report.Reaped)
	}
	if len(report.Protected) != 1 || !strings.Contains(report.Protected[0].Reason, "live") {
		t.Fatalf("Protected = %+v, want 1 live-protected entry", report.Protected)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("live worktree %s was removed: %v", wt, err)
	}
}

// TestReapDetachedRigWorktrees_ProtectsActiveSessionDir proves the session
// cross-check: no live cwd, but an open session records this directory.
func TestReapDetachedRigWorktrees_ProtectsActiveSessionDir(t *testing.T) {
	_, rigRoot := initReapRig(t)
	wt := addRigRootWorktree(t, rigRoot, "review-sess", true, 48*time.Hour)
	injectLiveness(t, liveWorktreeState{scanned: true})

	report, _ := runDetachedReap(t, rigWorktreeConfig(rigRoot), []string{wt}, false, events.Discard, nil)

	if len(report.Reaped) != 0 {
		t.Fatalf("Reaped = %+v, want 0 when a session claims the worktree", report.Reaped)
	}
	if len(report.Protected) != 1 || !strings.Contains(report.Protected[0].Reason, "session") {
		t.Fatalf("Protected = %+v, want 1 session-protected entry", report.Protected)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("session-claimed worktree %s was removed: %v", wt, err)
	}
}

// TestReapDetachedRigWorktrees_FailsClosedWhenLivenessUnavailable proves an
// indeterminate /proc scan protects everything.
func TestReapDetachedRigWorktrees_FailsClosedWhenLivenessUnavailable(t *testing.T) {
	_, rigRoot := initReapRig(t)
	wt := addRigRootWorktree(t, rigRoot, "deploy-noproc", true, 48*time.Hour)
	injectLiveness(t, liveWorktreeState{scanned: false})

	report, _ := runDetachedReap(t, rigWorktreeConfig(rigRoot), nil, false, events.Discard, nil)

	if len(report.Reaped) != 0 {
		t.Fatalf("Reaped = %+v, want 0 when liveness is unavailable", report.Reaped)
	}
	if len(report.Protected) != 1 || !strings.Contains(report.Protected[0].Reason, "liveness scan unavailable") {
		t.Fatalf("Protected = %+v, want 1 fail-closed entry", report.Protected)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree %s was removed: %v", wt, err)
	}
}

// TestReapDetachedRigWorktrees_ProtectsUncommittedWork proves the git-safety
// gate on a dirty tree (an untracked file counts).
func TestReapDetachedRigWorktrees_ProtectsUncommittedWork(t *testing.T) {
	_, rigRoot := initReapRig(t)
	wt := addRigRootWorktree(t, rigRoot, "deploy-dirty", true, 48*time.Hour)
	if err := os.WriteFile(filepath.Join(wt, "scratch.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	injectLiveness(t, liveWorktreeState{scanned: true})

	report, _ := runDetachedReap(t, rigWorktreeConfig(rigRoot), nil, false, events.Discard, nil)

	if len(report.Reaped) != 0 {
		t.Fatalf("Reaped = %+v, want 0 for a dirty worktree", report.Reaped)
	}
	if len(report.Protected) != 1 || !strings.Contains(report.Protected[0].Reason, "unsafe git state") {
		t.Fatalf("Protected = %+v, want 1 unsafe-git-state entry", report.Protected)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("dirty worktree %s was removed: %v", wt, err)
	}
}

// TestReapDetachedRigWorktrees_ProtectsUnreachableCommits is the core detached
// hazard: a commit made on a detached HEAD is reachable from no ref, and
// removing the checkout would orphan it.
func TestReapDetachedRigWorktrees_ProtectsUnreachableCommits(t *testing.T) {
	_, rigRoot := initReapRig(t)
	wt := addRigRootWorktree(t, rigRoot, "deploy-orphan", true, 48*time.Hour)
	if err := os.WriteFile(filepath.Join(wt, "work.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, wt, "add", ".")
	mustGit(t, wt, "-c", "commit.gpgsign=false", "commit", "-m", "detached work")
	backdateWorktreeGitFile(t, wt, 48*time.Hour)
	injectLiveness(t, liveWorktreeState{scanned: true})

	report, _ := runDetachedReap(t, rigWorktreeConfig(rigRoot), nil, false, events.Discard, nil)

	if len(report.Reaped) != 0 {
		t.Fatalf("Reaped = %+v, want 0 for a worktree holding an unreachable commit", report.Reaped)
	}
	if len(report.Protected) != 1 || !strings.Contains(report.Protected[0].Reason, "unsafe git state") {
		t.Fatalf("Protected = %+v, want 1 unsafe-git-state entry", report.Protected)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree %s with unreachable commit was removed: %v", wt, err)
	}
}

// TestReapDetachedRigWorktrees_ProtectsWhenRepoHasStash proves the stash gate
// the architecture ruling requires. `git stash list` is repo-wide, so any stash
// in the rig repo protects every candidate there (the safe direction).
func TestReapDetachedRigWorktrees_ProtectsWhenRepoHasStash(t *testing.T) {
	_, rigRoot := initReapRig(t)
	wt := addRigRootWorktree(t, rigRoot, "deploy-stash", true, 48*time.Hour)
	if err := os.WriteFile(filepath.Join(rigRoot, "README.md"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, rigRoot, "stash", "push", "-m", "ga-juwpxp-test-stash")
	injectLiveness(t, liveWorktreeState{scanned: true})

	report, _ := runDetachedReap(t, rigWorktreeConfig(rigRoot), nil, false, events.Discard, nil)

	if len(report.Reaped) != 0 {
		t.Fatalf("Reaped = %+v, want 0 when the repo has a stash", report.Reaped)
	}
	if len(report.Protected) != 1 || !strings.Contains(report.Protected[0].Reason, "stash") {
		t.Fatalf("Protected = %+v, want 1 stash-protected entry", report.Protected)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree %s was removed despite a stash: %v", wt, err)
	}
}

// TestReapDetachedRigWorktrees_NeverTouchesAgentHomeName proves the
// session-home guard: a directory named after a configured agent is left alone.
func TestReapDetachedRigWorktrees_NeverTouchesAgentHomeName(t *testing.T) {
	_, rigRoot := initReapRig(t)
	wt := addRigRootWorktree(t, rigRoot, "builder", true, 48*time.Hour)
	injectLiveness(t, liveWorktreeState{scanned: true})
	cfg := rigWorktreeConfig(rigRoot)
	cfg.Agents = []config.Agent{{Name: "builder", Dir: reapTestRigName}}

	report, _ := runDetachedReap(t, cfg, nil, false, events.Discard, nil)

	if len(report.Reaped) != 0 {
		t.Fatalf("Reaped = %+v, want 0 for an agent-home directory", report.Reaped)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("agent-home worktree %s was removed: %v", wt, err)
	}
}

// TestReapDetachedRigWorktrees_DryRunDeletesNothing proves dry-run reports the
// would-reap decision as a reap_skipped event and removes nothing.
func TestReapDetachedRigWorktrees_DryRunDeletesNothing(t *testing.T) {
	_, rigRoot := initReapRig(t)
	wt := addRigRootWorktree(t, rigRoot, "mayor-dry", true, 48*time.Hour)
	injectLiveness(t, liveWorktreeState{scanned: true})
	rec := &rigReapRecorder{}

	report, stderr := runDetachedReap(t, rigWorktreeConfig(rigRoot), nil, true, rec, nil)

	if !report.DryRun || len(report.Reaped) != 1 {
		t.Fatalf("report = %+v, want DryRun with 1 would-reap entry", report)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("dry-run removed worktree %s: %v", wt, err)
	}
	if len(rec.ofType(events.BeadWorktreeReaped)) != 0 {
		t.Fatal("dry-run emitted bead.worktree.reaped; it must emit only reap_skipped")
	}
	if len(rec.ofType(events.BeadWorktreeReapSkipped)) != 1 {
		t.Fatalf("reap_skipped events = %d, want 1 dry-run would-reap event", len(rec.ofType(events.BeadWorktreeReapSkipped)))
	}
	if !strings.Contains(stderr, "dry-run: would reap") {
		t.Fatalf("stderr = %q, want a dry-run would-reap line", stderr)
	}
}

// TestReapDetachedRigWorktrees_SkipsSuspendedRig proves a suspended rig is not
// swept.
func TestReapDetachedRigWorktrees_SkipsSuspendedRig(t *testing.T) {
	_, rigRoot := initReapRig(t)
	wt := addRigRootWorktree(t, rigRoot, "deploy-susp", true, 48*time.Hour)
	injectLiveness(t, liveWorktreeState{scanned: true})
	var stderr bytes.Buffer

	report := reapDetachedRigWorktrees(rigWorktreeConfig(rigRoot), map[string]bool{reapTestRigName: true}, nil, false, events.Discard, nil, &stderr)

	if len(report.Reaped) != 0 || len(report.Protected) != 0 {
		t.Fatalf("report = %+v, want a suspended rig skipped", report)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree in suspended rig was removed: %v", err)
	}
}

// TestReapDetachedRigWorktrees_SkipTrackerSuppressesRepeats proves the skip
// reporting is edge-triggered like its sibling: an unchanged protected decision
// is announced on the first pass only.
func TestReapDetachedRigWorktrees_SkipTrackerSuppressesRepeats(t *testing.T) {
	_, rigRoot := initReapRig(t)
	addRigRootWorktree(t, rigRoot, "review-steady", true, time.Hour)
	injectLiveness(t, liveWorktreeState{scanned: true})
	rec := &rigReapRecorder{}
	skips := newReapSkipTracker()
	cfg := rigWorktreeConfig(rigRoot)

	first, _ := runDetachedReap(t, cfg, nil, false, rec, skips)
	second, _ := runDetachedReap(t, cfg, nil, false, rec, skips)

	if len(first.Protected) != 1 || len(second.Protected) != 1 {
		t.Fatalf("Protected counts = %d, %d, want 1 on both passes (report is never suppressed)", len(first.Protected), len(second.Protected))
	}
	if got := len(rec.ofType(events.BeadWorktreeReapSkipped)); got != 1 {
		t.Fatalf("reap_skipped events across two passes = %d, want 1", got)
	}
}

// TestReapDetachedRigWorktrees_NilConfigIsNoop proves the nil guard.
func TestReapDetachedRigWorktrees_NilConfigIsNoop(t *testing.T) {
	var stderr bytes.Buffer
	report := reapDetachedRigWorktrees(nil, nil, nil, false, nil, nil, &stderr)
	if len(report.Reaped) != 0 || len(report.Protected) != 0 {
		t.Fatalf("report = %+v, want empty", report)
	}
}

// TestCityRuntimeTick_RigWorktreeReapDisabledByDefault proves the tick leaves a
// reap-eligible detached worktree alone when neither subset-B flag is set, even
// with the closed-bead reaper fully enabled.
func TestCityRuntimeTick_RigWorktreeReapDisabledByDefault(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addRigRootWorktree(t, rigRoot, "deploy-default", true, 48*time.Hour)
	cfg := rigWorktreeConfig(rigRoot)
	enabled := true
	cfg.Daemon.AutoReapClosedBeadWorktrees = &enabled
	injectLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	runReapTick(t, newReapTickRuntime(cityPath, cfg, nil, &stderr))

	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("detached worktree removed with subset-B flags unset: %v\nstderr:\n%s", err, stderr.String())
	}
}

// TestCityRuntimeTick_RigWorktreeReapWhenEnabled proves the tick runs the new
// reaper for real under auto_reap_rig_worktrees=true, independent of the
// closed-bead flags.
func TestCityRuntimeTick_RigWorktreeReapWhenEnabled(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addRigRootWorktree(t, rigRoot, "deploy-enabled", true, 48*time.Hour)
	cfg := rigWorktreeConfig(rigRoot)
	enabled := true
	cfg.Daemon.AutoReapRigWorktrees = &enabled
	injectLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	runReapTick(t, newReapTickRuntime(cityPath, cfg, nil, &stderr))

	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("detached worktree %s survived an enabled tick (stat err=%v)\nstderr:\n%s", wt, err, stderr.String())
	}
}

// TestCityRuntimeTick_RigWorktreeReapDryRunDeletesNothing proves the dry-run
// flag alone runs the new reaper report-only.
func TestCityRuntimeTick_RigWorktreeReapDryRunDeletesNothing(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addRigRootWorktree(t, rigRoot, "deploy-dryrun", true, 48*time.Hour)
	cfg := rigWorktreeConfig(rigRoot)
	dryRun := true
	cfg.Daemon.AutoReapRigWorktreesDryRun = &dryRun
	injectLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	runReapTick(t, newReapTickRuntime(cityPath, cfg, nil, &stderr))

	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("dry-run tick removed worktree %s: %v", wt, err)
	}
	if !strings.Contains(stderr.String(), "dry-run: would reap") {
		t.Fatalf("stderr = %q, want a dry-run would-reap line", stderr.String())
	}
}
