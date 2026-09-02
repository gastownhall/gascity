# Release Gate: Retry store-read timeouts instead of quarantining workflow finalizers

Bead: `ga-f681gz`  
Source review bead: `ga-7nq5kq`  
Build/source bead: `ga-x5fkq5`  
Reviewed commit: `b94c9710f6777a0c5ea4de0526fe9e1f83b67fec`  
Planned deploy branch: `deploy/ga-f681gz-gate`  
Base: `origin/main` at `615f5b7942220ee02f6825b9d3d52b7b4b9e9224`  
Gate evaluated: `2026-09-02`

`docs/PROJECT_MANIFEST.md` is not present in this checkout. This gate uses the
deployer release criteria, the build bead's done-when criteria, and the current
`TESTING.md` full-suite instructions.

## Result

PASS. The canonical full suite reported two red jobs, both attributable to
tracked host/tool conditions outside this diff under the non-diff-owned failure
protocol. Every diff-owned test executed and passed.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 6 | Branch diverges cleanly from main | PASS | Evaluated first and refreshed after the test run. `git fetch origin main` resolved `origin/main` to `615f5b7942220ee02f6825b9d3d52b7b4b9e9224`. `git merge-tree --write-tree origin/main b94c9710f6777a0c5ea4de0526fe9e1f83b67fec` exited 0 and returned tree `2c0d3f1a49e2370af9f770f5d57271000c0d5b25`. No self-rebase was needed. The pre-flight commit-to-PR lookup found no existing PR for the reviewed commit. |
| 1 | Review PASS present | PASS | Review bead `ga-7nq5kq` is closed with `VERDICT: PASS` for the exact reviewed commit. The SHA resolves locally to the full commit above. No review carryover was used. |
| 2 | Acceptance criteria met | PASS | The diff adds `TestFinalizeStoreTimeoutIsAvailabilityTier` with the reported finalizer wrapper, this repository's finalizer wrapper, and the already-supported work-query control. It adds `{needle: "timed out after", tier: TierAvailability}` to `transientNeedles`, leaves `isTransientWorkQueryFailure` unchanged, and touches no timeout values, workflow-root eligibility, cleanup, or self-healing behavior. The exact candidate test passed all three subtests. The reviewed ancestry contains the RED commit `4101276633b0f7815b07e65d4377a981f56065e0` and GREEN commit `b94c9710f6777a0c5ea4de0526fe9e1f83b67fec`. |
| 3 | Tests pass | PASS | Full-scope command: `make test-local-full-parallel`, the documented 40-job local full-suite target. Result: 38 PASS jobs, 2 raw FAIL jobs, 0 skipped jobs. The failures are attributed below with tracked conditions and causal proofs. Diff-owned JSON run: `go test -count=1 -json ./internal/dispatch -run '^TestFinalizeStoreTimeoutIsAvailabilityTier$'` — three subtests PASS, zero FAIL, zero SKIP; the parent test also PASSed. `go vet ./...`, `gofmt -l` on both changed Go files, and `git diff --check` all passed. |
| 4 | No high-severity review findings open | PASS | `ga-7nq5kq` records no blockers, majors, minors, or security findings. No unresolved HIGH finding is present. |
| 5 | Final branch is clean | PASS | `git status --porcelain` was empty after tests and before this checklist was created. The deploy branch will contain only the two reviewed commits plus this gate checklist commit. |
| 7 | Single feature theme | PASS | The two-file change is one `internal/dispatch` behavior: classify store-read timeout refusals as availability-tier so the existing controller retry path runs instead of first-refusal quarantine. |

## Test Evidence Integrity

- `test_cmd`: `make test-local-full-parallel`
- `test_cmd_scope`: `full-suite`
- environment: rootless Podman 5.8.4 via
  `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock`, socket present,
  `TESTCONTAINERS_RYUK_DISABLED=true`. The repository contains no
  testcontainers image pin, so cached-image tag verification is not applicable.
- full-suite counts: `38 PASS jobs / 2 raw FAIL jobs / 0 SKIP jobs` across all
  40 declared jobs. The runner does not emit a trustworthy aggregate leaf-test
  count, so no leaf count is invented.
- `diff_tests_executed`:
  `TestFinalizeStoreTimeoutIsAvailabilityTier/reported_finalize_outcome_read`,
  `.../this_repo's_own_finalize_wrapping`, and
  `.../work_query_read_(already_handled)` — `3 PASS / 0 FAIL / 0 SKIP`.
- `waiver_ref`: none. The raw failures qualify for attribution; no waiver is
  being self-granted.
- `policy_lane`: `make test-ci-policy` — PASS (workflow runner policy,
  CI-suite coverage, `scripts/cipolicy`, PR watchdog, and static-scope policy).
- `ci_lane_run`: `n/a (no CI-config change)`.

### Failure attribution

| Failure | Tracker | Attribution proof |
|---------|---------|-------------------|
| `TestBdFlagManifestCurrent` and its flag subtests | `ga-f0uceo` (open before this run) | Not diff-owned and no path overlap: failure is in `internal/bdflags`; the diff is limited to `internal/dispatch/control.go` and its new test. `go list -deps -test ./internal/bdflags` does not include `internal/dispatch`. The exact installed-`bd` manifest mismatch reproduced on untouched `origin/main@615f5b7942220ee02f6825b9d3d52b7b4b9e9224` and on the candidate. Proof: mechanism plus BASE_REF reproduction. |
| `TestSessionEventsLive` (`getAgent evt-a: ok=false err=nil`) | `ga-idsv6m` (open, predates this run) | Not diff-owned and no path overlap. `go list -deps -test ./internal/runtime/herdr` does not include `internal/dispatch`, so this live herdr test cannot reach the changed production package. The tracker already carries independent base reproductions; this run's candidate sighting was appended and verified. Proof: mechanism/import unreachability. |
| `TestProviderLiveClaudeKindPath` (`agent_pane_busy`, target pane unavailable) | `ga-iepsvr` (open condition-level tracker created during the discovering run) | Livelock exception applies: ownership and path separation are clear, and mechanism proof landed. The live herdr/tmux pane test cannot import `internal/dispatch` (`go list -deps -test` has no match); the same pane-contention condition is documented by earlier closed trackers `ga-fh1flg` and `ga-cqq3hs.1`. The new open tracker records this candidate sighting. Proof: mechanism/import unreachability. |

The inconclusive branch was not used: each attribution has a landed mechanism
or BASE_REF proof. The candidate does add unit-test load, but none of these
failures relies on an inconclusive one-off baseline result.

## Diff Scope

```text
internal/dispatch/control.go
internal/dispatch/finalize_query_timeout_tier_repro_test.go
```

`assert_deploy_ancestry_scope origin/main <reviewed-sha> ga-f681gz ga-x5fkq5
ga-r732z5` passed. `ga-r732z5` is the investigation/source bead cited by the RED
commit and belongs to this same timeout-classification feature.

## Audit Commands

```text
make test-local-full-parallel
  38/40 jobs PASS
  raw failures: integration-packages-core-1-of-4,
                integration-packages-core-3-of-4
  attributed above

go test -count=1 -json ./internal/dispatch \
  -run '^TestFinalizeStoreTimeoutIsAvailabilityTier$'
  3/3 subtests PASS, 0 FAIL, 0 SKIP

make test-ci-policy
  PASS

go vet ./...
  PASS
```
