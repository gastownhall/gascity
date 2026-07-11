# Strategy R: revert → merge → reapply (landing store-domain-objects onto origin/main)

**Chosen by Julian 2026-07-11** after an empirical comparison (this doc + `commit-analyses.json`
are the execution spec). Goal: a PR against origin/main carrying the full store-domain-objects
migration (interior raw-bead cracks = 0, sessiontest doubles, census guard, `infoFromPersistedBead`
unexported) WITHOUT losing any of main's 50 post-fork commits. PR label: `status/needs-review-auto`.

## Why R (measured, not estimated)
- Direct merge: 37 files / ~130 tangled hunks (two parallel refactors colliding, incl. 48 in
  session_reconciler.go). The same 5 hard reconciliations exist inside the soup, un-factored.
- **R: 18/20 reverts clean → merge collapses to 7 files / 14 hunks → reapply 17 commits in
  order, each semantically coherent and carrying its own tests. 3 commits SKIP (value verified
  already on our branch). Honest cost: 5 hard re-implementation ports = 70–80% of effort;
  total 4–6 focused days (agent-pipeline compresses wall-clock).**

## State (as of this commit)
- Our branch: `refactor/store-domain-objects` @ `e4eb1d13b` (W-test-fixture COMPLETE: interior=0,
  tests=0 raw cracks, codec unexported, census hard-pin; batches 0–7 all Fable-red-team approved).
- Experiment worktree: `/data/projects/gascity-sdo-revertexp`, branch `sdo/revert-exp` =
  origin/main(abbfad090) + 18 reverts + a CRUDE merge (`--ours` on 7 files — MEASUREMENT ONLY,
  must be redone properly; reset to the revert-tip commit before the merge and re-merge).
- Collision set: `20` commits (list + per-commit stats in commit-analyses.json). Unrevertible: P12
  `3d70a1ab8` + API-dedup `e4bc0eb2c` (later kept commits build on them) — left in place; their
  residue IS the 14 merge hunks.

## SKIP LIST (verified equivalent on our branch — do NOT cherry-pick; merge restores the value)
- `e4bc0eb2c` API session-codec dedup — dual-landed on our branch (ancestor f8d10ec6f, identical).
- `d87453d67` WaitInfo codec — our twin 0a28424e7, then superseded by WI-4 (Store methods).
- `f42eff6db` nudge vocab — Store.SweepStale byte-identical on our branch; DecodeShadow reads superseded.
After the merge, VERIFY tree matches branch-tip content for Store.SweepStale / AssigneeIdentities / ListWaits.

## REAPPLY ORDER (main merge order — NOT S-number order; ordering trap is real)
`7efe9935f → 33d5f98a7 → 45a35983d → daf17356c → bb9c90d73 → 7eb0c7045 → 8c85a4d33 →
e4c6382ab(engdocs hunks ONLY) → 730a0b920 → 783da3ca5 → e72e34771 → 7c24516b5 → 14d1dbf60 →
738c11517 → 0061b41a6 → 3d70a1ab8 → 57c1fa5df`
Per-commit port guidance (what applies clean, what re-expresses, which functions are gone):
**`commit-analyses.json` in this dir — READ THE ENTRY BEFORE EACH PICK.**

## Difficulty tiers
- TRIVIAL (~6 hunks): e4c6382ab(docs-only), 14d1dbf60, 0061b41a6, 57c1fa5df.
- MODERATE (1.5–2 days): 33d5f98a7, daf17356c, bb9c90d73, 7eb0c7045, 8c85a4d33, 783da3ca5,
  7c24516b5, 3d70a1ab8(P12: regen generated artifacts by tooling; convert our branch-new waits
  handlers + Wake-409 into errorStatuses/catalog or closed-contract guards fail).
- HARD (70–80% of effort; each a focused wave): 7efe9935f(S36 ~200-line re-derivation; ALSO a
  mustKeep fix — drift-relaunch conversation loss), 45a35983d(S09b codec table — regenerate onto
  unexported infoFromPersistedBead + our ~17 extra Info keys; codec half deferrable but that
  reverts main's form — decide), 730a0b920(S23 fold — re-implement around ReconcileSession rows +
  extend mutator API + rescope source-scan guard), e72e34771(S20 — re-author onto Info forms; 3 new
  projected fields), 738c11517(S19 stage2 — largest, ~3600 lines; shadow harness needs a REAL
  design decision: priming-key Info mirrors vs ReconcileSession compared-key snapshot — decide
  BEFORE porting; its write-site completeness test is the arbiter).

## MUST-KEEP FIXES (behavior; dropping any = regression): 7efe9935f, daf17356c, bb9c90d73,
7eb0c7045, 8c85a4d33, 14d1dbf60, 0061b41a6, 57c1fa5df (+ P12 contract).

## KEY RISKS (full list in commit-analyses.json synthesis.risks)
- killExistingOrphans: resolve 7c24516b5's test conflicts toward bb9c90d73's fail-closed error
  form (already applied earlier in order) — NOT our branch's void form.
- S23's source-scan guard breaks later reconciler picks unless they route through the tick
  mutators; extend guard for write-returns-Info folds via a new set(id, Info) mutator.
- Never hand-merge generated artifacts (openapi.json, genclient, dashboard TS) — regenerate by
  tooling after each wire-touching pick; gate `make dashboard-check` + TestOpenAPISpecInSync.
- Reapplied test files must be authored sessiontest-style (raw-bead fixtures = instant debt +
  possible guard trips).
- Verify ErrRuntimeUnavailable (from kept #4082) exists in merged tree before picking 14d1dbf60.
- daf17356c un-gated: loadSessionBeads edge read runs every tick on all runtimes — review cost.

## EXECUTION WAVES (each: Opus impl in the revertexp worktree → Fable red-team via
sdo-review.js → gates → commit; DO NOT use harness worktree isolation)
- Wave M: reset sdo/revert-exp to revert-tip; re-merge e4eb1d13b; hand-resolve the 14 hunks
  (ours + P12 apierr reintegration + waits-handler contract conversion); build+vet+census green.
- Wave T: the 4 trivial picks.
- Wave Mo: the 8 moderate picks (parallelizable where files disjoint; P12 last of the tier).
- Wave H: the 5 hard ports, one focused wave each, in order (S36 → S09b → S23 → S20 → S19).
- Final: full sharded suite + make dashboard-check + whole-delta Fable red-team vs origin/main
  (sdo-review.js, base=origin/main, head=tip) + PR with label `status/needs-review-auto`.
  Verify the PR diff loses NOTHING from main (per-commit spot audit vs the 20).

## Gates every wave
gofmt/build/vet; census ratchet green; touched-package tests; each pick's OWN tests pass
(they validate the port); no exported InfoFromPersistedBead reintroduced (permanent-zero guard).
