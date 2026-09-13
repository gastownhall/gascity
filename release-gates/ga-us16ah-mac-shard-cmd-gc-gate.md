# Release Gate: Mac cmd/gc Sharding

Gate evaluated: 2026-06-19T17:43:14Z

Bead: ga-us16ah
Source review bead: ga-zeeq0j
PR: https://github.com/gastownhall/gascity/pull/3598
Branch: fix/mac-shard-cmd-gc-ga-jfnpr8
Reviewed head: ca87dd1c81f07effe89dbd85a94d23003364e2ff
Current origin/main: b9288b2abddc1c5368713692f740cd2142c203af
Merge base: fea30636527f24ea515e28d831b2751630ac5448

## Candidate Scope

Three-dot diff from `origin/main...HEAD`:

- `.github/workflows/mac-regression.yml`
- `Makefile`

Commit set:

- `ca87dd1c8 fix(ci-mac): shard cmd/gc unit tests on Mac (ga-jfnpr8)`

The candidate is a single CI feature theme: Mac unit/coverage workflow split
and 12-way `cmd/gc` process sharding to mirror the Linux command-gc process
coverage pattern.

## Gate Checklist

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-zeeq0j` is closed with close reason `pass`. Notes include `REVIEWER VERDICT: PASS`, reviewed by `gascity/reviewer`, commit `ca87dd1c81f07effe89dbd85a94d23003364e2ff`, and `Blockers: None`. |
| 2 | Acceptance criteria met | PASS | `Makefile` adds `test-mac` and `test-cover-mac`; `test-mac` excludes `cmd/gc` via `MAC_UNIT_PKGS`; `.github/workflows/mac-regression.yml` runs `make test-mac`, adds the 12-way `mac-cmd-gc-process` matrix, runs `make test-cover-mac`, and gates the Mac regression summary on `CMD_GC`. |
| 3 | Tests pass | PASS | Local: `make build`, `go vet ./...`, `make test-fast-parallel`, `make test-mac`, and `make test-cmd-gc-process-shard CMD_GC_PROCESS_SHARD=1 CMD_GC_PROCESS_TOTAL=12` all exited 0. GitHub checks for PR #3598 are green, including all 12 Mac cmd/gc process shards, Mac acceptance, Mac quality, Mac test-cover, and Mac regression summary. |
| 4 | No high-severity review findings open | PASS | Review bead `ga-zeeq0j` records no blockers. `gh pr view 3598 --json comments,reviews,latestReviews` returned no comments and no reviews, so there is no unresolved HIGH finding or external contributor engagement. |
| 5 | Final branch is clean | PASS | `git status --short` returned no output before writing this gate artifact. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` exited 0 and printed `clean`; GitHub reports `mergeStateStatus=CLEAN`. |
| 7 | Single feature theme | PASS | Three-dot diff is limited to `.github/workflows/mac-regression.yml` and `Makefile`; the one commit only changes Mac CI sharding/test target wiring. |

## Local Command Evidence

```text
make build
PASS: exited 0

go vet ./...
PASS: exited 0

make test-fast-parallel
PASS: All fast jobs passed

make test-mac
PASS: observable go test: PASS log=/tmp/gascity-test-mac.jsonl.YOyvMf

make test-cmd-gc-process-shard CMD_GC_PROCESS_SHARD=1 CMD_GC_PROCESS_TOTAL=12
PASS: exited 0
```

## Decision

PASS. Open/update the PR for `fix/mac-shard-cmd-gc-ga-jfnpr8`, push this gate
artifact to the PR branch, and route the merge request to mayor/mpr. Deployer
does not merge.
