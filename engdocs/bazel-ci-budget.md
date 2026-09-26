# Bazel CI budget and the optimization loop

## Goal: `bazel test //...` <= 60s on T1 (steady-state CI)

## Measurement tiers

Bazel caching makes single numbers lie. Every number is tagged:

| tier | definition | what it captures |
|---|---|---|
| **T1 steady** | warm remote CAS/AC **and** warm runner disk cache | the PR experience; the goal metric |
| **T2 cold-client** | fresh runner (empty disk cache), warm remote CAS | input re-hashing cost; CI without runner caching |
| **T3 cold-farm** | after worker recycle / CAS eviction | worst case; track, never gate |

Every CI run prints `[T2] elapsed / critical-path / gap` via
`tools/bazel/critpath.py` and checks it against a budget (currently
360s at T2; tighten as the disk cache proves itself, then move the
gate to T1 / 60s).

## The decision rule (formalized)

```
gap = elapsed - critical_path          (per run, from --profile)

gap < 15% of elapsed   -> scheduling is fine; attack the longest
                          critical-path component:
                            test action  -> shard further / optimize its
                                           slowest individual tests
                            input upload -> warm CAS / worker fast stores
                            build action -> slim inputs, split target
gap >= 15% of elapsed  -> capacity problem; attack the farm:
                            autoscaler divisor, client --jobs,
                            worker fast-store hit rate, runner disk cache
```

## Current numbers (2026-09-26)

| run | tier | elapsed | critical path | gap | verdict |
|---|---|---|---|---|---|
| local, warm | - | 248s | 218s | 30s (12%) | sharding-bound |
| remote, cold-farm | T3 | 723s | 711s | 12s | 634s input upload after worker recycle |
| remote, warm | T1 | 92s | 84s | 8s | one gc shard at 82s |
| CI, --jobs=2 (default) | T2 | 841s | 87s | 754s (90%) | 2vcpu runner capped REMOTE actions at 2 in flight; one flag (--jobs=64) fixed it |
| CI, --jobs=64 | T2 | 167s | 156s | 11s (6%) | budget passes; gc shard is the path |

## Path to 60s (T1)

1. Runner disk+repo cache: kills the T2 re-hash, moves CI runs toward
   T1 behavior on cache hit.
2. gc_test shard 82s -> ~50s: shard 12->16 if durations redistribute;
   else optimize the shard's slowest individual tests.
3. Merge the CI build+test steps into one `bazel test` invocation
   (build is subsumed; saves a full graph walk).
4. Tighten the budget: 360s -> 180s -> 120s -> 60s, moving the gate
   from T2 to T1 as the caches hold.
