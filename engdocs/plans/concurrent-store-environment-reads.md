# Concurrent store environment reads

Goal: each concurrent rig-store open uses its intended `BEADS_*` values while sibling native Dolt opens temporarily project or unset process environment variables. Source: reviewer finding `ga-4yivia` from review `ga-0tsfr7`.

## User impact

The concurrent-store change opens bound rig stores in overlapping goroutines. The reviewer identified four undocumented bare environment reads that can observe a sibling open's temporary projection. Current managed-city paths make the observed divergence low impact or fail closed; another credential-file path can still cause an unexpected rig-open authentication failure. Proxied paths are opt-in today. The reviewer passed the original change and filed this follow-up at P3.

## Work package and dependency

| Bead | Initial route | Acceptance |
| --- | --- | --- |
| `ga-2j9lew` | validator (`needs-tests`), then builder on the same bead (`ready-to-build`) | A deterministic guard demonstrates unsafe projected-key reads; the fix makes concurrent opens use their intended values while preserving managed-session behavior, raw-value compatibility and documented ambient fallbacks. Relevant checks pass. |

`ga-2j9lew` traces to `ga-4yivia` and carries a blocking dependency on landing guard `ga-b4rua6`. The original reviewed commit `8e2a42fe7223e1017b268d1f8eb7ac96a0f98654` is absent from freshly fetched `origin/main` on 2026-10-06; `cmd/gc/run_concurrently.go` is also absent. The guard clears only after fresh ancestry or verified squash-equivalent behavior proves the implementation landed. Review or deploy bead closure alone is insufficient.

Validator records RED evidence and the focused command, then hands the same open bead to builder using the normal relabel, molecule-close, claim-release and sling protocol. The landing dependency remains on that bead throughout. The prior separate builder bead `ga-wwwjxe` is superseded; its full build scope is carried under `Build:` on `ga-2j9lew`.

## Acceptance and build scope

- A deterministic guard fails on the unsafe reads identified by the reviewer and on an additional unsafe read introduced in the concurrent-open path. Every exclusion has reachability evidence or an explicit, reviewed reason.
- Concurrent opens use their intended credentials, external socket and proxied root/mode values. A sibling open's transient process environment cannot alter those choices.
- Current managed-session behavior, raw-value compatibility with bd and documented deliberate ambient fallbacks remain intact.
- Builder resolves the recorded RED guard without silently allowing the four unsafe sites. The guard, relevant `cmd/gc` and `internal/beads` checks, and applicable race checks pass; commands and results are recorded.

## Scope and risk

The reviewer named reads in `internal/doltauth/auth.go`, `cmd/gc/bd_env.go` and `internal/beads/proxyendpoint/record.go`, with the existing guarded-access contract in `internal/beads/native_dolt_store.go`. The implementation must respect existing package layering; implementation choices belong to builder. Additional candidates need a reachability check before exclusion. The two documented historical reads listed on the finding retain their explained behavior.

The main risk is changing environment resolution beyond the overlapping-open paths or altering raw values that must agree with bd. Acceptance stays within the reported hazard. Separate dependency-ID lookup follow-up `ga-b0a8u1` remains outside this scope.
