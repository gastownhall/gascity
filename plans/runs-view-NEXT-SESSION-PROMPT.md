# Next-session kickoff prompt

Paste the block below into a fresh session to continue this work at **P3**.

---

Continue the dashboard **Runs-view event-sourcing** work — next phase is **P3 (the detail interpreter)**. Read `plans/runs-view-HANDOFF.md` first (in the `runs-proj` worktree): it has the full state, the decided architecture (ADR: `plans/runs-view-architecture-adr.md`), the worktree map, the verification commands, and — most importantly — the **expanded P3 section with verified design facts** you must read before porting. Also recall memory `runs-view-event-sourcing`, `gascity-runs-proj-worktree-layout`, and `gascity-precommit-hook-stale-absolute-hookspath`.

Where things stand: PR **#3793** (branch `feat/runs-event-projection`, stacked **draft** on dashboard PR #3727) has **P0** (events→bead fold + golden corpus), **P1** (`BuildRunSummary` Go port, golden GREEN), and **P2** landed and pushed (`8601e1826`):
- **P2a** (`006c5c32d`): `EnrichRunSummary`/`AdvanceProgressMarks`/`DashboardSession` + available-arm marshalers in `internal/runproj/{enrich,session}.go`; new `runsummary_enriched_golden.json` (+ `sessions_fixture.json`); ported `health.test.ts`/`liveness.test.ts` to Go. Byte-for-byte GREEN.
- **P2b** (`62a599efa`): `runproj.Projector` (order-preserving fold) + `internal/api/dashboardbff/runtailer.go` (cold replay → read-only `events.ReadFrom` byte-offset tail, race-free resume, server-side thrash marks) behind `GET /api/city/{cityName}/runs/summary` (warm fold + request-time loopback `/v0 sessions` enrich). Race-clean tests.

Architecture is **SingleSource-RunProjection**: run semantics in Go (`internal/runproj`), the SPA becomes a pure renderer, `runspec` codegen at the end (P5).

Hard rules:
- Work ONLY in `/data/projects/gascity/.claude/worktrees/runs-proj`. The `new-dashboard` worktree has ~300 uncommitted non-mine changes — LEAVE IT UNTOUCHED (no `git add -A`/`stash`/`checkout -- .` there). Shell cwd resets each Bash call, so prefix commands with `cd /data/projects/gascity/.claude/worktrees/runs-proj && …`.
- Gate EVERY change against the goldens: `go test ./internal/runproj/ ./internal/api/dashboardbff/ -count=1` green (run the dashboardbff tests under `-race`); `go vet`, `gofmt -l`, `golangci-lint run` clean on both packages; `npm run gen:run-goldens:check` (from `internal/api/dashboardspa/web`) green; keep `TestOpenAPISpecInSync` green (stay non-Huma).
- The shared pre-commit hook is broken (stale `docs/schema/` path) — run its checks manually, then `git commit --no-verify` and `git push --no-verify`. Rebase onto `origin/feat/dashboard-supervisor-hosting` if it advanced (it was 0-behind at P2). Keep #3793 a draft until #3727 merges; keep the SPA `FormulaRunDetail` DTO byte-stable until P4.

**Do P3, then stop and report** (P4/P5 are separate phases — see the handoff):
- Port the detail pipeline `shared/src/runs/{enrich(enrichFormulaRun),formula-run,groups,edges,node-shape,execution-instances,formula-order,session-link,display-state}.ts` → `internal/runproj` as `BuildRunDetail`. The graph-layout core (groups semantic-id disambiguation, alias maps, loop instancing; node-shape) is the hard, correctness-sensitive part — port as Go code, single-homed.
- **Critical design facts (full detail in the handoff's P3 section):** the golden path is BEAD-DERIVED ONLY (`snapshotForRun(beads,"dt-adopt1")` → `enrichFormulaRun(snapshot, {})`, no sessions/no formulaDetail); the Go port must reproduce the generator's `snapshotForRun`+`toRunSnapshotBead`+`depsForMembers` projection from the fold (`kind = metadata['gc.original_kind'] ?? issue_type`, `step_ref=ref`, `scope_ref/logical_bead_id` from metadata; deps = non-root→root `parent`); parameterize `snapshot_version`(=1) and `snapshot_event_seq`(=100 golden / tailer `LastSeq()` live); REUSE P1's `mapRunPhase`/`stageProgress`/`stagesForFormula` (ADR: detail stages == summary stages by construction); `session-link` only fires with `opts.sessions` so it's ABSENT from the golden — port it for the live endpoint, not golden-gated (port `session-link.test.ts` as a unit test).
- Add: a Go golden test (`BuildRunDetail(fixture, "dt-adopt1", 1, 100)` == `rundetail_golden.json`), a summary↔detail consistency test (same fixture run → same phase/stage through both), and `GET /api/city/{cityName}/runs/{runId}/detail` on the BFF plane (mirror `registerRunSummary`/`cityRunTailer`; the handler passes `Projector.Beads()` + `runId` to `BuildRunDetail`, then optional request-time session enrich). Do NOT regenerate `rundetail_golden.json` against the dirty `new-dashboard` tree (`session-link.ts` differs there) — the committed golden is valid on the clean base; only regenerate if you intentionally change the TS shape, on the clean base.
- Land P3 as a golden-gated commit on `feat/runs-event-projection`, push, update `plans/runs-view-HANDOFF.md` + the `runs-view-event-sourcing` memory, and report.

---
