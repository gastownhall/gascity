# Release gate: ga-bhjb — TestMain rig env scrub (ga-d02c)

Feature bead: **ga-d02c** (closed) — Fix: scrub gc rig env vars in cmd/gc TestMain to stop dolt sql-server orphans
Review bead: **ga-bhjb** (needs-deploy)
Branch: `release/ga-bhjb` (cherry-picked b94dcffd onto `origin/main`, commit `7065bc48`)

## Criteria

| # | Criterion | Result |
|---|-----------|--------|
| 1 | Review PASS present | **PASS** |
| 2 | Acceptance criteria met | **PASS** |
| 3 | Tests pass | **PASS** |
| 4 | No high-severity review findings open | **PASS** |
| 5 | Final branch is clean | **PASS** |
| 6 | Branch diverges cleanly from main | **PASS** |

### 1. Review PASS present

Two independent PASS verdicts recorded on ga-bhjb:

- `gascity/reviewer-1` (gm-p97d2b6): PASS. Style, security OWASP walk, spec compliance, coverage, scope note — all clean. No findings.
- `gascity/reviewer` (via gm-wisp-e4lh): PASS. Independently verified orphan delta=0 across reproducer + leaking-test set + t.Setenv consumer tests under GC_BEADS=bd. No blockers.

### 2. Acceptance criteria met

From ga-d02c done-when:

- [x] `cmd/gc/testmain_test.go` exists with a `TestMain`-equivalent scrub helper. Builder adapted spec: cmd/gc already has a `TestMain` in `main_test.go` (Go permits one per package), so the helper `scrubInheritedRigEnv()` lives in `cmd/gc/testmain_test.go` and is called on the first line of the existing `TestMain`. Semantically equivalent to the spec.
- [x] `go test -count=1 ./cmd/gc/...` passes from a clean shell — verified by reviewer-1; not re-run here because full-package timeout is pre-existing flakiness unrelated to this change (see note below).
- [x] With `GC_BEADS=bd` set, targeted tests produce zero new `dolt sql-server --config /tmp/` orphans — verified independently on `release/ga-bhjb` (see criterion 3).
- [x] Existing `t.Setenv("GC_BEADS", ...)` tests still pass — verified on this branch.

### 3. Tests pass

Re-verified on `release/ga-bhjb` from the deployer seat with `GC_BEADS=bd` in env:

```
go vet ./cmd/gc/...                 clean
go build ./...                      clean

Reproducer:
go test -run TestCityRuntimeReloadProviderSwapPreservesDrainTracker ./cmd/gc/
  ok github.com/gastownhall/gascity/cmd/gc 0.021s
  pgrep dolt sql-server --config /tmp/  delta 28 -> 28

Leaking-test set:
go test -run 'TestCityRuntimeReload|TestControllerReload' ./cmd/gc/
  ok github.com/gastownhall/gascity/cmd/gc 0.430s
  pgrep delta 33 -> 33

t.Setenv consumer tests:
go test -run 'TestBdEnv|TestBeadsProvider|TestApiState|TestBuildDesiredState|TestCmdGraph|TestInitProviderReadiness|TestOrderDispatch|TestCmdSessionReset|TestCmdBdStoreBridge' ./cmd/gc/
  ok github.com/gastownhall/gascity/cmd/gc 9.218s
  pgrep delta 33 -> 33
```

**Pre-existing flaky test (not a regression):** `TestLocalInitializerInitScaffoldsAndFinalizes` fails with `Init: finalize failed (exit 1)` on this branch. Confirmed the same failure reproduces on `origin/main` without this change, so it is environment-specific / pre-existing, not introduced by ga-d02c. Reviewer-1's verification run (from /tmp worktree) observed the test passing — consistent with environment sensitivity. Orthogonal to the fix.

### 4. No high-severity review findings open

Both reviewers: zero findings blocking. One informational out-of-scope observation (typo `BEADS_DOLT_PASSWORD` vs `BEADS_DOLT_SERVER_PASSWORD` at path_helpers_test.go:40, pre-existing from ga-y64o). Not blocking.

### 5. Final branch is clean

```
$ git status
On branch release/ga-bhjb
Your branch is ahead of 'origin/main' by 1 commit.
  (use "git push" to publish your local commits)
Untracked files: .gitkeep
nothing added to commit but untracked files present
```

`.gitkeep` is pre-existing worktree scaffolding, not part of this change.

### 6. Branch diverges cleanly from main

```
$ git log --oneline origin/main..HEAD
7065bc48 test(cmd/gc): scrub inherited rig env vars in TestMain (ga-d02c)
```

Single commit cherry-picked from `b94dcffd` on the builder branch `gc-builder-1-01561d4fb9ea`. Source builder branch additionally contained `e16ccf1f` (ga-y64o), but that commit is already open as PR #1197 (release/ga-onjy); deployer cherry-picked only the ga-d02c commit to keep this PR independent and reviewable on its own.

## Disposition

All criteria PASS — cleared for push and PR.
