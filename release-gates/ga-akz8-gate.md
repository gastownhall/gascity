# Release Gate — ga-akz8 (gc reload acceptance off main reconciler select)

**Bead:** ga-8nbr (originating) via review bead ga-akz8
**Branch:** `release/ga-akz8`
**Source commit:** 9ee8a10e (builder branch `gc-builder-1-01561d4fb9ea`) → cherry-picked onto `origin/main` as c0e4d13b (`issues.jsonl` stripped per deployer EXCLUDES discipline — does not exist on main).
**Evaluator:** gascity/deployer on 2026-04-23

## Gate criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Review PASS present | PASS | ga-akz8 notes: `review_verdict: pass` from gascity/reviewer at commit 9ee8a10e. Second-pass (gascity/reviewer-1) also PASS per mail gm-wisp-ceq. Single-pass required while gemini second-pass is disabled; two independent PASSes here exceed the bar. |
| 2 | Acceptance criteria met | PASS | See matrix below. |
| 3 | Tests pass | PASS | Reviewer's test glob verified on cherry-picked branch — see evidence below. |
| 4 | No high-severity review findings open | PASS | Two `severity: info` findings on neighboring concerns (test improvement and pre-existing race in convergence_tick.go, out of scope per originating spec). Zero HIGH findings. |
| 5 | Final branch is clean | PASS | `git status` clean on tracked paths. Untracked `.gitkeep` is pre-existing, not introduced by this change. |
| 6 | Branch diverges cleanly from main | PASS | Fresh cut from `origin/main`. Single commit. Only structural conflict was `issues.jsonl` (bd work-tracking artifact — not on main), stripped via the documented EXCLUDES pattern. |

## Acceptance criteria matrix (ga-8nbr scope)

| Criterion | Met | Evidence |
|-----------|-----|----------|
| `reloadMu sync.Mutex` added to `cityRuntime` adjacent to `activeReload` | YES | `cmd/gc/city_runtime.go:80`. |
| `handleReloadRequest` busy-check + activeReload store under lock, channel sends outside lock | YES | `cmd/gc/city_runtime.go:787-814`. |
| `failActiveReload` swaps activeReload under lock, replies outside | YES | `cmd/gc/city_runtime.go:816-827`. |
| All `tick()` activeReload access sites take `reloadMu` | YES | Trace-detail read (~604-609), reload-source read (~675-686), end-of-tick clear (~776-781), deferred panic-recovery clear (~673-676). |
| `run()` spawns dedicated accept goroutine with `safeTick(handleReloadRequest, "reload-accept")` | YES | `cmd/gc/city_runtime.go:499-512`. `reloadReqCh` case removed from main select. |
| Deferred cleanup waits for `acceptDone` then calls `failActiveReload` | YES | `cmd/gc/city_runtime.go:513-516` — avoids shutdown race between accept goroutine and the prior inline `ctx.Done()` path. |
| Regression test: reload accept unblocks during slow reconciler tick | YES | `TestCityRuntimeReloadAcceptNotBlockedBySlowTick` added in `cmd/gc/city_runtime_test.go`. |

## Test evidence

```
$ go test -race -count=5 -run "TestCityRuntimeReloadAcceptNotBlockedBySlowTick" ./cmd/gc/...
ok  	github.com/gastownhall/gascity/cmd/gc	1.306s

$ go test -race -count=1 -run "TestCityRuntimeFailActiveReloadRepliesAndClears|TestCityRuntimeHandleReloadRequestInitializesConfigDirty|TestCityRuntimeManualReloadReplyWaitsForTickCompletion|TestCityRuntimeManualReloadPanicAfterReloadKeepsReloadReplyAndClears|TestCityRuntimeWatchReloadPanicRestoresDirty|TestHandleReloadSocketCmd" ./cmd/gc/...
ok  	github.com/gastownhall/gascity/cmd/gc	2.683s

$ go vet ./cmd/gc/...
(clean)

$ go build ./...
(clean)
```

## Pre-existing failures — NOT caused by this change

The reviewer documented four pre-existing reload tests failing at baseline (dolt metadata table + pack-discovery, unrelated to this scope). The deployer independently reproduced on `origin/main` (without the patch) via stash-and-checkout:

- `TestCityRuntimeReloadProviderSwapPreservesDrainTracker` — FAIL on main (3.97s) AND on release/ga-akz8 (3.32s). Identical failure: `lastProviderName = "fake", want fail`.
- `TestCityRuntimeReloadAllowsRegistryAliasDifferentFromWorkspaceName` — FAIL on main AND release/ga-akz8. Identical stderr: `exec beads init: gc dolt-state ensure-project-id: read database _project_id: Error 1146 (HY000): table not found: metadata`.
- `TestCityRuntimeReloadKeepsRegisteredAliasForEffectiveIdentity` — FAIL on main AND release/ga-akz8. Identical: `configRev did not change after accepted reload`.
- `TestCityRuntimeReloadRestartsConfigWatcherWithNewPackTargets` — FAIL on main AND release/ga-akz8. Identical: pack-discovery does not populate `packs/extra` path.

These are baseline infrastructure issues (dolt metadata ensure path + pack discovery), not regressions from this change. They warrant separate beads; they do not block this release.

Two pre-existing data races in neighboring tests (`debounceDelay`; `convergence_tick.go`) were also confirmed by the reviewer on main at `adaa6f47` and are out-of-scope for this bead (explicitly: `convergenceReqCh` starvation is tracked separately).

## Security review

From ga-akz8 notes (gascity/reviewer):

- No new I/O, no new parsing surface; pure concurrency reordering.
- Mutex critical sections short; locks released before channel sends.
- `activeReload`-vs-`configDirty` ordering preserves the invariant that a tick observing `configDirty=true` will either see the accepted request on its own lock acquisition or pick it up on a subsequent tick (reconciler is the sole consumer).
- No changes to controller, session_lifecycle_parallel, cmd_reload_test, or convergence paths.

## Info-level findings (non-blocking)

1. **test-improvement** (`cmd/gc/city_runtime_test.go:3022-3030`) — reviewer noted the test's slow-tick goroutine is a small improvement over the bead spec (captures `tickBusyDelay` as a local and guards against `t.Cleanup` races). Deliberate, no change needed.
2. **pre-existing-race** (`cmd/gc/convergence_tick.go:39`; `convergence_store.go:30/50`) — `TestCityRuntimeRun_RetriesConvergenceStartupUntilIndexPopulated` hits a data race on `convergenceStoreAdapter`. Reproduced at `origin/main` (adaa6f47) — not a regression. Warrants a separate bead; out of scope here.

## Verdict: PASS

Cleared for PR.
