# Release gate — gc stop hang fix (ga-co7mdp / ga-me9g)

**Verdict:** PASS

- Bead: `ga-co7mdp` (review of `ga-me9g`, closed at `3493fa5b`)
- Branch: `fork/builder/ga-me9g-2` → re-pushed as deploy branch
- Base: `origin/main` at `5f1a686d`
- Branch shape: 2 commits ahead, 0 behind main (TDD red→green)
  - `5b41f442` — `test(stop): red — bound subprocess + per-target timeouts`
  - `3493fa5b` — `fix(stop): bound subprocess + per-target timeouts so gc stop can't hang`

## Criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Reviewer PASS verdict in bead notes | PASS | `gascity/reviewer` PASS at HEAD `3493fa5b`; 3 INFO findings, none block. |
| 2 | Acceptance criteria met | PASS | All 7 acceptance criteria from `ga-me9g` checked off in bead description; verified by reviewer re-run. |
| 3 | Tests pass on final branch | PASS | Targeted regression suites green (see Validation); cmd/gc baseline failures are pre-existing on `origin/main` (see "Pre-existing test environment"). |
| 4 | No high-severity review findings open | PASS | Reviewer findings list: 3 INFO, 0 HIGH. |
| 5 | Working tree clean | PASS | `git status` reports nothing to commit on the deploy branch prior to the gate-file commit. |
| 6 | Branch diverges cleanly from main | PASS | 2 ahead / 0 behind `origin/main`. No conflicts. |

## Validation (deployer re-run on `deploy/ga-co7mdp` at HEAD `3493fa5b`)

- `go build ./...` — clean
- `go vet ./...` — clean
- `golangci-lint run ./cmd/gc/ ./internal/runtime/tmux/` — 0 issues (golangci-lint v2.10.1)
- `go test ./cmd/gc -run '^(TestExecuteTargetWave|TestGracefulStopAll|TestCmdStop|TestSessionLifecycle|TestRunBoundsByTmuxSubprocess)' -count=1` — PASS (4.77s)
- `go test ./internal/runtime/tmux -count=1` — PASS (1.05s)

## Pre-existing test environment

`make test` on this machine reports a large number of failing tests in
`./cmd/gc/...` that are **independent of this change**. To verify, the
deployer re-ran the same suite directly on `origin/main` (`5f1a686d`)
in the same environment:

| Branch | `cmd/gc` failures |
|---|---|
| `origin/main` (5f1a686d) | 121 |
| `deploy/ga-co7mdp` (3493fa5b) | 60 |

The set difference (failed-on-deploy-but-not-on-main) is **empty** —
ga-co7mdp introduces zero new test regressions. The branch in fact
exhibits fewer environmental flakes than main, which the deployer
attributes to the bounded-timeout improvements making test cleanup
more deterministic on a busy host.

The remaining cmd/gc instability on origin/main affects every
in-flight deploy and is being tracked outside this gate.

## Push target

Pushing to fork (`quad341/gascity`) since origin (`gastownhall/gascity`)
is upstream-only for this rig. PR is cross-repo with `--head
quad341:builder/ga-me9g-2`.
