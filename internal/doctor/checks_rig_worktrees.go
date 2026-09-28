package doctor

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/config"
)

// RigWorktreesCheck reports the per-bead worktree population that
// formulas create at <rig>/worktrees/<bead-id>/.
//
// This is a different population from the one WorktreeDiskSizeCheck,
// NestedWorktreePruneCheck and WorktreeCheck inspect: those three are
// scoped to $CITY/.gc/worktrees/ (agent home worktrees and anything
// nested inside them). A rig whose worktrees/ directory holds tens of
// gigabytes of per-bead trees was therefore invisible to every worktree
// check doctor had, and the board came back green over it.
//
// Observation only. No automatic reclamation covers this population
// today: the bead-worktree reaper (cmd/gc/bead_worktree_reaper.go:179)
// skips any worktree not strictly under .gc/worktrees/<rig>/, and there
// is no rig-root equivalent yet, so these trees are removed by hand.
// Doctor still does not remove them — deciding when a worktree is
// disposable (clean tree, no unpushed work, no in-flight detached head)
// is a judgment doctor --fix should not grow a second, weaker opinion
// about.
type RigWorktreesCheck struct {
	rig config.Rig
	cfg config.DoctorConfig
	// measureDir is injectable so tests can avoid shelling out to du.
	// Production uses duDirBytes from checks.go.
	measureDir func(string) (int64, bool, error)
}

// NewRigWorktreesCheck creates the per-rig worktree population check.
// The cfg is read for thresholds at Run time, so reload-time changes
// propagate naturally, and the thresholds are deliberately the same
// [doctor].worktree_rig_* pair the city-scoped size check uses: the
// question ("is a worktree population eating this disk?") is the same
// one, and a second knob would be a second thing to keep in sync.
func NewRigWorktreesCheck(rig config.Rig, cfg config.DoctorConfig) *RigWorktreesCheck {
	measure := func(path string) (int64, bool, error) {
		n, ok, err := duDirBytes(path)
		if err != nil {
			return n, ok, fmt.Errorf("measure rig worktree dir %q: %w", path, err)
		}
		return n, ok, nil
	}
	return &RigWorktreesCheck{rig: rig, cfg: cfg, measureDir: measure}
}

// Name returns the check identifier.
func (c *RigWorktreesCheck) Name() string { return "rig:" + c.rig.Name + ":worktrees" }

// Run counts the per-bead worktrees under <rig>/worktrees/ and measures
// their aggregate footprint.
func (c *RigWorktreesCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	root := filepath.Join(c.rig.Path, "worktrees")

	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			r.Status = StatusOK
			r.Message = fmt.Sprintf("no %s directory", root)
			return r
		}
		// Unreadable is not the same as absent. Saying "none" here is
		// the exact failure this check exists to correct, so surface it.
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("reading %s: %v", root, err)
		r.FixHint = "check filesystem permissions on <rig>/worktrees/"
		return r
	}

	var count int
	for _, e := range entries {
		if e.IsDir() {
			count++
		}
	}
	if count == 0 {
		r.Status = StatusOK
		r.Message = fmt.Sprintf("no per-bead worktrees under %s", root)
		return r
	}

	measure := c.measureDir
	if measure == nil {
		measure = duDirBytes
	}
	// One measurement of the root, not one per worktree: a rig can hold
	// dozens, and this check runs on every doctor invocation.
	bytes, exists, err := measure(root)
	if err != nil {
		// "We can't tell" must not look like "we're fine". Matches
		// WorktreeDiskSizeCheck's policy of escalating on measurement
		// failure — the count alone is still worth reporting.
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("%d per-bead worktree(s) under %s, size unmeasurable", count, root)
		r.Details = []string{err.Error()}
		r.FixHint = "check filesystem permissions on <rig>/worktrees/"
		return r
	}
	if !exists {
		r.Status = StatusOK
		r.Message = fmt.Sprintf("no %s directory", root)
		return r
	}

	warn := c.cfg.WorktreeRigWarnBytes()
	errBytes := c.cfg.WorktreeRigErrorBytes()
	pruneHint := "no automatic reclamation covers this population — the bead-worktree reaper is scoped to .gc/worktrees/<rig>/; inspect with `git -C " + c.rig.Path + " worktree list` and remove individually with `git worktree remove <path>`"

	switch {
	case bytes >= errBytes:
		r.Status = StatusError
		r.Message = fmt.Sprintf("%d per-bead worktree(s) in %s using %s (exceeds %s error threshold)",
			count, root, humanSize(bytes), humanSize(errBytes))
		r.FixHint = pruneHint
	case bytes >= warn:
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("%d per-bead worktree(s) in %s using %s (exceeds %s warn threshold)",
			count, root, humanSize(bytes), humanSize(warn))
		r.FixHint = pruneHint
	default:
		r.Status = StatusOK
		r.Message = fmt.Sprintf("%d per-bead worktree(s) in %s using %s (under %s warn)",
			count, root, humanSize(bytes), humanSize(warn))
	}
	return r
}

// CanFix returns false — see the type comment: nothing reclaims this
// population automatically (the reaper is scoped to .gc/worktrees/<rig>/),
// and doctor --fix must not take over the judgment of when a worktree is
// disposable.
func (c *RigWorktreesCheck) CanFix() bool { return false }

// Fix is a no-op; see CanFix.
func (c *RigWorktreesCheck) Fix(_ *CheckContext) error { return nil }
