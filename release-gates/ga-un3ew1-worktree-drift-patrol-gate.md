# Release Gate: ga-un3ew1 worktree drift patrol

Bead: ga-un3ew1
Title: needs-deploy: independent health-patrol sweep for commit-class worktree drift
Branch: builder/ga-un3ew1-worktree-drift-patrol-v1
Candidate tip: 7158850c8801eaa0219d2dba0ead925946170f83
Base checked: origin/main at 78128ec184148cbb02709db76e63dfc48de378ad
Gate result: PASS
Evaluated: 2026-07-17

Note: `docs/PROJECT_MANIFEST.md` and `PROJECT_MANIFEST.md` are not present in
this branch, so no additional manifest-specific release criteria were available
to evaluate. This gate uses the deployer release criteria.

## Criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 6 | Branch diverges cleanly from main | PASS | Evaluated first. `git rev-list --left-right --count origin/main...HEAD` returned `1 3`; `git merge-tree --write-tree origin/main HEAD` exited 0 and produced tree `4bc54470b3e4950326a06dc480fff6ebee0ce90f`, so the candidate has no merge conflicts with current `origin/main`. The bounded self-rebase helper is absent in this checkout, but it was not needed because the freshness/conflict gate passed. |
| 1 | Review PASS present | PASS | Review bead `ga-wyret1` is closed with close reason `pass`; notes contain `REVIEW VERDICT: PASS`, `Security (OWASP pass): No findings.`, and `No blocking issues. Handing off to deployer.` The deploy bead `ga-un3ew1` also records `Review verdict: PASS`. |
| 2 | Acceptance criteria met | PASS | The diff implements one read-only health-patrol sweep for commit-class worktree drift: `config.Agent.IsCommitClass()` classifies from the resolved `--freshen-commit` config token; `internal/git.Git.AheadBehindRef*` and `InProgressOperation()` provide local-only drift checks and TOCTOU skip behavior; `cmd/gc/worktree_drift_patrol.go` records persistent `worktree_drift` beads and fires `events.WorktreeDriftStalled`; `config.DaemonConfig.WorktreeDriftThreshold` gates activation by config; generated config/OpenAPI/client outputs are updated. |
| 3 | Tests pass | PASS | `HOME=/home/jaword go build ./...`; `HOME=/home/jaword go vet ./...`; focused checks for API spec/client union coverage, event payload registration, worktree drift patrol, config, git, and resource census packages; `HOME=/home/jaword make dashboard-check`; `HOME=/home/jaword make test-fast-parallel` with all 8 fast jobs passed. |
| 4 | No high-severity review findings open | PASS | Review notes for `ga-wyret1` report no blocking issues and no security findings. No unresolved HIGH finding is recorded in the review or deploy bead notes. |
| 5 | Final branch is clean | PASS | Before writing this gate file, `git status --short --branch` returned only `## HEAD (no branch)`. `git config core.hooksPath` returned `.githooks`. `gofmt -l` on touched Go files returned no files. Final clean status is rechecked after committing this gate before push. |
| 7 | Single feature theme | PASS | The commit set is one subsystem theme: a commit-class worktree drift health-patrol check plus the config, event, API/schema, tests, and resource-census/generated documentation updates required to ship that same feature. No independent user-facing feature is bundled. |

## Candidate Commits

| Commit | Purpose |
|---|---|
| 636f45271 | Add commit-class worktree drift patrol, config, event payload, generated schema/client, and tests. |
| 6a9660163 | Sync resource-census ledger for the new test subprocess usage. |
| 7158850c8 | Correct post-rebase resource-census ledger baseline after upstream drift. |

## Test Evidence

Commands run from `/var/tmp/gc-deployer-ga-un3ew1-release-gate-20260717T1119-RMkltv`:

- `HOME=/home/jaword go build ./...` — PASS
- `HOME=/home/jaword go vet ./...` — PASS
- `HOME=/home/jaword go test ./internal/api -run 'TestOpenAPISpecInSync|TestGeneratedClientInSync|TestTypedEventEnvelopeUnionsCoverKnownEventTypes'` — PASS
- `HOME=/home/jaword go test ./internal/events -run TestEveryKnownEventTypeHasRegisteredPayload` — PASS
- `HOME=/home/jaword go test ./cmd/gc -run TestPatrolCommitClassWorktreeDrift` — PASS
- `HOME=/home/jaword go test ./internal/config ./internal/git ./internal/testpolicy/resourcecensus` — PASS
- `HOME=/home/jaword make dashboard-check` — PASS
- `HOME=/home/jaword make test-fast-parallel` — PASS (`fsys-darwin-compile`, `unit-core`, and `unit-cmd-gc-1-of-6` through `unit-cmd-gc-6-of-6`)
