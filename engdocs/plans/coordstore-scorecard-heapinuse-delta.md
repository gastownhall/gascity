# Coordstore Scorecard HeapInuse Delta Plan

Source bead: `ga-si3tq`
Design source: `source:actual-designer`
Target agent: `gascity/builder`
Priority: P2

## Goal

Turn the completed scorecard design into builder-ready work packages so the
coordstore benchmark measures workload-induced heap growth instead of static
backend baseline size. The primary memory gate becomes `HeapInuseDelta <= 256
MiB`; absolute peak remains visible as informational output.

## Work Packages

1. `ga-si3tq.1` - Builder: score memory by `HeapInuseDelta`
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Acceptance: `MemReport` includes `HeapInuseDelta`; `memSampler.stop()`
     computes it from peak minus baseline; `HeapInuseDeltaTarget` is added;
     `Score()` uses delta for `MemPass`; `PrintTable()` shows a primary delta
     row and an informational peak row; focused scorecard tests cover pass,
     fail, unsampled memory, and baseline-above-peak behavior.

2. `ga-si3tq.2` - Builder: report HQStore live object counts
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Acceptance: HQStore coordstore adapter `Stats(context.Context)` returns
     `live_objects` for main plus wisp records when a store exists; nil store
     behavior remains safe; implementation does not expand `StoreAdapter`;
     tests verify main and wisp records are counted and lifecycle changes are
     reflected.

3. `ga-si3tq.3` - Builder: emit HeapInuse delta and live object telemetry
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-si3tq.1`, `ga-si3tq.2`
   - Acceptance: coordstore time-series JSONL includes
     `heap_inuse_delta_bytes` from `MemReport.HeapInuseDelta` and
     `live_object_count` from `StoreAdapter.Stats()["live_objects"]` where
     reported; existing `store_size_bytes` remains compatible; tests cover a
     reporting backend and a backend that does not report live object stats.

## Dependency Graph

- `ga-si3tq.1` and `ga-si3tq.2` can proceed independently.
- `ga-si3tq.1` and `ga-si3tq.2` both block `ga-si3tq.3`.

## Guardrails

- Do not change `HeapInusePeakTarget`; keep it as informational output.
- Do not add a new `StoreAdapter` method for live objects.
- Keep implementation scoped to coordstore benchmark scorecard, memory
  sampling, adapter stats, and recorder telemetry.
- If recorder surfaces are unexpectedly absent on the target branch, record the
  blocker on `ga-si3tq.3` instead of folding recorder construction into the
  scorecard work.
- Preserve the zero-hardcoded-role invariant.
