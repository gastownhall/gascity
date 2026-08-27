package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// Work-record close gate (ADR-0009). Closing a work bead through the SDK close
// seam (`gc bd close`) is validated against the typed work-record contract: the
// bead must carry a typed gc.work_outcome, and a "shipped" outcome must point at
// a commit that has been DELIVERED — present on a remote-tracking ref — and
// reachable on the recorded gc.work_branch, with branch names resolved
// remote-first (refs/remotes/origin/<branch> when it exists, because local refs
// go stale in push-based topologies; gastownhall/gascity#5037). This turns the
// recurring "drain-without-commit" close (a close that leaves no artifact at
// all) into a machine-checkable violation.
//
// gc.work_branch is stamped at claim time (cmd_hook_claim.go) — before the work
// exists — so it is a PROVISIONAL handle, not a promise. A shipped commit that
// landed on a different branch than the stamp yields a non-blocking stale-stamp
// advisory naming the actual landing branch (Defect C); only undelivered work
// (no remote ref contains the commit) is a violation. Delivery is the guarantee;
// the branch is the pointer.
//
// The gate ships warn-only by default — violations are logged but the close
// proceeds — so existing open beads migrate without breakage. Set
// GC_WORK_RECORD_ENFORCE to a truthy value to make violations block the close.
//
// # What the gate does NOT see: a close the by-ID class door served
//
// doBd runs the by-ID class door (cmd_bd_by_id.go maybeRouteBdByID) BEFORE this
// gate, and a routed close returns from doBd without reaching it. So on a city
// that relocates a coordination class, `gc bd close <id>` and `gc bd update
// <id> --status closed` are gated only when they fall through to the bd
// subprocess. This is a coverage boundary, and it is recorded here rather than
// closed because closing it would mean re-deriving the gate against a store
// this file does not resolve.
//
// It is narrower than it sounds, in three steps:
//
//   - This gate reads the PREFIX store (the caller's resolved work scope). A
//     bead resident only in the class binding was never visible to it: the Get
//     missed and the loop skipped the id. Routing those closes through the door
//     removed nothing, because there was nothing to remove.
//   - The canonical worker spelling was already outside. graph-worker.md renders
//     `gc bd update <id> --set-metadata gc.outcome=pass --status closed`, and a
//     served update on a class-owned bead has been answered by the door since
//     that verb landed — well before close joined it.
//   - What the door's close DOES take from the gate is the DUAL-RESIDENT case,
//     and that population is real rather than hypothetical. `gc storage migrate`
//     copies every non-work bead with its id preserved and keeps the source
//     (readInfraSnapshot / infra_class_migrate.go), and coordclass.Classify
//     routes ANY bead carrying gc.root_bead_id to ClassGraph
//     (isWorkflowMetadata) — including a plain task-typed molecule work step
//     with no gc.kind, which is exactly isWorkRecordGatedBead's population. On a
//     migrated city those steps exist in both stores, and before the door served
//     close, this gate evaluated them against the work store's RETAINED copy —
//     the one frozen at migration time, not the one the close now writes. So the
//     gate's pre-door verdict on a dual resident was already a verdict about a
//     stale row.
//
// The drain path for that population is the sweep, not this gate. Both the CLI
// door and the HTTP by-id lane now resolve a dual resident to the CLASS copy —
// the door by its own residence probe, internal/api through the residency
// resolver's ByID plan, which leads with the binding for exactly this reason —
// so the two surfaces agree, and both write the row the controller reads. The
// retained work copy stays reachable through raw bd against the work scope, and
// it still has to be drained; that is the sweep's job, not this gate's.

// workRecordEnforceEnvVar gates whether work-record violations block the close
// (enforce) or are logged only (warn-only, the default).
const workRecordEnforceEnvVar = "GC_WORK_RECORD_ENFORCE"

// workRecordEnforceEnabled reports whether the close gate should block closes
// that violate the work-record contract, rather than only warning.
func workRecordEnforceEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(workRecordEnforceEnvVar))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// validWorkOutcome reports whether v is one of the four typed work-record close
// dispositions. The vocabulary is owned here (the consumer), not in beadmeta,
// per that package's data-only convention.
func validWorkOutcome(v string) bool {
	switch v {
	case beadmeta.WorkOutcomeShipped, beadmeta.WorkOutcomeNoOp,
		beadmeta.WorkOutcomeBlocked, beadmeta.WorkOutcomeAbandoned:
		return true
	default:
		return false
	}
}

// isWorkRecordGatedBead reports whether the work-record close contract applies
// to bead. It applies to worker-claimable work units — plain task beads — and
// deliberately NOT to control/structural beads (anything carrying gc.kind:
// workflow roots, scope/run/check/drain steps, etc.) or non-task beads (convoy,
// message). Those use the disjoint control-plane gc.outcome vocabulary and are
// closed by the dispatch engine, not by a worker reporting a work outcome.
func isWorkRecordGatedBead(bead beads.Bead) bool {
	if t := strings.TrimSpace(bead.Type); t != "" && t != "task" {
		return false
	}
	if strings.TrimSpace(bead.Metadata[beadmeta.KindMetadataKey]) != "" {
		return false
	}
	return true
}

// validateWorkRecordOnClose checks bead against the typed work-record contract.
// It returns a human-readable message for each violation (blocking under
// enforcement) and each advisory (never blocking). Empty violations ⇒ the bead
// satisfies the contract. commitReachable reports whether a commit SHA is an
// ancestor of a branch; commitOnRemote reports whether it is contained in any
// remote-tracking ref; remoteBranchesContaining reports which remote-tracking
// branches contain it. All three are injected so the rule is unit-testable
// without a real repo. The caller is responsible for scoping
// (isWorkRecordGatedBead).
//
// The oracles answer different questions and a shipped close needs them all.
// Reachability is a LOCAL property: a commit on a branch that was never pushed is
// still reachable from it, so reachability alone cannot distinguish delivered
// work from work that exists only in a worktree. az-6n75 is that gap — nine beads
// closed as delivered whose commits had no remote ref, one of them the only copy
// in existence.
//
// The containment resolver exists for the converse gap (ADR-0009 Defect C):
// gc.work_branch is stamped at claim time, before the work exists, with whatever
// branch the claiming worktree was on. When the work lands elsewhere the stamp is
// stale, and treating the resulting unreachability as a violation blocks honest
// delivered closes — under GC_WORK_RECORD_ENFORCE, citywide. Delivery is the
// guarantee this gate protects; the branch handle is the record's pointer to the
// work. So: a commit unreachable on its stamped branch but present on a
// remote-tracking ref yields a precise stale-stamp ADVISORY naming the branch the
// work actually landed on (so the record can be corrected), not a violation —
// while a commit on no remote ref remains a blocking violation regardless of the
// stamp.
func validateWorkRecordOnClose(bead beads.Bead, commitReachable func(commit, branch string) bool, commitOnRemote func(commit string) bool, remoteBranchesContaining func(commit string) []string) (violations, advisories []string) {
	outcome := strings.TrimSpace(bead.Metadata[beadmeta.WorkOutcomeMetadataKey])
	if outcome == "" {
		return []string{fmt.Sprintf("missing %s (want one of shipped|no-op|blocked|abandoned)", beadmeta.WorkOutcomeMetadataKey)}, nil
	}
	if !validWorkOutcome(outcome) {
		return []string{fmt.Sprintf("invalid %s=%q (want one of shipped|no-op|blocked|abandoned)", beadmeta.WorkOutcomeMetadataKey, outcome)}, nil
	}
	if outcome != beadmeta.WorkOutcomeShipped {
		// no-op / blocked / abandoned carry their reason in the close-reason; no
		// commit artifact is required.
		return nil, nil
	}
	commit := strings.TrimSpace(bead.Metadata[beadmeta.WorkCommitMetadataKey])
	branch := strings.TrimSpace(bead.Metadata[beadmeta.WorkBranchMetadataKey])
	if commit == "" {
		violations = append(violations, fmt.Sprintf("%s=shipped requires %s (the commit that satisfied the bead)", beadmeta.WorkOutcomeMetadataKey, beadmeta.WorkCommitMetadataKey))
	}
	if branch == "" && commit != "" {
		// The claim never stamped a branch (detached HEAD / non-repo claim dir).
		// When the containment evidence can name where the work landed, the
		// record is correctable — advise instead of blocking the delivered
		// close; the durability rule below still decides delivery.
		if containing := remoteBranchesContaining(commit); len(containing) > 0 {
			advisories = append(advisories, fmt.Sprintf(
				"%s is missing (claim stamped no branch): commit %s is contained in %s — correct the record with --set-metadata %s=%s",
				beadmeta.WorkBranchMetadataKey, commit, strings.Join(containing, ", "), beadmeta.WorkBranchMetadataKey, containing[0]))
		} else {
			violations = append(violations, fmt.Sprintf("%s=shipped requires %s (the branch the commit lives on)", beadmeta.WorkOutcomeMetadataKey, beadmeta.WorkBranchMetadataKey))
		}
	} else if branch == "" {
		violations = append(violations, fmt.Sprintf("%s=shipped requires %s (the branch the commit lives on)", beadmeta.WorkOutcomeMetadataKey, beadmeta.WorkBranchMetadataKey))
	}
	if commit != "" && branch != "" && !commitReachable(commit, branch) {
		if containing := remoteBranchesContaining(commit); len(containing) > 0 {
			// Delivered (or, in a repo with no publication remotes, present on a
			// local branch), but the claim-time stamp went stale (Defect C):
			// name where the work actually landed and how to correct the
			// record. The resolver returns suggestible branch names —
			// normalized, default-branch first, bounded.
			advisories = append(advisories, fmt.Sprintf(
				"%s %q is stale (stamped at claim time): commit %s is contained in %s — correct the record with --set-metadata %s=%s",
				beadmeta.WorkBranchMetadataKey, branch, commit, strings.Join(containing, ", "), beadmeta.WorkBranchMetadataKey, containing[0]))
		} else {
			violations = append(violations, fmt.Sprintf("%s %s is not reachable on %s %s", beadmeta.WorkCommitMetadataKey, commit, beadmeta.WorkBranchMetadataKey, branch))
		}
	}
	if commit != "" && !commitOnRemote(commit) {
		violations = append(violations, fmt.Sprintf("%s %s is not present on any remote — the work exists only locally and any worktree prune or branch GC destroys it (push before closing)", beadmeta.WorkCommitMetadataKey, commit))
	}
	return violations, advisories
}

// gitCommitReachableOnBranch reports whether commit is an ancestor of branch in
// the git repository at repoDir (worktrees share one object store, so any
// worktree dir resolves refs across the repo). A non-nil error from git — bad
// repo, unknown ref, unknown commit — reads as "not reachable". A commit/branch
// that looks like a flag (leading "-") is rejected outright so a malformed
// metadata value can never be parsed as a git option.
//
// The branch name resolves against BOTH the remote-tracking refs of every
// publication remote AND the local ref, and the commit is reachable when ANY of
// them contains it (gastownhall/gascity#5037):
//
//   - The remote refs matter because in any topology where merges reach the
//     target branch over the network, nothing ever advances the local ref —
//     the refinery merges in a detached worktree and pushes, and no one may
//     move a branch checked out in another worktree — so refs/heads/<branch>
//     goes permanently stale while refs/remotes/<r>/<branch> is the truth. A
//     bare name resolves to the stale local ref by gitrevisions precedence,
//     reporting genuinely-merged commits unreachable. Every publication remote
//     is consulted, not a hardcoded "origin" — a rig cloned with -o upstream is
//     equally entitled to the fix.
//   - The local ref matters because a commit sitting exactly on the stamped
//     branch's local head, just not yet pushed, has a factually-correct branch
//     record — its only defect is the missing push, which is the durability
//     rule's job to report, precisely and alone.
//
// Each existence probe is a separate rev-parse call, deliberately NOT a
// fallback on the merge-base exit code. A genuinely-unreachable commit is
// contained by none of the refs and stays unreachable — fail closed.
func gitCommitReachableOnBranch(repoDir, commit, branch string) bool {
	if strings.TrimSpace(repoDir) == "" || commit == "" || branch == "" {
		return false
	}
	if strings.HasPrefix(commit, "-") || strings.HasPrefix(branch, "-") {
		return false
	}
	refs := []string{}
	for _, remote := range gitPublicationRemotes(repoDir) {
		remoteRef := "refs/remotes/" + remote + "/" + branch
		if exec.Command("git", "-C", repoDir, "rev-parse", "--verify", "--quiet", remoteRef).Run() == nil {
			refs = append(refs, remoteRef)
		}
	}
	if exec.Command("git", "-C", repoDir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch).Run() == nil {
		refs = append(refs, "refs/heads/"+branch)
	}
	for _, ref := range refs {
		if exec.Command("git", "-C", repoDir, "merge-base", "--is-ancestor", commit, ref).Run() == nil {
			return true
		}
	}
	return false
}

// gitCommitOnRemote reports whether commit is contained in a remote-tracking
// ref of a PUBLICATION remote in the repository at repoDir — i.e. whether the
// work has been published and would survive the worktree being pruned.
//
// It lists commits reachable from commit but from no publication remote: empty
// output means the commit is already on one. Any git error reads as "not on a
// remote" so the gate fails closed — a false "at risk" costs one push, a false
// "durable" costs the work.
//
// Self-referential path remotes are excluded (gas-6tc; witness-side twin
// gas-6wq): a remote whose URL resolves back into this same repository — the
// gascity repo carries herdr-src = its own path — snapshots local branches into
// refs/remotes/*, so a blanket --remotes reads any previously-fetched local
// commit as "published" while it exists nowhere off this repo. That is the
// az-6n75 hole reopened by configuration.
//
// This deliberately consults remote-tracking refs rather than running ls-remote:
// the close path must not depend on network reachability. The tradeoff is that a
// commit pushed by another clone since the last fetch reads as not-durable.
func gitCommitOnRemote(repoDir, commit string) bool {
	if strings.TrimSpace(repoDir) == "" || commit == "" {
		return false
	}
	if strings.HasPrefix(commit, "-") {
		return false
	}
	// A repo with no publication remotes has nowhere to publish, so durability
	// is not applicable and the rule is skipped. Without this, every close in a
	// local-only repo would be flagged for a push that cannot exist. A repo
	// whose only remotes are self-referential is local-only in the same sense.
	//
	// Known tradeoff: a rig whose origin is removed or renamed silently stops
	// being protected by this rule. Detecting that is the config layer's job —
	// the close gate cannot tell "deliberately local" from "misconfigured".
	publication := gitPublicationRemotes(repoDir)
	if publication == nil {
		return false
	}
	if len(publication) == 0 {
		return true
	}
	args := []string{"-C", repoDir, "rev-list", "--max-count=1", commit, "--not"}
	for _, name := range publication {
		args = append(args, "--glob=refs/remotes/"+name+"/*")
	}
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		return false
	}
	return len(strings.TrimSpace(string(out))) == 0
}

// gitPublicationRemotes returns the names of repoDir's remotes that can confer
// durability: every configured remote except those whose URL resolves back into
// this same repository (gas-6tc). nil means the remote list could not be read
// (callers fail closed); an empty slice means there are remotes but none that
// publish, or none at all.
func gitPublicationRemotes(repoDir string) []string {
	out, err := exec.Command("git", "-C", repoDir, "remote").Output()
	if err != nil {
		return nil
	}
	selfCommon := gitCommonDir("", repoDir)
	publication := []string{}
	for _, name := range strings.Fields(string(out)) {
		if strings.HasPrefix(name, "-") {
			continue
		}
		urlOut, err := exec.Command("git", "-C", repoDir, "remote", "get-url", name).Output()
		if err != nil {
			// Unreadable URL: treat as a publication remote so the durability
			// rule stays armed rather than silently skipped.
			publication = append(publication, name)
			continue
		}
		url := strings.TrimSpace(string(urlOut))
		if isSelfRemoteURL(repoDir, url, selfCommon) {
			continue
		}
		publication = append(publication, name)
	}
	return publication
}

// isSelfRemoteURL reports whether a remote URL is a filesystem path that
// resolves to the repository whose git common dir is selfCommon. Network URLs
// (scheme://host/..., scp-style user@host:path) are never self-referential.
//
// repoDir anchors the resolution. Git resolves a relative remote URL against
// the repository, so resolving it against the gc process's own working
// directory instead would classify a self-referential remote as a publication
// remote and let its snapshot refs stand in as delivery evidence.
func isSelfRemoteURL(repoDir, url, selfCommon string) bool {
	if url == "" || selfCommon == "" {
		return false
	}
	if strings.Contains(url, "://") {
		if !strings.HasPrefix(url, "file://") {
			return false
		}
		url = strings.TrimPrefix(url, "file://")
	} else if strings.Contains(url, "@") || looksLikeSCPRemote(url) {
		return false
	}
	common := gitCommonDir(repoDir, url)
	return common != "" && common == selfCommon
}

// looksLikeSCPRemote reports whether url is scp-style (host:path) rather than a
// local path: a colon before the first slash.
func looksLikeSCPRemote(url string) bool {
	colon := strings.Index(url, ":")
	if colon < 0 {
		return false
	}
	slash := strings.Index(url, "/")
	return slash < 0 || colon < slash
}

// gitCommonDir resolves dir's git common directory (shared across worktrees) to
// an absolute, symlink-resolved path, or "" when dir is not a git repository.
// Two paths into the same repository — the root and any of its worktrees —
// resolve to the same common dir, which is what makes it the right identity for
// self-remote detection.
//
// baseDir, when non-empty, is the directory a relative dir resolves against.
// Git applies -C flags left to right and resolves each against the previous, so
// passing both reproduces git's own resolution of a relative remote URL —
// against the repository, not against this process's working directory.
func gitCommonDir(baseDir, dir string) string {
	if strings.TrimSpace(dir) == "" || strings.HasPrefix(dir, "-") {
		return ""
	}
	args := []string{}
	if strings.TrimSpace(baseDir) != "" && !strings.HasPrefix(baseDir, "-") {
		args = append(args, "-C", baseDir)
	}
	args = append(args, "-C", dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		return ""
	}
	p := strings.TrimSpace(string(out))
	if p == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
}

// gitRemoteBranchesContaining reports which branches contain commit in the
// repository at repoDir, as SUGGESTIBLE branch names: normalized (the
// "<remote>/" qualifier stripped), deduplicated, remote default branches
// first, bounded. It powers the stale claim-stamp advisory (Defect C): when
// the stamped branch does not contain a shipped commit, the branches that DO
// name where the work actually landed, and the first entry is the value the
// advisory tells the closer to stamp.
//
// Sourcing rules:
//   - Only publication remotes are consulted (gas-6tc): a self-referential
//     path remote's snapshot of local branches is not delivery evidence and
//     must never appear as a landing branch.
//   - Bare-remote entries are dropped: %(refname:short) renders the symbolic
//     refs/remotes/<r>/HEAD as the bare remote name, which is not a branch and
//     must never become the suggested correction.
//   - A remote's default branch (its HEAD symref target) sorts first when it
//     contains the commit — for an older commit contained in many branches the
//     alphabetically-first branch is an arbitrary and often transient choice,
//     while the default branch is the durable pointer.
//   - In a repo with no publication remotes — the local-only topology the
//     durability rule deliberately accommodates — local branches are the only
//     landing evidence there is, so refs/heads/* are consulted instead.
//
// Any git error reads as "no containing branches", which downgrades the caller
// to the blocking not-reachable violation — fail closed, never open.
//
// Known shared window with the durability oracle: a remote-tracking ref whose
// branch was deleted on the server still contains the commit locally until
// `fetch --prune`, so recently-unpublished work can read as delivered. The
// close path deliberately never touches the network (same tradeoff as
// gitCommitOnRemote, documented there); closing this window for real is the
// land-time SHA stamp + fetch-then-check design tracked for the universal bd
// gate (az-5vwo / beads#4960).
func gitRemoteBranchesContaining(repoDir, commit string) []string {
	if strings.TrimSpace(repoDir) == "" || commit == "" || strings.HasPrefix(commit, "-") {
		return nil
	}
	publication := gitPublicationRemotes(repoDir)
	if publication == nil {
		return nil
	}
	if len(publication) == 0 {
		return gitLocalBranchesContaining(repoDir, commit)
	}
	publishes := make(map[string]bool, len(publication))
	for _, name := range publication {
		publishes[name] = true
	}
	out, err := exec.Command("git", "-C", repoDir, "branch", "-r", "--contains", commit, "--format=%(refname:short)").Output()
	if err != nil {
		return nil
	}
	defaults := gitRemoteDefaultBranches(repoDir, publication)
	var preferred, rest []string
	seen := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasSuffix(line, "/HEAD") {
			continue
		}
		remote, branch, ok := strings.Cut(line, "/")
		if !ok || branch == "" || !publishes[remote] {
			continue
		}
		if seen[branch] {
			continue
		}
		seen[branch] = true
		if defaults[branch] {
			preferred = append(preferred, branch)
		} else {
			rest = append(rest, branch)
		}
	}
	return boundLandingBranches(append(preferred, rest...))
}

// gitLocalBranchesContaining lists local branches containing commit, in
// suggestible form, bounded. Used only when the repo has no publication
// remotes (see gitRemoteBranchesContaining).
func gitLocalBranchesContaining(repoDir, commit string) []string {
	out, err := exec.Command("git", "-C", repoDir, "branch", "--contains", commit, "--format=%(refname:short)").Output()
	if err != nil {
		return nil
	}
	var branches []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		branches = append(branches, line)
	}
	return boundLandingBranches(branches)
}

// gitRemoteDefaultBranches resolves each publication remote's HEAD symref to
// its default branch name. Missing symrefs (a remote never fetched, or set-head
// never run) are simply absent.
func gitRemoteDefaultBranches(repoDir string, publication []string) map[string]bool {
	defaults := map[string]bool{}
	for _, remote := range publication {
		out, err := exec.Command("git", "-C", repoDir, "symbolic-ref", "--quiet", "refs/remotes/"+remote+"/HEAD").Output()
		if err != nil {
			continue
		}
		ref := strings.TrimSpace(string(out))
		if branch, ok := strings.CutPrefix(ref, "refs/remotes/"+remote+"/"); ok && branch != "" {
			defaults[branch] = true
		}
	}
	return defaults
}

// maxLandingBranches bounds the advisory's branch list: enough to orient a
// human, small enough to stay one readable line. The suggested correction is
// always the first entry, so truncation costs context, never correctness.
const maxLandingBranches = 5

// boundLandingBranches caps branches at maxLandingBranches, appending a
// "+N more" marker when truncating.
func boundLandingBranches(branches []string) []string {
	if len(branches) <= maxLandingBranches {
		return branches
	}
	over := len(branches) - maxLandingBranches
	return append(branches[:maxLandingBranches], fmt.Sprintf("(+%d more)", over))
}

// workRecordCloseTargets returns the bead IDs a bd invocation closes, and
// whether the invocation is a close at all. It covers both forms the SDK seam
// sees: the `close` subcommand and `update --status=closed` (the form the
// worker formulas use to stamp metadata and close in one call). Ambiguous or
// ID-less invocations report not-a-close so the gate stays out of the way.
func workRecordCloseTargets(bdArgs []string) ([]string, bool) {
	if len(bdArgs) == 0 {
		return nil, false
	}
	switch bdArgs[0] {
	case "close":
	case "update":
		if !bdUpdateClosesStatus(bdArgs) {
			return nil, false
		}
	default:
		return nil, false
	}
	ids, ok, ambiguous := bdMutationWriteIDs(bdArgs)
	if !ok || ambiguous || len(ids) == 0 {
		return nil, false
	}
	return ids, true
}

// bdUpdateClosesStatus reports whether a `bd update` arg list sets the status to
// "closed" (in any of the --status=closed, --status closed, -s closed forms).
// bd registers status as a scalar flag, so the last occurrence wins. Values of
// other known flags are consumed before looking for status, and `--` terminates
// flag parsing, matching the mutation target scanner and pflag.
func bdUpdateClosesStatus(bdArgs []string) bool {
	valueFlags := bdSubcmdValueFlags("update")
	status := ""
	seen := false
	for i := 1; i < len(bdArgs); i++ {
		arg := bdArgs[i]
		if arg == "--" {
			break
		}
		if v, ok := strings.CutPrefix(arg, "--status="); ok {
			status, seen = v, true
			continue
		}
		if v, ok := strings.CutPrefix(arg, "-s="); ok {
			status, seen = v, true
			continue
		}
		if arg == "--status" || arg == "-s" {
			if i+1 >= len(bdArgs) {
				return false
			}
			i++
			status, seen = bdArgs[i], true
			continue
		}
		if !strings.Contains(arg, "=") && valueFlags[arg] && i+1 < len(bdArgs) {
			i++
		}
	}
	return seen && strings.EqualFold(strings.TrimSpace(status), "closed")
}

// runWorkRecordCloseGate validates every bead a `gc bd close` (or
// `gc bd update --status=closed`) invocation closes against the work-record
// contract. Best-effort: it never blocks on its own read failure. Returns
// whether the close should be blocked (only when enforcement is enabled).
//
// preOpened and preFetched let a caller that already opened the store and
// fetched the target beads (e.g. the write-ID collision guard, which reads
// the same beads for the same IDs immediately before this gate runs) hand
// them in instead of paying a second openStoreAtForCity + store.Get round
// trip. Both are optional (nil is fine): preOpened falls back to opening its
// own store, and any ID missing from preFetched falls back to store.Get.
func runWorkRecordCloseGate(bdArgs []string, scopeRoot, cityPath string, cfg *config.City, preOpened beads.Store, preFetched map[string]beads.Bead, stderr io.Writer) bool {
	if _, ok := workRecordCloseTargets(bdArgs); !ok {
		return false
	}
	store := preOpened
	if store == nil {
		var err error
		store, err = openStoreAtForCityWithConfig(scopeRoot, cityPath, cfg)
		if err != nil {
			// Cannot verify — never block a close on our own read failure.
			return false
		}
	}
	return evaluateWorkRecordCloseGate(bdArgs, store, preFetched, scopeRoot, workRecordEnforceEnabled(), stderr)
}

// evaluateWorkRecordCloseGate is the store-driven core of the close gate, split
// from the IO wrapper so it is unit-testable with an in-memory store. It logs
// each violation and reports whether the close should be blocked. preFetched
// (optional) supplies beads already read by an earlier guard in this same
// invocation, avoiding a duplicate store.Get for the same ID.
func evaluateWorkRecordCloseGate(bdArgs []string, store beads.Store, preFetched map[string]beads.Bead, scopeRoot string, enforce bool, stderr io.Writer) (block bool) {
	ids, ok := workRecordCloseTargets(bdArgs)
	if !ok {
		return false
	}
	mode := "warn-only"
	if enforce {
		mode = "enforced"
	}
	for _, id := range ids {
		bead, cached := preFetched[id]
		if !cached {
			var getErr error
			bead, getErr = store.Get(id)
			if getErr != nil {
				continue
			}
		}
		if !isWorkRecordGatedBead(bead) {
			continue
		}
		var projectionErr error
		bead, projectionErr = applyWorkRecordUpdateMetadata(bead, bdArgs)
		repoDir := strings.TrimSpace(bead.Metadata[beadmeta.WorkDirMetadataKey])
		if repoDir == "" {
			repoDir = scopeRoot
		}
		var violations, advisories []string
		if projectionErr != nil {
			violations = []string{projectionErr.Error()}
		} else {
			violations, advisories = validateWorkRecordOnClose(bead, func(commit, branch string) bool {
				return gitCommitReachableOnBranch(repoDir, commit, branch)
			}, func(commit string) bool {
				return gitCommitOnRemote(repoDir, commit)
			}, func(commit string) []string {
				return gitRemoteBranchesContaining(repoDir, commit)
			})
		}
		for _, v := range violations {
			fmt.Fprintf(stderr, "gc bd: work-record gate (%s): close of %s: %s\n", mode, id, v) //nolint:errcheck // best-effort stderr
		}
		for _, a := range advisories {
			fmt.Fprintf(stderr, "gc bd: work-record gate (advisory): close of %s: %s\n", id, a) //nolint:errcheck // best-effort stderr
		}
		if enforce && len(violations) > 0 {
			block = true
		}
	}
	return block
}

// workRecordMetadataEdits is the parsed metadata mutation of a `bd update` arg
// list: either a whole-object --metadata merge (hasMetadataJSON) or a set of
// --set-metadata / --unset-metadata edits. The two forms are mutually exclusive
// in bd; applyWorkRecordMetadataEdits enforces that.
type workRecordMetadataEdits struct {
	metadataJSON    string
	hasMetadataJSON bool
	setMetadata     []string
	unsetMetadata   []string
}

// applyWorkRecordUpdateMetadata overlays metadata mutations from an atomic
// `bd update ... --status=closed` invocation onto the stored bead before the
// close gate validates it. The documented worker close form stamps the typed
// work record and closes in one update, so validating only the pre-update bead
// would reject a valid enforced close and warn incorrectly in migration mode.
//
// The parse and apply phases are split so neither carries the whole projection's
// branch density; together they match bd's update flag semantics exactly.
func applyWorkRecordUpdateMetadata(bead beads.Bead, bdArgs []string) (beads.Bead, error) {
	if len(bdArgs) == 0 || bdArgs[0] != "update" {
		return bead, nil
	}
	metadata := make(beads.StringMap, len(bead.Metadata))
	for key, value := range bead.Metadata {
		metadata[key] = value
	}
	bead.Metadata = metadata
	edits, err := parseWorkRecordMetadataEdits(bdArgs)
	if err != nil {
		return bead, err
	}
	if err := applyWorkRecordMetadataEdits(bead.Metadata, edits); err != nil {
		return bead, err
	}
	return bead, nil
}

// parseWorkRecordMetadataEdits extracts the metadata mutations from a `bd update`
// arg list, matching bd's flag semantics: --metadata is a scalar whose last
// occurrence wins, and every known update flag's separate value is consumed so a
// value that itself looks like a metadata flag never mutates the prospective
// record. `--` terminates flag parsing.
func parseWorkRecordMetadataEdits(bdArgs []string) (workRecordMetadataEdits, error) {
	valueFlags := bdSubcmdValueFlags("update")
	var edits workRecordMetadataEdits
	for i := 1; i < len(bdArgs); i++ {
		arg := bdArgs[i]
		switch {
		case arg == "--":
			i = len(bdArgs)
		case arg == "--metadata":
			if i+1 >= len(bdArgs) {
				return edits, fmt.Errorf("cannot project --metadata: missing JSON value")
			}
			i++
			edits.metadataJSON = bdArgs[i]
			edits.hasMetadataJSON = true
		case strings.HasPrefix(arg, "--metadata="):
			edits.metadataJSON = strings.TrimPrefix(arg, "--metadata=")
			edits.hasMetadataJSON = true
		case arg == "--set-metadata":
			if i+1 >= len(bdArgs) {
				return edits, fmt.Errorf("cannot project --set-metadata: missing key=value")
			}
			i++
			edits.setMetadata = append(edits.setMetadata, bdArgs[i])
		case strings.HasPrefix(arg, "--set-metadata="):
			edits.setMetadata = append(edits.setMetadata, strings.TrimPrefix(arg, "--set-metadata="))
		case arg == "--unset-metadata":
			if i+1 >= len(bdArgs) {
				return edits, fmt.Errorf("cannot project --unset-metadata: missing key")
			}
			i++
			edits.unsetMetadata = append(edits.unsetMetadata, bdArgs[i])
		case strings.HasPrefix(arg, "--unset-metadata="):
			edits.unsetMetadata = append(edits.unsetMetadata, strings.TrimPrefix(arg, "--unset-metadata="))
		case !strings.Contains(arg, "=") && valueFlags[arg] && i+1 < len(bdArgs):
			i++
		}
	}
	return edits, nil
}

// applyWorkRecordMetadataEdits overlays parsed edits onto metadata, matching bd:
// --metadata cannot be combined with the edit flags, and bd applies every
// --set-metadata edit before every --unset-metadata edit regardless of their
// order in argv. A more permissive projection could validate prospective
// metadata that bd never persists and allow an invalid close.
func applyWorkRecordMetadataEdits(metadata beads.StringMap, edits workRecordMetadataEdits) error {
	if edits.hasMetadataJSON && (len(edits.setMetadata) > 0 || len(edits.unsetMetadata) > 0) {
		return fmt.Errorf("cannot project metadata: --metadata cannot be combined with --set-metadata or --unset-metadata")
	}
	if edits.hasMetadataJSON {
		if err := mergeWorkRecordMetadataJSON(metadata, edits.metadataJSON); err != nil {
			return fmt.Errorf("cannot project --metadata: %w", err)
		}
		return nil
	}
	for _, edit := range edits.setMetadata {
		key, value, ok := strings.Cut(edit, "=")
		if !ok || key == "" {
			return fmt.Errorf("cannot project --set-metadata %q: expected key=value", edit)
		}
		metadata[key] = value
	}
	for _, key := range edits.unsetMetadata {
		if key == "" {
			return fmt.Errorf("cannot project --unset-metadata: key is empty")
		}
		delete(metadata, key)
	}
	return nil
}

// mergeWorkRecordMetadataJSON applies bd update's --metadata object as an
// additive metadata merge. Decode through beads.StringMap so the prospective
// bead sees the same boolean/number coercion as a bead read back from bd.
// @file inputs deliberately fail closed: resolving a caller-relative file in
// this preflight would introduce a second filesystem interpretation of bd's
// input and could validate bytes different from the mutation bd performs.
func mergeWorkRecordMetadataJSON(metadata beads.StringMap, value string) error {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "@") {
		return fmt.Errorf("@file input is not supported by the close gate")
	}
	var update beads.StringMap
	if err := json.Unmarshal([]byte(value), &update); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	for key, item := range update {
		metadata[key] = item
	}
	return nil
}
