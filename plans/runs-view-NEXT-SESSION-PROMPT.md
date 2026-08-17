# Next-session kickoff prompt

> ⚠️ **OBSOLETE — P4 IS COMPLETE** (P4a `30dbb7214` + `ceaeac078`, P4b `c2aa9ac53`).
> The SPA cutover and the dead-fold deletion both landed and pushed on
> `feat/runs-event-projection`. The block below kicks off P4 and must NOT be
> re-run. The only remaining phase is **P5 (`runspec` codegen)** — deferrable;
> the system rests stably at single-home-in-Go. See `runs-view-HANDOFF.md`.

Paste the block below into a fresh session to continue this work at **P4**.

---

Continue the dashboard **Runs-view event-sourcing** work — next phase is **P4 (SPA cutover + delete the TS run logic)**. Read `plans/runs-view-HANDOFF.md` first (in the `runs-proj` worktree): it has the full state, the decided architecture (ADR: `plans/runs-view-architecture-adr.md`), the worktree map, the verification commands, and — most importantly — the **expanded P4 section** (concrete files/hooks/error-contract/gates). Also recall memory `runs-view-event-sourcing`, `gascity-runs-proj-worktree-layout`, and `gascity-precommit-hook-stale-absolute-hookspath`.

Where things stand: PR **#3793** (branch `feat/runs-event-projection`, stacked **draft** on dashboard PR #3727) has **P0–P3 landed and pushed** (HEAD `366741c42`, rebased onto dashboard tip `05b3a8ca6`). The Go projection is done and golden-byte-green: `BuildRunSummary`/`EnrichRunSummary` (P1/P2) and `BuildRunDetail` (P3) in `internal/runproj`, plus **both BFF endpoints already serve the unchanged DTOs**: `GET /api/city/{city}/runs/summary` and `GET /api/city/{city}/runs/{runId}/detail`. So **P4 is frontend-only** — no new Go logic (only optionally strengthening `wire_contract_test.go`).

Architecture is **SingleSource-RunProjection**: run semantics live once in Go; the SPA becomes a **pure renderer** of the existing `RunSummary`/`FormulaRunDetail` DTOs. P4 deletes ~4.3k LOC of TS run logic.

Hard rules:
- Work ONLY in `/data/projects/gascity/.claude/worktrees/runs-proj`. The `new-dashboard` worktree has ~300 uncommitted non-mine changes — LEAVE IT UNTOUCHED. Shell cwd resets each Bash call, so prefix commands with `cd /data/projects/gascity/.claude/worktrees/runs-proj && …`.
- **Keep the DTO types byte-stable** — `shared/src/{snapshot/types,run-detail,run-snapshot}.ts` are now the Go↔TS contract; render components import them `import type`. The Go side marshals to exactly these shapes (golden-gated).
- Gate every change: `make dashboard-check` (`dashboard-build` → `npm run typecheck` → `go test ./internal/api/dashboardspa/... ./internal/api/dashboardbff/...`) green; keep `go test ./internal/runproj/ -count=1` + `-race ./internal/api/dashboardbff/` green; keep `npm run gen:run-goldens:check` (from `internal/api/dashboardspa/web`) green; keep `TestOpenAPISpecInSync` green. Rebuild + re-embed `dist` and check the `internal/api/dashboardspa/dist/` git diff (`make dashboard-ci`).
- The shared pre-commit hook is broken (stale absolute `core.hooksPath`) — run gates manually, then `git commit --no-verify` / `git push --no-verify`. The dashboard branch is periodically **rebased**; if it advanced, re-rebase with `git rebase --onto origin/feat/dashboard-supervisor-hosting <current-base>` (a plain rebase replays old dashboard commits and conflicts). Keep #3793 a draft until #3727 merges.
- ⚠️ An explore pass mis-listed the `RunPhase` enum — **re-read the actual `.ts` type files** before trusting any enum/field list in the handoff.

**Do P4 in revertable phases (≤5 files each), then stop and report:**
1. **API client** (`frontend/src/api/client.ts`): add two GET methods mirroring `api.runDiff`'s `request('GET', cityPath(...), objectDecoder<…>())` pattern, returning the existing `RunSummary` / `FormulaRunDetail` types.
2. **Summary loader** (`frontend/src/supervisor/runSummary.ts`): replace the multi-read fold (`listBeads`+`formulaFeed`+per-rig `task`+`molecule(all=true)`+`listSessions` → `buildRunSummary`→`enrichRunSummary`) with one warm GET to `/runs/summary`; **preserve the export surface** (`loadSupervisorRunSummary{,Mount,Active,Preview}Source`) so `runs/runSummarySubscription.tsx` keeps working; **keep the SSE nudge + `REFRESH_DEBOUNCE_MS=10_000` debounce** — it now guards a sub-second warm read.
3. **Detail loader** (`frontend/src/supervisor/runDetail.ts`): replace `workflowRun`+`formulaDetail`+`enrichFormulaRun` with one GET to `/runs/{runId}/detail`; **map the BFF error contract** to the existing `useFormulaRunDetail` states (`frontend/src/hooks/useFormulaRunDetail.ts`): 422+`reason:'not_run_view'` → `'unsupported'`; 422+`reason:'invalid_snapshot'` → load error; 404 → `'not_found'`; 503 → transient retry. The detail route + its SSE wiring stay.
4. **Shadow-compare** both paths over a soak window against a live city before deleting (the golden covers only the simple path; live data exercises loops/retries/scope-check bridging). If a divergence shows, fix it in `internal/runproj` (+ fixture case + regenerate golden on the clean base), NOT by reviving TS.
5. **Strengthen `wire_contract_test.go`** to run emitted Go JSON through the real TS decoders in Vitest (ADR Phase 0 follow-up) — do this BEFORE the deletion, while the decoders still exist.
6. **Delete** `shared/src/runs/*.ts` (+ `*.test.ts`, ~4.3k LOC), the two `supervisor/run{Summary,Detail}.ts` fold bodies, and the `runs/*` re-exports in `shared/src/index.ts`; KEEP the three DTO type modules; repoint/delete the lone value-import test (`frontend/src/attention/registry.test.ts` → `selectBlockedRuns`). Rebuild + re-embed `dist`; `make dashboard-check` green; preview the SPA (`npm run preview -- --host 127.0.0.1 --port <port>` from `…/web`) and verify both views render live.
7. Land P4 as gated commit(s) on `feat/runs-event-projection`, push, update `plans/runs-view-HANDOFF.md` + the `runs-view-event-sourcing` memory, and report. (P5 = `runspec` codegen is a separate, deferrable phase; the system rests stably at P4 = single-home-in-Go.)

---
