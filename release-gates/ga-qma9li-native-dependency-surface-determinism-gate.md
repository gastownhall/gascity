# Release gate: deterministic native dependency surface measurement

- Deploy bead: `ga-qma9li`
- Build bead: `ga-iuznq2`
- Review bead: `ga-n8apor`
- Reviewed source: `09139999930b1c2aa7d47df288d1f2e26f28b4eb`
- Reviewed source branch: `builder/ga-iuznq2` (provenance only)
- Deploy branch: `deploy/ga-qma9li-gate`
- Base checked at gate time: `origin/main@52b5f4045c8c3e779daaf814b1fde7573f3af752`
- Gate result: **PASS**

## Gate criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Closed review bead `ga-n8apor` records `verdict: pass` for exact source `09139999930b1c2aa7d47df288d1f2e26f28b4eb`, with no uncovered criterion. |
| 2 | Acceptance criteria met | **PASS** | The measurement build now uses `CGO_ENABLED=0 go build -trimpath` while remaining unstripped for the downstream symbol and literal scans. The default byte ceiling is re-baselined from 270,000,000 to 180,000,000 beside dated measurement, variance, growth-rate, and future re-baseline evidence. Two consecutive exact guard runs were byte-identical at 172,098,893 bytes. |
| 3 | Tests pass | **PASS** | The path-relevant required CI coverage is green: the `Preflight / static checks` commands pass, including the exact native-surface guard twice; `Preflight / acceptance A` passes via `make test-acceptance`; and the broader fast baseline passes all 10 jobs. No test file changes in this one-file shell diff, so there is no diff-owned test to enumerate or waive. |
| 4 | No unresolved HIGH review findings | **PASS** | Review bead `ga-n8apor` records no style, security, specification, or high-severity finding. |
| 5 | Final branch clean | **PASS** | Before this record was added, `git status --short --branch` showed only `## deploy/ga-qma9li-gate`; `git diff --check origin/main...HEAD` passed. Final cleanliness is rechecked after the gate commit. |
| 6 | Branch diverges cleanly from main | **PASS** | `git merge-tree --write-tree --messages origin/main HEAD` returned tree `897f0a2b7201af728ab4e29b762f16cc16d829b8` with no conflict messages. The source is 2 commits behind and 1 commit ahead of current `origin/main`. |
| 7 | Single feature theme | **PASS** | One reviewed commit changes only `scripts/check-native-dependency-surface.sh`, making its byte measurement cross-host deterministic and re-baselining the same guard from evidence. |

## Source and acceptance evidence

The recorded source resolves exactly and is the deploy branch's pre-gate
`HEAD`:

```text
git rev-parse --verify --quiet '09139999930b1c2aa7d47df288d1f2e26f28b4eb^{commit}'
09139999930b1c2aa7d47df288d1f2e26f28b4eb
```

The isolated branch was created directly from that SHA. The source has one
commit over merge base `8940ee38c24c8c587293e0f456b42ae76bb298b1`:

```text
0913999993 fix(ci): make native-dependency-surface build deterministic, re-baseline cap
```

The exact guard was run twice in sequence after `bash -n` and `shellcheck`:

```text
native dependency guard: modules=727 aws=25 azure=9 dolthub=15 googleapi=1 binary_bytes=172098893
native dependency guard: modules=727 aws=25 azure=9 dolthub=15 googleapi=1 binary_bytes=172098893
```

Both runs preserved every module-count tripwire and produced the same binary
size. The 180,000,000-byte ceiling leaves 7,901,107 bytes of measured headroom,
while remaining 90,000,000 bytes tighter than the old host-dependent ceiling.

## Test and static evidence

- `test_cmd`: `make check-native-dependency-surface` twice.
- `test_cmd_scope`: focused, exact required policy check changed by this diff.
- `test_counts`: 2 PASS runs, 0 FAIL, byte-identical; no test SKIP applies.
- `determinism_log`: `/var/tmp/ga-qma9li-determinism.log`.
- `fast_cmd`: `LOCAL_TEST_JOBS=15 make test-fast-parallel`.
- `fast_result`: 10 PASS jobs, 0 FAIL jobs.
- `fast_log`: `/var/tmp/ga-qma9li-fast.log`.
- `acceptance_cmd`: `make test-acceptance`.
- `acceptance_result`: PASS for the Tier-A command-level PR gate.
- `static_acceptance_log`: `/var/tmp/ga-qma9li-static-acceptance.log`.
- `diff_tests_executed`: none; the diff changes no test file and no existing test wraps this script.
- `waiver_ref`: none.
- `uncovered_criteria`: none.

```text
bash -n scripts/check-native-dependency-surface.sh   PASS
shellcheck scripts/check-native-dependency-surface.sh PASS
make test-ci-policy                                 PASS
make check-gomod-replace                            PASS
make check-eventexport-isolation                    PASS
make check-core-boundary                            PASS
make test-native-doltlite-beads                     PASS
make check-docs                                     PASS
LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main make fmt-check-changed PASS
LINT_CHANGED_REF=origin/main make lint-affected     PASS (0 issues)
go build ./...                                      PASS
go vet ./...                                        PASS
make test-acceptance                                PASS
```

## PR #5489 conflict guard

PR #5489 is still open at gate time. Its current head
`9ef8c29b969e573eaa390dde18a64e3348c903c6` contains cited commit
`3c4090e81a`, and its copy of `scripts/check-native-dependency-surface.sh`
still uses the host-dependent plain `go build` plus a 285,000,000-byte default.
If #5489 lands after this deploy, it will silently replace the deterministic
180,000,000-byte re-baseline with that older behavior.

The deploy PR body must flag this conflict explicitly. Mayor/mpr must preserve
this PR's deterministic build and 180,000,000-byte ceiling when resolving or
sequencing #5489.

## Release disposition

**Gate PASS.** Commit this record on `deploy/ga-qma9li-gate`, push that isolated
branch, open the PR with the mandatory #5489 conflict warning, publish deploy
clearance on its exact head, and route the merge request to mayor/mpr. The
deployer does not merge.
