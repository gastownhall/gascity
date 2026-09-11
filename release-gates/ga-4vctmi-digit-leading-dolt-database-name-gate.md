# Release gate: digit-leading Dolt database names

- Deploy bead: `ga-4vctmi`
- Source bead: `ga-p658sc`
- Reviewed source: `adbf5fed223ef1f707f9c27799a251cbe091da10`
- Gate base: `origin/main@6fd8f97c4042bcbf37b734278ef4df24035f5436`
- Evaluation date: 2026-07-31
- Disposition: **PASS**

`docs/PROJECT_MANIFEST.md` is not present in this repository at the evaluated
commit. This checklist applies the deployer role's release criteria and the
repository's documented CI-equivalent test policy.

## Gate checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | The deploy bead records reviewer PASS for the exact source SHA. The source bead's notes contain `REVIEWER VERDICT: PASS` after an independent build, vet, focused-unit, acceptance, scope, compatibility, and security review. |
| 2 | Acceptance criteria met | **PASS** | Non-HQ default Dolt database names derived from digit-leading rig prefixes now receive an `r` prefix at the single prefix-to-database boundary. HQ remains `hq`; ordinary letter-led prefixes and digits after the first character remain unchanged. The focused test passed 5 subtests, and `TestRegression_GastownWithRigs` passed both end-to-end subtests. The rig's display name, bead prefix, `DeriveBeadsPrefix`, configuration serialization, and existing metadata override precedence are unchanged. |
| 3 | Tests pass | **PASS** | The authoritative GitHub CI run for exact head `adbf5fed223ef1f707f9c27799a251cbe091da10` ([run 30608984961](https://github.com/gastownhall/gascity/actions/runs/30608984961)) completed with **44 jobs PASS, 0 FAIL, 14 SKIP**, including `CI / required`, preflight static, acceptance A, all 12 non-short `cmd/gc` process shards, product-metrics testhook, all path-required package/tmux/bdstore/REST-smoke integration lanes, and worker phase 2. The 14 skips are intentional push-only, unrelated-path, unsupported-OS, or optional live-contract lanes; none owns this change. Locally, `go build ./...` and `go vet ./...` passed; the focused unit owner passed **5 PASS, 0 FAIL, 0 SKIP**, and the acceptance owner passed **2 PASS, 0 FAIL, 0 SKIP**. A 40-job local diagnostic retained **36 PASS, 4 FAIL, 0 SKIP**: all four failures were push-only REST-full jobs contaminated by stale Dolt processes from earlier interrupted diagnostics, with three reporting explicit foreign-PID port collisions; these jobs are not part of the PR-required graph and the exact-sha CI run is the authoritative clean execution. |
| 4 | No high-severity review findings open | **PASS** | Reviewer notes report no blocking, security, compatibility, or scope findings. Unresolved HIGH/CRITICAL finding count: 0. |
| 5 | Final branch is clean | **PASS** | Before adding this gate, `git status --porcelain=v1 --untracked-files=no` produced no output and `git diff --check origin/main...adbf5fed223ef1f707f9c27799a251cbe091da10` exited 0. The only untracked paths are provider-materialized skill metadata under `.claude/skills/`; they are not staged or part of the deploy branch. This gate file is the sole deployer-authored change and will be committed before push. |
| 6 | Branch diverges cleanly from main | **PASS** | Evaluated first and rechecked after tests. `git merge-tree --write-tree origin/main adbf5fed223ef1f707f9c27799a251cbe091da10` exited 0 against current `origin/main@6fd8f97c4042bcbf37b734278ef4df24035f5436` and produced tree `0a2187801f8c3c2ff4cfa10fbfe25f0527199079`. The source is two commits ahead and two behind current main with no content conflict; no self-rebase was needed. |
| 7 | Single feature theme | **PASS** | The two-commit TDD diff changes only `cmd/gc/beads_provider_lifecycle.go` and its adjacent test. Both commits address one behavior: making default Dolt database identifiers valid when a rig-derived prefix starts with a digit. |

## Test evidence

```text
GitHub CI run 30608984961 at adbf5fed223ef1f707f9c27799a251cbe091da10
44 jobs PASS, 0 FAIL, 14 SKIP
CI / required: PASS

go build ./...
PASS

go vet ./...
PASS

PATH=<CI tools> go test -count=1 -v ./cmd/gc \
  -run '^TestDefaultScopeDoltDatabase$'
5 subtests PASS, 0 FAIL, 0 SKIP

PATH=<CI tools> go test -tags acceptance_a -count=1 -v \
  ./test/acceptance/... -run '^TestRegression_GastownWithRigs$'
2 subtests PASS, 0 FAIL, 0 SKIP
```

The CI-matched local tool bundle used the repository-pinned `bd` 1.1.0
release build, Dolt 2.1.7, and tmux 3.4. The first two local diagnostics
identified host-tool drift (`bd` reported the same version from a different
build, Dolt was 2.2.1, and tmux was 3.7b); those results were not counted as
release evidence. The later REST-full failures were retained rather than
retried into green and are classified as runner contamination because they
name stale, foreign-project Dolt PIDs occupying newly selected ports. The
exact-sha required CI run is clean.

## Scope evidence

```text
cmd/gc/beads_provider_lifecycle.go      | 13 ++++++++++++-
cmd/gc/beads_provider_lifecycle_test.go | 60 ++++++++++++++++++++++++++++++++
2 files changed, 72 insertions(+), 1 deletion(-)
```
