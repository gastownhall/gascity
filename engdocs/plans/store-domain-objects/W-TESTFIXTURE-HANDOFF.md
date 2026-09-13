# W-test-fixture — HANDOFF (resume here to complete the final wave)

**As of:** migration branch `refactor/store-domain-objects`, tip **`d8e65dc35`** (+ this doc).
Local branch, **UNPUSHED**. Gascity Dolt is local-only — **`git push` only**, never `bd dolt push`.

## Where the migration stands (the substantive work is DONE)
The store-domain-objects migration reached its substantive goal: **interior (non-test)
`InfoFromPersistedBead` = TRUE ZERO across all 4 scan dirs** (cmd/gc, internal/api, internal/worker,
internal/dispatch). Raw beads no longer flow through business logic; `make test-local-full-parallel`
is green. Waves done: WI-0..WI-6 + R1–R5-lite + W-tick + W-pool (`507f7bf4a`) + W-delete (`e0c186205`)
+ W-flip+unexport (`13c0ff6f9`). See `work-items.md` for the full history + SHAs.

## The ONE remaining wave: W-test-fixture
**Goal:** eliminate the code smell where TEST code hand-crafts `beads.Bead` literals and cracks them
into `session.Info` via `session.InfoFromPersistedBead` — instead, tests create sessions through a
**real store test double** and read typed objects back, the way production does. Terminal payoff:
`InfoFromPersistedBead` **unexports** (`→ infoFromPersistedBead`) — the compiler boundary the DoD called
for. (Julian's framing: "tests using raw beads instead of real store object test doubles is a code
smell we need to fix." A `sessiontest.InfoFromBead` shim was REJECTED — it just relocates the smell.)

## THE AUTHORITATIVE PLAN: `test-double-migration-plan.md` (this dir) — READ IT FIRST
Grounded by a 14-agent read-only categorization of all 68 test files (~498 codec call sites). It has:
the site inventory by replacement category, the canonical test-double pattern, the edge-oracle
disposition (nothing forces the codec to stay exported), the 4 human-decision points (with defaults),
the phased execution, and the risk register. **Everything below is the operational overlay on that plan.**

### The scope in one table (~498 sites)
- **store-read** ~200/40 — bead already in a store → `sessionFrontDoor(store).Get(id)` (Get runs the codec internally, byte-identical).
- **store-create** ~117/30 — build via `Store.CreateSessionInfo(spec)` (persists + returns Info).
- **`session.Info{}` literal** ~115/36 — no store under test, or a deliberately divergent fixture.
- **lowercase rename** ~57/19 — internal/session oracles (mechanical; these STAY on the codec).
- **twin oracles** 9/5 — raw-vs-Info cmd/gc twins (route Info side through the front door).
- Package split: cmd/gc ~420 (the smell) · internal/session ~67 (rename-only) · internal/api 1 · **internal/worker 1 (the sole cross-package caller that blocks the unexport)**.
- Volume: 4 files ≈ 60% — `session_lifecycle_parallel_test.go` (105), `session_reconcile_test.go` (48), `session_wake_test.go` (~28), `session_reconciler_test.go` (~20).

### Canonical pattern (plan §"Canonical test-double pattern")
New pkg `internal/session/sessiontest`: `Store(t)→(*session.Store, beads.Store)`, `Info(t,s,spec)→Info`,
`InfoFromMeta(t,meta)→Info`, `SeedBead(t,s,mem,bead)→Info` (raw Create + front-door Get, for fixtures
needing Status=closed/pinned CreatedAt/custom labels `CreateSpec` can't express). Plus cmd/gc
`reconcilerTestEnv.sessionInfo(id)`/`createSessionInfo(name,template)`. **internal/session white-box
tests keep their existing `seedSessionStore`/`sessionBeadFixture`** (import-cycle: `sessiontest` imports
`session`). Struct literals for divergent fixtures; degraded/non-round-trippable corpora stay on the raw codec.

## Execution order (each wave = the proven Opus-impl → Fable-red-team → fix → integrate loop)
1. **Phase 0 — Foundation (land + verify + merge FIRST; blocks all).** Add
   `internal/session/sessiontest/sessiontest.go` (+ `go run scripts/add-testenv-import.go`) + the
   `reconcilerTestEnv` helpers in `cmd/gc/session_reconciler_test.go`. No conversions. Green build +
   `go test ./internal/session/sessiontest/`. Merge to the branch before fanning out (shared file →
   keep it OFF the parallel worktrees or they conflict).
2. **Phases 1–9 — cmd/gc + api + worker conversions (parallel, disjoint file groups, ≤5 files/agent).**
   Big files solo (parallel_test 105; reconcile_test 48). Grouping = plan §"Phased execution" A–J.
   Each wave: convert → `go test` the touched packages + `go vet` → **the census guard must stay
   UNCHANGED** (non-test scan is unmoved until Phase 10). Red-team each wave.
3. **Phase 10 — rename + unexport + census (LAST, gated).** **Repo-wide grep GATE first:**
   `git grep -n 'session.InfoFromPersistedBead\|sessionpkg.InfoFromPersistedBead\|PersistedResponseFromBead' -- '*.go'`
   must return ONLY internal/session files (zero cmd/gc, internal/api, internal/worker). If any external
   hit remains, STOP and route it back to a conversion wave. Then lowercase the internal/session sites +
   the definition in `info_store.go`; delete the exported name. Convert the census in
   `typedclass_edge_guard_test.go`: `InfoFromPersistedBead(` → a hard `== 0` pin (now compiler-guaranteed);
   add `infoFromPersistedBead(` as a needle policed to zero in cmd/gc/api/worker. Full sharded suite +
   `TestTypedClassCodecCensusRatchet` green.

## Human-decision points (defaults set in the plan — hold each to the red-team)
1. `session_reconcile_test.go` shim helpers → **keep one projection boundary via a `sessiontest` shim** (no caller fan-out).
2. The 9 raw-vs-Info twins → **convert Info side to front-door store-read now**; retire-vs-golden is follow-on. `session_wtick_twins` pins TrimSpace fidelity → keep raw/struct there.
3. `session_record_equiv_test.go` (worker) → **store-read** (`Store.Get`); if strict bead-form is required, relocate the oracle into internal/session instead.
4. time/bool-metadata struct-literals (`pending_create_claim`, `last_woke_at`, CreatedAt) → confirm exact codec metadata→Info field names before flipping; prefer the front-door/shim route when not 1:1.

## Non-negotiable discipline (the red-team WILL check)
- **Behavior-identical fixtures.** A converted fixture must produce the SAME `Info` (and same on-store
  bytes where a store is involved) as the raw-bead form. Verify field-by-field for nuanced sites.
- **`CreateSpec` can't express Status(closed)/CreatedAt/custom labels** → use `SeedBead`; naive
  `CreateSessionInfo` silently DROPS load-bearing metadata/labels (parallel_test, r3, fork_launch, pool_replacement).
- **Front-door `Get` narrows via `IsSessionBeadOrRepairable`** → deliberately degraded / non-session /
  whitespace / legacy corpora MUST stay on the raw codec (list_from_infos, wdelete non-session, worker_dir
  legacy, drainack). Do NOT blanket-convert oracles to store-read.
- **Deliberately divergent-from-store fixtures** (stale twin, `ID:"missing"`, pinned CreatedAt) → struct
  literal ONLY; a store read erases the divergence the test asserts.
- **`testStore` in some files is a write-tracking MOCK** (batch-capture / error-injection), not a memstore
  (reconcile, ratelimit, telemetry) → a memstore swap changes how writes are asserted; verify the
  quarantine/patch/recordWakeFailure assertions still fire (~45 nuanced sites).
- **The census guard is the enforcement mechanism + its own literals are needles** → do NOT touch it until
  Phase 10; a stray `InfoFromPersistedBead` string added to a scanned NON-test file trips it.
- **Import cycle:** `sessiontest` imports `session` → internal/session white-box tests can't use it.
- **TDD; oracles stay load-bearing.** Existing pins (census ratchet, tick budget, the wave characterization
  pins) stay green throughout. The conversion changes test SETUP, not what's asserted.

## The execution loop (per wave — DO NOT skip the red-team)
**Opus impl (worktree-isolated off the current tip) → Fable red-team (`sdo-review.js`) → fix blockers by
resuming the impl agent → integrate (`git merge --no-ff`).**
- **Impl agent:** a `general-purpose` agent, `model:"opus"`, briefed with the wave's file group + the
  plan §canonical-pattern + this discipline. Two commits: A additive (Phase 0 only) / for conversion waves
  a single "convert group X" commit is fine (no additive twin needed — the helper already exists).
- **Red-team:** `Workflow({scriptPath: "engdocs/plans/store-domain-objects/sdo-review.js", args:{key,
  base, head, opportunity, designPath, verifyPath}})`. Feed it: "verify each converted fixture is
  behavior-identical (same Info fields / on-store bytes); divergent fixtures stayed struct-literals;
  degraded corpora stayed on the raw codec; no store round-trip corrupted a fixture; census unchanged."
  It grounds against the head COMMIT (checkout-independent). Verdict: approve / -with-nits / changes-needed.
- **Integrate:** `git checkout refactor/store-domain-objects; git merge --no-ff <impl-tip>`. For
  conversion waves there should be NO census-guard conflict (non-test scan unmoved). Verify build+vet+the
  touched package tests + census green.

## Environment gotchas (hard-won this session — READ)
- **WORKTREE BASE-REF (CRITICAL):** the branch is LOCAL/UNPUSHED and diverged from `origin/main`. The
  harness `Agent(isolation:"worktree")` / `EnterWorktree` default `worktree.baseRef=fresh` branches from
  **origin/main → this LOSES the entire migration.** DO NOT use harness worktree isolation. Instead create
  the impl worktree MANUALLY off HEAD:
  `git worktree add -b sdo/<wave>-impl /data/projects/gascity-sdo-<wave> HEAD`, and gate the impl agent's
  FIRST action on `git merge-base --is-ancestor <current-tip> HEAD && echo BASE_OK`. Launch a plain
  `general-purpose` Opus agent instructed to `cd /data/projects/gascity-sdo-<wave>` and run all git/go/make
  there, all Read/Write/Edit paths absolute under it. Clean up after merge: `git worktree remove <path>` + `git branch -D`.
- **Hooks HANG** (stale absolute `core.hooksPath`) → `git commit --no-verify`; manual gates + CI are the real gate.
- **NEVER `go clean -cache`** (corrupts shared GOCACHE) → `GOCACHE=$(mktemp -d) go build ./...` for cold
  builds; `go clean -testcache` is fine. **NEVER `tmux kill-server`.**
- **Box is thread-capped** → `make test-cmd-gc-process-parallel` may die `fork/exec: resource temporarily
  unavailable`; run the 6 shards SEQUENTIALLY as fallback. Shards run slow under concurrent-agent `-race`
  contention — not a hang.
- **Model division:** Opus for impl, Fable for the red-team workflow (`sdo-review.js` already pins `model:'fable'`).

## Verify commands
```
gofmt -l cmd/gc/ internal/session/ internal/api/ internal/worker/ ; go build ./... ; go vet ./...
go test ./internal/session/... ./internal/api/ ./internal/worker/ -count=1   # after each wave touching them
go test ./cmd/gc/ -run TestTypedClassCodecCensus -count=1                      # must stay green every wave
# Phase 10 GATE:
git grep -n 'session.InfoFromPersistedBead\|sessionpkg.InfoFromPersistedBead\|PersistedResponseFromBead' -- '*.go'  # → internal/session only
make test-cmd-gc-process-parallel                                             # 6 shards (sequential if thread-capped)
make test-local-full-parallel                                                 # ONCE before final report
```

## Definition of done
`sessiontest` foundation landed; all ~420 cmd/gc + 1 api + 1 worker sites converted to real store test
doubles (or struct-literals / kept-raw where the plan dictates); internal/session oracles lowercased;
**`InfoFromPersistedBead` unexported → `infoFromPersistedBead`** (compiler boundary); census guard is a
hard zero-pin (`InfoFromPersistedBead(` == 0, `infoFromPersistedBead(` policed to zero in the interior);
`make test-local-full-parallel` green once. Then STOP and report — **do not push unless asked** (Dolt
local-only → `git push` only).

## Red-team tooling
`sdo-review.js` (this dir) — the durable 2-lens Fable adversarial review, invoked via the Workflow tool.
It runs behavior + convention lenses + synth, grounds against the head COMMIT (`git show`/`git grep`),
returns a verdict. Reuse it per wave.
