# Release Gate: docs CI hardening

- Deploy bead: `ga-bzzvst`
- Source build bead: `ga-y7r05r`
- Review bead: `ga-w8a0v2`
- Source branch: `builder/ga-y7r05r`
- PR: https://github.com/gastownhall/gascity/pull/3504
- Gate run: `2026-06-15T11:19:55-07:00`
- Base tip checked: `origin/main` at `1ff917a8913d69443392829c26a2b9f2e2196ab8`
- Branch merge base: `a52bb3626da330c3ef2718a48a448751432753d1`
- Reviewed source tip checked: `e3e24a21502464b1253654beac50b6b31c6b8538`
- Manifest note: `docs/PROJECT_MANIFEST.md` is not present in this checkout, so the deployer release criteria from the handoff prompt were used.
- File note: this path is retained because PR #3504 already links it; this content supersedes the earlier gate evidence on the branch.

## Gate Criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-w8a0v2` is closed with `Review verdict: PASS` for commit `e3e24a21502464b1253654beac50b6b31c6b8538`. |
| 2 | Acceptance criteria met | PASS | Source bead `ga-y7r05r` is closed as implemented; deployer rechecked each acceptance surface below against code and tests. |
| 3 | Tests pass | PASS | `make check-docs`, `go test ./test/docsync`, negative `TestLocalMarkdownLinks` smoke, `BASE_REF=origin/main .github/scripts/docs-render-check.sh origin/main`, `make test`, `go vet ./...`, and `git diff --check origin/main...HEAD` all completed as expected. PR #3504 status checks on the reviewed tip were rechecked and all non-skipped checks were successful. |
| 4 | No high-severity review findings open | PASS | Review notes list one minor non-blocking dead-code cleanup item (`extract_page_links` unused); unresolved HIGH findings count is 0. |
| 5 | Final branch is clean | PASS | Gate ran in a clean isolated worktree on `deploy/ga-bzzvst-docs-ci-gate` tracking `origin/builder/ga-y7r05r`; only this gate markdown was edited for the deploy commit. |
| 6 | Branch diverges cleanly from main | PASS | PR #3504 reported `mergeStateStatus: CLEAN`, and `git merge-tree --write-tree origin/main HEAD` exited 0 against `origin/main` at `1ff917a8913d69443392829c26a2b9f2e2196ab8`. The branch was not rebased from the deployer seat. |
| 7 | Single feature theme | PASS | Commit set is one docs-safety theme: docsync route validation, rendered-docs CI, docs ownership, contributor guidance, and docs-domain correction. |

## Acceptance Evidence

- `test/docsync` rejects internal Mintlify page links ending in `.md` or `.mdx` after stripping anchors and query strings.
- A negative smoke edit changing `docs/tutorials/index.md` to link to `/tutorials/03-sessions.md` made `go test -run TestLocalMarkdownLinks ./test/docsync` fail with the expected broken-link report.
- `.github/workflows/docs-render.yml` is scoped to docs PRs, skips drafts, uses read-only contents permission, cancels in-progress reruns for the same PR, and pins `actions/checkout` and `actions/setup-node` by SHA.
- `.github/scripts/docs-render-check.sh` runs Mintlify broken-link checking against `origin/main`, ignores non-page static assets, and fails only on net-new page-link regressions versus the base branch.
- `.github/CODEOWNERS` assigns `/docs/` to `@csells`.
- `CONTRIBUTING.md` describes `make check-docs` as validating local links and the docs route convention, and it points rendered-link validation at the docs-render workflow.
- `.github/pull_request_template.md` reminds contributors that docs links should use Mintlify routes, not GitHub markdown paths.
- The follow-up source commit `e3e24a21502464b1253654beac50b6b31c6b8538` makes the docs-link convention discoverable in failure output and contributor docs.
- `rg --hidden --glob '!.git' 'docs\\.gascity\\.com' .` returned no matches.

## Test Evidence

- `make check-docs`: PASS (`ok github.com/gastownhall/gascity/test/docsync 1.914s`).
- `go test ./test/docsync`: PASS (`ok github.com/gastownhall/gascity/test/docsync 2.286s`).
- `go test -run TestLocalMarkdownLinks ./test/docsync` with an injected `/tutorials/03-sessions.md` link: failed as expected with `broken local markdown links`.
- `BASE_REF=origin/main .github/scripts/docs-render-check.sh origin/main`: PASS; Mintlify returned non-zero, but the script found no page-link regressions and exited 0.
- `make test`: PASS (`observable go test: PASS log=/tmp/gascity-test.jsonl.l41hQg`).
- `go vet ./...`: PASS.
- `git diff --check origin/main...HEAD`: PASS.
- GitHub checks on PR #3504 at `e3e24a21502464b1253654beac50b6b31c6b8538`: all non-skipped checks successful, including CI required, pack compatibility gate, CodeQL, dashboard SPA, preflight, worker core, and integration shards.
