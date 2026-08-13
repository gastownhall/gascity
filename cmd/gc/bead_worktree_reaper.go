package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/pathutil"
	"github.com/gastownhall/gascity/internal/sling"
)

// reapDecision records one worktree the reaper acted on or declined to act on,
// for the dry-run report and event stream.
type reapDecision struct {
	BeadID string
	Path   string
	Rig    string
	Branch string
	// Reason explains a protected decision (why the worktree was left in
	// place). Empty for a reap/would-reap decision.
	Reason string
}

// reapReport is the outcome of one reapClosedBeadWorktrees pass. Reaped holds
// the worktrees removed (or, in dry-run, the ones that would be removed);
// Protected holds worktrees left in place with the reason (too young/quarantined,
// referenced by a non-terminal bead in another molecule, live process, active
// session, unsafe git state, or an indeterminate age/liveness/borrow-veto scan).
type reapReport struct {
	Reaped    []reapDecision
	Protected []reapDecision
	DryRun    bool
}

// reapClosedBeadWorktrees discovers per-bead git worktrees under
// cityPath/.gc/worktrees/<rig>/ and removes any whose associated bead is closed
// and that pass every safety gate. It returns a reapReport describing what was
// reaped and what was protected.
//
// Discovery is authoritative at any nesting depth: for each rig it runs
// `git worktree list --porcelain` from the rig's own repository (the repo that
// owns these worktrees, per worktree-setup.sh's `git -C <rig> worktree add`),
// rather than a single-level directory scan. Per-bead worktrees are nested
// under agent-home directories (depth-2, sometimes deeper); the old
// os.ReadDir(.gc/worktrees/<rig>/) scan saw only the agent homes and reaped
// nothing (gastownhall/gascity#4492 root cause A).
//
// Safety gates, in order, all fail closed toward keeping the worktree:
//  1. Named agent-home directories are never removed.
//  2. The bead named by the worktree must exist and be closed.
//  3. Freshness quarantine: a worktree younger than
//     cfg.Daemon.AutoReapClosedBeadWorktreesMinAge is exempt, protecting
//     against the race between worktree creation and its owning bead's
//     work-dir metadata being stamped by the next reconcile pass. An
//     indeterminate age (the ".git" pointer file cannot be stat'd) protects.
//  4. Borrow-veto scan: batched once per rig per tick, this finds any
//     non-terminal bead — in any molecule — whose gc.work_dir/work_dir
//     metadata still points at the worktree's path and protects it if so.
//     A query error protects every remaining candidate in that rig's tick.
//  5. Liveness: no live process cwd and no active-session working directory may
//     sit at or beneath the worktree. If the liveness scan is indeterminate
//     (no /proc), NOTHING is reaped this pass — the reaper cannot prove any
//     tree is idle (root cause B: closed-bead != end-of-use).
//  6. Git state: no uncommitted changes, no stashes, and no commits that
//     removing the worktree would orphan — commits reachable from no branch,
//     tag, or remote-tracking ref (git.HasUnreachableCommitsResult). The test
//     is deliberately reachability, not push state: `git worktree remove`
//     deletes the checkout, not refs/heads. Gating on push state instead made
//     the reaper a no-op for exactly the worktrees it exists to collect,
//     because a merge queue that deletes the merged branch from origin leaves
//     every merged bead's HEAD permanently unreached by any remote ref
//     (gastownhall/gascity ga-uh1m). A failed probe protects the tree.
//
// When dryRun is true the reaper performs all discovery and classification and
// emits bead.worktree.reap_skipped events describing what it would reap and
// what it protected, but removes nothing. liveSessionDirs is the active-session
// working-directory set the liveness gate cross-checks against, alongside the
// authoritative /proc cwd scan.
//
// skips carries skip-reporting history across sweeps so an unchanged decision
// is announced once rather than on every tick; a nil tracker reports every
// skip. It never changes what is reaped or what the returned report contains —
// see reapSkipTracker.
func reapClosedBeadWorktrees(
	cityPath string,
	cfg *config.City,
	rigBeadStores map[string]beads.Store,
	liveSessionDirs []string,
	dryRun bool,
	rec events.Recorder,
	skips *reapSkipTracker,
	stderr io.Writer,
) reapReport {
	report := reapReport{DryRun: dryRun}
	if stderr == nil {
		stderr = io.Discard
	}
	if rec == nil {
		rec = events.Discard
	}
	if cfg == nil || len(rigBeadStores) == 0 {
		return report
	}

	skips.beginPass()
	defer skips.endPass()

	// Build a guard set of session home names so agent template directories
	// are never touched.
	sessionHomes := make(map[string]bool, len(cfg.Agents))
	for i := range cfg.Agents {
		if name := cfg.Agents[i].BindingQualifiedName(); name != "" {
			sessionHomes[name] = true
		}
	}

	// Authoritative liveness signal, gathered once for the whole pass. When the
	// scan is indeterminate the reaper protects every candidate (fail closed).
	live := collectLiveWorktreeStateFn()
	if live.scanned && live.source != "" && live.source != liveScanSourceProc {
		// Name the mechanism when it is not the primary one, so a reap decision
		// made on a fallback scan is not indistinguishable from one made on
		// /proc.
		fmt.Fprintf(stderr, "reapClosedBeadWorktrees: liveness scanned via %s (/proc unavailable)\n", live.source) //nolint:errcheck
	}

	wtRoot := filepath.Join(cityPath, ".gc", "worktrees")

	for rigName, store := range rigBeadStores {
		if store == nil {
			continue
		}
		rigRoot := rigRootByName(cfg, rigName)
		if rigRoot == "" {
			// No configured filesystem path for this rig — cannot resolve the
			// owning repository, so we cannot safely enumerate or remove.
			continue
		}
		rigWorktreeDir := filepath.Join(wtRoot, rigName)

		worktrees, err := git.New(rigRoot).WorktreeList()
		if err != nil {
			fmt.Fprintf(stderr, "reapClosedBeadWorktrees: listing worktrees for rig %s (%s): %v\n", rigName, rigRoot, err) //nolint:errcheck
			continue
		}

		// Pass 1: discover reap-eligible candidates — closed bead, and old
		// enough to be past the freshness quarantine (FR-5). Every other gate
		// (borrow-veto, liveness, git safety) is deferred to pass 2 so the
		// borrow-veto scan below can run as a single batched query per rig
		// (FR-3) instead of once per worktree.
		var candidates []reapCandidate
		for _, wt := range worktrees {
			worktreePath := wt.Path

			// Only per-bead worktrees under this rig's .gc/worktrees/<rig>/
			// subtree are in scope. This excludes the rig's main working tree
			// and any worktree checked out elsewhere.
			if !pathutil.PathWithin(rigWorktreeDir, worktreePath) || pathutil.SamePath(rigWorktreeDir, worktreePath) {
				continue
			}
			// Defense in depth: never act on a path that is not strictly under
			// the city worktree root.
			if !isStrictlyUnderDir(wtRoot, worktreePath) {
				continue
			}

			base := filepath.Base(worktreePath)

			// Session home guard: never touch agent template directories.
			if sessionHomes[base] {
				continue
			}

			// Resolve which bead this worktree belongs to from its path.
			beadID := extractBeadIDFromWorktreePath(cfg, rigWorktreeDir, worktreePath)
			if beadID == "" {
				continue
			}

			// Confirm the bead exists and is closed in this rig's store.
			bead, err := store.Get(beadID)
			if err != nil || bead.Status != "closed" {
				// ErrNotFound, transient error, or bead not yet closed — skip.
				continue
			}

			// Freshness quarantine (FR-5): a worktree younger than the
			// configured minimum age is exempt from reaping, protecting
			// against the race between worktree creation and its owning
			// bead's work-dir metadata being stamped by the next reconcile
			// pass. Age is fail-closed — an indeterminate age protects.
			minAge := cfg.Daemon.AutoReapClosedBeadWorktreesMinAge()
			age, ok := computeWorktreeAge(worktreePath)
			reason := ""
			switch {
			case !ok:
				reason = "worktree age indeterminate (failing closed)"
			case minAge > 0 && age < minAge:
				reason = fmt.Sprintf("worktree too young to reap (quarantine): min_age=%s", minAge)
			}
			if reason != "" {
				branch, _ := git.New(worktreePath).CurrentBranch()
				if skips.shouldSurface(worktreePath, reason) {
					fmt.Fprintf(stderr, //nolint:errcheck
						"reapClosedBeadWorktrees: protecting %s (bead %s closed but %s)\n",
						worktreePath, beadID, reason,
					)
					recordReapSkipped(rec, beadID, worktreePath, rigName, reason)
				}
				report.Protected = append(report.Protected, reapDecision{
					BeadID: beadID, Path: worktreePath, Rig: rigName, Branch: branch, Reason: reason,
				})
				continue
			}

			candidates = append(candidates, reapCandidate{beadID: beadID, worktreePath: worktreePath})
		}

		if len(candidates) == 0 {
			continue
		}

		// Borrow-veto scan (FR-1/FR-2/FR-3): one batched query for every
		// surviving candidate in this rig instead of one query per candidate.
		// A query error fails closed — every remaining candidate in this
		// rig's tick is protected (NFR-1).
		referencingBeads, listErr := scanBorrowVetoReferences(store, candidates)
		if listErr != nil {
			reason := fmt.Sprintf("borrow-veto scan failed (failing closed): %v", listErr)
			for _, c := range candidates {
				branch, _ := git.New(c.worktreePath).CurrentBranch()
				if skips.shouldSurface(c.worktreePath, reason) {
					fmt.Fprintf(stderr, //nolint:errcheck
						"reapClosedBeadWorktrees: protecting %s (bead %s closed but %s)\n",
						c.worktreePath, c.beadID, reason,
					)
					recordReapSkipped(rec, c.beadID, c.worktreePath, rigName, reason)
				}
				report.Protected = append(report.Protected, reapDecision{
					BeadID: c.beadID, Path: c.worktreePath, Rig: rigName, Branch: branch, Reason: reason,
				})
			}
			continue
		}

		// Pass 2: apply the borrow-veto verdict, then the existing
		// liveness/git-safety gates, to each surviving candidate.
		for _, c := range candidates {
			worktreePath := c.worktreePath
			beadID := c.beadID

			// Borrow-veto (FR-1/FR-2/FR-7): protect when any non-terminal
			// bead — regardless of molecule — still references this path via
			// work-dir metadata.
			reason := ""
			if refs := referencingBeads[worktreePath]; len(refs) > 0 {
				reason = fmt.Sprintf("borrow-veto: referenced by non-terminal bead(s) %s", strings.Join(refs, ", "))
			}

			// Liveness gate (fail closed). Protect the tree when a live process
			// or active session is working in it, or when liveness could not be
			// determined at all.
			if reason == "" {
				switch {
				case !live.scanned:
					reason = "liveness scan unavailable (failing closed, protecting all)"
				default:
					if isLive, why := worktreeIsLive(worktreePath, live, liveSessionDirs); isLive {
						reason = "live: " + why
					}
				}
			}

			// Git safety gates, only if not already protected. A probe error
			// protects the tree: an errored probe proves nothing, and treating
			// it as a clean answer would fail open.
			if reason == "" {
				wg := git.New(worktreePath)
				hasUncommitted := wg.HasUncommittedWork()
				hasUnreachable, unreachableErr := wg.HasUnreachableCommitsResult()
				hasStashes, stashErr := wg.HasStashesResult()
				switch {
				case unreachableErr != nil:
					reason = fmt.Sprintf("git probe failed (failing closed): %v", unreachableErr)
				case stashErr != nil:
					reason = fmt.Sprintf("git probe failed (failing closed): %v", stashErr)
				case hasUncommitted || hasUnreachable || hasStashes:
					reason = fmt.Sprintf("unsafe git state: uncommitted=%v unreachable=%v stashes=%v", hasUncommitted, hasUnreachable, hasStashes)
				}
			}

			branch, _ := git.New(worktreePath).CurrentBranch()

			if reason != "" {
				if skips.shouldSurface(worktreePath, reason) {
					fmt.Fprintf(stderr, //nolint:errcheck
						"reapClosedBeadWorktrees: protecting %s (bead %s closed but %s)\n",
						worktreePath, beadID, reason,
					)
					recordReapSkipped(rec, beadID, worktreePath, rigName, reason)
				}
				report.Protected = append(report.Protected, reapDecision{
					BeadID: beadID, Path: worktreePath, Rig: rigName, Branch: branch, Reason: reason,
				})
				continue
			}

			if dryRun {
				const whatIf = "dry-run: would reap (closed bead, clean tree, no live process)"
				if skips.shouldSurface(worktreePath, whatIf) {
					fmt.Fprintf(stderr, //nolint:errcheck
						"reapClosedBeadWorktrees: %s: %s for closed bead %s\n",
						whatIf, worktreePath, beadID,
					)
					recordReapSkipped(rec, beadID, worktreePath, rigName, whatIf)
				}
				report.Reaped = append(report.Reaped, reapDecision{
					BeadID: beadID, Path: worktreePath, Rig: rigName, Branch: branch,
				})
				continue
			}

			// Remove the worktree from the OWNING rig repository. git worktree
			// remove must be run from the main repo root, not from within the
			// worktree being removed.
			if err := git.New(rigRoot).WorktreeRemove(worktreePath, false); err != nil {
				fmt.Fprintf(stderr, "reapClosedBeadWorktrees: removing %s: %v\n", worktreePath, err) //nolint:errcheck
				continue
			}
			fmt.Fprintf(stderr, //nolint:errcheck
				"reapClosedBeadWorktrees: removed worktree %s for closed bead %s\n",
				worktreePath, beadID,
			)
			if raw, err := json.Marshal(events.BeadWorktreeReapedPayload{
				BeadID: beadID,
				Path:   worktreePath,
				Rig:    rigName,
				Branch: branch,
			}); err == nil {
				rec.Record(events.Event{
					Type:    events.BeadWorktreeReaped,
					Actor:   "gc",
					Subject: beadID,
					Payload: raw,
				})
			}
			report.Reaped = append(report.Reaped, reapDecision{
				BeadID: beadID, Path: worktreePath, Rig: rigName, Branch: branch,
			})
		}
	}
	return report
}

// reapCandidate is a worktree that survived the closed-bead check and the
// freshness quarantine in pass 1, awaiting the batched borrow-veto scan and
// the remaining safety gates in pass 2.
type reapCandidate struct {
	beadID       string
	worktreePath string
}

// reapSkipTracker makes the reaper's skip reporting edge-triggered, so a
// worktree is announced when its situation changes rather than on every sweep.
//
// The reaper re-evaluates every worktree on each controller tick (~12s), and a
// tree it cannot reap — unpushed commits, a live process, an indeterminate age
// — stays protected for hours or days. Re-announcing that unchanged decision
// every tick made this one event 95% of all city telemetry (~500 events/min,
// ~260 MB/day of events.jsonl) while carrying no information a reader did not
// already have. The steady state is exactly what is not worth saying.
//
// A worktree is therefore surfaced when it is first skipped and whenever its
// reason changes; while the reason holds, both the event and the log line stay
// silent. Paths absent from a pass are forgotten, so a tree that stops being a
// candidate and later returns announces itself again. Suppression governs
// reporting only — reapReport still lists every worktree acted on, so callers
// reading the report (the dry-run summary, the tick's phase counters) see the
// full picture on every pass.
//
// The tracker is owned by the controller runtime and touched only from the
// serial reconciler tick, so it carries no lock, matching the other per-tick
// state on CityRuntime. A nil *reapSkipTracker surfaces every skip, preserving
// the unsuppressed behavior for one-shot callers that have no pass history to
// compare against.
type reapSkipTracker struct {
	lastReason map[string]string   // worktree path -> reason last surfaced
	thisPass   map[string]struct{} // paths evaluated in the pass under way
}

// newReapSkipTracker returns a tracker with no recorded history, so the first
// pass through it surfaces every skip.
func newReapSkipTracker() *reapSkipTracker {
	return &reapSkipTracker{
		lastReason: make(map[string]string),
		thisPass:   make(map[string]struct{}),
	}
}

// beginPass starts a sweep, clearing the set of paths seen so endPass can
// forget the ones that dropped out.
func (t *reapSkipTracker) beginPass() {
	if t == nil {
		return
	}
	t.thisPass = make(map[string]struct{}, len(t.lastReason))
}

// shouldSurface records that worktreePath is being skipped for reason during
// the pass under way, and reports whether that is news: true when the path was
// not skipped as of the previous pass, or was skipped for a different reason.
// False means the decision is an unchanged repeat, and the caller emits
// neither the event nor the log line.
func (t *reapSkipTracker) shouldSurface(worktreePath, reason string) bool {
	if t == nil {
		return true
	}
	t.thisPass[worktreePath] = struct{}{}
	if prev, tracked := t.lastReason[worktreePath]; tracked && prev == reason {
		return false
	}
	t.lastReason[worktreePath] = reason
	return true
}

// endPass forgets every path the pass did not evaluate, bounding the tracker to
// the worktrees currently in the sweep and letting a path that returns later
// surface again.
func (t *reapSkipTracker) endPass() {
	if t == nil {
		return
	}
	for path := range t.lastReason {
		if _, seen := t.thisPass[path]; !seen {
			delete(t.lastReason, path)
		}
	}
}

// computeWorktreeAge returns how long ago worktreePath was created, using the
// mtime of its ".git" pointer file (written once by `git worktree add` and not
// rewritten during normal use) as a creation-time proxy. Worktree structs carry
// no timestamp of their own. ok is false when the file cannot be stat'd, so the
// caller can fail closed instead of treating an indeterminate age as zero.
func computeWorktreeAge(worktreePath string) (age time.Duration, ok bool) {
	info, err := os.Stat(filepath.Join(worktreePath, ".git"))
	if err != nil {
		return 0, false
	}
	return time.Since(info.ModTime()), true
}

// scanBorrowVetoReferences issues one batched beads.Store.List query and
// returns, for each candidate's worktree path, the IDs of any non-terminal
// beads — in any molecule — whose gc.work_dir or legacy work_dir metadata
// still points at that path (FR-1/FR-2/FR-3). Terminal status is decided by
// convoycore.IsTerminalStatus, not a bare "!= closed" check, so a tombstoned
// reference does not veto. Path matching is symlink/alias-normalized on both
// sides via pathutil.NormalizePathForCompare, matching the liveness gate, so a
// metadata path recorded in a different-but-equivalent form still vetoes.
// A query error is returned as-is; the caller must fail closed and protect
// every candidate in the rig (NFR-1).
func scanBorrowVetoReferences(store beads.Store, candidates []reapCandidate) (map[string][]string, error) {
	// The query excludes closed beads at the store level (IsTerminalStatus
	// would discard them anyway) and skips label hydration this scan never
	// reads. TierBoth is explicit so the reaper's safety contract does not
	// depend on a wrapping store expanding the default tier for it.
	all, err := store.List(beads.ListQuery{AllowScan: true, SkipLabels: true, TierMode: beads.TierBoth})
	if err != nil {
		return nil, err
	}
	byNorm := make(map[string]string, len(candidates)) // normalized -> raw candidate path
	for _, c := range candidates {
		byNorm[pathutil.NormalizePathForCompare(c.worktreePath)] = c.worktreePath
	}
	refs := make(map[string][]string)
	for _, b := range all {
		if convoycore.IsTerminalStatus(b.Status) {
			continue
		}
		for _, key := range [...]string{beadmeta.WorkDirMetadataKey, beadmeta.LegacyWorkDirMetadataKey} {
			p := strings.TrimSpace(b.Metadata[key])
			if p == "" {
				continue
			}
			if raw, hit := byNorm[pathutil.NormalizePathForCompare(p)]; hit {
				refs[raw] = append(refs[raw], b.ID)
				break
			}
		}
	}
	return refs, nil
}

// recordReapSkipped emits a bead.worktree.reap_skipped event carrying the
// reason a worktree was protected or (in dry-run) flagged as would-reap.
func recordReapSkipped(rec events.Recorder, beadID, path, rig, reason string) {
	raw, err := json.Marshal(events.BeadWorktreeReapSkippedPayload{
		BeadID: beadID,
		Path:   path,
		Rig:    rig,
		Reason: reason,
	})
	if err != nil {
		return
	}
	rec.Record(events.Event{
		Type:    events.BeadWorktreeReapSkipped,
		Actor:   "gc",
		Subject: beadID,
		Payload: raw,
	})
}

// rigRootByName returns the configured filesystem path of the rig with the
// given name, or "" when the rig is unknown or has no path. This is the
// repository that owns the rig's per-bead worktrees.
func rigRootByName(cfg *config.City, rigName string) string {
	if cfg == nil {
		return ""
	}
	for i := range cfg.Rigs {
		if cfg.Rigs[i].Name == rigName {
			return strings.TrimSpace(cfg.Rigs[i].Path)
		}
	}
	return ""
}

// extractBeadIDFromWorktreePath resolves which bead a worktree belongs to from
// its path: from the leaf directory name, or — only when the leaf carries no
// bead ID — from its parent directory name.
//
// The parent fallback covers worktrees laid out as
// "<bead-id>-<slug>/worktree", where the leaf is a fixed literal and only the
// parent names the bead. Reading the leaf alone resolved those to no bead at
// all, so they were skipped before any safety gate ran and could never be
// reaped or even reported as protected.
//
// The climb is exactly one level and stops at boundary (the rig's worktree
// root), so it can never mistake an ancestor directory outside the rig's
// worktree subtree for the owning bead.
func extractBeadIDFromWorktreePath(cfg *config.City, boundary, worktreePath string) string {
	if beadID := extractBeadIDFromWorktreeName(cfg, filepath.Base(worktreePath)); beadID != "" {
		return beadID
	}
	parent := filepath.Dir(worktreePath)
	if !isStrictlyUnderDir(boundary, parent) {
		return ""
	}
	return extractBeadIDFromWorktreeName(cfg, filepath.Base(parent))
}

// extractBeadIDFromWorktreeName scans consecutive dash-separated segment pairs
// in name for one that LooksLikeConfiguredBeadID. Returns the first match, or
// "" if none. Handles names like "builder-ga-34q3ss-pr2738" → "ga-34q3ss" and
// bare "ga-06kfi6" → "ga-06kfi6".
func extractBeadIDFromWorktreeName(cfg *config.City, name string) string {
	if name == "" || cfg == nil {
		return ""
	}
	parts := strings.Split(name, "-")
	for i := 0; i+1 < len(parts); i++ {
		candidate := parts[i] + "-" + parts[i+1]
		if sling.LooksLikeConfiguredBeadID(cfg, candidate) {
			return candidate
		}
	}
	return ""
}

// isStrictlyUnderDir reports whether path is strictly contained within dir
// (i.e., it is not dir itself and has dir as a prefix component).
func isStrictlyUnderDir(dir, path string) bool {
	// Normalize both sides. git worktree list reports canonical paths, while
	// dir is derived from the configured city path, which may still contain a
	// symlinked ancestor (on macOS every $TMPDIR path does, via /var ->
	// private/var). Comparing the two raw forms makes filepath.Rel return a
	// "../.." escape for a worktree that is plainly inside the city, so this
	// defense-in-depth check silently drops every reap candidate. The
	// PathWithin gate directly above already compares normalized.
	dir = pathutil.NormalizePathForCompare(dir)
	path = pathutil.NormalizePathForCompare(path)
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != "." && !strings.HasPrefix(rel, "..")
}
