# Release Gate: env-read baseline golden fix

Deploy bead: `ga-kj3xzg`
Build bead: `ga-wo2x9l`
Review bead: `ga-0rb2fp`
Reviewed source: `0fceec4254f2a2a54d620880e0ced2d10b097405`
Base checked: `origin/main@a57af6e922274b68b8af68b5041a42e2ed7a98be`
Gate run: 2026-08-06 America/Los_Angeles
Verdict: **PASS**

`docs/PROJECT_MANIFEST.md` is not present in this worktree, so this gate uses
the deployer role's seven release criteria and the repository testing policy in
`TESTING.md`.

## Gate criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Review bead `ga-0rb2fp` is closed with reason `pass`; its notes record an independent reviewer verdict of PASS on the reviewed source. |
| 2 | Acceptance criteria met | PASS | The source adds `GC_TRANSCRIPT_META_ENABLED` to `internal/testenv/testdata/gc_env_read_baseline.golden`. A detached synthetic integration of the reviewed source with current `origin/main` produced tree `8e6f94af69609ab69a9bcafec7cb4d8d7445a9fa`; `TestGCEnvReadBaseline` passed there. |
| 3 | Tests pass | PASS | `go build ./...` and `go vet ./...` passed. The focused source test passed (1 PASS, 0 FAIL, 0 SKIP). `make test-fast-parallel` passed all 10 jobs. A retained-JSON `make test` run reported 35,100 named test/subtest PASS events, 0 FAIL, and 183 SKIP. The skips are pre-existing fast-profile exclusions for process-backed, platform/capability, and opt-in live tests; this diff changes no test file, and the owning env-baseline test ran and passed both on the source and the current-main integration tree. `diff_tests_executed: none (no test files in diff)`. `waiver_ref: none`. |
| 4 | No high-severity review findings open | PASS | The reviewer reported no blocking findings and no HIGH-severity finding. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean at the reviewed source before this gate artifact was added. Clean status is rechecked after the gate commit. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree --messages origin/main 0fceec4254f2a2a54d620880e0ced2d10b097405` exited 0 with no conflicts and produced tree `8e6f94af69609ab69a9bcafec7cb4d8d7445a9fa`; `git diff --check` also passed. No self-rebase was needed. |
| 7 | Single feature theme | PASS | The commit changes one testdata golden file with one insertion, solely restoring the environment-read census for the existing transcript-metadata toggle. |

## Acceptance evidence

- Current `origin/main` lacks `GC_TRANSCRIPT_META_ENABLED` in the golden file;
  the reviewed source contains it in sorted position between
  `GC_TRANSCRIPTS_SRC` and `GC_WEBHOOK_`.
- The source commit changes no production code, so event-export behavior and
  environment parsing remain unchanged.
- The GitHub commit-to-PR lookup returned no PR for the reviewed source, so the
  target had not already merged and normal release evaluation applied.
- The rootless Podman socket answered `_ping`; this repository has no
  testcontainers dependency or pinned container image relevant to this fast
  gate.

## Commands run

```text
gh api repos/gastownhall/gascity/commits/0fceec4254f2a2a54d620880e0ced2d10b097405/pulls
git merge-tree --write-tree --messages origin/main 0fceec4254f2a2a54d620880e0ced2d10b097405
git diff --check origin/main...0fceec4254f2a2a54d620880e0ced2d10b097405
go build ./...
go vet ./...
go test -count=1 -v ./internal/testenv/... -run '^TestGCEnvReadBaseline$'
make test-fast-parallel
OBSERVABLE_TEST_LOG=<temporary-jsonl> make test
```
