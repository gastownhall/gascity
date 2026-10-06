# Concurrent store environment reads

**Goal:** A rig-store open must use that scope's intended `BEADS_*` values even while another native Dolt store open temporarily projects process environment variables. Source: reviewer finding `ga-4yivia` from `ga-0tsfr7`.

## User impact

The concurrent-store change opens bound rig stores in overlapping goroutines. Some reads of `BEADS_*` keys still use bare process environment access while a sibling native open can temporarily replace or unset those keys. The reviewer found four undocumented read sites in the concurrent-open path. Current managed-city configuration makes their observed impact low or fail-closed, but a wrong credential path could surface as an unexpected rig-open failure. The proxied path is opt-in today.

The reviewer passed the main change and recorded this as a P3 follow-up. Its reviewed commit `8e2a42fe7223e1017b268d1f8eb7ac96a0f98654` was not on `origin/main` at PM intake on 2026-10-06. Review vehicle `ga-0tsfr7` is the initial dependency; downstream agents must confirm the change actually merged before working on a standalone follow-up.

## Work packages

| Order | Bead | Route | Acceptance |
| --- | --- | --- | --- |
| 1 | `ga-2j9lew` | validator (`needs-tests`) | A deterministic guard fails on unsafe projected-key reads reachable during sibling opens and on newly introduced reads of that class. It records reasons for any deliberate exceptions and gives builder RED evidence. |
| 2 | `ga-wwwjxe` | builder (`ready-to-build`) | Concurrent opens use scope-correct credentials, socket, and proxied-root/mode values; raw-value compatibility and documented fallbacks remain intact; the guard and relevant cmd/gc and internal/beads checks pass. |

Both beads trace to `ga-4yivia` and wait for `ga-0tsfr7`. Builder work also waits for the validator's RED evidence. Review closure does not prove merge. If the review creates a separate deploy bead, carry that gate forward and verify `origin/main` before implementation.

## Scope and risk

The reviewer named bare reads in `internal/doltauth/auth.go`, `cmd/gc/bd_env.go`, and `internal/beads/proxyendpoint/record.go`. The existing guarded accessor is documented in `internal/beads/native_dolt_store.go`; the implementation choice belongs to builder within the repository's package-layering rules. Other bare reads that a guard finds need a reachability check and an explicit reason before exclusion. The two documented historical reads listed on the reviewer bead are not silently rewritten.

The principal risk is expanding a narrow concurrency fix into unrelated environment plumbing, or changing raw values that must match bd's own resolution. The acceptance criteria keep the boundary at reads reachable during concurrent rig-store opens and require current managed-session behavior to remain the same. This plan does not duplicate the separate dependency-ID lookup follow-up `ga-b0a8u1`.
