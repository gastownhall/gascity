# Release gate: normalize configured city and rig paths at ingest

- Deploy bead: `ga-jx0gqf`
- Build bead: `ga-iawy13.8`
- Source review: `ga-lb56pa`
- Reviewed commit: `5dc166233f37aff9817be18c7a38a33b70e1ebd5`
- Reviewed base: `2ff1536d9b014ea9728f46bbe7ece6f3378d76ad`
- Main evaluated: `origin/main@1f948e67b0ac088492af67c0748f521aad5768b0`
- Deploy branch: `deploy/ga-jx0gqf-gate`
- Evaluated: `2026-08-03T18:16:05Z`
- Overall verdict: **PASS**

`docs/PROJECT_MANIFEST.md` is not present at the evaluated commit, so this
checklist applies the deployer role's release-gate criteria together with
`engdocs/contributors/release-gate-criteria-conventions.md`.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 6 | Branch diverges cleanly from main | **PASS** | Checked first and rechecked after tests. `git merge-tree --write-tree origin/main 5dc166233f37aff9817be18c7a38a33b70e1ebd5` exited 0 against `origin/main@1f948e67b0ac088492af67c0748f521aad5768b0` and produced tree `6e80d2f2ce92899e47d232c6b12815253142242a`. The reviewed SHA remained the deploy source; no remote source branch was changed. |
| 1 | Review PASS present | **PASS** | Review bead `ga-lb56pa` is closed with reason `pass` for exact commit `5dc166233f37aff9817be18c7a38a33b70e1ebd5`. The review records `verdict: pass`, no style findings, and no blocking security or correctness findings. |
| 2 | Acceptance criteria met | **PASS** | Nine focused tests passed, 0 failed, 0 skipped. The four TDD regressions prove symlink-ancestor convergence for `--city`, `GC_CITY_PATH`, positional city/rig paths, and the `GC_RIG_ROOT`/`BEADS_DIR` projection. Existing passing contracts cover relative/local city input and source precedence, missing leaves through `pathutil.NormalizePathForCompare`, and contextual unknown-city errors. The three production changes replace inconsistent `Abs`/`Clean` ingest with the shared normalizer; no schema, flag, environment-variable, or API contract changes. The build/review notes inventory `city.toml`, `--city`, `--rig`, `GC_CITY*`, and `GC_RIG_ROOT`, and verify already-canonical or out-of-increment seams rather than adding duplicate downstream normalization. |
| 3 | Tests pass | **PASS** | On the exact reviewed SHA, `go build ./...`, `go vet ./...`, `gofmt -l` on all four changed files, and `git diff --check` passed. `make test-fast-parallel` passed 10/10 jobs (0 fail, 0 skip). The documented non-short CLI lane ran with `GC_FAST_UNIT=0` and the checksum-pinned CI `bd` archive: 15,362 PASS, 0 FAIL, 11 SKIP; the skips are helper-only, platform/opt-in, optional-pack, or ambient-CWD fallback cases, and none exercises the migrated explicit-ingest branches. The product-metrics testhook passed 12, failed 0, skipped 0. Worker phase 2 passed 26/26 requirements for each of Claude, Codex, and Gemini (78 PASS, 0 FAIL, 0 unsupported). Focused acceptance coverage passed 9/9. The PR integration smoke/core/cmd-gc/bdstore jobs and an isolated review-formula retry passed. The broad local RC stress sweep additionally exposed unchanged host-only limitations: tmux 3.7b does not return builtin key bindings without a server, and five `rest-full` shards timed out waiting for supervisors during the 29-way run. Those are outside the four-file diff; the exact merge-base CI run [30826419301](https://github.com/gastownhall/gascity/actions/runs/30826419301) and current-main CI run [30833610783](https://github.com/gastownhall/gascity/actions/runs/30833610783) passed the corresponding lanes. |
| 4 | No high-severity review findings open | **PASS** | The reviewer reports no blocker or major style, correctness, or security findings. The only informational note is the shared normalizer's pre-existing best-effort fallback if `filepath.Abs` cannot resolve a relative path. Unresolved HIGH/CRITICAL findings: 0. |
| 5 | Final branch is clean | **PASS** | The detached reviewed commit had an empty `git status --short` before this checklist was added. `git diff --check 2ff1536d9b014ea9728f46bbe7ece6f3378d76ad..5dc166233f37aff9817be18c7a38a33b70e1ebd5` passed, and `core.hooksPath` is `.githooks`. This checklist is the only deployer-authored release commit. |
| 7 | Single feature theme | **PASS** | The two-commit TDD set changes four files in `cmd/gc` (+112/-9), all for one behavior: canonicalizing configured city and rig paths once at their CLI/environment ingest boundaries. No independent feature is bundled. |

## Acceptance evidence

| Surface | Owning boundary | Evidence |
|---|---|---|
| `--city`, `GC_CITY`, `GC_CITY_PATH`, `GC_CITY_ROOT` | `validateCityPath` | `TestResolveCityFlagValueResolvesSymlinkAlias`, `TestResolveExplicitCityPathEnvResolvesSymlinkAlias` |
| Positional city/rig path | `resolveContextFromPath` | `TestResolveCommandContextPathArgResolvesSymlinkAlias` |
| `GC_RIG_ROOT`, `BEADS_DIR` | `bdRuntimeEnvForRigWithErrorRecoveryContext` | `TestBdRuntimeEnvForRigResolvesSymlinkAlias` |
| Relative/local input and source precedence | Existing city reference resolver | `TestNormalizePathForCompare`, `TestResolveExplicitCityPathEnvLocalWinsOverRegistration` |
| Missing leaf under a symlinked ancestor | Shared `pathutil` normalizer | `TestNormalizePathForCompareResolvesSymlinkAncestorForMissingLeaf` |
| Contextual invalid-city error | Existing city reference resolver | `TestResolveCityRefNameNoMatchLoudError` |

## Review notes

- This is internal path canonicalization only. It adds no configuration fields,
  flags, environment variables, endpoints, migrations, or dependencies.
- `--rig` and `city.toml` paths already converge through their existing
  normalized registry/config boundaries; this increment fixes only the three
  proven gaps whose raw string values could escape.
- The diff replaces three local `filepath.Abs`/`filepath.Clean` operations with
  the existing `normalizePathForCompare` wrapper. It does not add another
  normalization mechanism.

## Commands

```bash
git fetch origin main
git merge-tree --write-tree origin/main 5dc166233f37aff9817be18c7a38a33b70e1ebd5
git diff --check 2ff1536d9b014ea9728f46bbe7ece6f3378d76ad..5dc166233f37aff9817be18c7a38a33b70e1ebd5
gofmt -l cmd/gc/bd_env.go cmd/gc/bd_env_test.go cmd/gc/city_arg_resolve_test.go cmd/gc/main.go
go build ./...
go vet ./...
make test-fast-parallel
GC_FAST_UNIT=0 scripts/go-test-observable gate-cmd-gc-process -- -timeout 25m ./cmd/gc
make test-productmetrics-testhook
make test-worker-core-phase2-all PROFILE=claude/tmux-cli
make test-worker-core-phase2-all PROFILE=codex/tmux-cli
make test-worker-core-phase2-all PROFILE=gemini/tmux-cli
go test -count=1 -v ./internal/pathutil ./cmd/gc -run '<focused acceptance regex>'
make test-integration-shards-parallel
```
