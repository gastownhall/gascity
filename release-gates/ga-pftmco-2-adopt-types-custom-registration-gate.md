# Release Gate: ga-pftmco.2 adopt types.custom registration

Status: PASS
Date: 2026-06-07

## Scope

- Deploy bead: ga-pftmco.2
- Source review bead: ga-bcip1f
- Clean branch: origin/work/ga-pftmco-adopt-fix
- Clean branch commit: cc17578e37f67a7137f23adb4de7bbbaae3ae614
- Reviewed source commit: 255fae84175e8e287b53c3959b7667e8954f9aa3
- Base checked: origin/main 9ac732cd80335f8157861419f8f24202903d78b1
- Manifest note: docs/PROJECT_MANIFEST.md was not present in this checkout, so this gate uses the deployer role release criteria.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-bcip1f` contains `Review verdict: PASS` for reviewed commit `255fae84175e8e287b53c3959b7667e8954f9aa3`. The review lists only two INFO findings and no blocking findings. |
| 2 | Acceptance criteria met | PASS | `bd show ga-pftmco.1` records clean branch `work/ga-pftmco-adopt-fix`, cherry-picked commit `cc17578e37f67a7137f23adb4de7bbbaae3ae614`, base `origin/main` `9ac732cd8`, and verification that the forbidden files are absent. Deployer diff check against current `origin/main` shows exactly `cmd/gc/cmd_rig.go`, `cmd/gc/cmd_rig_test.go`, and `docs/reference/cli.md`; no `internal/api/handler_status_test.go` or `release-gates/fix-status-degrade-wall-bounds-gate.md` entries are present. The code routes managed-Dolt `gc rig add --adopt` through `initDirIfReady`, preserving adopt validation while syncing DB config such as `types.custom`. |
| 3 | Tests pass | PASS | `go test ./cmd/gc/... -run 'TestDoRigAdd_Adopt'` passed. `make test-fast-parallel` passed all 8 fast jobs. `go vet ./...` exited clean. `go build -o /tmp/gc-ga-pftmco-2 ./cmd/gc` passed, and `/tmp/gc-ga-pftmco-2 rig add --help` shows the managed-Dolt idempotent config sync help text. |
| 4 | No high-severity review findings open | PASS | Reviewer notes on ga-bcip1f list two INFO findings: deferred-path output asymmetry and a hard-to-test non-destructive reinit guarantee. No HIGH, blocking, security, or correctness findings are recorded. |
| 5 | Final branch is clean | PASS | The isolated release worktree was clean at candidate checkout (`git status --short --branch` printed only `## HEAD (no branch)`). After committing this gate file, deployer rechecks the worktree before push; the committed gate file is the only deployer-added change. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-base HEAD origin/main` equals `9ac732cd80335f8157861419f8f24202903d78b1`, the current `origin/main`; the release branch is a direct descendant of main. |
| 7 | Single feature theme | PASS | `git diff --name-status origin/main...HEAD` touches one subsystem and its docs: `cmd/gc/cmd_rig.go`, `cmd/gc/cmd_rig_test.go`, and `docs/reference/cli.md`. The diff is one feature theme: managed-Dolt adopt registration of `types.custom` and matching CLI documentation. |

## Diff Evidence

```text
M	cmd/gc/cmd_rig.go
M	cmd/gc/cmd_rig_test.go
M	docs/reference/cli.md
```

```text
3 files changed, 114 insertions(+), 6 deletions(-)
```
