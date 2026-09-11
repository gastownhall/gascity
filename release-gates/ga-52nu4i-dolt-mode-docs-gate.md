# Release Gate: dolt_mode_safe troubleshooting docs

- Result: PASS
- Beads: ga-52nu4i, ga-1awyb6
- Candidate branch: `builder/ga-4qbgqf.2-partial-demand-create-gate`
- Candidate tip: `5144cfc4cde3bc792f4c7407040f2d536e7089ac`
- Base: `origin/main` at `3b1391cf9cfbe249ad6d25617d0c391982ee2890`
- Release criteria source: `docs/PROJECT_MANIFEST.md` is not present in this checkout, so this gate uses the deployer release criteria from the active role prompt.

## Scope

The final candidate is docs-only and touches one file:

```text
docs/getting-started/troubleshooting.md | 42 +++++++++++++++++++++++++++++++++
```

The branch adds a `dolt_mode_safe` troubleshooting section and corrects the
`gc status --json` guidance to reference the `.beads` object.

## Commits

| Commit | Purpose |
| --- | --- |
| `864146b3d` | Add the `dolt_mode_safe` preflight gate troubleshooting section. |
| `437b77b94` | Align the supervisor log snippet with the current preflight checker text. |
| `5144cfc4c` | Fix the `gc status --json` paths to use `jq .beads`. |

## Review And Acceptance Evidence

| Bead | Result | Evidence |
| --- | --- | --- |
| ga-52nu4i | PASS | Deploy bead references source review bead ga-vpvml7, which is closed with `Reviewer Verdict: PASS`; the notes verify the log snippet, commands, JSON fields, and `make check-docs`. |
| ga-1awyb6 | PASS | Deploy bead references source bead ga-kzp5e4, which is closed after the `.beads` JSON path correction; reviewer handoff mail reports PASS and the deploy bead notes record `make check-docs` PASS. |

Acceptance checks on the final branch:

- The section documents the supervisor log symptom for `native_store_unavailable gate=dolt_mode_safe`.
- The repair path uses existing commands: `gc doctor --fix`, `gc restart`, and `bd context --json`.
- The JSON guidance uses `gc status --json | jq .beads`, matching the nested `BeadsDiagnostic` object.
- The change is limited to public troubleshooting docs; no generated docs, code, schemas, or dashboard assets changed.

## Test Evidence

| Check | Result | Evidence |
| --- | --- | --- |
| `make check-docs` | PASS | `ok github.com/gastownhall/gascity/test/docsync 1.877s` |
| `make test-fast-parallel` | PASS | All 8 fast shards passed, including `unit-core`, `fsys-darwin-compile`, and `unit-cmd-gc-1-of-6` through `unit-cmd-gc-6-of-6`. |
| `go vet ./...` | PASS | Command exited 0 with no diagnostics. |

## Criteria

| # | Criterion | Result | Evidence |
| --- | --- | --- | --- |
| 1 | Review PASS present | PASS | ga-vpvml7 is closed with reviewer PASS; ga-kzp5e4 is closed and reviewer handoff mail reports PASS for the follow-up deploy bead. |
| 2 | Acceptance criteria met | PASS | The final docs section covers the symptom, root cause, diagnostics, repair path, and the `.beads` JSON field location. |
| 3 | Tests pass | PASS | `make check-docs`, `make test-fast-parallel`, and `go vet ./...` all pass on the final branch. |
| 4 | No high-severity review findings open | PASS | ga-vpvml7 listed only one LOW non-blocking finding, and ga-kzp5e4 fixes it. No HIGH findings are recorded in the deploy or review notes. |
| 5 | Final branch is clean | PASS | `git status --short --branch` reports detached HEAD with no worktree changes before gate file creation. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree $(git merge-base origin/main HEAD) origin/main HEAD` reports `merged` with no conflict markers or unmerged paths. |
| 7 | Single feature theme | PASS | All commits modify one troubleshooting section in `docs/getting-started/troubleshooting.md` for the `dolt_mode_safe` native-store fallback path. |
