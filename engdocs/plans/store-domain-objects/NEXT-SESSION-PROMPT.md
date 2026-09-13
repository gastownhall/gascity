# Next-session prompt (paste this to continue)

Continue the **store-domain-objects migration** on branch `refactor/store-domain-objects`
(local, unpushed; tip `23a0e2d43`). Everything you need is in
`engdocs/plans/store-domain-objects/`.

**First, read (in this order):**
1. `engdocs/plans/store-domain-objects/HANDOFF.md` — full state, what's done, what remains,
   the execution loop, the discipline, and every gotcha. START HERE.
2. `engdocs/plans/store-domain-objects/work-items.md` — wave-by-wave status + all merge SHAs
   (the "Corrected remaining endgame" block near WI-6 lists the 4 remaining waves).
3. `engdocs/plans/store-domain-objects/tickfeed-design.md` §3 — the AUTHORITATIVE design for the
   4 remaining waves (W-pool → W-delete → W-flip → W-unexport). §5 has the honest codec-unexport verdict.

**Then execute the remaining 4 waves**, one at a time, each through the proven loop
(the design already exists, so no per-wave Fable design pass is needed — go straight to impl):

> **Opus impl (worktree-isolated, off the current migration tip) → Fable red-team → fix blockers by
> resuming the impl agent → integrate (`git merge --no-ff`, resolve the census-guard conflict by regen).**

- **Impl agent:** `Agent(subagent_type:"general-purpose", model:"opus", isolation:"worktree",
  run_in_background:true)`. Brief it with the wave's `tickfeed-design.md §3` section + the HANDOFF
  discipline (TDD, load-bearing oracles, census honesty, the front-door-Get bridge, no re-Get, honest
  under-reach). Two commits: A additive twins/oracles/pins, B migrate+delete+census.
- **Red-team:** `Workflow({scriptPath: "engdocs/plans/store-domain-objects/sdo-review.js",
  args:{key:"<wave>", base:"<base-sha>", head:"<impl-tip>", opportunity:"<one-para scope + the
  specific checks>", designPath:"<a /tmp brief>", verifyPath:"<a /tmp verify capturing the agent's report>"}})`.
  Feed it the riskiest checks explicitly. Treat `changes-needed` blockers as mandatory; resume the impl
  agent with the exact fix + demand a fail-then-pass mutation demo for any strengthened pin.
- **Integrate:** merge `--no-ff` onto `refactor/store-domain-objects`; the only conflict is
  `cmd/gc/typedclass_edge_guard_test.go` — take `--ours`, run `go test ./cmd/gc/ -run TestTypedClassCodecCensus`,
  paste its regenerated literal (keep the annotation comments). Confirm build+vet+census; then next wave.

**Wave order + gates (do NOT reorder — each frees the next):**
1. **W-pool** — types the pool create/reuse path; frees the raw snapshot half's class-(b) consumers.
2. **W-delete** — deletes the raw snapshot half (gated on W-pool); zeros `session_bead_snapshot`/`session_hash`/
   `session_logs_resolve` `InfoFromPersistedBead`. `ListAllSessionBeads` stays pinned (documented — do not force it to 0).
3. **W-flip** — front-door flip (`class_store.go` + `api.State`); migrates `cmd_session:cmdSessionKill` +
   `session_resolution` (the last two `InfoFromPersistedBead` interior sites). **Bridge every moved Get.**
4. **W-unexport** — `InfoFromPersistedBead` → `infoFromPersistedBead` (compiler boundary); zero
   `GetWithPersistedResponse`; guards → permanent zero-pins. Orders codecs (`RunFromTrackingBead`/
   `MaxSeqFromLabels`) stay — they're the deferred WI-3 residual, NOT this endgame.

**Definition of done:** `InfoFromPersistedBead` unexported (compiler-enforced boundary); census guard is
permanent zero-pins for the retired codecs; `make test-local-full-parallel` green once; the branch is ready
for review. Then STOP and report — do not push unless the user asks (Dolt is local-only → `git push` only).

**Model division:** Opus for impl, Fable for design/red-team. **Env:** `git commit --no-verify` (hooks hang);
shards SEQUENTIAL if `fork/exec` thread-capped; NEVER `go clean -cache` / `tmux kill-server`.
