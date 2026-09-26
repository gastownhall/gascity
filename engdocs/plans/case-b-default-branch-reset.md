# Case B Default Branch Reset Plan

Root bead: `ga-3tt5iq`
Parent review: `ga-azwhj2`
Design source: `gascity/designer`, completed 2026-06-14
Route: `gascity/builder`

## Goal

Harden closed-bead agent home worktree cleanup so Case B resets to the
repository's probed default branch instead of assuming `origin/main`, while
preserving the existing `origin/main` fallback when the default branch cannot
be probed.

## Work Packages

1. `ga-3tt5iq.1` - Builder: Case B reset targets the probed default branch
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Acceptance: `agentWorktreeGitProbe` exposes `ProbeDefaultBranch()`;
     `cleanupClosedBeadAgentHomeWorktrees` Case B builds `resetRef` from
     `wg.ProbeDefaultBranch()` and falls back to `origin/main` only when the
     probe returns an empty string; `CheckoutDetach` and Case B reset logs use
     the computed ref; Case A, session-home detection, bead ID extraction, and
     the cleanup function signature are unchanged.

2. `ga-3tt5iq.2` - Builder: cleanup tests cover non-main Case B default
   branches
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-3tt5iq.1`
   - Acceptance: `fakeAgentWorktreeGit` has a `probeDefaultBranch` field and
     `ProbeDefaultBranch()` method; tests cover `master`, `develop`, and empty
     probe values with expected refs `origin/master`, `origin/develop`, and
     `origin/main`; each case asserts `cleaned == 1` and the captured checkout
     ref; existing zero-value tests continue through the `origin/main`
     fallback.

3. `ga-3tt5iq.3` - Builder: default-branch reset fix passes focused
   verification
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-3tt5iq.1`, `ga-3tt5iq.2`
   - Acceptance: targeted cleanup tests pass; `go test ./...` and
     `go vet ./...` pass, or unrelated/pre-existing failures are documented
     with exact packages and output summaries; final handoff records that
     non-main default-branch behavior and the empty-probe fallback were
     verified.

## Dependency Graph

- `ga-3tt5iq.2` depends on `ga-3tt5iq.1`.
- `ga-3tt5iq.3` depends on `ga-3tt5iq.1` and `ga-3tt5iq.2`.

## Guardrails

- Do not change `internal/git/git.go`; `git.Git.ProbeDefaultBranch()` already
  exists.
- Do not change `city_runtime.go` or the cleanup function signature.
- Do not route this work back to design; the design handoff is complete.
- Do not introduce hardcoded agent role names in production code or tests.
- Keep the reset behavior idempotent and non-fatal on checkout failure.

## Out Of Scope

- Changes to `beadIDFromBranch`, `extractBeadIDFromWorktreeName`, Case A, or
  session-home detection.
- New cleanup policies, new reaper scheduling behavior, or new event types.
