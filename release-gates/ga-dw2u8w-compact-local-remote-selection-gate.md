# Release Gate: Compact Local Remote Selection

- Deploy bead: `ga-dw2u8w`
- Build bead: `ga-cr66lk`
- Review bead: `ga-s40eru`
- Reviewed commit: `5d06034de2cb7dc5bc5f8af59b97912eee287e91`
- Base: `origin/main@c7a92b25ebb100ccfd0f3a31cf2e865a5d7bfb1c`
- Deploy mode: remote
- Deploy branch: `deploy/ga-dw2u8w-gate`
- Verdict: **PASS**

The reviewed change removes count- and name-based automatic remote selection
from the Dolt compact command. An explicit operator override remains
authoritative; without one, compact selects only the first name-sorted
`file://` remote and refuses when no local remote exists.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | `ga-s40eru` records `REVIEW VERDICT: PASS` on exact commit `5d06034de2cb7dc5bc5f8af59b97912eee287e91`. |
| 2 | Acceptance criteria met | PASS | `select_remote` now implements override -> first sorted local `file://` remote -> refusal. Tests cover a sole non-local remote and a non-local `origin` alongside a local candidate. The deliberately broader multi-local/backup-eligibility policy remains tracked separately by open P1 `ga-ubowno`. |
| 3 | Tests pass | PASS | The canonical isolated 40-job run had 38 jobs exit 0 and two non-diff-owned failures attributed below. All seven added/modified behavioral tests passed by exact name. Policy, build, vet, lint, format, docs, and native-DoltLite gates passed. |
| 4 | No high-severity review findings open | PASS | Review records no findings; security, spec, scope, and consistency checks all passed. |
| 5 | Final branch is clean | PASS | The detached candidate worktree was clean before this checklist was written. Hooks are active at `/home/jaword/projects/gascity/.githooks`. |
| 6 | Branch diverges cleanly from main | PASS | Evaluated first and refreshed before PR creation. Candidate is 2 commits ahead and 1 behind current `origin/main`; `git merge-tree --write-tree origin/main 5d06034de2cb7dc5bc5f8af59b97912eee287e91` exited 0 with tree `7f03ff3f4d0663ad6cb5b19a878d7e5aa2270832`; `git diff --check` passed. The new base commit changes only opencode/hooks files, with no overlap. No PR already carries the reviewed SHA. |
| 7 | Single feature theme | PASS | Three files, +99/-34, all confined to compact remote selection and its test fixture/coverage under `examples/bd/dolt`. |

## Criterion 3 evidence

The first full-suite invocation was invalid harness evidence: the caller-supplied
log directory did not yet exist, so all jobs failed before test execution. It was
discarded. The canonical rerun created the dedicated directory first and used:

`DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true LOCAL_TEST_JOBS=2 LOCAL_TEST_LOG_DIR=/var/tmp/gc-deploy-ga-dw2u8w-eval-logs make test-local-full-parallel`

`test_counts`:

- Full-suite wrapper jobs: 38 PASS, 2 FAIL, 0 SKIP.
- Diff-owned behavioral tests: 7 PASS, 0 FAIL, 0 SKIP.

`diff_tests_executed`:

- `TestCompactScriptFailsWhenSoleRemoteIsNonLocal`: PASS
- `TestCompactScriptPrefersLocalRemoteOverNonLocalOrigin`: PASS
- `TestCompactScriptFailsWhenMultipleRemotesLackOrigin`: PASS
- `TestCompactScriptPrefersOriginWhenMultipleRemotesExist`: PASS
- `TestCompactScriptReconcilesFileBackupRemoteAlongsideOrigin`: PASS
- `TestCompactScriptIsolatesBackupPushFailureFromPrimaryPush`: PASS
- `TestCompactScriptExcludesNonFileRemotesAndAuthoritativeFromBackupReconciliation`: PASS

`failure_attribution`:

- `TestSessionEventsLive` -> `ga-vkhfnj` | mechanism proof: the candidate changes only a shell script and Go tests in `examples/bd/dolt`; none is compiled into or invoked by `internal/runtime/herdr`. The tracker predates this run, covers the exact `getAgent evt-a: ok=false err=<nil>` signature, the failing test is not diff-owned, and there is no path overlap.
- `TestBdFlagManifestCurrent` -> `ga-he29xi` | mechanism proof: installed-CLI flag discovery in `internal/bdflags` cannot execute the compact shell script or its package tests. The PR-audit tracker predates this run and covers the exact manifest drift; the failing test is not diff-owned and there is no path overlap.

The diff adds two tests inside an already-covered package, but the attribution
protocol's inconclusive guard is not reached: direct mechanism proof settles both
failures before that branch.

Additional required gates:

- `go test -json -count=1 ./examples/bd/dolt/...` with exact-name filtering — all seven names above PASS; package command exited 0.
- `make test-ci-policy check-gomod-replace check-native-dependency-surface check-eventexport-isolation check-core-boundary test-native-doltlite-beads check-docs` — PASS.
- `go build ./...` — PASS.
- `make vet` — PASS.
- `GOLANGCI_LINT_CACHE=<fresh on-disk cache> make lint` — PASS, 0 issues.
- `make fmt-check` — PASS.
- `shellcheck -S warning examples/bd/dolt/commands/compact/run.sh` — three inherited warnings (`SC1083`, `SC2254`, `SC2034`), identical to `origin/main`; no diff-introduced warning.

`waiver_ref`: none.

`ci_lane_run`: n/a — no CI configuration changed.

## Disposition

Proceed from exact reviewed SHA `5d06034de2cb7dc5bc5f8af59b97912eee287e91`
on isolated branch `deploy/ga-dw2u8w-gate`. Publish the deploy-clearance status
on the gated head and route the pull request to mayor/mpr; the deployer does not
merge.
