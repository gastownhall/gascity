---
title: "Executor-Identity Stamp Lifecycle On Re-Route"
---

Source bead: `ga-cm2o5t`
Generated: 2026-09-06 by `gascity/planner`
Type: internal reliability/infra PRD (no end-user UI surface)

## Problem Statement

`stampRunSessionIdentity` (`cmd/gc/build_desired_state.go:5006-5072`) durably
stamps `gc.session_name` and `gc.work_dir` onto every `in_progress` assigned
work bead, copied from the session currently *executing* it. This makes the
transient session↔bead link (`Assignee`, cleared on close) durable, so the
dashboard's session-drill-in and per-run-diff panels can resolve which
session ran a completed bead and in which worktree — without any consumer
changes.

Nothing clears those stamps when a bead is later **re-routed** to a
different executor via `gc.routed_to`. The stamp then points at the *prior*
executor's worktree, not the new one. Worked example (`be-lr5hd`): the
reviewer correctly slung the bead to the builder for round-1 rework
(`gc.routed_to=beads/builder`, correct and intentional), but its
`gc.session_name`/`gc.work_dir` stamps still named the reviewer's worktree.
That stale stamp was consumed downstream as if it were current, and became
one of the inputs that produced the `ga-hunfla` pool-seat-churn
misdiagnosis (hours of silent "creating" state, root-caused separately as
`ga-2ygo4s`).

A fleet-wide audit run 2026-09-06 19:26Z (gc-management, gascity, MCDClient,
beads, hold-court; pool slots normalized so `rig/role-N` and `rig/role` are
treated as the same executor) found **57 open/in_progress beads** carrying a
conflicting work-dir stamp: 29 where `gc.work_dir` names a different agent
than `gc.routed_to`, 20 where legacy `work_dir` and canonical `gc.work_dir`
disagree, 8 with both (gascity 55, beads 2). Only one, `be-v33pa`, was
`in_progress` — a live launch hazard — at audit time. **This 57-count is a
point-in-time snapshot, not a durable list**: bead state changes
continuously, so by the time this fix is designed and built, the live
residue set will have drifted from the audited one.

Mayor ruled on this 2026-09-06 19:29Z (`ga-6af29d`, DECISION 1): fix = clear
`gc.session_name`/`gc.work_dir` (and legacy `work_dir`) on re-route so the
next executor re-stamps on pickup; sweep = one-shot cleanup of the residue,
touching non-`in_progress` beads only. This PRD formalizes that ruling into
requirements for the architect. It does not reopen the ruling itself.

**Explicit companion boundary.** `ga-2ygo4s` (routed to `gascity/builder`,
`ready-to-build`, molecule `ga-34583y`) fixes a *different* bug found in the
same investigation — the async-start command gate's exact-string-equality
check — and is already fully specified and in the builder pipeline. It must
not be merged with this work. Relatedly, PR #6066 (`ga-rpvye5`, open)
removes `gc.work_dir` as session-launch *authority* (the read side of a
related hazard) but does not clear stale stamps or touch the write-side
lifecycle gap this PRD covers — the two are complementary, not overlapping,
and carry no merge-order dependency on each other.

**A note on this document's location.** This PRD was originally drafted at
the long-established planner convention path `docs/PRD.md` (overwritten
per-cycle, history preserved via git log). That path now fails
`TestEveryDocsPageIsPublished` (`test/docsync/docsync_test.go`, added
2026-08-24, commit `e736d74d0a`), which enforces that every file under
`docs/` — a tree published live to docs.gascityhall.com — appear in
`docs/docs.json`'s navigation. A churn-in-place internal PRD is not a
published product page, and the test's own comment is explicit that
engineering docs "do NOT belong in docs/ at all — move them under engdocs/
instead of adding an exemption." This PRD is filed here, under
`engdocs/proposals/`, accordingly. See `ga-f11x81` (References) for the
fleet-process gap this reveals (the planner role's own prompt template
still names `docs/PRD.md`).

## Goals

1. Re-routing a bead (`gc.routed_to` changes to a genuinely different
   executor) reliably clears the prior executor's identity stamps
   (`gc.session_name`, `gc.work_dir`, legacy `work_dir`) so the next
   executor's pickup re-stamps fresh values. No bead should ever display a
   stale worktree/session after a legitimate handoff.
2. The audited residue of pre-existing conflicting stamps is remediated in a
   single, safe, auditable sweep, touching only non-`in_progress` beads.
3. The fix is a durable lifecycle rule covering every code path that can
   change `gc.routed_to` (not just `gc sling`), so the residue cannot
   silently regrow from an uninstrumented call site.
4. `stampRunSessionIdentity`'s existing behavior for beads that have *not*
   been re-routed is unchanged — this is a targeted lifecycle fix, not a
   redesign of the stamping mechanism the dashboard already depends on.

## Non-Goals

- Changing `stampRunSessionIdentity`'s stamp-*write* behavior itself (out of
  scope; this PRD covers only the clear-on-change side).
- The async-start command-gate fix (`ga-2ygo4s`) — different subsystem,
  already specified and owned by builder.
- DECISION 2's stale-discard-storm escalation policy — mayor folded this
  into `ga-2ygo4s`'s spec for builder to add as a separate commit; not this
  PRD's scope.
- Changing `gc.work_dir`'s role in session-launch directory *selection* —
  that is PR #6066 / `ga-rpvye5`'s scope (read side); this PRD is the write
  side (stamp lifecycle).
- Re-litigating DECISION 1 itself (clear-on-reroute; non-`in_progress`-only
  sweep) — final per mayor's ruling on `ga-6af29d`, 2026-09-06 19:29Z.
- Deciding the planner role-prompt/convention fix for where future PRDs get
  filed — flagged as a follow-up bead, not decided here.

## User Stories

1. **As a bead's next executor** (any role picking up a freshly re-routed
   bead), I want `gc.session_name`/`gc.work_dir` to reflect either nothing
   (freshly cleared) or my own identity once I claim it — never the
   *previous* executor's worktree — so nothing launches against a stale,
   wrong-owner path.
2. **As the dashboard** (or an operator reading it), I want the
   session-drill-in panel to resolve the *correct* executing session for a
   bead's most recent run, even across one or more re-routes over its life.
3. **As mayor** (or any role issuing a correction via `gc sling` or a
   doctor-repair command), I want my correction to take full effect
   immediately, with no dangling stale-identity artifact requiring a manual
   follow-up cleanup or feeding a future misdiagnosis — as happened with
   `be-lr5hd` feeding the `ga-hunfla` investigation.
4. **As the investigator/on-call role** auditing fleet health, I want the
   current residue remediated in one safe pass, with no risk of clobbering
   an actively-running bead's live identity stamp.
5. **As a future contributor** adding a new code path that writes
   `gc.routed_to`, I want a mechanism I can't forget to use — not a
   convention I have to remember to hand-apply at my new call site.

## Functional Requirements

| ID | Requirement | Priority | Acceptance Criteria |
|----|-------------|----------|---------------------|
| FR-1 | **Clear-on-change trigger.** When a work bead's `gc.routed_to` changes to a genuinely different resolved executor, `gc.session_name`, `gc.work_dir`, and legacy `work_dir` are cleared (deleted, not blanked) in the same logical operation. | Must | Unit test: bead with `routed_to=A`, `session_name=X`, `work_dir=Y`; route-change path sets `routed_to=B`; resulting bead has none of the three keys. |
| FR-2 | **No false-positive clears on non-relocating rewrites.** A `gc.routed_to` rewrite that resolves to the *same* logical executor (e.g. canonicalizing a pool-slot suffix, or a doctor/repair pass re-asserting an already-correct value) must NOT clear the identity stamps. | Must | Unit test covering at least one canonicalization site (e.g. `demand_serve_predicate.go`'s `routeCollapseRewriteTarget`) confirms no clear when old and new resolve to the same normalized target via `agentutil.NormalizePoolRouteTarget`. |
| FR-3 | **Applies uniformly across all route-changing call sites.** ~15+ non-test call sites write `beadmeta.RoutedToMetadataKey` today (`cmd_sling.go`, `sling_attachment.go`, `route_recovery.go`, `route_recovery_lane.go`, `pool_detached_orphan_sweep.go`, `detached_orphan_lane.go`, `convergence_store.go`, `order_dispatch.go`, `cmd_order.go`, `cmd_convoy_dispatch.go`, `wisp_step_inject.go`, `cmd_github.go`, `demand_serve_predicate.go`, three `doctor_*.go` repair tools, `build_desired_state.go`'s own control-dispatcher repair). The fix must cover genuine re-routes through all of them without requiring each future call site author to hand-apply the clear. | Must | Architect's chosen mechanism (see Open Questions) is exercised by a test per call-site *class*, not just `gc sling`. |
| FR-4 | **Re-stamp on pickup, not on clear.** Clearing must not itself write a new identity — no executor is executing yet at re-route time. The existing `stampRunSessionIdentity` path re-populates the fields once the new target's session claims and begins executing the bead, exactly as for a freshly-created bead. | Must | Integration test: after a clear, the next `in_progress` pass with the new assignee's session stamps fresh values with no manual intervention. |
| FR-5 | **One-shot residue sweep, non-`in_progress` only.** A one-time remediation clears `gc.session_name`/`gc.work_dir`/legacy `work_dir` on every bead in the conflicting-stamp set, **except** any bead whose status is `in_progress` at sweep execution time. | Must | Sweep's candidate query provably excludes `status=in_progress`; dry-run output lists 0 in_progress beads touched. |
| FR-6 | **Sweep re-derives its target set at execution time.** The 57-bead/29-20-8 split is a 2026-09-06 19:26Z snapshot, not a durable list — no full 57-ID enumeration was persisted in bd, only 10 representative IDs (`ga-drlztz`, `ga-05ci52`, `ga-45tz5p`, `ga-ujjbiw`, `ga-yshs1v`, `ga-lfmahe`, `ga-3rv7ov`, `ga-1x7ezi`, `ga-jaj5wi`, `ga-csg1lu`) plus `be-v33pa`/`be-lr5hd`/`ga-tcyghc` as named spot-checks. The sweep must re-run the audit query (conflicting `gc.work_dir` vs. `gc.routed_to`, legacy vs. canonical `work_dir`, pool-slot-normalized) across the same repo scope at execution time and act on whatever it finds then. | Must | Sweep tool re-queries live state; the 10 representative IDs plus the 3 named spot-checks are used as regression fixtures, not as the sweep's input list. |
| FR-7 | **Sweep is idempotent.** Running the sweep twice produces no additional writes the second time. | Should | Matches the idempotent-by-design convention already documented on `stampRunSessionIdentity` itself. |
| FR-8 | **Sweep is auditable.** The sweep reports exactly which bead IDs were modified and which fields were cleared on each. | Should | Mirrors the existing `doctor_*.go` repair-tool convention of per-bead repair reporting. |

## Non-Functional Requirements

| ID | Requirement | Metric |
|----|-------------|--------|
| NFR-1 | **Zero impact to `in_progress` beads.** Neither the lifecycle rule nor the sweep ever writes identity-stamp fields on an `in_progress` bead, except via the existing `stampRunSessionIdentity` re-stamp path. | Verified by unit test asserting the sweep's candidate query excludes `status=in_progress`. |
| NFR-2 | **No hardcoded role names.** Per `AGENTS.md`'s "ZERO hardcoded roles" invariant, the fix operates purely on metadata field values, never on a specific role name. | Code review / grep for role-name literals in the diff. |
| NFR-3 | **SDK self-sufficiency.** The clear-on-change rule is reconciler/storage-layer logic, not role-specific behavior; it must function with only the controller running. | Test: removing all `[[agent]]` entries does not break the rule. |
| NFR-4 | **Bounded write budget.** If implemented inside `build_desired_state.go`'s reconciliation pass, it must respect the same per-pass write-budget discipline as neighboring repair logic (see `controlDispatcherRouteRepair.persist`'s `writesRemaining` pattern), so a large sweep cannot starve normal reconciliation writes in one tick. | Code review against existing pattern. |
| NFR-5 | **No panics; best-effort.** A write failure during clear-on-change or during the sweep is logged and skipped, never a panic, and never blocks reconciliation. | Matches `stampRunSessionIdentity`'s existing convention (see its doc comment). |

## Technical Constraints

Derived from `AGENTS.md`:

- **No upward dependencies / Layer 0 confinement** — side effects stay
  confined to the reconciler/storage layer already used by
  `stampRunSessionIdentity` and its neighbors.
- **Beads is the persistence substrate; no status files** — the sweep must
  not write PID/lock/progress files; re-derive state by querying bd.
- **Zero hardcoded roles.**
- **Atomic writes; no panics in library code.**
- **TDD** — red test first, matching the convention `ga-2ygo4s` already
  demonstrated for the sibling bug in this same investigation.
- Must NOT modify `stampRunSessionIdentity`'s write-side stamping logic,
  `resolveTaskBeadWorkDir` / `gc.work_dir` launch-authority logic (PR
  #6066's scope), or `asyncStartPreparedCommandStaleInfo` /
  `shouldPreserveStoredRuntimeCommand` (`ga-2ygo4s`'s scope) — all
  explicitly reserved to other work.

## Dependencies

- `internal/beadmeta/keys.go` — `RoutedToMetadataKey` (`gc.routed_to`),
  `SessionNameMetadataKey` (`gc.session_name`), `WorkDirMetadataKey`
  (`gc.work_dir`), `LegacyWorkDirMetadataKey` (`work_dir`) — the four fields
  this feature reads/writes.
- `cmd/gc/build_desired_state.go:5006-5085` (`stampRunSessionIdentity`,
  `workDirStampHasOwnershipEvidence`) — the existing stamp-write path this
  feature complements; must keep working unchanged for freshly-claimed
  beads.
- The ~15+ existing non-test call sites that write
  `beadmeta.RoutedToMetadataKey` (enumerated in FR-3) — architect must
  decide which of these the fix instruments, or whether a lower-level choke
  point (`internal/beads.Store` interface, `internal/beads/beads.go:692`)
  covers all of them at once.
- `internal/agentutil/resolve.go:229` (`NormalizePoolRouteTarget`) —
  existing pool-slot canonicalization helper; the sweep's audit query and
  FR-2's "same logical executor" check should reuse this rather than
  reimplementing pool-suffix normalization.
- PR #6066 / `ga-rpvye5` (open, unmerged) — removes `gc.work_dir` as launch
  authority (read side). Related but independent; no merge-order
  dependency identified.
- `ga-2ygo4s` (open, routed to `gascity/builder`, `ready-to-build`) — the
  async-start command-gate fix. No code dependency; both trace back to the
  same `ga-hunfla` investigation and should be cross-referenced in release
  notes.
- `ga-6af29d` (closed) — the mayor ruling this PRD formalizes (DECISION 1,
  comment dated 2026-09-06 19:29Z).

## Open Questions

### For the architect

1. **Choke point selection.** Where does "clear identity stamps on
   `routed_to` change" live: (a) a single interception point in the
   `beads.Store` implementation(s) that detects any write changing
   `RoutedToMetadataKey`'s *resolved* value and clears the identity keys as
   a side effect, covering all ~15+ call sites automatically; (b) a shared
   helper each call site is migrated to call explicitly; or (c) instrument
   only the `gc sling` path (`cmd_sling.go` / `sling_attachment.go`) on the
   theory that sling is the only path representing a genuine
   executor-changing "re-route," and the other ~14 sites are
   canonicalization/repair passes that should already resolve to the same
   logical target (in which case FR-2 makes the clear a no-op there
   anyway). Recommend evaluating (c) first as the smallest, lowest-risk
   change; fall back to (a) only if a genuine re-route is found through a
   non-sling path.
2. **"Changed" comparison semantics.** Should change-detection use raw
   string equality or resolved/normalized equality (via
   `NormalizePoolRouteTarget`)? FR-2 requires the latter for canonicalization
   passes; confirm this is correct for *all* ~15 call sites, or whether some
   (e.g. `pool_detached_orphan_sweep.go`, which reassigns an actually
   orphaned bead to a live executor) represent genuine re-routes even when
   the string form looks like a normalization.
3. **Sweep execution shape.** One-shot CLI command (matching the existing
   `doctor_*.go` convention already neighboring this code) vs. a
   migration-style script vs. folding into the next reconciliation pass
   behind a one-time marker. Architect to choose, consistent with how
   similar one-shot residue cleanups are conventionally shaped in this
   codebase.
4. **Cross-repo execution.** The audit scope was gc-management, gascity,
   MCDClient, beads, hold-court (only gascity and beads actually had
   conflicts). Confirm whether the sweep needs to run once per repo's bd
   store or can run centrally against every store the executing session can
   reach.
5. **Legacy `work_dir` retirement (informational, non-blocking).** Given PR
   #6066 already narrows `gc.work_dir`'s role and this PRD clears both
   legacy and canonical keys on re-route, is there appetite to deprecate
   the legacy key in a follow-up, or must both be maintained in parallel
   indefinitely?

### For the designer

Not applicable — this is a backend/reconciler reliability fix with no UI/UX
surface. The dashboard's session-drill-in panels are a downstream consumer
of the stamped fields, not a UI this feature modifies (Goal 4 constrains the
fix to preserve their existing read behavior unchanged).

## References

- Source bead: `ga-cm2o5t`
- Decision record: `ga-6af29d` (DECISION 1, mayor ruling, comment dated
  2026-09-06 19:29Z)
- Root-cause investigation: `ga-hunfla` (`gascity/investigator`, closed
  2026-09-06)
- Companion fix, different subsystem, do not merge together: `ga-2ygo4s`
  (`gascity/builder`, `ready-to-build`, molecule `ga-34583y`)
- Related open PR: #6066 / `ga-rpvye5` ("Stop observed work directories
  from overriding session launches")
- Follow-up fleet-process bug filed by this PRD's author: `ga-f11x81`
  (`needs-mayor`, decision) — the planner-convention/docsync-gate conflict
  this PRD's filing location documents above.
- Code: `cmd/gc/build_desired_state.go:5006-5085` (`stampRunSessionIdentity`
  and neighbors), `internal/beadmeta/keys.go` (metadata key constants),
  `internal/agentutil/resolve.go:229` (`NormalizePoolRouteTarget`)
