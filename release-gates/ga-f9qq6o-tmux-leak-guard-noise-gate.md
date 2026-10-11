# Release Gate: isolate command tests from tmux leak-guard diagnostics

Date: 2026-09-02
Deployer: gascity/deployer
Status: PASS
Deploy bead: ga-f9qq6o
Build bead: ga-5pe5xv
Review bead: ga-w22vvl
Reviewed commit: 2a4de4b124c452cdfa8851a37b46817516da0e45
Verified carryover commit: 76d63cb6adf9f708b3ae5da3ac2e446da10ace46
Base checked: origin/main at faf4e3b89d115316e556edb0dcc2b4f2952c0e64

`docs/PROJECT_MANIFEST.md` is not present in this checkout, so this gate uses
the deployer release criteria and the repository testing policy in `TESTING.md`.

## Gate summary

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-w22vvl` records `verdict: pass` for `2a4de4b124c452cdfa8851a37b46817516da0e45`. The builder recorded a rebase-only carryover to `76d63cb6adf9f708b3ae5da3ac2e446da10ace46`; independent recomputation produced the same stable full-diff patch ID, `6be3468fc4ae5e7078f46f27dfb04a02da427fe3`, for both commits. |
| 2 | Acceptance criteria met | PASS | The test-only helper removes only the tmux leak guard's three harness-diagnostic prefixes from captured subprocess stderr, while preserving real command stderr, ordering, and trailing-newline behavior. The table test covers empty and no-op input plus leading, trailing, and sandwiched guard blocks. Every affected call path passed in the full suite. |
| 3 | Tests pass | PASS | The documented full-scope command completed all 40 jobs: 39 job PASS, 1 attributed raw FAIL, 0 skipped jobs; 48,379 top-level test executions PASS, 1 raw FAIL, 208 SKIP. The sole failure was the pre-existing installed-`bd` manifest skew tracked by `ga-f0uceo`; it is outside the diff and attributed below. All diff-owned and affected tests passed. The independent policy lane and `go vet ./...` passed. |
| 4 | No high-severity review findings open | PASS | The review records no style, security, or specification blocker and no HIGH finding. It reports clean formatting, vet, build, and lint results. |
| 5 | Final branch is clean | PASS | The carryover checkout was clean before the gate artifact was written. The gate artifact is committed on the isolated deploy branch; no implementation edits or unrelated files are present. |
| 6 | Branch diverges cleanly from main | PASS | The verified carryover commit contains `origin/main@faf4e3b89d115316e556edb0dcc2b4f2952c0e64` (`git merge-base --is-ancestor` exit 0), and `git merge-tree --write-tree` completed at tree `1be1696676e92c0661df542f6bc727c8fbb7827b`. No self-rebase was needed. |
| 7 | Single feature theme | PASS | The two-commit series changes only `cmd/gc/cmd_commands_test.go` to make command-process stderr assertions independent of concurrent tmux leak-guard diagnostics. |

## Criterion 3 evidence

```text
test_cmd: DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true GOFLAGS=-v GO_TEST_TIMEOUT=30m make test-local-full-parallel
test_cmd_scope: full-suite
test_job_counts: 39 PASS, 1 raw FAIL, 0 SKIP (40 total)
test_counts: 48379 PASS, 1 raw FAIL, 208 SKIP (top-level Go test executions)
policy_lane: make test-ci-policy — PASS
vet_lane: go vet ./... — PASS
ci_lane_run: n/a (no CI configuration change)
waiver_ref: n/a
full_log_dir: /var/tmp/ga-f9qq6o-full.9hpJrD
```

The rootless Podman 5.8.4 socket was live before the run. Cached images included
the repository-pinned `docker.io/dolthub/dolt:2.1.7`, current
`docker.io/dolthub/dolt-sql-server:2.2.0`, and
`docker.io/testcontainers/ryuk:0.14.0` tags.

`skip_justification`: the 208 top-level skips are suite-controlled helper,
platform, unavailable-provider, or explicit opt-in cases exercised by the full
matrix. None is in the modified test file, and the immediately preceding full
gate on this feature reported the same 208-skip baseline.

`diff_tests_executed`:

- `TestStripTmuxLeakGuardNoise` — PASS in the process-backed and integration
  `cmd/gc` shards.
- `TestPackCommandCobraHelpAndUnknownParity` — PASS in both shard families.
- `TestPackCommandExitReturnsThroughRun` — PASS in both shard families.
- `TestPackCommandGroupMissRejectsUnknownSubcommands` — PASS in both shard
  families.

`failure_attribution`: `TestBdFlagManifestCurrent` -> `ga-f0uceo` | clause
3(a), mechanism. The candidate modifies only package-local test code in
`cmd/gc/cmd_commands_test.go`; it cannot affect `internal/bdflags` or change
the installed `bd` binary. The failing test file is not diff-owned, the tracker
predates this run and names the exact installed-binary/manifest skew, and there
is no path overlap. This recurrence was appended to the tracker.

## Decision

PASS. The reviewed test-harness fix is current, content-identical to the
reviewed diff, independently reverified across the complete local test matrix,
and ready for an isolated deploy branch and pull request.
