# Release Gate: DoltLite wisp issue-dependency resolution

- Deploy bead: `ga-h3dioy`
- Source bug: `ga-rh2mgk`
- Review bead: `ga-x97emt`
- Reviewed commit: `2dbb5c79a6bd253a50438dd7601769a86bb4f601`
- Base checked: `origin/main@e5ed16b566302e5cd9bdbb7d75ff47bd52a8cbfc`
- Deploy branch: `deploy/ga-h3dioy-gate`
- Deploy mode: remote
- Overall verdict: **PASS WITH CRITERION 3 WAIVER**

`docs/PROJECT_MANIFEST.md` is absent from this checkout. This checklist
therefore applies the active deployer role's seven release criteria, the source
bead's acceptance criteria, `TESTING.md`, and
`engdocs/contributors/release-gate-criteria-conventions.md`.

## Criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-x97emt` is closed with reason `pass`; its notes record `Verdict: PASS` for the exact reviewed commit. |
| 2 | Acceptance criteria met | **PASS** | The SQL gate now resolves `depends_on_issue_id`, `depends_on_id`, and `depends_on_external` through the durable issue table while retaining the existing `depends_on_wisp_id` arm. The new pinning test proves an open durable blocker excludes the wisp and a closed durable blocker releases it. |
| 3 | Tests pass | **WAIVED** | The exact-SHA, CI-equivalent sharded run remains red at 26 PASS / 14 FAIL / 0 SKIP jobs. Mayor ruling `gm-wisp-hmtkss` waives criterion 3 for this gate only because the failures carry no information about this diff. The diff-owned test passes, no failed job overlaps the two changed files or graph-ready dependency logic, the environmental schema skew was independently confirmed, and the required failure attribution is recorded on `ga-h3dioy` and linked to `ga-cqq3hs`. Detailed retained evidence is below. |
| 4 | No high-severity review findings open | **PASS** | The independent review records no HIGH findings; unresolved HIGH count is 0. |
| 5 | Final branch is clean | **PASS** | `git status --short` was empty on the isolated exact-SHA deploy branch before this gate record was added. `git diff --check` over the reviewed two-commit feature diff passed. |
| 6 | Branch diverges cleanly from main | **PASS** | Checked after refreshing `origin/main` to `e5ed16b566302e5cd9bdbb7d75ff47bd52a8cbfc`. `git merge-tree --write-tree origin/main 2dbb5c79a6bd253a50438dd7601769a86bb4f601` exited 0 and produced tree `652b70fa1f48e54004a6070171d25f6c1392f723`. Merge base: `25117dd6204bc8c487adaa174a1926307cd5f14d`. No self-rebase was needed. |
| 7 | Single feature theme | **PASS** | Two TDD commits modify only the DoltLite graph-ready dependency gate and its adjacent pinning test under `internal/beads`: 56 insertions and 3 deletions across two files. |

## Criterion 3 retained evidence

The failed gate was executed on the exact reviewed commit. No deterministic
product-test failure was retried into green, and the worst result for this SHA
is retained.

Commands and results:

1. `go test -tags gascity_native_beads -count=1 -run '^TestDoltliteReadStoreReadyGraphOnlyExcludesBlockedWispsByIssueDependency$' -v ./internal/beads`
   — **1 PASS, 0 FAIL, 0 SKIP**.
2. `make test-native-doltlite-beads`
   — **PASS** (`ok github.com/gastownhall/gascity/internal/beads`).
3. `make test-local-full-parallel`
   — **FAIL** (Make exit 2; runner exit 123):
   **26 PASS, 14 FAIL, 0 SKIP, 40 total** at job level.
4. `go vet ./...`
   — **PASS**.

The 14 failing required jobs were:

- `unit-core`
- `cmd-gc-process-3-of-6`
- `cmd-gc-process-6-of-6`
- `integration-packages-core-4-of-4`
- `integration-packages-cmd-gc-2-of-6`
- `integration-packages-cmd-gc-5-of-6`
- `integration-packages-runtime-tmux-1-of-3`
- `integration-packages-runtime-tmux-3-of-3`
- `integration-review-formulas-retries-1-of-2`
- `integration-review-formulas-retries-2-of-2`
- `integration-bdstore`
- `integration-rest-full-1-of-8`
- `integration-rest-full-3-of-8`
- `integration-rest-full-8-of-8`

Attribution recorded on `ga-h3dioy` maps the named failures to these active
trackers:

- `ga-f0uceo`: installed `bd` flag-manifest drift.
- `ga-afqddr` / `ga-k3fxvj`: missing host tmux defaults.
- `ga-rntpsh`: isolated-supervisor readiness in formula/REST shards.
- `ga-lpfjhc`: beads issue 4566 dirty-table schema-migration failures.
- `ga-yp5e0n`: event archive/anchor host-contention flake.
- `ga-hgjlhi`: session-reconciler trace flake under shard-parallel load.
- `ga-2eekbp`: deadline/unread-store-notice test failure, newly tracked.
- `ga-3l0vyy`: workspace-service publication-refresh test failure, newly
  tracked.

Two integration `cmd/gc` jobs had no named test evidence from which to make a
sounder attribution and remain explicitly recorded as unattributed rather than
guessed. The full per-test and per-job breakdown is in `ga-h3dioy`'s notes and
is linked from `ga-cqq3hs`.

`diff_tests_executed`:
`TestDoltliteReadStoreReadyGraphOnlyExcludesBlockedWispsByIssueDependency PASS`.

`waiver_ref`: mayor ruling `gm-wisp-hmtkss`; conditional follow-through and
full attribution recorded on `ga-h3dioy`, with the failure-class rollup linked
to `ga-cqq3hs`.

`skip_justification`: not applicable; no job-level SKIP was observed. The
waiver is specific to this gate and does not convert the failures to passing or
carry forward to another gate.

## Disposition

Release evaluation passes under the explicit, one-gate criterion 3 waiver.
Push this isolated deploy branch, open a pull request from it, and route merge
authority to mayor/mpr; no rig agent merges the pull request.
