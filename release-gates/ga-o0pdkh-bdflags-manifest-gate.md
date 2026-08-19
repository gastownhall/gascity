# Release Gate: synchronize the bd flag manifest with supported bd v1.1.0 builds

Deploy bead: ga-o0pdkh
Implementation bead: ga-gqxh5s
Initial review bead: ga-i8rhmj
Follow-up review bead: ga-7huhms
Deploy branch: deploy/ga-o0pdkh-gate
Initial reviewed source: 3289b5673985f237fe655170162b89a715b84269
Current reviewed PR source: 570e38be5ce7cdf45cb0efc6a911c6fa70170e87
Base checked: origin/main at b63623d08ecf565de82c226f0af1ca2fc359d45d
Merge base: cb456b85ecd923186a50493074f8b8a4c75d7eac

## Gate Summary

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | ga-i8rhmj is closed `pass` for the initial implementation through 3289b5673985f237fe655170162b89a715b84269. ga-7huhms is closed `pass` for the exact post-review delta from f90f0a04eac870b31440e04e449f075ef043d332 through current PR head 570e38be5ce7cdf45cb0efc6a911c6fa70170e87. |
| 2 | Acceptance criteria met | PASS | The manifest is a compatibility superset across both supported bd v1.1.0 builds. `TestBdFlagManifestCurrent` passed all 17 subcommands against fleet build 0954be416 and checksum-pinned CI release build 8e4e59d39. The provenance and compatibility comments name the two-build boundary. |
| 3 | Tests pass | PASS | `make test-fast-parallel`: 10 PASS jobs, 0 FAIL, 0 SKIP. `make test-cmd-gc-process-parallel`: 7 PASS jobs, 0 FAIL, 0 SKIP, including all six `GC_FAST_UNIT=0` shards and product-metrics. Focused regression: `TestCityRuntimeForceShutdownTearsDownAfterLateAsyncSweep` 10 PASS, 0 FAIL, 0 SKIP. Manifest contract: 17/17 subtests PASS against each supported bd build. `diff_tests_executed`: `TestGlobalValueFlagsIsComplete` PASS; `TestGlobalBoolFlagsIsComplete` PASS. `waiver_ref`: none. Live PR checks independently observed at 58 SUCCESS, 34 SKIPPED, 0 FAILURE; the skips are path-gated jobs outside this `internal/bdflags/**` change, while all 12 required `cmd/gc process` checks succeeded. |
| 4 | No high-severity review findings open | PASS | ga-i8rhmj records no style or security findings. ga-7huhms records no high-severity finding on the post-review two-file delta. |
| 5 | Final branch is clean | PASS | The checklist was committed on the isolated deploy branch; `git status --porcelain` returned empty and `git diff --check origin/main...HEAD` passed afterward. |
| 6 | Branch diverges cleanly from main | PASS | Already-merged pre-flight found PR #5284 OPEN, not merged. `git merge-tree --write-tree origin/main HEAD` exited 0 on the final gate tip. No self-rebase was needed. |
| 7 | Single feature theme | PASS | The feature changes only `internal/bdflags/bdflags.go` and `internal/bdflags/bdargs_test.go`, updating one CLI-flag classifier manifest and its completeness tests. The release-gate file is audit evidence for that same feature. |

## Acceptance Evidence

| Acceptance item | Result | Evidence |
|-----------------|--------|----------|
| Reflect every supported bd flag for all 17 known subcommands without removing real coverage. | PASS | `TestBdFlagManifestCurrent` passed 17/17 subcommands against both builds. Released v1.1.0 build 8e4e59d39 accepts persistent `--profile`; fleet build 0954be416 accepts the renamed `--cpu-profile`. Retaining both is the required compatibility superset. |
| Pass the live freshness contract. | PASS | Fleet build: 17 PASS subtests, 0 FAIL, 0 SKIP. CI release build: 17 PASS subtests, 0 FAIL, 0 SKIP. |
| Keep manifest provenance and compatibility intent explicit. | PASS | `internal/bdflags/bdflags.go` identifies the fleet source date/build and documents the released-vs-fleet compatibility boundary, including the pre-rename `--profile` spelling. |

## Commands Run

```text
gh pr view https://github.com/gastownhall/gascity/pull/5284 --json ...
git merge-tree --write-tree origin/main origin/deploy/ga-o0pdkh-gate
make test-fast-parallel
make test-cmd-gc-process-parallel
go test ./cmd/gc -run '^TestCityRuntimeForceShutdownTearsDownAfterLateAsyncSweep$' -count=10 -v
go test ./internal/bdflags/... -run '^(TestGlobalValueFlagsIsComplete|TestGlobalBoolFlagsIsComplete)$' -count=1 -v
go test ./internal/bdflags/... -tags integration -run '^TestBdFlagManifestCurrent$' -count=1 -v
PATH=<checksum-pinned-bd-v1.1.0>:$PATH go test ./internal/bdflags/... -tags integration -run '^TestBdFlagManifestCurrent$' -count=1 -v
go build ./...
go vet ./...
git diff --check origin/main...HEAD
```

The rootless Podman socket was available before criterion 3. These selected CI-equivalent lanes and the diff-owned tests do not require testcontainers; the changed package is pure Go and shells out only to `bd` for its integration freshness check.

## Touched Files

```text
internal/bdflags/bdargs_test.go
internal/bdflags/bdflags.go
release-gates/ga-o0pdkh-bdflags-manifest-gate.md
```
