# Store-domain-objects migration — HANDOFF (resume here)

**As of:** migration branch `refactor/store-domain-objects`, tip **`23a0e2d43`**
(W-tick keystone merged at `1d0260f90`). Local branch, **UNPUSHED**. Gascity Dolt is
local-only — **`git push` only**, never `bd dolt push`.

## The goal (one paragraph)
Stores return typed **domain objects**; raw `beads.Bead` must not flow through business
logic — **de/serialization ONLY at the store edge**. Work/Graph classes keep `beads.Bead`
as their domain object (not a leak). Typed classes return typed objects via their front
door: Sessions→`session.Info`, Messaging→`mail.Message`, Orders→`orders.OrderRun`,
Nudges→`nudgequeue.NudgeShadow`, Waits→`session.WaitInfo`. Write model =
`Store.ApplyPatchInfo(info, patch)` (persist + LOCAL fold, **no re-Get**). Enforced by the
CI census ratchet `cmd/gc/typedclass_edge_guard_test.go`.

## What is DONE (all integrated + verified on the branch)
- **WI-0..WI-6** — the entire interior: API read-model, worker boundary, start-execution
  feed, the full reconciler read/write cluster (every W6/coupling mirror dropped, the
  lease + async classifier families deleted), messaging/orders/nudges/waits classes, periphery.
- **Remainder R1–R5-lite** — leaf sweeps, display-reason lane, the two HIGH-risk coupled
  waves (R3 heal+sleep, R4 start-execution), periphery Info wins.
- **W-tick (the keystone)** — reconciler tick-feed refactor: `ListAllForReconcile()
  []ReconcileSession{Info,Circuit}`, Phase-0 heal/dedup as `ApplyPatchInfo` folds
  (fold-then-build). **`session_reconciler.go` `InfoFromPersistedBead` = 0**, 0-Get tick
  budget held. Added `Info.WorkerDir`.

Full wave-by-wave status + every merge SHA is in **`work-items.md`** (WI-6 section + the
"Corrected remaining endgame" block). Designs: **`tickfeed-design.md`** (the remaining
W-pool→W-unexport plan — AUTHORITATIVE), `remainder-design.md` (R1–R5), `r6-finding-tickfeed-keystone.md`.

## What REMAINS — 4 waves (see `tickfeed-design.md` §3 for the spec)
1. **W-pool** (MED-HIGH) — type the pool selection/creation/reuse path (`findOpenSessionBeadByID`,
   `selectOrPlanPoolSessionBead`, `reusablePoolSessionBeads`, `reusableDependencyPoolSessionBeads`,
   `createPoolSessionBeadWithGuardedAlias`, `normalizeNonExpandingPoolSessionBeadForSelection`) +
   `snapshot.add(info)`. These CREATE/REUSE beads — needs a typed create-returning-Info.
2. **W-delete** (MED, falls out) — delete the raw `sessionBeadSnapshot` half (`Open()`/`FindByID`/
   `newSessionBeadSnapshot(beads)`/the raw `open` slice) after W-pool frees it; flip
   `loadSessionBeadSnapshot` to build from Info; zero `session_bead_snapshot` `InfoFromPersistedBead`
   (3→0), `session_hash` (1→0), `session_logs_resolve` (2→0 via an `Info.AwakeStartedAt` field-add +
   ResolveCodexTranscriptBySessionOrder→[]Info). `sessionBeadSnapshotFingerprint` is NOT Info-projectable
   (hashes ALL metadata keys) — compute it at snapshot construction (edge). One flagged behavior delta:
   sync-tail re-list.
3. **W-flip** (§5b) — front-door flip: `cmd/gc/class_store.go` + `internal/api` State accessors flip from
   `beads.XStore` wrappers to domain-store front doors, built from the `resolve*Store` outputs (preserve
   the #4017 capability assertions). Migrates `cmd_session.go:cmdSessionKill` + `internal/api/session_resolution.go`
   + the permission-mode raw lane. **Every moved read must bridge the front-door-Get contract (below).**
4. **W-unexport** (§5e) — unexport `InfoFromPersistedBead` → `infoFromPersistedBead` (compiler boundary;
   reachable after W-tick+W-delete+W-flip drive it to true interior zero) + the all-zero tripwires; reimplement
   `catalog.GetWithPersistedResponse` over `Store.GetPersistedResponse`+`EnrichInfo` so its needle zeroes;
   convert the WI-0 ratchet rows to permanent zero-pins.

## Current census (green at `23a0e2d43`) — the remaining tail
```
InfoFromPersistedBead(:  build_desired_state 2, cmd_session 1, cmd_stop 1,
                         session_bead_snapshot 3, session_hash 1,
                         session_logs_resolve 2, internal/api/session_resolution 1   (= 11, → 0 after W-delete+W-flip)
ListAllSessionBeads(:    doctor_session_model 1, session_bead_snapshot 1, session_beads 1
                         (→ stays PINNED at ~1: sync internals + internal/mail/beadmail compile dep;
                          full sync-typing is a separate out-of-budget "W-sync" wave — HONEST, documented)
GetWithPersistedResponse(: internal/worker/catalog 1   (→ 0 in W-unexport)
RunFromTrackingBead( 1 / MaxSeqFromLabels( 2:  ORDERS residuals, gated on deferred WI-3 two-class graph wiring — NOT this endgame.
```
**Honest endgame verdict (from `tickfeed-design.md` §5):** `InfoFromPersistedBead` reaches true
interior zero and UNEXPORTS (the compiler-enforced boundary). `ListAllSessionBeads` does NOT fully
unexport this endgame — pinned at ~1, stated plainly. Orders codecs stay (WI-3).

## The execution loop (used for every wave — DO NOT skip the red-team)
**Fable design (exists in `tickfeed-design.md`) → Opus impl (worktree-isolated, off the current tip)
→ Fable red-team (the `sdo-review.js` workflow) → fix blockers via agent resume → integrate.**
- **Impl:** launch a `general-purpose` agent, `model: opus`, `isolation: worktree`, off the current
  migration tip. Give it the wave's design section + the discipline below. Two commits: A additive
  twins/oracles+pins, B migrate+delete+census ratchet.
- **Red-team:** `Workflow({scriptPath: "engdocs/plans/store-domain-objects/sdo-review.js", args:{key,
  base, head, opportunity, designPath, verifyPath}})`. It runs 2 lenses (behavior + convention) + synth,
  grounds against the head COMMIT via `git show/git grep` (checkout-independent). Verdict:
  approve / approve-with-nits / changes-needed. Address blockers by SendMessage-resuming the impl agent.
- **Integrate:** `git checkout refactor/store-domain-objects; git merge --no-ff <fix-tip>`. The ONLY
  cross-wave conflict is the census guard `cmd/gc/typedclass_edge_guard_test.go` — resolve by
  `git checkout --ours` it, then run `go test ./cmd/gc/ -run TestTypedClassCodecCensus` and paste the
  **regenerated literal** it prints on fail (preserve the WI-6 annotation comments). Verify build+vet+census;
  the shard suite was already green per-branch on a clean merge.

## Non-negotiable discipline (every wave)
- **TDD; every oracle LOAD-BEARING + self-sufficient.** The red-team WILL mutation-test — a pin that a
  mutation of the twin's non-trivial branch does NOT fail is a blocker (caught in R1, R2, R3, R5, W-tick).
- **Census HONEST.** Blind spot: the guard counts codec-CALL needles, NOT raw `bead.Metadata["key"]` inline
  reads — never inline a magic string to dodge a needle (that's the W2 anti-pattern the red-team caught).
  Either route through the front door or keep the honest codec + its count. An honest nonzero > a gamed zero.
- **Front-door-Get contract (bit W2/W3/W5, W-flip will hit it hardest).** `session.Store.Get`/
  `GetPersistedResponse` differ from raw `store.Get`: they return `ErrSessionNotFound`, wrap `"loading
  session %q"`, and REJECT non-`IsSessionBeadOrRepairable` beads. Every moved Get MUST bridge it — mirror
  `internal/api/session_get_read.go:60` (`bridgeSessionGetError` / `bridgeSessionRecordError`).
- **No re-Get (spec §7).** `TestReconcileSessionBeadsFastPathGetBudget` pins 0 fast-path Gets — keep it green.
- **Honest under-reach.** If a consumer needs a raw field absent from `Info`: add the field if a clean edge
  add (like `Info.BuiltinAncestor`/`WorkerDir`), else STOP + report + defer. Two waves (R5, R6) correctly
  stopped and re-scoped rather than force a false zero — that's the expected behavior.

## Environment gotchas
- **Hooks HANG** (stale absolute `core.hooksPath`) → commit with `git commit --no-verify`; manual gates + CI
  are the real gate.
- **Box is thread-capped** → `make test-cmd-gc-process-parallel` may die with `fork/exec: resource
  temporarily unavailable`; run the 6 shards SEQUENTIALLY as fallback.
- **NEVER `go clean -cache`** (corrupts shared GOCACHE) → `GOCACHE=$(mktemp -d) go build ...` for cold
  builds; `go clean -testcache` is fine. **NEVER `tmux kill-server`.**
- **Known-good integration reds** (verify any red reproduces on the wave's base, then it's not a regression):
  `TestE2E_AgentLifecycleEvents`, `TestGCLiveContract_BeadsAndEvents`, `TestHumaBinary_CityCreateAsync`,
  `TestCleanInstallTutorialPath` (sandbox/infra); `TestGraphWorkflowSuccessPath`,
  `TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash`, tmux `TestGetAllDescendants` (contention flakes).
- Model division: **Opus** for explore/impl, **Fable** for design + red-team.

## Verify commands
```
gofmt -l cmd/gc/ internal/session/ ; go build ./... ; go vet ./...
go test ./internal/session/ -count=1
go test ./cmd/gc/ -run TestTypedClassCodecCensus -count=1     # the census ratchet
make test-cmd-gc-process-parallel                              # 6 shards (sequential if thread-capped)
make test-local-full-parallel                                 # ONCE before the final merge to main
```

## Ship (when the endgame is done — only if asked)
`git pull --rebase && git push` (branch is local-only Dolt — `git push` ONLY). Then the branch is ready
for review/merge to `main`. Do NOT push mid-wave.
