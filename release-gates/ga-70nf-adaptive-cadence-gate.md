# Release gate — adaptive reconciler cadence + telemetry (ga-70nf, AD-03 part 3)

**Verdict:** PASS (with maintainer-side fixup)

- Bead: `ga-70nf` (AD-03 part 3, deferred from ga-zor1n2 stagger gate)
- Branch: `quad341:builder/ga-70nf-1`
- HEAD: `27965cfe` after maintainer fixup (was `5df84922` at original gate review)
- PR: [gastownhall/gascity#1820](https://github.com/gastownhall/gascity/pull/1820)
- Diff (post-fixup): 2 files, +579 / -1 production + +N / 0 maintainer fixup; 14 unit tests + 1 end-to-end

## Criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Latency-driven promotion to MEDIUM cadence | PASS | `recomputeCadenceLocked` flips `latencyDriverActive` when nearest-rank P95 exceeds `cacheLatencyHighWaterMark`; verified by `TestAdaptiveCadencePromotesOnHighLatency`. |
| 2 | Hysteresis demotion via window-drain | PASS | Window must fully drain to demote (N=`cacheLatencyWindowSize` consecutive low samples); verified by `TestAdaptiveCadenceDemotesAfterHysteresisWindow`. The spike-reset property — one slow scan during the drain re-arms the driver — is verified by `TestAdaptiveCadenceSpikeResetsHysteresisCounter`. |
| 3 | Composes with bead-count cadence | PASS | `effectiveCadence` returns the slower of the two drivers; verified by `TestAdaptiveCadenceCompositionBeadCountAlone` and `TestAdaptiveCadenceCompositionBoth`. |
| 4 | LARGE preserved via bead count only | PASS | `beadCountCadence(>=5000)` returns LARGE regardless of latency state; verified by `TestAdaptiveCadencePreservesLargeInterval`. |
| 5 | Single transition log per ↕ change | PASS | `recomputeCadenceLocked` only logs on transition edges; verified by `TestAdaptiveCadenceLogsOnceOnPromote` and `TestAdaptiveCadenceLogsOnceOnDemote`. |
| 6 | End-to-end: real reconciler feeds the latency window | PASS | `TestRunReconciliationFeedsLatencyWindow` exercises the full path. |
| 7 | Race-free | PASS | `go test -race ./internal/beads/` — clean. |
| 8 | Nearest-rank P95 generalizes beyond N=10 | PASS (after fixup) | Reviewer flagged that the original `sorted[len-1]` was P100 not P95; correct only by coincidence at N=10. Maintainer fixup replaces with `int(math.Ceil(0.95*N))-1`, which equals `len-1` for N=10 (preserves current behavior bit-for-bit) but stays P95 if `cacheLatencyWindowSize` is raised. |

## Validation (post-fixup)

- `go build ./internal/beads/...` — clean
- `go test ./internal/beads/ -run 'TestLatency|TestCadence|TestRecompute' -count=1` — PASS

## Open items deferred to follow-up

- **`sort.Slice` allocation per `recomputeCadenceLocked` call** is unnecessary at N=10; could be replaced with a streaming max or pre-sorted insert. Not a correctness issue; bounded allocation cost. File as `ga-70nf-followup` if follow-up is desired.

## Notes

- Maintainer fixup commit: nearest-rank P95 generalization. Behavior at the current window size is unchanged; the fix is forward-looking.
- This gate file lands as part of the maintainer fixup commit so the PR includes the gate doc per the convention established by `ga-zor1n2`.
