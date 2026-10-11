# Release gate: strict prime prompt-delivery budget reporting (`ga-tlm40u`)

- Overall verdict: **PASS with attributed/waived raw test failures**
- Evaluated: 2026-09-03 PDT / 2026-09-03 UTC
- Deploy mode: `remote`
- Reviewed deploy source: `23778f3df10e1460ac31209dfdaa82fc10bfd2fe`
- Source branch: `builder/ga-q8wgom.1.2` (provenance only)
- Planned deploy branch: `deploy/ga-tlm40u-gate`
- Base evaluated: `origin/main@c9f32fbbfd10070f64de9fbcd3c1c8d3e2965005`
- Review bead: `ga-3pjj2f`
- Build bead: `ga-q8wgom.1.2`

`docs/PROJECT_MANIFEST.md` is not present at the reviewed commit or on the
evaluated base. This gate therefore applies the active seven deployer criteria,
the build bead's six acceptance criteria, and the repository-wide test policy
in `TESTING.md`.

## Criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Closed review bead `ga-3pjj2f` records `verdict: pass` for the exact resolved commit `23778f3df10e1460ac31209dfdaa82fc10bfd2fe`. It records no style, security, specification, or coverage finding. |
| 2 | Acceptance criteria met | **PASS** | Independent diff inspection confirms strict text mode preserves rendered-prompt stdout and emits the budget diagnostic on stderr; strict JSON uses a typed `prompt_budget` object; safe configured, ACP, fallback, and `none` paths succeed; unsupported oversized subprocess delivery fails without leaking prompt content; non-strict output is unchanged; and the existing 100,000-byte raw and 128,000-byte quoted thresholds are reused. The ten named cases below all passed. |
| 3 | Tests pass | **PASS with attributed/waived raw failures** | The documented full-scope command, `make test-local-full-parallel`, scheduled all 40 jobs and completed **37 PASS / 3 raw FAIL / 0 SKIP jobs**. The three non-diff-owned failures are adjudicated below. Every diff-owned test ran and passed. |
| 3a | Non-diff-owned failures attributed | **PASS** | `TestBdFlagManifestCurrent` is attributed to predating tracker `ga-f0uceo`. `TestAdoptPRFormulaRetriesTransientReviewerStep` and `TestCleanInstallTutorialPath` failed during fixture bootstrap with the exact beads#4566 dirty-schema condition tracked by `ga-esyijp`; they remain raw **FAIL-WAIVED** under the mayor standing authorization recorded on `ga-lpfjhc` / `ga-6bnc42`. The first pre-push fast gate then hit the known fork/exec ETXTBSY fixture race in `TestGoTestShardManifestFailsClosed`, tracked by `ga-vkhfnj`. Current sightings were appended and read back from all three open trackers. |
| 3b | Policy/lint lane | **PASS** | `make test-ci-policy`, merge-base-scoped changed formatting and affected lint, `go build ./...`, `go vet ./...`, `make check-docs`, and `git diff --check` all exited 0. The affected lint invocation reported `0 issues`. |
| 3c | CI-config lane run | **PASS / n/a** | No workflow, job, matrix, timeout, required-check list, or other CI configuration changed. `ci_lane_run: n/a (no CI-config change)`. |
| 4 | No high-severity review findings open | **PASS** | Review bead `ga-3pjj2f` records no style, security, specification, or coverage findings and no unresolved HIGH finding. |
| 5 | Final branch clean | **PASS** | `git status --short` produced no output after all test and static runs and before this checklist was added. `git diff --check ed146d8d9f2fdf142b4b23540ff0412fd2eec33c...HEAD` also passed. Cleanliness is rechecked after the gate commit. |
| 6 | Branch diverges cleanly from main | **PASS** | The already-merged pre-flight found no pull request associated with the reviewed SHA. `git merge-tree --write-tree origin/main 23778f3df10e1460ac31209dfdaa82fc10bfd2fe` exited 0 and produced tree `4cd21aaf9cd47ee2b5ea6661117c4e95625e5df9` with no conflict messages. No bounded self-rebase was needed. |
| 7 | Single feature theme | **PASS** | The three reviewed commits and five paths implement one operator-visible feature: reporting the existing launch-time prompt-delivery decision from `gc prime --strict`, with its command tests and required resource-census synchronization. No independent behavior is bundled. |

## Source and ancestry evidence

The recorded source resolved to the exact full commit used by review and this
gate:

```text
git rev-parse --verify --quiet '23778f3df10e1460ac31209dfdaa82fc10bfd2fe^{commit}'
23778f3df10e1460ac31209dfdaa82fc10bfd2fe
```

The reviewed range is three commits:

```text
7cd8f9b71e test(prime): red — report prompt delivery budgets in gc prime --strict (refs ga-q8wgom.1.2)
4caedf8531 chore(testpolicy): reconcile census ledger for prompt-budget test fixture (refs ga-q8wgom.1.2)
23778f3df1 feat: green — report prompt delivery budgets in gc prime --strict (refs ga-q8wgom.1.2)
```

Net diff: 5 files, 515 insertions, 35 deletions. Paths are confined to
`cmd/gc`, `TESTING.md`, and the resource-census ledger under
`internal/testpolicy/resourcecensus` / `test`. There are no `.claude/**`, API
schema, dashboard, workflow, or unrelated package paths.

The mandatory scope guard passed with the deploy bead and implementation bead:

```text
assert_deploy_ancestry_scope origin/main 23778f3df10e1460ac31209dfdaa82fc10bfd2fe \
  ga-tlm40u ga-q8wgom.1.2
```

## Criterion 3 evidence

Environment established before the complete run:

- `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock`
- `TESTCONTAINERS_RYUK_DISABLED=true`
- the rootless Podman socket was reachable
- `cairn list dolt-tests-via-podman` returned `not_found`, and the candidate's
  Go test paths contain no testcontainers or Podman dependency

Test evidence:

- `test_cmd`: `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true make test-local-full-parallel`
- `test_cmd_scope`: `full-suite`
- `test_counts`: **37 PASS / 3 raw FAIL / 0 SKIP jobs**
- `shard_logs`: `/var/tmp/gc-local-tests.BJH9j2`
- `skip_justification`: none; no test skip was observed
- `diff_tests_executed`: **10 PASS / 0 FAIL / 0 SKIP**
- `waiver_ref`: mayor standing authorization on `ga-lpfjhc` / `ga-6bnc42`, limited to the two exact beads#4566 dirty-schema fixture failures below
- `ci_lane_run`: n/a — no CI-config change

### Diff-owned tests

All six non-short `cmd/gc` process shards passed and enumerated every candidate
test by name. A supplemental exact-head verbose run also completed 10 PASS, 0
FAIL, 0 SKIP:

```text
GC_FAST_UNIT=1 go test -count=1 -v ./cmd/gc -run '^TestPrimePromptBudget'
```

`diff_tests_executed`:

- `TestPrimePromptBudgetBelowThresholdArgvDelivery`: PASS
- `TestPrimePromptBudgetExactThresholdFallback`: PASS
- `TestPrimePromptBudgetACPOversizedSuccess`: PASS
- `TestPrimePromptBudgetSubprocessOversizedFailure`: PASS
- `TestPrimePromptBudgetConfiguredNoneSuccess`: PASS
- `TestPrimePromptBudgetQuoteInflatedArgvBytes`: PASS
- `TestPrimePromptBudgetUnicodeByteCounts`: PASS
- `TestPrimePromptBudgetStderrVsStdoutSeparation`: PASS
- `TestPrimePromptBudgetTypedJSONFields`: PASS
- `TestPrimePromptBudgetUnchangedNonStrictOutput`: PASS

### Raw failure attribution

- `failure_attribution: TestBdFlagManifestCurrent -> ga-f0uceo | clause 3(a), mechanism — attributed`
  - `integration-packages-core-1-of-4` reported the tracker's exact installed-`bd` flag-manifest drift.
  - The candidate changes neither `internal/bdflags` nor the installed `bd` executable. `go list -deps ./internal/bdflags` reaches none of the changed production packages, and there is no failing-path overlap.
  - The tracker predates this run; the current sighting was appended and read back at `2026-09-03T02:16:22Z`.

- `failure_attribution: TestAdoptPRFormulaRetriesTransientReviewerStep -> ga-esyijp | clause 3(a), mechanism — raw FAIL-WAIVED`
  - `integration-review-formulas-retries-1-of-2` failed while fixture `gc init` encountered the exact beads#4566 pending dirty `issues` / `schema_migrations` condition, before the review-formula assertions.
  - The candidate changes neither Dolt schema migration nor store bootstrap. There is no failing-path overlap.
  - The current sighting was appended and read back at `2026-09-03T02:23:57Z`; the standing authorization is recorded on `ga-lpfjhc` / `ga-6bnc42`.

- `failure_attribution: TestCleanInstallTutorialPath -> ga-esyijp | clause 3(a), mechanism — raw FAIL-WAIVED`
  - `integration-rest-full-2-of-8` failed while its rig fixture encountered the same beads#4566 dirty `comments` / `issues` migration plus missing `leases` table.
  - The candidate changes neither Dolt schema migration, rig-store bootstrap, nor this integration test. There is no failing-path overlap.
  - The current sighting was appended and read back at `2026-09-03T02:32:46Z`; the standing authorization is recorded on `ga-lpfjhc` / `ga-6bnc42`.

- `failure_attribution: TestGoTestShardManifestFailsClosed/unreadable -> ga-vkhfnj | clause 3(a), mechanism — attributed`
  - The first pre-push fast gate's `unit-core` job reported the exact consolidated ETXTBSY signature: the unchanged generated fake `go` executable failed with `/bin/sh: bad interpreter: Text file busy` before the manifest assertion.
  - The candidate changes no `scripts` path, and `go list -deps ./scripts` reaches none of its changed production packages. The same test passed at the same reviewed source during the immediately preceding full-suite run, while the fix history consolidated from `ga-zdhadv` documents the unchanged fixture's parallel fork/exec race. There is no path overlap.
  - The current sighting was appended to canonical predating gate tracker `ga-vkhfnj` and read back at `2026-09-03T03:02:28Z`. The failed push wrote no remote branch; the complete fast gate is rerun before push.

No failed test is diff-owned. The candidate adds the ten declared command tests
and reconciles the repository's test-resource census for that declared load;
none of the attributed failures is an inconclusive contention-only result.

## Policy and static evidence

```text
make test-ci-policy                                                     PASS
LINT_CHANGED_SCOPE=tracked \
  LINT_CHANGED_REF=ed146d8d9f2fdf142b4b23540ff0412fd2eec33c \
  make fmt-check-changed lint-affected                                  PASS (0 issues)
go build ./...                                                          PASS
go vet ./...                                                            PASS
make check-docs                                                         PASS
git diff --check ed146d8d9f2fdf142b4b23540ff0412fd2eec33c...HEAD       PASS
git config --get core.hooksPath                                         .githooks
```

A preliminary affected-lint invocation against the newer `origin/main` rather
than the candidate's merge base selected a main-only deleted dashboard asset
and exposed the known ignored third-party `node_modules/flatted` condition
tracked by `ga-bvixfw`. The sighting was recorded on that predating tracker.
The PR-equivalent merge-base-scoped invocation above is the authoritative lane
and passed.

## Disposition

Gate PASS. Create `deploy/ga-tlm40u-gate` from the exact reviewed source,
commit this checklist, push only the isolated branch, open a pull request
against `main`, publish `release-gate/deploy-clearance=success` on the exact PR
head, and route the merge request to mayor/mpr. The deployer does not merge.
