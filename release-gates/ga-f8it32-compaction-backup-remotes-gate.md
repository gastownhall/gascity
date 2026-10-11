# Release gate: ga-f8it32 — compaction backup-remote reconciliation

**Verdict:** **FAIL — WAIVED BY MAYOR**

**Evaluated:** 2026-08-18 (America/Los_Angeles)

**Deploy bead:** `ga-f8it32`

**Build bead:** `ga-s52yku`

**Review bead:** `ga-kiapum`

**Reviewed commit:** `f590515412a08fa320876422786e6dac775a9849`

**Base:** `origin/main@a565081fb87c13de8366594ad40ddfd731469539`

**Deploy mode:** remote (`origin` and `fork` are GitHub remotes)

**Deploy branch:** `deploy/ga-f8it32-gate`

The original technical gate remains a failure. Criterion 3 failed because two
non-diff-owned test failures did not reproduce in the same tests on the current
base, so the release-gate attribution policy did not permit them to be treated
as pre-existing failures. The Mayor's bead-specific 2026-08-18 ruling, recorded
verbatim in `ga-f8it32`, explicitly authorizes this deploy to proceed without
rewriting that result as green.

`docs/PROJECT_MANIFEST.md` is absent from both this checkout and
`origin/main`; this record evaluates the active seven release criteria from
the deployer contract and the test commands documented in `TESTING.md`.

## Gate criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-kiapum` records `REVIEW VERDICT: PASS` for the exact reviewed commit. |
| 2 | Acceptance criteria met | PASS | The diff adds a scheme-based `file://` backup-remote selector that excludes the authoritative remote, a separate `compact-pending-push-backup` marker namespace keyed by database and remote, and `reconcile_backup_remotes`, which reuses `push_remote_after_compaction`. Backup failures are isolated from the authoritative push. `run.sh` contains no hardcoded `usb` role/name. Three focused behavioral tests pass. |
| 3 | Tests pass | **FAIL — WAIVED** | The focused diff-owned tests and the complete affected package pass, but the documented broad local sweep has two failures that cannot meet the mandatory pre-existing-failure attribution clause. The Mayor explicitly waived this failed criterion for `ga-f8it32`; the original counts and failures remain recorded below. |
| 4 | No high-severity review findings open | PASS | Reviewer recorded no blocking or HIGH findings. Unresolved HIGH count: 0. |
| 5 | Final branch is clean | PASS | `git status --short` was empty at the reviewed commit immediately before this gate file was written. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree --messages origin/main f590515412a08fa320876422786e6dac775a9849` exited 0 and produced tree `8726a41c432e0fabf14824e46ec7a4c227f0b0c1`. Merge base: `6fbd6bdaa260ca98fece2156df77d40204b9fc23`; base is one commit ahead and the feature is two commits ahead. No self-rebase was needed. |
| 7 | Single feature theme | PASS | Both commits and all three changed files concern one subsystem and behavior: Dolt compaction reconciliation for local backup remotes. |

## Pre-flight

The reviewed commit resolves to a commit object at the full SHA above.
`gh api repos/gastownhall/gascity/commits/<reviewed-sha>/pulls` returned no
associated pull requests, so the target has not already merged and is not a
closed/superseded PR.

## Criterion 3 evidence

The test environment was prepared before execution:

- `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock`
- `TESTCONTAINERS_RYUK_DISABLED=true`
- cached `dolthub/dolt-sql-server:2.1.7` and related images present
- CI-pinned Dolt `2.1.7` and official `bd 1.1.0` installed under
  `/var/tmp/gc-gate-ga-f8it32-tools/bin`
- `TMPDIR=/var/tmp`; the shared Go build cache was not cleaned or relocated

### Diff-owned tests

Command:

```text
go test -json -count=1 ./examples/bd/dolt -run '^(TestCompactScriptReconcilesFileBackupRemoteAlongsideOrigin|TestCompactScriptIsolatesBackupPushFailureFromPrimaryPush|TestCompactScriptExcludesNonFileRemotesAndAuthoritativeFromBackupReconciliation)$'
```

Counts: **3 PASS, 0 FAIL, 0 SKIP**.

`diff_tests_executed`:

- `TestCompactScriptReconcilesFileBackupRemoteAlongsideOrigin` — PASS
- `TestCompactScriptIsolatesBackupPushFailureFromPrimaryPush` — PASS
- `TestCompactScriptExcludesNonFileRemotesAndAuthoritativeFromBackupReconciliation` — PASS

`waiver_ref`: Mayor's 2026-08-18 bead-specific ruling recorded verbatim in
`ga-f8it32` under "TWO WAIVERS AND A STANDING AUTHORIZATION"; deployer
notification `gm-wisp-if58ly`.

`skip_justification`: none required; zero tests skipped.

The complete affected package also passed:

```text
go test -json -count=1 ./examples/bd/dolt
```

Counts: **355 PASS, 0 FAIL, 0 SKIP**.

### Documented broad sweep

Command: `LOCAL_TEST_JOBS=4 make test-local-full-parallel` with the environment
above. Candidate log directory: `/var/tmp/gc-local-tests.PczzF8`.

Job counts: **35 PASS, 5 FAIL, 0 SKIP** out of 40 jobs. All six `cmd/gc`
process shards, all integration-core shards, all integration `cmd/gc` shards,
both PR REST-smoke shards, and product-metrics passed.

The identical command and environment were rerun at exact base
`origin/main@a565081fb87c13de8366594ad40ddfd731469539`. Base log directory:
`/var/tmp/gc-local-tests.0azm2T`.

### Failure attribution

The following candidate failures satisfy all four attribution clauses: they
are not diff-owned, each has a tracker, the exact test reproduced on the base,
and their packages have no path overlap with this diff.

- `TestProviderLiveClaudeKindPath` → `ga-fh1flg`; exact base reproduction:
  `agent_pane_busy` on the live herdr/tmux pane.
- `TestGetKeyBinding_CapturesDefaultBinding` → `ga-afqddr`; exact base
  reproduction: expected `next-window`, received an empty binding.
- `TestGetKeyBinding_CapturesDefaultBindingWithArgs` → `ga-afqddr`; exact base
  reproduction: expected `choose-tree`, received an empty binding.
- `TestCleanInstallTutorialPath` → `ga-rsktma`; exact base reproduction:
  circuit-breaker cleanup text polluted `bd config get issue_prefix` output.
  This is in the supplementary push/nightly full lane, not the PR REST-smoke
  lane, but it was still base-verified.

The following failures do **not** satisfy clause 3 (proven pre-existing by an
exact-test failure on the base), so criterion 3 must fail:

- `TestSQLiteLegacySnapshotSIGKILLAtBoundaries/legacy-claim-release-after`
  → `ga-5dsf6n`. The candidate timed out after 10 seconds waiting for the
  SQLite child-protocol line. The exact package/test passed in the base sweep.
  Clauses 1, 2, and 4 pass; clause 3 fails.
- `TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash`
  → `ga-lpfjhc`. The candidate encountered the known beads/Dolt dirty-table
  signature, but this exact test passed in the base recovery shard. A sibling
  base test reproduced the same environmental signature, which is not a
  substitute for the policy's exact-test base proof. Clauses 1, 2, and 4 pass;
  clause 3 fails.

### Policy and static lanes

- `policy_lane: make test-ci-policy` — PASS (runner policy, CI suite coverage,
  `scripts/cipolicy`, and named static-scope checks)
- `GOLANGCI_LINT_CACHE=/var/tmp/gc-gate-ga-f8it32-lint-cache LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main make lint-affected` — PASS, 0 issues
- `make fmt-check-changed` — PASS (`no changed existing Go files`)
- `go build ./...` — PASS
- `go vet ./...` — PASS
- `bash -n examples/bd/dolt/commands/compact/run.sh` — PASS
- `git diff --check origin/main...f590515412a08fa320876422786e6dac775a9849` — PASS

The first lint attempt used a poisoned shared golangci-lint cache containing
paths under a deleted `/var/tmp` checkout. The required rerun with a fresh,
on-disk golangci-lint cache passed with zero findings; no Go cache was cleaned.

## Waiver disposition

The technical gate result is still **FAIL**. The Mayor's 2026-08-18 ruling
explicitly names `ga-f8it32` as allowed to proceed because the
`TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash` failure matches
the `ga-lpfjhc` / gastownhall/beads#4566 dirty-table-during-schema-migration
signature and this compaction backup-remote diff has no plausible mechanism to
touch schema migration or store bootstrap. The corroborating occurrence is
logged on `ga-lpfjhc` with deploy bead `ga-f8it32`, build bead `ga-s52yku`, and
the failing test name.

The separate
`TestSQLiteLegacySnapshotSIGKILLAtBoundaries/legacy-claim-release-after`
failure remains recorded against `ga-5dsf6n`; it is not recast as a passing
test or folded into the beads#4566 signature. The bead-specific waiver covers
the failed criterion as recorded and authorizes pushing this isolated deploy
branch, opening its pull request, and routing merge authority to mayor/mpr. No
rig agent merges the pull request.
