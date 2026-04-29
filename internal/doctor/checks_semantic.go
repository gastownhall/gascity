package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/pathutil"
)

// --- Duration reasonableness check ---

// DurationRangeCheck validates that duration fields in the config have
// reasonable values. Extremely small durations (< 100ms for timeouts) or
// extremely large ones (> 7 days for patrol intervals) are likely typos.
type DurationRangeCheck struct {
	cfg *config.City
}

// NewDurationRangeCheck creates a check for duration field reasonableness.
func NewDurationRangeCheck(cfg *config.City) *DurationRangeCheck {
	return &DurationRangeCheck{cfg: cfg}
}

// Name returns the check identifier.
func (c *DurationRangeCheck) Name() string { return "duration-range" }

// durationRange defines min/max bounds for a duration field.
type durationRange struct {
	context string
	field   string
	value   string
	min     time.Duration
	max     time.Duration
}

// Run checks all duration fields against reasonable bounds.
func (c *DurationRangeCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	var issues []string

	ranges := c.collectRanges()
	for _, dr := range ranges {
		if dr.value == "" {
			continue
		}
		d, err := time.ParseDuration(dr.value)
		if err != nil {
			// ValidateDurations handles parse errors; skip here.
			continue
		}
		if d < dr.min {
			issues = append(issues, fmt.Sprintf(
				"%s %s = %q (%v) is below minimum %v",
				dr.context, dr.field, dr.value, d, dr.min))
		}
		if d > dr.max {
			issues = append(issues, fmt.Sprintf(
				"%s %s = %q (%v) exceeds maximum %v",
				dr.context, dr.field, dr.value, d, dr.max))
		}
	}

	if len(issues) == 0 {
		r.Status = StatusOK
		r.Message = "all durations within reasonable bounds"
		return r
	}
	r.Status = StatusWarning
	r.Message = fmt.Sprintf("%d duration(s) outside reasonable bounds", len(issues))
	r.Details = issues
	return r
}

// collectRanges builds the list of (field, value, min, max) entries to check.
func (c *DurationRangeCheck) collectRanges() []durationRange {
	const (
		minTimeout  = 100 * time.Millisecond
		maxTimeout  = 1 * time.Hour
		minInterval = 100 * time.Millisecond
		maxInterval = 24 * time.Hour
		minWindow   = 1 * time.Minute
		maxWindow   = 7 * 24 * time.Hour // 7 days
		minTTL      = 1 * time.Minute
		maxTTL      = 30 * 24 * time.Hour // 30 days
	)

	var ranges []durationRange

	// Session config.
	ranges = append(ranges,
		durationRange{"[session]", "setup_timeout", c.cfg.Session.SetupTimeout, minTimeout, maxTimeout},
		durationRange{"[session]", "nudge_ready_timeout", c.cfg.Session.NudgeReadyTimeout, minTimeout, maxTimeout},
		durationRange{"[session]", "nudge_retry_interval", c.cfg.Session.NudgeRetryInterval, minInterval, maxTimeout},
		durationRange{"[session]", "nudge_lock_timeout", c.cfg.Session.NudgeLockTimeout, minTimeout, maxTimeout},
		durationRange{"[session]", "startup_timeout", c.cfg.Session.StartupTimeout, minTimeout, maxTimeout},
	)

	// Daemon config.
	ranges = append(ranges,
		durationRange{"[daemon]", "patrol_interval", c.cfg.Daemon.PatrolInterval, minInterval, maxInterval},
		durationRange{"[daemon]", "restart_window", c.cfg.Daemon.RestartWindow, minWindow, maxWindow},
		durationRange{"[daemon]", "shutdown_timeout", c.cfg.Daemon.ShutdownTimeout, minTimeout, maxTimeout},
		durationRange{"[daemon]", "wisp_gc_interval", c.cfg.Daemon.WispGCInterval, minInterval, maxInterval},
		durationRange{"[daemon]", "wisp_ttl", c.cfg.Daemon.WispTTL, minTTL, maxTTL},
		durationRange{"[daemon]", "drift_drain_timeout", c.cfg.Daemon.DriftDrainTimeout, minTimeout, maxTimeout},
	)

	// Per-agent durations.
	for _, a := range c.cfg.Agents {
		ctx := fmt.Sprintf("agent %q", a.QualifiedName())
		if a.IdleTimeout != "" {
			ranges = append(ranges,
				durationRange{ctx, "idle_timeout", a.IdleTimeout, minTimeout, maxWindow})
		}
		if a.DrainTimeout != "" {
			ranges = append(ranges,
				durationRange{ctx, "drain_timeout", a.DrainTimeout, minTimeout, maxTimeout})
		}
	}

	return ranges
}

// CanFix returns false — unreasonable durations must be corrected by the user.
func (c *DurationRangeCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *DurationRangeCheck) Fix(_ *CheckContext) error { return nil }

// --- Event log size check ---

// EventLogSizeCheck warns when .gc/events.jsonl exceeds a size threshold.
// The event log grows unbounded; large files slow down reads and waste disk.
type EventLogSizeCheck struct {
	// MaxSize is the warning threshold in bytes. Defaults to 100 MB.
	MaxSize int64
}

// NewEventLogSizeCheck creates a check for event log size.
func NewEventLogSizeCheck() *EventLogSizeCheck {
	return &EventLogSizeCheck{MaxSize: 100 * 1024 * 1024} // 100 MB
}

// Name returns the check identifier.
func (c *EventLogSizeCheck) Name() string { return "events-log-size" }

// Run checks the size of events.jsonl.
func (c *EventLogSizeCheck) Run(ctx *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	path := filepath.Join(ctx.CityPath, ".gc", "events.jsonl")
	fi, err := os.Stat(path)
	if err != nil {
		// File missing is OK — EventsLogCheck handles that.
		r.Status = StatusOK
		r.Message = "events.jsonl not present (nothing to check)"
		return r
	}

	size := fi.Size()
	if size <= c.MaxSize {
		r.Status = StatusOK
		r.Message = fmt.Sprintf("events.jsonl size: %s", humanSize(size))
		return r
	}

	r.Status = StatusWarning
	r.Message = fmt.Sprintf("events.jsonl is %s (exceeds %s threshold)",
		humanSize(size), humanSize(c.MaxSize))
	r.FixHint = "consider truncating or archiving .gc/events.jsonl"
	return r
}

// CanFix returns false — the user should decide how to handle large logs.
func (c *EventLogSizeCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *EventLogSizeCheck) Fix(_ *CheckContext) error { return nil }

// --- Worktree disk size check ---

// rigSize pairs a rig directory name with its measured byte footprint
// under .gc/worktrees/<rig>/. Used as the sort key for ordered output.
type rigSize struct {
	name  string
	bytes int64
}

// WorktreeDiskSizeCheck warns when a per-rig footprint under
// .gc/worktrees/<rig>/ exceeds the configured threshold. Build
// artifacts, nested task worktrees, and accumulated state can grow
// unboundedly here; without this check the disk fills silently.
type WorktreeDiskSizeCheck struct {
	cfg config.DoctorConfig
	// measureDir is injectable so tests can avoid shelling out to du.
	// Production uses duDirBytes from checks.go.
	measureDir func(string) (int64, bool, error)
}

// NewWorktreeDiskSizeCheck creates a worktree disk-footprint check.
// The cfg is read for thresholds and policy at Run time, so reload-time
// changes propagate naturally.
func NewWorktreeDiskSizeCheck(cfg config.DoctorConfig) *WorktreeDiskSizeCheck {
	return &WorktreeDiskSizeCheck{cfg: cfg, measureDir: duDirBytes}
}

// Name returns the check identifier.
func (c *WorktreeDiskSizeCheck) Name() string { return "worktree-disk-size" }

// Run measures each rig's worktree footprint and reports any rigs
// exceeding the configured warn or error thresholds.
func (c *WorktreeDiskSizeCheck) Run(ctx *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	wtRoot := filepath.Join(ctx.CityPath, ".gc", "worktrees")

	rigEntries, err := os.ReadDir(wtRoot)
	if err != nil {
		if os.IsNotExist(err) {
			r.Status = StatusOK
			r.Message = "no .gc/worktrees directory"
			return r
		}
		r.Status = StatusError
		r.Message = fmt.Sprintf("reading .gc/worktrees: %v", err)
		return r
	}

	measure := c.measureDir
	if measure == nil {
		measure = duDirBytes
	}

	var sizes []rigSize
	var measureErrs []string
	for _, e := range rigEntries {
		if !e.IsDir() {
			continue
		}
		root := filepath.Join(wtRoot, e.Name())
		bytes, exists, err := measure(root)
		if err != nil {
			measureErrs = append(measureErrs, fmt.Sprintf("%s: %v", e.Name(), err))
			continue
		}
		if !exists {
			continue
		}
		sizes = append(sizes, rigSize{name: e.Name(), bytes: bytes})
	}

	if len(sizes) == 0 {
		r.Status = StatusOK
		if len(measureErrs) > 0 {
			r.Message = "no rig worktree directories measurable"
			r.Details = measureErrs
		} else {
			r.Message = "no rig worktree directories"
		}
		return r
	}

	sort.Slice(sizes, func(i, j int) bool { return sizes[i].bytes > sizes[j].bytes })

	warn := c.cfg.WorktreeRigWarnBytes()
	errBytes := c.cfg.WorktreeRigErrorBytes()

	var details []string
	var status CheckStatus = StatusOK
	for _, s := range sizes {
		switch {
		case s.bytes >= errBytes:
			details = append(details, fmt.Sprintf("rig %q: %s (exceeds %s error threshold)",
				s.name, humanSize(s.bytes), humanSize(errBytes)))
			if status < StatusError {
				status = StatusError
			}
		case s.bytes >= warn:
			details = append(details, fmt.Sprintf("rig %q: %s (exceeds %s warn threshold)",
				s.name, humanSize(s.bytes), humanSize(warn)))
			if status < StatusWarning {
				status = StatusWarning
			}
		}
	}
	for _, e := range measureErrs {
		details = append(details, "measure error: "+e)
	}

	r.Status = status
	switch status {
	case StatusError:
		r.Message = fmt.Sprintf("%d rig(s) over worktree size threshold (largest: %q at %s)",
			len(details), sizes[0].name, humanSize(sizes[0].bytes))
		r.Details = details
		r.FixHint = "investigate .gc/worktrees/<rig>/ for build-artifact accumulation; consider routing builds out of worktrees, periodic clean steps, or running `gc doctor --fix` to remove safely-prunable nested worktrees"
	case StatusWarning:
		r.Message = fmt.Sprintf("%d rig(s) approaching worktree size limit (largest: %q at %s)",
			len(details), sizes[0].name, humanSize(sizes[0].bytes))
		r.Details = details
		r.FixHint = "see fix hint for nested-worktree-prune; tune [doctor].worktree_rig_warn_size if 10 GB is too tight for this install"
	default:
		// All under thresholds: report the worst rig as info.
		r.Message = fmt.Sprintf("largest rig worktree: %q at %s (under %s warn)",
			sizes[0].name, humanSize(sizes[0].bytes), humanSize(warn))
	}
	return r
}

// CanFix returns false — pruning is the responsibility of
// NestedWorktreePruneCheck, which has the safety logic. This check is
// observation-only.
func (c *WorktreeDiskSizeCheck) CanFix() bool { return false }

// Fix is a no-op; see CanFix.
func (c *WorktreeDiskSizeCheck) Fix(_ *CheckContext) error { return nil }

// --- Nested-worktree prune check ---

// nestedWorktreeFinding describes one nested worktree under an agent
// home and whether it is mechanically safe to remove.
type nestedWorktreeFinding struct {
	path     string // absolute, canonical
	parent   string // agent home that contains it
	branch   string // branch name (best-effort; empty for detached)
	reason   string // why it was rejected (empty if safe)
	safeToRm bool
}

// gitWorktree is the slice of internal/git.Git used by NestedWorktreePruneCheck.
// Defined as an interface so tests can inject a fake without standing up real
// repositories.
type gitWorktree interface {
	WorktreeList() ([]git.Worktree, error)
	HasUncommittedWork() bool
	HasUnpushedCommits() bool
	HasStashes() bool
	WorktreeRemove(path string, force bool) error
}

// NestedWorktreePruneCheck identifies nested git worktrees inside agent
// home worktrees that are safely reclaimable: no uncommitted changes,
// no unpushed commits, no stashed work. These reproduce from the remote
// via `git worktree add path origin/<branch>`, so removing the local
// directory is non-destructive.
//
// The rule is mechanical, never role-coupled: any nested worktree whose
// branch tip is reachable from a remote and whose working tree is clean
// is reclaimable, regardless of which agent created it.
type NestedWorktreePruneCheck struct {
	cfg config.DoctorConfig
	// newGit produces a gitWorktree handle for a given path. Production
	// uses git.New; tests inject fakes.
	newGit func(path string) gitWorktree
	// findings is populated by Run for Fix to consume.
	findings []nestedWorktreeFinding
}

// NewNestedWorktreePruneCheck creates the prune check using real git.
func NewNestedWorktreePruneCheck(cfg config.DoctorConfig) *NestedWorktreePruneCheck {
	return &NestedWorktreePruneCheck{
		cfg:    cfg,
		newGit: func(p string) gitWorktree { return git.New(p) },
	}
}

// Name returns the check identifier.
func (c *NestedWorktreePruneCheck) Name() string { return "nested-worktree-prune" }

// Run walks .gc/worktrees/<rig>/<agent>/ for each agent home that is a
// git worktree, lists its sibling worktrees, and classifies each
// nested entry as safe-to-prune or rejected with a reason.
func (c *NestedWorktreePruneCheck) Run(ctx *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	c.findings = nil

	wtRoot := filepath.Join(ctx.CityPath, ".gc", "worktrees")
	rigEntries, err := os.ReadDir(wtRoot)
	if err != nil {
		if os.IsNotExist(err) {
			r.Status = StatusOK
			r.Message = "no .gc/worktrees directory"
			return r
		}
		r.Status = StatusError
		r.Message = fmt.Sprintf("reading .gc/worktrees: %v", err)
		return r
	}

	// Discover agent homes: <wtRoot>/<rig>/<agent>/ that hold a .git
	// pointer. Multiple rigs may share a single repo, so we deduplicate
	// nested findings by canonical path below.
	var homes []string
	for _, rigEntry := range rigEntries {
		if !rigEntry.IsDir() {
			continue
		}
		rigDir := filepath.Join(wtRoot, rigEntry.Name())
		agentEntries, err := os.ReadDir(rigDir)
		if err != nil {
			continue
		}
		for _, agentEntry := range agentEntries {
			if !agentEntry.IsDir() {
				continue
			}
			home := filepath.Join(rigDir, agentEntry.Name())
			if isGitWorktreePath(home) {
				homes = append(homes, pathutil.NormalizePathForCompare(home))
			}
		}
	}

	if len(homes) == 0 {
		r.Status = StatusOK
		r.Message = "no agent worktrees to inspect"
		return r
	}

	seen := make(map[string]bool)
	adminSeen := make(map[string]bool)
	for _, home := range homes {
		// Agent homes inside the same rig typically share one git
		// admin dir, so WorktreeList from each returns identical
		// output. Dedup at the admin-dir level to skip the redundant
		// subprocess. Falls through on parse failure (best-effort).
		if admin := readGitAdminDir(home); admin != "" {
			if adminSeen[admin] {
				continue
			}
			adminSeen[admin] = true
		}
		gw := c.newGit(home)
		entries, err := gw.WorktreeList()
		if err != nil {
			r.Details = append(r.Details, fmt.Sprintf("listing worktrees from %s: %v", home, err))
			continue
		}
		for _, wt := range entries {
			candidate := pathutil.NormalizePathForCompare(wt.Path)
			if !pathStrictlyInside(candidate, home) {
				continue
			}
			if seen[candidate] {
				continue
			}
			seen[candidate] = true
			c.findings = append(c.findings, classifyNested(c.newGit, candidate, home, wt.Branch))
		}
	}

	if len(c.findings) == 0 {
		r.Status = StatusOK
		r.Message = "no nested worktrees found"
		return r
	}

	var safe, unsafe []string
	for _, f := range c.findings {
		line := fmt.Sprintf("%s (branch %q)", f.path, f.branch)
		if f.safeToRm {
			safe = append(safe, line)
		} else {
			unsafe = append(unsafe, fmt.Sprintf("%s — %s", line, f.reason))
		}
	}

	if len(safe) == 0 {
		r.Status = StatusOK
		r.Message = fmt.Sprintf("%d nested worktree(s); none safely prunable",
			len(c.findings))
		r.Details = unsafe
		return r
	}

	if c.cfg.NestedWorktreePrune {
		r.Status = StatusError
	} else {
		r.Status = StatusWarning
	}
	r.Message = fmt.Sprintf("%d nested worktree(s) safely prunable (%d kept due to local work)",
		len(safe), len(unsafe))
	r.Details = append(append([]string(nil), safe...), unsafe...)
	r.FixHint = "run `gc doctor --fix` to remove safely-prunable nested worktrees (mechanical: only those with clean work tree, no unpushed commits, no stashes)"
	return r
}

// CanFix returns true — Fix removes the safely-prunable findings.
func (c *NestedWorktreePruneCheck) CanFix() bool { return true }

// Fix removes each safely-prunable nested worktree found by Run. Stops
// on first failure so the caller sees a clear error rather than partial
// state with a vague aggregate. Worktrees marked unsafe (uncommitted /
// unpushed / stashed) are never touched.
func (c *NestedWorktreePruneCheck) Fix(_ *CheckContext) error {
	for _, f := range c.findings {
		if !f.safeToRm {
			continue
		}
		gw := c.newGit(f.path)
		if err := gw.WorktreeRemove(f.path, true); err != nil {
			return fmt.Errorf("removing nested worktree %s: %w", f.path, err)
		}
	}
	return nil
}

// classifyNested runs the safety gates on a candidate nested worktree
// and returns a finding describing whether it is safe to remove and,
// if not, the first reason it was rejected. Order of checks matches
// the user's manual recovery procedure: status, log, stash.
func classifyNested(newGit func(string) gitWorktree, path, parent, branch string) nestedWorktreeFinding {
	f := nestedWorktreeFinding{path: path, parent: parent, branch: branch}
	gw := newGit(path)
	if gw.HasUncommittedWork() {
		f.reason = "has uncommitted changes"
		return f
	}
	if gw.HasUnpushedCommits() {
		f.reason = "has unpushed commits"
		return f
	}
	if gw.HasStashes() {
		f.reason = "has stashed work"
		return f
	}
	f.safeToRm = true
	return f
}

// isGitWorktreePath reports whether path holds a .git file or .git
// directory, indicating it is either the main repo or a worktree of one.
func isGitWorktreePath(path string) bool {
	gitPath := filepath.Join(path, ".git")
	_, err := os.Stat(gitPath)
	return err == nil
}

// readGitAdminDir returns the shared git admin directory that backs the
// worktree at home. For a worktree, .git is a file containing
// "gitdir: <repo>/.git/worktrees/<name>"; the admin root is the prefix
// before "/worktrees/". Returns "" if the file is missing, malformed,
// or not a worktree pointer (e.g. a main checkout where .git is a dir).
// Used to dedup WorktreeList calls across agent homes that share a repo.
func readGitAdminDir(home string) string {
	data, err := os.ReadFile(filepath.Join(home, ".git"))
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(data))
	const prefix = "gitdir: "
	if !strings.HasPrefix(line, prefix) {
		return ""
	}
	target := pathutil.NormalizePathForCompare(strings.TrimPrefix(line, prefix))
	const sep = string(filepath.Separator) + "worktrees" + string(filepath.Separator)
	if i := strings.Index(target, sep); i > 0 {
		return target[:i]
	}
	return target
}

// pathStrictlyInside reports whether child is a strict subpath of
// parent. Wraps the package-local isSubpath with an equal-paths check
// so a worktree home isn't mistakenly classified as nested under
// itself. Inputs must already be canonical (use
// pathutil.NormalizePathForCompare).
func pathStrictlyInside(child, parent string) bool {
	return child != parent && isSubpath(parent, child)
}

// humanSize returns a human-readable file size string.
func humanSize(bytes int64) string {
	const (
		kb = 1024
		mb = kb * 1024
		gb = mb * 1024
	)
	switch {
	case bytes >= gb:
		return fmt.Sprintf("%.1f GB", float64(bytes)/float64(gb))
	case bytes >= mb:
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(mb))
	case bytes >= kb:
		return fmt.Sprintf("%.1f KB", float64(bytes)/float64(kb))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

// --- Config semantics check ---

// ConfigSemanticsCheck surfaces warnings from config.ValidateSemantics
// as doctor check results. This catches provider reference errors, bad
// enum values, and cross-field constraint violations.
type ConfigSemanticsCheck struct {
	cfg    *config.City
	source string
}

// NewConfigSemanticsCheck creates a check that runs semantic validation.
func NewConfigSemanticsCheck(cfg *config.City, source string) *ConfigSemanticsCheck {
	return &ConfigSemanticsCheck{cfg: cfg, source: source}
}

// Name returns the check identifier.
func (c *ConfigSemanticsCheck) Name() string { return "config-semantics" }

// Run executes ValidateSemantics and reports any warnings.
func (c *ConfigSemanticsCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	warnings := config.ValidateSemantics(c.cfg, c.source)
	if len(warnings) == 0 {
		r.Status = StatusOK
		r.Message = "config semantics valid"
		return r
	}
	r.Status = StatusWarning
	r.Message = fmt.Sprintf("%d config semantic warning(s)", len(warnings))
	r.Details = warnings
	return r
}

// CanFix returns false — semantic issues require manual config correction.
func (c *ConfigSemanticsCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *ConfigSemanticsCheck) Fix(_ *CheckContext) error { return nil }
