# Release Gate: push-ownership-guard deploy-gate branch resolution fix

- Deploy bead: `ga-anwmtr`
- Source bead: `ga-wwswme`
- Review bead: `ga-uq9095`
- Reviewed commit: `b7e762eaf1eeaaca876d1c14dd63c45777d442ec`
- Deploy branch: `deploy/ga-anwmtr-gate`
- Evaluated: 2026-07-27
- Gate source: deployer prompt release-gate table (matched against sibling
  gates `release-gates/ga-hzy30q-push-ownership-guard-gate.md` and
  `release-gates/ga-evd1s7-pre-push-ownership-guard-gate.md`, same script
  family). `docs/PROJECT_MANIFEST.md` was not present in this checkout.

## Summary

PASS. Single-theme shell guard fix: `_pog_resolve_bead_id` in
`scripts/push-ownership-guard.sh` prefers the live in-progress assignee over
the closed gated bead when resolving a `deploy/*-gate` branch name, instead
of trusting the branch-embedded bead ID (which is routinely already closed
by push time -- that's the point of a deploy gate). Fixes a real cited
incident (PR #4731 incorrectly blocked). Downgrades the resulting
disagreement log line from WARNING to NOTE.

An earlier attempt at this same gate (this bead, same reviewed commit) FAILED
criterion 3 on `make test-fast-parallel`'s `unit-core` shard
(`TestCachingStoreHandlesCachedListUsesActiveSnapshotAfterPrimeActive`,
"cached active List did not return promptly from PrimeActive snapshot").
That failure is retained here per TESTING.md rather than silently discarded:
first-attempt gate record committed locally as `d70126efb` on a since-
discarded `deploy/ga-anwmtr-gate` (never pushed); full first-run log at
`/var/tmp/gc-local-tests.ZefBTS/unit-core.log` (that attempt's worktree, not
this one). This retry cuts a fresh isolated worktree/branch off the same
pinned reviewed commit and reruns the full gate from scratch, including a
full (not just focused) `make test-fast-parallel` -- all 9 shards, including
`unit-core`, pass clean this time. The diff under test (a bash script) has
no code path into the Go caching-store snapshot logic the failing test
exercises. Fleet memory `city-runtime-convergence-startup-flaky-under-shard-load`
independently documents a recurring class of single-shard, unrelated-diff
timing flakes under full `make test-fast-parallel` contention on this shared
host (root-caused to `nice`/`ionice` deprioritization + uncapped GOMAXPROCS
oversubscription across 6 concurrent shard processes, not a code defect).
This is a single occurrence of a different test in that same general
failure class, not (yet) independently confirmed recurring -- noted for
visibility, not treated as fully closed.

## Criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 6 | Branch diverges cleanly from main | PASS | `git fetch origin main`; main had drifted 5 commits past this branch's merge-base (`af42a9424`) since cut, current tip `431711fe0`. `git merge-tree --write-tree origin/main b7e762eaf1eeaaca876d1c14dd63c45777d442ec` returned tree `fd7b636bbc4a88173ef0adf70992fb57aa7d75d0` (clean, no conflict markers); `git diff --check origin/main...b7e762eaf1eeaaca876d1c14dd63c45777d442ec` produced no output. |
| 1 | Review PASS present | PASS | Review bead `ga-uq9095`, close reason `pass`. Notes contain `REVIEW VERDICT: PASS` and `tdd_green: b7e762eaf... — 28/28 tests pass (27 pre-existing + new deploy-gate regression test); go build/vet/gofmt/shellcheck all clean`, matching this gate's own independent rerun. |
| 2 | Acceptance criteria met | PASS | Commit set is the expected red/green pair: `acd2e16b3` (test: red -- adds the failing deploy-gate-branch regression test) and `b7e762eaf1` (fix: green). Diff is limited to `scripts/push-ownership-guard.sh` (17 lines); no `cmd/gc` files touched. Guard suite includes the new regression `resolve/deploy-gate-branch-prefers-live-assignee` (live assignee `ga-mit0gh` used instead of closed gated bead `ga-g5ihlp`). |
| 3 | Tests pass | PASS | `shellcheck scripts/push-ownership-guard.sh` clean. `go build ./...` clean. `go vet ./...` clean. `bash scripts/test-push-ownership-guard.sh` passed `28/28`, matching the reviewer's own evidence exactly. `make test-fast-parallel` passed all 9 fast jobs (fresh full run, not a focused single-test rerun -- see Summary for why a full rerun mattered here). |
| 4 | No high-severity review findings open | PASS | `bd list --status open --limit 0 \| grep -iE 'ga-anwmtr\|ga-uq9095\|ga-wwswme'` returned only routine sling-tracking beads (`ga-2igi0a`, `ga-4td6gw`, `ga-lbnewn`, `ga-sc25lw`, all P2); no open HIGH/request-changes finding. |
| 5 | Final branch is clean | PASS | Before adding this gate file, `git status --short --branch` on `deploy/ga-anwmtr-gate` returned only the branch header (worktree cut directly from the pinned reviewed commit, nothing else applied). This gate file is committed as the final branch tip before push. |
| 7 | Single feature theme | PASS | The commit set touches one subsystem: `scripts/push-ownership-guard.sh` plus its test harness. Removing this fix would only affect deploy-gate branch-to-bead-ID resolution in the push ownership guard. |

## Commands

```bash
git fetch origin main
git merge-tree --write-tree origin/main b7e762eaf1eeaaca876d1c14dd63c45777d442ec
git diff --check origin/main...b7e762eaf1eeaaca876d1c14dd63c45777d442ec
git log --oneline -8 b7e762eaf1eeaaca876d1c14dd63c45777d442ec
shellcheck scripts/push-ownership-guard.sh
go build ./...
go vet ./...
bash scripts/test-push-ownership-guard.sh
make test-fast-parallel
bd list --status open --limit 0 | grep -iE 'ga-anwmtr|ga-uq9095|ga-wwswme'
```
