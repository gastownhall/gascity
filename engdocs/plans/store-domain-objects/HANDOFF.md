# Store-domain-objects migration — HANDOFF (resume here)

**As of:** migration branch `refactor/store-domain-objects`, tip **`13c0ff6f9`**
(W-tick `1d0260f90`; W-pool `507f7bf4a`; W-delete `e0c186205`; **W-flip+unexport merged
`13c0ff6f9`**). Local branch, **UNPUSHED**. `git push` only (Dolt local-only).

## ENDGAME OUTCOME (2026-07-10)
**Interior (non-test) `InfoFromPersistedBead` = TRUE ZERO across all 4 scan dirs** — the anti-leak
goal (raw beads out of business logic; de/serialization only at the store edge) is ACHIEVED and
census-enforced. Every session codec needle is at zero or a documented honest floor. Remaining census
rows: `ListAllSessionBeads: session_beads.go 1` (sync/beadmail floor — W-sync, out of budget) and the
orders codecs `RunFromTrackingBead`/`MaxSeqFromLabels` (WI-3, gated on two-class graph wiring).
**The `InfoFromPersistedBead` COMPILER unexport is DEFERRED** (honest under-reach): the codec is a
test-fixture constructor at ~444 external sites / 51 test files; the compiler rename breaks them all.
The census ratchet already enforces the boundary at runtime-scan level; the unexport needs a separate
**W-test-fixture** wave — migrate the ~498 raw-bead test fixtures to REAL STORE TEST DOUBLES (Julian's
directive: hand-cracked raw beads in tests is a code smell; a shim was rejected as it relocates the smell),
which then lets `InfoFromPersistedBead` unexport. **That wave is planned + ready to execute — resume via
`W-TESTFIXTURE-HANDOFF.md` + `W-TESTFIXTURE-PROMPT.md` (this dir); authoritative plan =
`test-double-migration-plan.md`.** Below this line is the pre-endgame history.

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

## What REMAINS — 2 waves (see `tickfeed-design.md` §3 for the spec)
✅ DONE: **W-pool** (`507f7bf4a`, `build_desired_state` IFP 2→0 + skew-reload fix) · **W-delete**
(`e0c186205`, raw-half deleted; `session_bead_snapshot`/`session_hash`/`session_logs_resolve`/`cmd_stop`/
`doctor` census zeros; edge-side fingerprint; `Info.AwakeStartedAt`+`Info.UsageComputeEmittedAt`).
1. **W-flip** (§5b, §4 residual table) — front-door flip: `cmd/gc/class_store.go` + `internal/api` State
   accessors flip from `beads.XStore` wrappers to domain-store front doors, built from the `resolve*Store`
   outputs (preserve the #4017 capability assertions). Zeros the **last two interior `InfoFromPersistedBead`
   sites**: `cmd_session.go:cmdSessionKill` (raw `sessStore.Get`+codec → session front-door Get→Info; its
   own census comment defers it here) and `internal/api/session_resolution.go` (raw retire lane over
   `ExactMetadataSessionCandidates` → an Info-returning sibling; the lane needs only `SessionNameMetadata`).
   Also the WI-6 W2 permission-mode raw lane (`huma_handlers_sessions_command.go:updateSessionPermissionMode`).
   **Every moved read MUST bridge the front-door-Get contract (below).** After W-flip: interior
   `InfoFromPersistedBead` = **0 across all scan dirs**.
2. **W-unexport** (§5e) — unexport `InfoFromPersistedBead` → `infoFromPersistedBead` (compiler boundary;
   reachable after W-flip drives it to true interior zero) + the all-zero tripwires (`PollerKeyFromBead`,
   `PersistedResponseFromBead`, etc.); reimplement `catalog.GetWithPersistedResponse` over
   `Store.GetPersistedResponse`+`EnrichInfo` so its needle zeroes; convert the WI-0 ratchet rows to
   permanent zero-pins. **STAYS exported (honest):** `ListAllSessionBeads` (`session_beads.go:1` sync/beadmail
   floor — W-sync, out of budget); orders codecs `RunFromTrackingBead`/`MaxSeqFromLabels` (WI-3).

## Current census (green at `e0c186205`) — the remaining tail
```
InfoFromPersistedBead(:  cmd_session 1, internal/api/session_resolution 1   (= 2, → 0 after W-flip; W-pool+W-delete DONE)
ListAllSessionBeads(:    session_beads 1
                         (→ stays PINNED at 1: sync internals + internal/mail/beadmail compile dep;
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
