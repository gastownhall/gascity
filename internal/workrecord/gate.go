// Package workrecord holds the ADR-0009 work-record close contract: the rule
// that closing a work bead must leave a typed, machine-checkable record of what
// the work produced.
//
// The bead must carry a typed gc.work_outcome, and a "shipped" outcome must
// point at a commit that is reachable on the stamped gc.work_branch. This turns
// the recurring "drain-without-commit" close (a close that leaves no artifact at
// all) into a machine-checkable violation.
//
// The contract ships warn-only by default — violations are logged but the close
// proceeds — so existing open beads migrate without breakage. Set
// GC_WORK_RECORD_ENFORCE to a truthy value to make violations block the close.
//
// # Why this is a package and not a CLI detail
//
// A bead closes through more than one door: `gc bd close` and
// `gc bd update --status closed` on the CLI plane, POST /v0/bead/{id}/close and
// a closed-status POST /v0/bead/{id}/update on the HTTP plane. Every door has to
// ask the same question of the same population, or the ungated door becomes the
// way closes get done. This package owns the question — which beads the contract
// covers, what a valid record looks like, whether enforcement is on — and each
// plane owns only its own plumbing: argv projection and stderr in cmd/gc, the
// resolved owner store and the 409 in internal/api.
package workrecord

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// EnforceEnvVar gates whether work-record violations block the close (enforce)
// or are logged only (warn-only, the default).
const EnforceEnvVar = "GC_WORK_RECORD_ENFORCE"

// EnforceEnabled reports whether the close gate should block closes that violate
// the work-record contract, rather than only warning.
func EnforceEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnforceEnvVar))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// ValidOutcome reports whether v is one of the four typed work-record close
// dispositions. The vocabulary is owned here (the consumer), not in beadmeta,
// per that package's data-only convention.
func ValidOutcome(v string) bool {
	switch v {
	case beadmeta.WorkOutcomeShipped, beadmeta.WorkOutcomeNoOp,
		beadmeta.WorkOutcomeBlocked, beadmeta.WorkOutcomeAbandoned:
		return true
	default:
		return false
	}
}

// Gated reports whether the work-record close contract applies to bead. It
// applies to worker-claimable work units — plain task beads — and deliberately
// NOT to control/structural beads (anything carrying gc.kind: workflow roots,
// scope/run/check/drain steps, etc.) or non-task beads (convoy, message). Those
// use the disjoint control-plane gc.outcome vocabulary and are closed by the
// dispatch engine, not by a worker reporting a work outcome.
func Gated(bead beads.Bead) bool {
	if t := strings.TrimSpace(bead.Type); t != "" && t != "task" {
		return false
	}
	if strings.TrimSpace(bead.Metadata[beadmeta.KindMetadataKey]) != "" {
		return false
	}
	return true
}

// ValidateOnClose checks bead against the typed work-record contract and returns
// a human-readable message for each violation (empty slice ⇒ the bead satisfies
// the contract). commitReachable reports whether a commit SHA is an ancestor of
// a branch; it is injected so the rule is unit-testable without a real repo. The
// caller is responsible for scoping (Gated).
func ValidateOnClose(bead beads.Bead, commitReachable func(commit, branch string) bool) []string {
	outcome := strings.TrimSpace(bead.Metadata[beadmeta.WorkOutcomeMetadataKey])
	if outcome == "" {
		return []string{fmt.Sprintf("missing %s (want one of shipped|no-op|blocked|abandoned)", beadmeta.WorkOutcomeMetadataKey)}
	}
	if !ValidOutcome(outcome) {
		return []string{fmt.Sprintf("invalid %s=%q (want one of shipped|no-op|blocked|abandoned)", beadmeta.WorkOutcomeMetadataKey, outcome)}
	}
	if outcome != beadmeta.WorkOutcomeShipped {
		// no-op / blocked / abandoned carry their reason in the close-reason; no
		// commit artifact is required.
		return nil
	}
	commit := strings.TrimSpace(bead.Metadata[beadmeta.WorkCommitMetadataKey])
	branch := strings.TrimSpace(bead.Metadata[beadmeta.WorkBranchMetadataKey])
	var violations []string
	if commit == "" {
		violations = append(violations, fmt.Sprintf("%s=shipped requires %s (the commit that satisfied the bead)", beadmeta.WorkOutcomeMetadataKey, beadmeta.WorkCommitMetadataKey))
	}
	if branch == "" {
		violations = append(violations, fmt.Sprintf("%s=shipped requires %s (the branch the commit lives on)", beadmeta.WorkOutcomeMetadataKey, beadmeta.WorkBranchMetadataKey))
	}
	if commit != "" && branch != "" && !commitReachable(commit, branch) {
		violations = append(violations, fmt.Sprintf("%s %s is not reachable on %s %s", beadmeta.WorkCommitMetadataKey, commit, beadmeta.WorkBranchMetadataKey, branch))
	}
	return violations
}

// PreferredReachabilityRef decides which ref CommitReachableOnBranch should
// check commit-reachability against: refs/remotes/origin/<branch> when it
// resolves, otherwise the bare branch name. gitrevisions precedence puts a
// local refs/heads/<branch> ahead of any remote-tracking ref, but the
// worktree named by gc.work_dir is rarely the one whose local branch tip
// actually moves — a refinery or polecat that merges/pushes from a different
// worktree advances the remote-tracking ref, never the local one checked out
// elsewhere. Resolving against the local ref alone reports a landed commit as
// unreachable until something happens to fast-forward it, which in that
// topology may be never (gastownhall/gascity#5037).
//
// remoteRefResolves is injected so the decision is unit-testable without a
// real git repository; the only production caller runs
// `git rev-parse --verify --quiet <ref>`. Kept as a separate probe rather
// than a fallback on the merge-base exit code so the caller can distinguish
// "no such ref" from "not reachable"; see CommitReachableOnEitherRef.
func PreferredReachabilityRef(branch string, remoteRefResolves func(ref string) bool) string {
	if remote := "refs/remotes/origin/" + branch; remoteRefResolves(remote) {
		return remote
	}
	return branch
}

// CommitReachableOnEitherRef reports whether a commit is reachable from the
// branch's remote-tracking ref OR from the branch itself. The remote-tracking
// ref is probed first (see PreferredReachabilityRef) because it is the ref
// that actually advances in a refinery/polecat topology; the bare branch name
// is then still checked, because ADR-0009's contract is that the commit is
// reachable on gc.work_branch — not that it has been pushed. Checking only the
// remote ref would reject a commit that is genuinely on the local branch but
// sits ahead of (or was never pushed to) origin, a false negative in the exact
// mirror image of gastownhall/gascity#5037.
//
// Both probes are injected so the decision is unit-testable without a real git
// repository. The local ref is never probed twice.
func CommitReachableOnEitherRef(branch string, remoteRefResolves, reachableOnRef func(ref string) bool) bool {
	ref := PreferredReachabilityRef(branch, remoteRefResolves)
	if reachableOnRef(ref) {
		return true
	}
	if ref == branch {
		return false
	}
	return reachableOnRef(branch)
}

// CommitReachableOnBranch reports whether commit is an ancestor of branch in the
// git repository at repoDir (worktrees share one object store, so any worktree
// dir resolves refs across the repo). A non-nil error from git — bad repo,
// unknown ref, unknown commit — reads as "not reachable". A commit/branch that
// looks like a flag (leading "-") is rejected outright so a malformed metadata
// value can never be parsed as a git option. See CommitReachableOnEitherRef for
// how branch is resolved to the refs that can prove reachability.
func CommitReachableOnBranch(repoDir, commit, branch string) bool {
	if strings.TrimSpace(repoDir) == "" || commit == "" || branch == "" {
		return false
	}
	if strings.HasPrefix(commit, "-") || strings.HasPrefix(branch, "-") {
		return false
	}
	return CommitReachableOnEitherRef(branch,
		func(candidate string) bool {
			return exec.Command("git", "-C", repoDir, "rev-parse", "--verify", "--quiet", candidate).Run() == nil
		},
		func(ref string) bool {
			return exec.Command("git", "-C", repoDir, "merge-base", "--is-ancestor", commit, ref).Run() == nil
		})
}
