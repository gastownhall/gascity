# ValidateSyntheticRepo Fast/Full Split

Owner: `gascity/pm`
Created: 2026-06-14
Root bead: `ga-7ijie3.1`
Parent decision: `ga-7ijie3`
Designer handoff: `gm-wisp-raom1rs`

## Goal

Recover the #3344 review-formulas CI regression by moving bundled pack
resolution onto a marker-only fast validation path while keeping full cache
integrity checks on install, doctor, and post-materialization paths.

The accepted design introduces `ValidateSyntheticRepoFast(dir, commit)` next to
`ValidateSyntheticRepo`. The fast variant proves the cache belongs to the
current binary by checking the marker schema, repository, commit, and content
hash. The full variant remains responsible for walking the cache file set and
comparing materialized file contents.

## Context

The parent architecture decision selected fix-forward over revert or timeout
increases. The regression comes from running full synthetic cache validation on
fresh `gc` subprocesses during bundled pack resolution. Each subprocess resolves
the builtin packs and pays repeated marker, hash, walk, and file-read costs.

The designer handoff completed the API design, call-site map, test list, and
performance target. No work routes back to design.

## Work Packages

| Bead | Target | Purpose |
| --- | --- | --- |
| `ga-7ijie3.1.1` | `gascity/builder` | Add the fast/full validation API boundary and focused builtinpacks tests. |
| `ga-7ijie3.1.2` | `gascity/builder` | Route only bundled pack resolution hot paths to fast validation. |
| `ga-7ijie3.1.3` | `gascity/builder` | Prove the review-formulas performance regression is recovered. |

All three child beads carry `ready-to-build`, `source:actual-pm`, and
`gc.routed_to=gascity/builder`.

## Acceptance Summary

`ga-7ijie3.1.1` is complete when `ValidateSyntheticRepoFast` exists in
`internal/builtinpacks/registry.go`, validates only root and marker invariants,
uses `syntheticContentHashOnce`, avoids file-set walking and byte-for-byte file
checks, and the full `ValidateSyntheticRepo` still performs full validation
while also using the cached hash helper. It must add the seven fast-path tests
from the design and document which tamper checks remain full-validator-only.

`ga-7ijie3.1.2` is complete when the two resolution hot-path call sites in
`internal/config/pack_include.go` use `ValidateSyntheticRepoFast`, while
post-materialization, install, and doctor paths keep `ValidateSyntheticRepo`.
Fast-validation failure must preserve the existing fallback to materialization.

`ga-7ijie3.1.3` is complete when builder records concrete timing evidence that
the investigator reproduction test,
`TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash`, returns to
`<=90s` on the local test host, or records the exact timing and blocker if the
environment prevents that gate. This bead must not include timeout increases,
sleep-budget increases, or a #3344 revert.

## Dependency Graph

`ga-7ijie3.1.1` -> `ga-7ijie3.1.2` -> `ga-7ijie3.1.3`

The API and tests must land before call-site migration. Performance proof waits
for both the API and hot-path migration so it measures the intended fix.

## Out Of Scope

- Reverting #3344.
- Raising CI or formula timeout budgets to mask the regression.
- Adding process-external stamp files or a new cache invalidation design.
- Moving install, doctor, or post-materialization paths to the fast validator.
- Routing any child work back to design.

## Risks

The main correctness risk is accidentally replacing full validation on install
or doctor paths. The builder beads call this out explicitly, and reviewers
should verify the unchanged full-validation call sites.

The main delivery risk is discovering another performance cost after the fast
path lands. If the `<=90s` reproduction target is still missed, builder should
stop and route a `needs-architecture` follow-up with profiling evidence instead
of shipping a partial or timeout-based fix.
