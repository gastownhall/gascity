# Release Gate: event-list `after_seq` lower-bound filter

- Date: 2026-07-27
- Bead: `ga-g5ihlp`
- Source review bead: `ga-dicuca`
- Original reviewed commit: `0c4048fd7b6ad182297b5a633848f736c569ba70`
- Builder-remediated commit: `af72d0bd0fa6dba1a22ba5eac9e2cec8dc548ee1`
- Final deploy commit before this gate file: `3ddee573f2c9d7102164966a1641554b9b976079`
- Deploy branch: `deploy/ga-g5ihlp-gate`
- Base checked: `origin/main` at `74d62e08cfd3424df468dfd8f9396bbe5ea7bb93`

## Release criteria source

`docs/PROJECT_MANIFEST.md` is not present in this repository. This gate applies
the active deployer release criteria, the API control-plane invariants, the
Gas City documentation verification guidance, and `TESTING.md`.

## Gate criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Review bead `ga-dicuca` is closed with a PASS close reason and a `Verdict: PASS` note. `git range-diff` reports both feature commits as patch-identical after rebasing (`fdb3c64e5 = 02651b906`, `0c4048fd7 = 3ddee573f`), and the eight feature files have no content diff between the reviewed and final pre-gate tips. |
| 2 | Acceptance criteria met | PASS | `GET /v0/city/{cityName}/events` exposes documented `after_seq`; `EventListInput.Resolve` rejects malformed or negative values through Huma's typed 422 validation; the handler maps the validated value to `events.Filter.AfterSeq`; `TestEventListAfterSeqSkipsArchive` proves archives at or below the bound are not opened; `TestEventListOmittedAfterSeqIncludesArchive` proves omitted-parameter behavior is unchanged; OpenAPI and the generated Go client include the parameter. |
| 3 | Tests pass | PASS | The bounded self-rebase's guarded push ran `.githooks/pre-push` and returned 0 only after `make test-fast-parallel` passed on `3ddee573f`. Additional checks passed: focused `internal/api` acceptance/regression tests, `TestGeneratedClientInSync`, `go build ./...`, serial `go vet ./...`, `make dashboard-check`, and a local Vite preview returning HTTP 200 with the dashboard root. |
| 4 | No high-severity review findings open | PASS | `ga-dicuca` contains no HIGH or CRITICAL finding. Its only noted discrepancy is non-blocking: Huma resolver validation correctly returns 422 rather than the source bead's literal 400 wording. |
| 5 | Final branch is clean | PASS | `git status --porcelain` was empty before this gate file was added. `core.hooksPath` is `.githooks`; the gate commit is followed by another clean-status check. |
| 6 | Branch diverges cleanly from main | PASS | Evaluated first. The required bounded helper rebased and pushed `af72d0bd0fa6dba1a22ba5eac9e2cec8dc548ee1 -> 3ddee573f2c9d7102164966a1641554b9b976079` with return code 0. Main then advanced by the zsh-collision fix; `git merge-tree --write-tree HEAD origin/main` still returned tree `ef9af0a7ae87341b39ff438ea2f988328adf6d8b` with exit 0 and no conflicts. |
| 7 | Single feature theme | PASS | The commit set changes one subsystem and one user-visible behavior: the typed lower-bound filter for the city event-list endpoint, with its direct tests and generated API artifacts. |

## Changed surface

| Path | Purpose |
|---|---|
| `internal/api/huma_types_events.go` | Declares and validates the `after_seq` query parameter. |
| `internal/api/huma_handlers_events.go` | Passes the resolved bound into the event filter. |
| `internal/api/handler_events_test.go` | Covers invalid, negative, archive-skip, and omitted-parameter behavior. |
| `internal/api/pagination_dialect_guard_test.go` | Records the reviewed lower-bound-filter exception alongside the existing cursor pagination contract. |
| `internal/api/openapi.json` | Generated OpenAPI contract. |
| `docs/reference/schema/openapi.json` | Generated public OpenAPI contract. |
| `docs/reference/schema/openapi.txt` | Generated text projection. |
| `internal/api/genclient/client_gen.go` | Generated typed Go client. |

## Commands and evidence

```text
git range-diff <reviewed-base>..0c4048fd7 <final-base>..3ddee573f
git diff 0c4048fd7 3ddee573f -- <eight feature paths>
bash -lc 'source scripts/rebase-resolve-lib.sh; attempt_bounded_self_rebase deploy/ga-g5ihlp-gate main'
git merge-tree --write-tree HEAD origin/main
go test ./internal/api/... -count=1 -run '<focused event-list, pagination, and generated-client tests>' -v
go build ./...
go vet ./...
make dashboard-check
npm run --workspace gas-city-dashboard-frontend preview -- --host 127.0.0.1 --port 4178
curl --fail http://127.0.0.1:4178/
```

An initial `go vet ./...` invocation overlapped `npm ci` from
`make dashboard-check`; npm removed a transient nested Go package under
`node_modules` while vet was scanning it. After the dashboard command completed,
the required serial `go vet ./...` run passed with no diagnostics.

## Decision

PASS. The branch is ready for merge-authority review.
