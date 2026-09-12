# Release Gate: Python fallback readiness handshake

Gate date: 2026-08-20

Deploy bead: `ga-40p955`

Build bead: `ga-t1psb0`

Review bead: `ga-xk5jch`

Reviewed and gated source: `570096564de5ee6e0fd6514a2dd91ea6629ab0b6`

Base checked: `origin/main@4f4a37b28c9cfaa1ebe1c587576b69663a47f078`

Merge base: `75b12a0461254034effb319db9b1509258a899f6`

Clean merge tree: `d1c2685117158fce28a0f33236e1fd26e9bf7e1e`

Overall result: **PASS — 7/7 criteria**.

## Criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Review bead `ga-xk5jch` is closed with an explicit PASS at the exact reviewed source. The reviewer verified the single-file scope, acceptance behavior, security surface, build, vet, full package, and five focused repetitions. |
| 2 | Acceptance criteria met | PASS | The inline Python child writes `READY_MARKER` immediately after installing its SIGTERM handler and before sleeping. Not-ready attempts retry up to three times; a ready child that misses SIGTERM still fails hard; exhausted retries name host load and the readiness marker. The production 1-second bound and 2-second grace remain unchanged. |
| 3 | Tests pass | PASS | The documented 40-job full union recorded 33 PASS, 7 raw FAIL, 0 SKIP. Every raw failure is independently tracked and attributed or covered by a signature-scoped mayor disposition below; none is diff-owned. The changed regression test passed 5/5 with no skip, the complete changed package recorded 352 PASS / 0 FAIL / 0 SKIP, `make test-ci-policy` passed, and `go vet ./...` passed. |
| 4 | No high-severity review findings open | PASS | The review records no unresolved security, correctness, or HIGH-severity finding. Unresolved HIGH findings: 0. |
| 5 | Final branch is clean | PASS | `git status --porcelain`, `git diff --check origin/main...HEAD`, and `gofmt -l examples/bd/dolt/runtime_bounded_test.go` produced no output before this checklist was authored. `core.hooksPath` is `.githooks`. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` exited 0 against `origin/main@4f4a37b28c9cfaa1ebe1c587576b69663a47f078` and produced tree `d1c2685117158fce28a0f33236e1fd26e9bf7e1e`. Ancestry scope passed for `ga-40p955` and `ga-t1psb0`; no `.claude/**` path is introduced. |
| 7 | Single feature theme | PASS | The one candidate commit changes only `examples/bd/dolt/runtime_bounded_test.go` and serves one theme: making the Python fallback SIGTERM regression test distinguish child readiness from a real signal-delivery regression. |

## Test evidence

Environment:

- `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock`
- `TESTCONTAINERS_RYUK_DISABLED=true`
- rootless Podman socket present

Full command:

```text
LOCAL_TEST_JOBS=4 GO_TEST_TIMEOUT=30m make test-local-full-parallel
40 jobs selected: 33 PASS, 7 FAIL, 0 SKIP
raw exit: 2 (tracked attribution and standing dispositions below)
logs: /var/tmp/gc-local-tests.gLE424
```

Focused candidate-owned commands:

```text
go test -v -count=5 \
  -run '^TestRunBoundedPython3FallbackSendsSigtermBeforeKill$' \
  ./examples/bd/dolt/...

go test -v -count=1 ./examples/bd/dolt/...
```

`diff_tests_executed`:

- `TestRunBoundedPython3FallbackSendsSigtermBeforeKill` — PASS 5/5,
  0 FAIL, 0 SKIP.
- Full changed package — 352 PASS, 0 FAIL, 0 SKIP.

`waiver_ref` for diff-owned tests: none required; no diff-owned test skipped or
failed.

`policy_lane`: `make test-ci-policy` — PASS.

`go_vet`: `go vet ./...` — PASS.

## Failure attribution

- `TestBdFlagManifestCurrent` → tracker `ga-f0uceo`; not diff-owned, no path
  overlap, and the integration-tagged exact test failed identically on current
  base `4f4a37b28c9cfaa1ebe1c587576b69663a47f078`. Base log:
  `/var/tmp/ga-40p955-base-bdflags.log`.
- `TestGetKeyBinding_CapturesDefaultBinding` and
  `TestGetKeyBinding_CapturesDefaultBindingWithArgs` → tracker `ga-afqddr`;
  neither is diff-owned, there is no `internal/runtime/tmux` overlap, and both
  exact tests failed identically on current base. Base log:
  `/var/tmp/ga-40p955-base-tmux.log`.
- `TestCleanInstallTutorialPath` circuit-breaker stdout contamination → tracker
  `ga-hrdd3h`; not diff-owned and no path overlap. The candidate is a Go
  `_test.go` change compiled only into the `examples/bd/dolt` test binary, so
  it has no mechanism to affect the spawned `bd config` stdout path. The
  tracker records the same signature on current main and multiple unrelated
  candidates. Candidate log:
  `/var/tmp/gc-local-tests.gLE424/integration-rest-full-1-of-8.log`.
- `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix` and
  `TestGCLiveContract_BeadsAndEvents` → tracker `ga-lpfjhc`; both carry the
  exact `gastownhall/beads#4566` dirty-table schema-migration signature. The
  candidate cannot affect schema migration or store bootstrap. Applied the
  mayor's 2026-08-18 signature-scoped standing authorization recorded on
  `ga-6bnc42`; these raw FAILs remain recorded as **WAIVED**, not rewritten as
  green. Occurrence logging on `ga-lpfjhc` is verified.
- `TestProviderLiveClaudeKindPath` (`agent_pane_busy` on `w1:p1`) → tracker
  `ga-fh1flg`; the candidate has no `internal/runtime/herdr`, tmux/session
  lifecycle, or pane-allocation path. Applied standing disposition
  `waiver_ref: mayor-2026-08-20-herdr-pane-standing` / memory key
  `herdr-pane-busy-standing-disposition` at the exact gated source. Use is
  recorded on the deploy bead.

All raw failures are not diff-owned, have a tracker, have base/mechanism proof,
and have no candidate path overlap. Criterion 3 therefore passes under the
repository's non-diff-owned failure policy and the two explicit standing
dispositions.
