# Release gate: Dolt compact same-count writer-race handling

- Deploy bead: `ga-i61ym3`
- Build bead: `ga-braayf`
- Review bead: `ga-09gd0z`
- Reviewed source: `164de8221d830458a1ab72a087cf9339c13bfaac`
- Reviewed source branch: `builder/ga-vyrswz-samecount-rebase` (provenance only)
- Deploy branch: `deploy/ga-i61ym3-gate`
- Base checked at gate time: `origin/main@52b5f4045c8c3e779daaf814b1fde7573f3af752`
- Gate result: **PASS with attributed pre-existing failures**

## Gate criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-09gd0z` records `verdict: pass` for exact source `164de8221d830458a1ab72a087cf9339c13bfaac`. Deploy bead `ga-i61ym3`, created by `gascity/reviewer`, independently records a fresh rebase-resume review pass. |
| 2 | Acceptance criteria met | **PASS** | The compact path distinguishes same-count value-hash drift, requires the existing additive-only `DOLT_DIFF` proof before treating it as a writer race, excludes any row gain from that path, defers on proof success, and preserves quarantine/manual review on proof failure. The complete affected package and all three diff-owned tests pass. |
| 3 | Tests pass | **PASS with attributed failures** | `make test-local-full-parallel` completed 37/40 jobs PASS and 3/40 FAIL with 3 failing top-level tests and 0 observed SKIP. All three failures have trackers predating this run and are structurally unreachable from the two changed files. Independently, `go test -count=1 -v ./examples/bd/dolt/...` passed 289 PASS, 0 FAIL, 0 SKIP. Both PR REST smoke shards and all six `cmd/gc` process shards passed. |
| 3b | Policy, build, and static checks | **PASS with attributed baseline failure** | `make test-ci-policy`, `make check-gomod-replace`, `make check-eventexport-isolation`, `make check-core-boundary`, `make check-docs`, `go build ./...`, `go vet ./...`, and `make test-native-doltlite-beads` passed. `make check-native-dependency-surface` preserved the known host-dependent old measurement failure (`270668536 > 270000000`), tracked before this run by `ga-iuznq2`; reviewed deploy bead `ga-qma9li` carries the isolated fix. This candidate cannot affect the `gc` binary graph. |
| 4 | No unresolved HIGH review findings | **PASS** | Review bead `ga-09gd0z` records no style, security, specification, or high-severity finding. |
| 5 | Final branch clean | **PASS** | Before this record was added, `git status --short --branch` showed only `## deploy/ga-i61ym3-gate`; `git diff --check origin/main...HEAD` passed. Final cleanliness is rechecked after the gate commit. |
| 6 | Branch diverges cleanly from main | **PASS** | `git merge-tree --write-tree --messages origin/main HEAD` returned tree `238cf7c5dca2174aa523f6f8f76b61f5f0f683c0` with no conflict messages. The source is 3 commits behind and 4 commits ahead of current `origin/main`. |
| 7 | Single feature theme | **PASS** | The reviewed range changes only `examples/bd/dolt/commands/compact/run.sh` and `examples/bd/dolt/dog_exec_scripts_test.go`; all four commits implement or test same-count writer-race handling in Dolt compaction. |

## Source and ancestry evidence

The handoff SHA resolves as a commit and is the deploy branch's pre-gate `HEAD`:

```text
git rev-parse --verify --quiet '164de8221d830458a1ab72a087cf9339c13bfaac^{commit}'
164de8221d830458a1ab72a087cf9339c13bfaac
```

The isolated branch was created directly from that SHA. Its reviewed range over
merge base `157858d9ee8bd6ab85e4a0d2128f34dc2e166a7f` is exactly:

```text
3fd2e43963 test(dolt): red — cover same-count hash drift writer-race defer (ga-h0bj7x)
22067fa6ce feat: green — defer same-count hash drift as proven writer-race UPDATE (refs ga-h0bj7x)
70b0345f98 fix(dolt): restore missing gain-exclusion guard on same-count defer path (ga-loyfg8)
164de8221d test(dolt): wire upstream's same-count writer-race test into the diff-proof mock (ga-h0bj7x)
```

## Acceptance and focused-test evidence

- `TestCompactScriptDefersProvenWriterRaceSameCountHashDrift`: PASS.
- `TestCompactScriptQuarantinesSameCountDriftWhenDiffProofFails`: PASS.
- `TestCompactScriptDefersWhenWriterCommitsCausingSameCountHashDrift`: PASS.
- Complete package command: `go test -count=1 -v ./examples/bd/dolt/...`.
- Complete package result: 289 PASS, 0 FAIL, 0 SKIP.
- Focused log: `/var/tmp/ga-i61ym3-focused.log`.

The changed Go file is package-local test code. The production change is one
shell script under the same package. Neither can be imported or executed by
`internal/bdflags`, `internal/runtime/herdr`, or `test/integration`'s city
suspend/resume scenario.

## Full-sweep failure attribution

- `test_cmd`: `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true LOCAL_TEST_JOBS=4 make test-local-full-parallel`
- `test_cmd_scope`: full-suite, including the path-triggered integration lane.
- `test_counts`: 37 PASS jobs, 3 FAIL jobs, 0 observed SKIP; 3 failing top-level tests.
- `full_log`: `/var/tmp/ga-i61ym3-full.log`.
- `shard_logs`: `/var/tmp/gc-local-tests.xHKZD3`.
- `waiver_ref`: none; each failure is attributed under structural non-reachability with a predating tracker.

| Raw failing test | Predating tracker | Disposition and proof |
|---|---|---|
| `TestBdFlagManifestCurrent` | `ga-f0uceo`, opened 2026-08-15 | Installed `bd` exposes flags absent from the checked-in manifest. The candidate does not touch `internal/bdflags`, the installed binary, or any manifest-generation path. |
| `TestSessionEventsLive` | `ga-idsv6m`, opened 2026-08-29 15:03 UTC | Exact known herdr event-stream race (`getAgent evt-a: ok=false err=nil`). The candidate does not touch or reach `internal/runtime/herdr`. |
| `TestE2E_SuspendResume_City` | `ga-yc0e3a`, opened 2026-08-18 | Exact known missing `citysus.report` timeout. The candidate does not touch city suspend/resume, session lifecycle, or the integration test. |

`failure_attribution`: clause 3(a), structural mechanism/import reachability,
plus no changed-path overlap. All trackers predate this gate run. No diff-owned
test failed or skipped.

## Policy and static evidence

```text
make test-ci-policy                      PASS
make check-gomod-replace                 PASS
make check-eventexport-isolation         PASS
make check-core-boundary                 PASS
make check-docs                          PASS
go build ./...                           PASS
go vet ./...                             PASS
make test-native-doltlite-beads          PASS
make check-native-dependency-surface     FAIL-ATTRIBUTED: 270668536 > 270000000 (ga-iuznq2; fix ga-qma9li)
```

The binary-size check's old measurement is host-path/cgo dependent. `ga-iuznq2`
predates this run and records the same failure on unmodified main; its reviewed
fix makes the build deterministic with `-trimpath` and `CGO_ENABLED=0`. This
candidate changes no `cmd/gc` or dependency file and cannot change the measured
binary.

## Release disposition

**Gate PASS.** Commit this record on `deploy/ga-i61ym3-gate`, push that isolated
branch, open the PR, publish deploy clearance on its exact head, and route the
merge request to mayor/mpr. The deployer does not merge.
