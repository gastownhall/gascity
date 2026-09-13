# Release gate: dependencies.id DEFAULT repair

- Deploy bead: ga-s6qc7f
- Source bug: ga-iwikr0
- PR: https://github.com/gastownhall/gascity/pull/3365
- Branch: fix/gc-sling-convoy-dep-id-default
- Gate commit under review: cb03298cd3091ab2b04bde2c3aca222230ba4ca1
- Gate run date: 2026-06-11

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Source review bead ga-z2f3h9 is closed with `REVIEW VERDICT: PASS`; deploy bead ga-s6qc7f records reviewer PASS. |
| 2 | Acceptance criteria met | PASS | Sling regression test verifies auto-convoy `tracks` dependency creation with no `MetadataErrors`; native Dolt open repairs missing `dependencies.id DEFAULT (uuid())`; `TestFindCity/not_found` now pins `HOME` to the test dir. |
| 3 | Tests pass | PASS | PR #3365 GitHub checks are green, including Check, Analyze, package/integration shards, cmd/gc process shards, dashboard, and worker gates. Local focused checks passed: `go test ./internal/sling -run TestFinalizeAutoConvoyTracksDepCreated -count=1`, `go test ./internal/beads -count=1`, `go test ./cmd/gc -run TestFindCity -count=1`, `go vet ./...`. Local `make test-fast-parallel` was attempted; failures were unrelated local baseline/environment failures from `/tmp/.gc` discovery and one supervisor temp cleanup flake. The cmd/gc failure set reproduced on `origin/main`, and `go test ./internal/supervisor -run TestRunSnapshot_Integration_RealDoltRoundTrip -count=1` passed on rerun. |
| 4 | No high-severity review findings open | PASS | Reviewer notes list no blocking findings and no high-severity findings. |
| 5 | Final branch is clean | PASS | `git status --short --branch` shows `fix/gc-sling-convoy-dep-id-default...origin/main [ahead 1]` before gate-file commit, with no uncommitted changes. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree $(git merge-base origin/main HEAD) origin/main HEAD` reports merged results and no conflict markers; GitHub reports PR #3365 mergeable. |
| 7 | Single feature theme | PASS | Commit set touches one feature theme: native beads/Dolt dependency ID repair plus the sling regression and a targeted test-environment cleanup. |

## Acceptance Criteria

| Source criterion | Result | Evidence |
|------------------|--------|----------|
| Reproducing `gc sling` no longer emits `Field 'id' doesn't have a default value` while linking convoy tracks dependencies. | PASS | `TestFinalizeAutoConvoyTracksDepCreated` exercises `DoSling` and requires no `MetadataErrors`. |
| Sling-created convoy tracking rows receive stable IDs or the write path supplies the required ID explicitly. | PASS | `repairDependenciesIDDefault` restores `DEFAULT (uuid())` on `dependencies.id`, preserving the existing beadslib insert contract. |
| Existing routing behavior is preserved: target hook receives the slung bead and metadata remains intact. | PASS | Regression is scoped to the existing `DoSling` path and verifies convoy dependency creation without changing routing metadata behavior. |
| A regression test covers the convoy dependency-linking path for sling-created beads. | PASS | `internal/sling.TestFinalizeAutoConvoyTracksDepCreated`. |
| If root cause is schema migration mismatch, record evidence and route correct follow-up before closing. | PASS | Root cause recorded in source review and PR body: Dolt strips the expression default from migration 0043 on some versions. Repair is in native Dolt store open. |

## Local Test Notes

Commands run on the release branch:

- `go test ./internal/sling -run TestFinalizeAutoConvoyTracksDepCreated -count=1`: PASS
- `go test ./internal/beads -count=1`: PASS
- `go test ./cmd/gc -run TestFindCity -count=1`: PASS
- `go vet ./...`: PASS
- `git diff --check origin/main...HEAD`: PASS
- `make test-fast-parallel`: local baseline red; not a PR regression.

Baseline comparison:

- The targeted cmd/gc failures from the local fast run also fail on `origin/main` with the same `/tmp/.gc`/`/tmp/city.toml` discovery behavior.
- The single `internal/supervisor` cleanup failure passed on focused rerun.
