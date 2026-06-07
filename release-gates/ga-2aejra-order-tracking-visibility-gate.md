# Release Gate: Order-Tracking Retention Visibility

Date: 2026-06-07
Deploy bead: ga-2aejra
Source implementation bead: ga-0op21n.1
Source review bead: ga-ny8cxu
Dependency deploy bead: ga-f0efcs
Branch: release/ga-2aejra-order-tracking-visibility
Base checked: origin/main fc4fd18021e0d6e257fece81693d60eb35694b9b
Final code commits:

| Commit | Source | Purpose |
|---|---|---|
| f64a9b940 | ga-tjp87g.1 / ga-f0efcs | Adds the controller retention watchdog and bounded closed order-tracking sweep. |
| f58fdcf26 | PR #3198 follow-up | Corrects the bounded sweep budget-enforcement comment. |
| c355da6c6 | ga-0op21n.1 / ga-ny8cxu | Adds startup backlog visibility and the `gc doctor` advisory check. |

`docs/PROJECT_MANIFEST.md` is not present in this worktree, so this gate uses the deployer prompt criteria and the deploy bead acceptance checklist.

## Scope

The source builder branch `builder/ga-0op21n-order-tracking-visibility` was behind current `origin/main`. The release branch was rebuilt from current `origin/main` and cherry-picked the already-gated watchdog work plus the reviewed visibility commit, yielding one clean order-tracking retention release unit.

The release diff contains one feature theme:

| Path | Purpose |
|---|---|
| `cmd/gc/city_runtime.go` | Runs the retention watchdog during controller dispatch and prints a best-effort startup warning when closed order-tracking backlog is large. |
| `cmd/gc/order_dispatch.go` | Adds bounded closed order-tracking retention sweep helpers. |
| `cmd/gc/cmd_doctor.go` | Registers the order-tracking retention advisory check. |
| `cmd/gc/doctor_order_tracking_retention.go` | Implements the advisory `gc doctor` check. |
| `internal/config/config.go` | Documents the controller-managed closed tracking bead retention default. |
| `docs/reference/config.md`, `docs/schema/city-schema.*` | Regenerated config reference/schema for the retention default description. |
| `cmd/gc/*_test.go`, `cmd/gc/testdata/doctor_check_names.golden` | Covers watchdog, startup warning, doctor behavior, and registration/golden updates. |

## Gate Checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | `bd show ga-ny8cxu` is closed with `VERDICT: PASS` for `builder/ga-0op21n-order-tracking-visibility` at `3ccdcd3d4`; dependency `bd show ga-f0efcs` is closed with gate PASS and PR #3198. |
| 2 | Acceptance criteria met | PASS | See acceptance evidence below. The release branch does not use `builder/ga-uzijoh-mac-ci-timeout-fix`; both dependency beads are complete; the branch is derived from current `origin/main` and contains only order-tracking retention watchdog/visibility work. |
| 3 | Tests pass | PASS | `make test-fast-parallel` passed on rerun with `TMPDIR=/home/jaword/tmp/gascity-deploy-ga-2aejra-testtmp LOCAL_TEST_JOBS=12 CMD_GC_PROCESS_TOTAL=6`; `go vet ./...` passed; `make check-schema` passed. An earlier fast run failed during compilation because `/tmp` ran out of space, with no assertion failure. |
| 4 | No high-severity review findings open | PASS | Review bead `ga-ny8cxu` reports `BLOCKERS: none`; only one non-blocking advisory about documenting the threshold difference. Dependency bead `ga-f0efcs` records gate PASS. Unresolved HIGH findings count is 0. |
| 5 | Final branch is clean | PASS | `git status --short --branch` showed `release/ga-2aejra-order-tracking-visibility...origin/main [ahead 3]` with no uncommitted changes before writing this gate. |
| 6 | Branch diverges cleanly from main | PASS | Branch was created with `git worktree add -B release/ga-2aejra-order-tracking-visibility ... origin/main`; `git merge-base --is-ancestor origin/main HEAD` exited 0 before writing this gate. |
| 7 | Single feature theme | PASS | All code and docs changes are one order-tracking retention feature: controller pruning, startup backlog visibility, and an advisory doctor check over the same closed tracking bead backlog. The watchdog and visibility pieces are coupled because the warning/check surfaces the backlog the controller retention path manages. |

## Acceptance Evidence

| Deploy bead criterion | Result | Evidence |
|---|---|---|
| Do not open or update a PR from `builder/ga-uzijoh-mac-ci-timeout-fix`. | PASS | The release branch is `release/ga-2aejra-order-tracking-visibility`, rebuilt from `origin/main`; the stale CI-timeout branch was not used. |
| Wait for `ga-ny8cxu` to close with reviewer PASS for the clean visibility branch. | PASS | `bd show ga-ny8cxu` is closed with `VERDICT: PASS` for `builder/ga-0op21n-order-tracking-visibility` at `3ccdcd3d4`. |
| Wait for `ga-f0efcs` to complete the clean watchdog deploy path, or record why visibility can safely proceed as a stacked unit. | PASS | `bd show ga-f0efcs` is closed with PR #3198. This branch includes the same watchdog feature plus the visibility commit as one stacked order-tracking retention release unit, so it remains self-contained if PR #3198 has not merged yet. |
| Run the standard deploy gate against a branch containing only order-tracking retention work. | PASS | This gate ran on `release/ga-2aejra-order-tracking-visibility`, whose commits are the watchdog, watchdog comment correction, and visibility/doctor check. |
| On PASS, open a PR scoped to order-tracking retention visibility and route a merge-request to mayor/mpr. | PENDING | This gate artifact is being committed before PR creation; PR URL and merge-request evidence will be recorded in the bead notes after push/PR creation. |
| On FAIL, record the gate artifact and route back to PM without opening a PR. | N/A | Gate result is PASS. |

## Test Evidence

- PASS: `TMPDIR=/home/jaword/tmp/gascity-deploy-ga-2aejra-testtmp LOCAL_TEST_JOBS=12 CMD_GC_PROCESS_TOTAL=6 make test-fast-parallel`
- PASS: `TMPDIR=/home/jaword/tmp/gascity-deploy-ga-2aejra-testtmp go vet ./...`
- PASS: `TMPDIR=/home/jaword/tmp/gascity-deploy-ga-2aejra-testtmp make check-schema`
- Environmental retry note: an initial `make test-fast-parallel` using `/tmp` failed with `no space left on device` during Go compile/link output. The passing rerun moved temp files to `/home` and lowered parallelism; no code assertion failure was observed.

Gate result: PASS.
