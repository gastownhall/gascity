# Release Gate — ga-u4mw.1 (recover missing auto-convoy on pre-routed beads)

**Bead:** ga-u4mw (originating) via review bead ga-u4mw.1
**Branch:** `release/ga-u4mw-1` — cherry-pick of 225d34b3 + b33a8336 onto `origin/main` (issues.jsonl stripped per EXCLUDES discipline)
**Evaluator:** gascity/deployer on 2026-04-23
**Verdict:** **PASS**

## Gate criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Review PASS present | PASS | ga-u4mw.1 passed at commit 225d34b3 (mail `gm-wisp-qdsv` from gascity/reviewer). Single-pass sufficient while gemini second-pass is disabled. |
| 2 | Acceptance criteria met | PASS | All eight done-when checkboxes in ga-u4mw satisfied; three new tests (`TestCheckBeadStateRoutedWithoutConvoyIsNotIdempotent`, `TestCheckBeadStateRoutedWithClosedConvoyIsNotIdempotent`, `TestDoSlingRecoversMissingConvoyOnPreRoutedBead`) all PASS on the release branch. |
| 3 | Tests pass | PASS | `TestDryRunIdempotentBead` — the regression that blocked the previous gate — now PASSes after cherry-picking builder follow-up `b33a8336`. `go vet ./...` clean, `go build ./...` clean. Two `TestDoConvoyAutoclose*` failures reproduce identically on `origin/main@adaa6f47` (JSON unmarshal errors in `provider_store_resolution_test.go`) — pre-existing, not introduced by this change. |
| 4 | No high-severity review findings open | PASS | Zero HIGH findings; reviewer PASS with no blocking items. |
| 5 | Final branch is clean | PASS | `git status` shows tracked tree clean; only `release-gates/` (this file) and a `.gitkeep` untracked (pre-existing workspace scaffold). |
| 6 | Branch diverges cleanly from main | PASS | Two commits ahead of `origin/main` with no merge conflicts. |

## Cherry-pick log

| Source SHA | Branch SHA | Summary |
|------------|------------|---------|
| 225d34b3 | 3d1e61d3 | fix(sling): recover missing auto-convoy on pre-routed beads (ga-u4mw) |
| b33a8336 | 07f14576 | test(sling): seed live convoy parent in TestDryRunIdempotentBead (ga-u4mw.1) |

`EXCLUDES`: `issues.jsonl` (bd sync artifact not present on `origin/main`).

## Regression fix verification

Previously-failing `TestDryRunIdempotentBead` now PASSes:

```
$ go test ./cmd/gc/... -run '^TestDryRunIdempotentBead$' -v -count=1
=== RUN   TestDryRunIdempotentBead
--- PASS: TestDryRunIdempotentBead (0.00s)
PASS
```

The builder's follow-up fix (`b33a8336`) seeds a live convoy parent in the
fixture, matching the pattern already applied to the other idempotent tests
in the original commit.

## New tests — verified PASSing on the release branch

```
$ go test ./internal/sling/... -run 'TestCheckBeadStateRoutedWithoutConvoyIsNotIdempotent|TestCheckBeadStateRoutedWithClosedConvoyIsNotIdempotent' -v -count=1
--- PASS: TestCheckBeadStateRoutedWithoutConvoyIsNotIdempotent (0.00s)
--- PASS: TestCheckBeadStateRoutedWithClosedConvoyIsNotIdempotent (0.00s)
PASS

$ go test ./cmd/gc/... -run 'TestDoSlingRecoversMissingConvoyOnPreRoutedBead' -v -count=1
--- PASS: TestDoSlingRecoversMissingConvoyOnPreRoutedBead (0.00s)
PASS
```

## Pre-existing failures (NOT regressions)

Confirmed by re-running on `origin/main@adaa6f47` with an identical baseline clone:

- `TestDoConvoyAutocloseUsesProviderAwareStore` — `bd show: parsing JSON: json: cannot unmarshal object into Go value of type []beads.bdIssue`
- `TestDoConvoyAutocloseUsesBeadsDirStoreRoot` — same unmarshal error

Both fail at the parent commit and at HEAD. Reviewer independently flagged them as unrelated in the PASS mail.
