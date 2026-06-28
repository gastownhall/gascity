# Next-session kickoff prompt

Paste the block below into a fresh session to continue this work.

---

Continue the dashboard **Runs-view event-sourcing** work. Read `plans/runs-view-HANDOFF.md` first (in the `runs-proj` worktree) — it has the full state, the decided architecture (ADR: `plans/runs-view-architecture-adr.md`), the worktree map, the per-phase plan, and the verification commands. Also recall memory `runs-view-event-sourcing`, `gascity-runs-proj-worktree-layout`, and `gascity-precommit-hook-stale-absolute-hookspath`.

Where things stand: PR **#3793** (branch `feat/runs-event-projection`, stacked **draft** on dashboard PR #3727) has **P0** (events→bead fold + golden corpus) and **P1** (`BuildRunSummary` Go port, golden-parity GREEN) landed in `internal/runproj/`. The architecture is **SingleSource-RunProjection**: run semantics in Go, SPA becomes a pure renderer, `runspec` codegen at the end.

Hard rules:
- Work ONLY in `/data/projects/gascity/.claude/worktrees/runs-proj`. The `new-dashboard` worktree has ~300 uncommitted non-mine changes — LEAVE IT UNTOUCHED (no `git add -A`/`stash`/`checkout -- .` there). Shell cwd resets each Bash call, so prefix commands with `cd /data/projects/gascity/.claude/worktrees/runs-proj && …`.
- Gate EVERY phase against the golden corpus: `go test ./internal/runproj/ -count=1` must stay green; also `go vet`, `gofmt -l`, `golangci-lint run ./internal/runproj/` clean.
- The shared pre-commit hook is broken (stale `docs/schema/` path) — run its checks manually, then `git commit --no-verify` and `git push --no-verify`. Rebase onto `origin/feat/dashboard-supervisor-hosting` if it advanced.
- Land each phase as a golden-gated commit on `feat/runs-event-projection` (keep #3793 draft until #3727 merges). Keep the SPA `RunSummary`/`FormulaRunDetail` DTOs byte-stable until P4.

Do the remaining phases in order, each verified before the next:
- **P2** — port the session enrich (`deriveRunHealth`/`buildCensus`/`enrich`/`advanceProgressMarks`) to Go with a new enriched golden; build a per-city background fold tailer in `internal/api/dashboardbff` (mirror `citySampler` in `samplers.go`; cold-replay via `runproj.FoldFile` + live tail via the read-only `transientCityEventProvider` `Watch`); expose `GET /api/city/{name}/runs/summary` (non-Huma BFF plane) reading `/v0/.../sessions` over loopback for enrich; wire `Start`/`Stop` in `cmd/gc/supervisor_dashboard.go`.
- **P3** — port the detail pipeline (`groups`/`edges`/`node-shape`/`execution-instances`/`formula-run`/`session-link`/`display-state`) into `internal/runproj` sharing the P1 `mapRunPhase`/`stageProgress`; regenerate `rundetail_golden.json` on the clean base; add a summary↔detail consistency test; expose `GET /api/city/{name}/runs/{runId}/detail`.
- **P4** — repoint the SPA to the BFF endpoints behind a flag, shadow-compare, then DELETE `shared/src/runs/*` logic + the frontend enrich; rebuild + re-embed `dist`; `make dashboard-check` green.
- **P5** — lift run knowledge into `runspec/*.toml` + codegen to Go+TS + a regenerate-and-diff CI gate (mirror `TestOpenAPISpecInSync`).

If you discover you need any of the in-flight beads-attribution work for richer lineage, call it out in the PR rather than depending on uncommitted changes. Report at each phase gate.

---
