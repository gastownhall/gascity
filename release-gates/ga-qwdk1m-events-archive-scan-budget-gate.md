# Release Gate: bounded archive scan for city events

- Deploy bead: `ga-qwdk1m`
- Source bead: `ga-hzfu61`
- Review bead: `ga-wy8j04`
- Reviewed source head: `1e9d1b76a90e9658652f2e6b60c732d4b11264fb`
- Deploy branch: `deploy/ga-qwdk1m-gate`
- Candidate head before this gate file: `9daa030ff6e378b25d70d471a1544a46bfe5fc3c`
- Base: `origin/main` at `1a9921943ec4bea15677f9c63ebe517ba47e547b`
- Release criteria source: `docs/PROJECT_MANIFEST.md` is not present in this
  checkout, so this checklist applies the active deployer release criteria and
  the repository gates in `TESTING.md`.

## Scope

This change bounds the no-lower-bound archive fallback for
`GET /v0/city/{cityName}/events`. It walks archives newest-first under the
configured `[events.scan_budget]` byte limit, preserves forward progress for
an oversized archive, returns `scan_truncated` with a resumable cursor when the
budget is exhausted, and surfaces truncation in `gc events` and the dashboard.
The OpenAPI, Go client, TypeScript client, and embedded dashboard artifacts are
regenerated with the feature.

The source bead split context cancellation across the full
`events.Provider.List` interface into follow-up `ga-49gfy6` after discovering
that it affects unrelated order, health, maintenance, conformance, and
provider implementations. That split is recorded in the source and review
beads and is not silently omitted from this release.

## Criterion 6: Branch diverges cleanly from main

PASS. Evaluated first.

- The prior candidate `aa229d6f4c56f8691c9c67b0e49bd945aec4e074`
  did not contain the current `origin/main`.
- The repository's `attempt_bounded_self_rebase` helper returned `0` and
  reported:
  - `BEFORE_SHA=aa229d6f4c56f8691c9c67b0e49bd945aec4e074`
  - `AFTER_SHA=9daa030ff6e378b25d70d471a1544a46bfe5fc3c`
- `git range-diff` maps all five pre-rebase commits one-for-one to the final
  range.
- `git merge-base --is-ancestor origin/main HEAD` returned `0` after the
  remaining gate checks.
- `git ls-remote origin refs/heads/deploy/ga-qwdk1m-gate` returned the exact
  candidate head `9daa030ff6e378b25d70d471a1544a46bfe5fc3c`.

## Release criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-wy8j04` is closed with reason `pass` and contains `REVIEW VERDICT: PASS`. Its four reviewed feature commits map to the rebased source/config/CLI/docs commits; differences against the reviewed range are confined to rebased embedded-dashboard hashes, followed by a fresh generated-artifact commit. |
| 2 | Acceptance criteria met | PASS | The focused tests verify budget exhaustion, forward progress, newest-first equivalence, cursor no-gap/no-duplicate behavior, in-flight rotation, configured/default/non-positive budgets, unchanged complete responses, and CLI/API surfacing. The dashboard reuses its existing incomplete-history banner for `scan_truncated`. Generated OpenAPI, Go, TypeScript, Zod, schema, and embedded dashboard artifacts are present and in sync. |
| 3 | Tests pass | PASS | The bounded helper's successful push ran `.githooks/pre-push`, which executes `make test-fast-parallel` for this Go-changing range. Explicit final-SHA checks also passed: `go build ./...`; `go vet ./...`; focused tests in `internal/events`, `internal/api`, `internal/config`, and `cmd/gc`; `make dashboard-check`; and `make dashboard-smoke`. |
| 4 | No high-severity review findings open | PASS | `ga-wy8j04` records no blocking issue and no HIGH or CRITICAL finding. Its final verdict is PASS. |
| 5 | Final branch is clean | PASS | `git status --porcelain`, `git diff --exit-code`, and `git diff --cached --exit-code` were empty before this gate file was written. The branch will be checked again after committing the gate. |
| 6 | Branch diverges cleanly from main | PASS | Evaluated first; see the dedicated evidence above. |
| 7 | Single feature theme | PASS | The 49-file range is one vertical feature: bounding city-event archive scans and carrying the resulting configuration and truncation signal through API, CLI, dashboard, tests, docs, and generated artifacts. No independently shippable feature is bundled. |

## Acceptance evidence

- `go test ./internal/events -run 'Test(ReadNewestBounded|FileRecorderListNewestBounded|WatchBackfillBoundedBuffer)' -count=1`: PASS.
- `go test ./internal/api -run 'TestEventListBoundedScan' -count=1`: PASS.
- `go test ./internal/config -run 'TestEventsScanBudgetConfig' -count=1`: PASS.
- `go test ./cmd/gc -run 'Test(FetchCityEventsScanTruncatedWarnsWithBudgetDiagnostic|OpenCityEventsProviderAppliesScanBudgetConfig)' -count=1`: PASS.
- `go build ./...`: PASS.
- `go vet ./...`: PASS.
- `make dashboard-check`: PASS.
- `make dashboard-smoke`: PASS; the Vite preview became reachable after its
  expected cold-start connection retries.

## Verdict

PASS.
