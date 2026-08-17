# Next-session prompt (paste this to complete W-test-fixture)

Complete the **W-test-fixture wave** of the store-domain-objects migration on branch
`refactor/store-domain-objects` (local, unpushed; tip `d8e65dc35`). This is the FINAL wave: migrate the
~498 TEST sites that hand-craft `beads.Bead` literals and crack them into `session.Info` via
`session.InfoFromPersistedBead` over to **real store test doubles**, then unexport the codec
(`InfoFromPersistedBead → infoFromPersistedBead`) — the compiler boundary the DoD called for. The
substantive migration is already done (interior non-test `InfoFromPersistedBead` = 0; full suite green);
this removes the test smell and lands the unexport.

**Read first, in this order:**
1. `engdocs/plans/store-domain-objects/W-TESTFIXTURE-HANDOFF.md` — full operational state, execution
   order, discipline, the CRITICAL worktree base-ref gotcha, verify commands, DoD. START HERE.
2. `engdocs/plans/store-domain-objects/test-double-migration-plan.md` — the AUTHORITATIVE plan (14-agent
   categorization): site inventory by replacement category, the canonical `sessiontest` pattern, the
   edge-oracle disposition, the 4 human-decision points (defaults set), the phased execution, the risks.
3. `engdocs/plans/store-domain-objects/work-items.md` — migration history + all merge SHAs (context).

**Then execute, in order (each wave through the proven Opus-impl → Fable-red-team → fix → integrate loop):**
- **Phase 0 (land + merge FIRST):** add `internal/session/sessiontest/` (`Store`/`Info`/`InfoFromMeta`/
  `SeedBead`) + `reconcilerTestEnv.sessionInfo`/`createSessionInfo`. Verify + merge before fan-out.
- **Phases 1–9 (parallel, disjoint ≤5-file groups; big files solo):** convert each group to store test
  doubles / struct-literals / kept-raw per the plan. Census guard must stay UNCHANGED. Red-team each wave.
- **Phase 10 (LAST, gated):** repo-wide grep gate (external `InfoFromPersistedBead`/`PersistedResponseFromBead`
  callers must be zero) → lowercase the internal/session sites + the definition + delete the exported name →
  convert the census to a hard zero-pin (`InfoFromPersistedBead(` == 0; add `infoFromPersistedBead(` policed
  to zero in the interior). Full sharded suite + census green.

**Discipline (red-team enforces):** converted fixtures BEHAVIOR-IDENTICAL (same Info fields / on-store
bytes); use `SeedBead` for Status=closed/pinned CreatedAt/custom labels (`CreateSpec` can't express them);
degraded/non-round-trippable corpora STAY on the raw codec (front-door `Get` narrows via
`IsSessionBeadOrRepairable`); deliberately-divergent fixtures are struct-literals ONLY; some files use a
write-tracking MOCK store, not a memstore — verify write assertions still fire; `sessiontest` imports
`session` so internal/session white-box tests keep their own helpers; touch the census guard ONLY in Phase 10.

**Execution mechanics:** the branch is LOCAL/UNPUSHED and diverged from origin/main, so **do NOT use harness
`isolation:"worktree"`** (it branches from origin/main and would LOSE the migration). Create each impl
worktree MANUALLY off HEAD: `git worktree add -b sdo/<wave>-impl /data/projects/gascity-sdo-<wave> HEAD`,
gate the agent on `git merge-base --is-ancestor <tip> HEAD && echo BASE_OK`, launch a plain
`general-purpose` Opus agent working in that dir. Red-team via
`Workflow({scriptPath:"engdocs/plans/store-domain-objects/sdo-review.js", args:{...}})`. Integrate with
`git merge --no-ff`. Commit `--no-verify` (hooks hang); NEVER `go clean -cache` / `tmux kill-server`;
shards SEQUENTIAL if `fork/exec` thread-capped. Opus for impl, Fable for red-team.

**Definition of done:** `InfoFromPersistedBead` unexported; census a hard zero-pin; `make test-local-full-parallel`
green once; branch ready for review. Then STOP and report — do NOT push unless asked (`git push` only, Dolt local-only).
