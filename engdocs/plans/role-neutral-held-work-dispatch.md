# Plan — Role-neutral held-work dispatch

**Bead:** `gm-sn1zd`
**Rig:** `gascity`
**Branch:** `gc-pm-b8e04a1ccdb4`
**Supersedes:** `ga-vxkuqm` as a standalone triage item

## Outcome

Gas City must keep intentionally paused beads out of automatic routing and
recovery while preserving explicit assignment semantics. The behavior must be
role-neutral: generic Go code cannot depend on a configured role name or on a
project-specific label whose meaning exists only by convention.

This plan decomposes city intake `gm-sn1zd` into an architecture decision,
production-path regression tests, and implementation. The sequence prevents a
fourth apparently green fix from reaching merge authority without exercising
the code that actually serves work.

## Why the original fix is not sufficient

The July 4 report described a `bd-human` custom flag. Feature archaeology found
three later attempts and two corrections:

- PRs #3950, #4775, and #4782 filter the retired bare `human` label. Their
  `cmd/gc` change edits a generated shell that the production control-ready
  cache path bypasses. They must not merge.
- PR #4787 moves the control-ready filter to an executing production path and
  tests cache and fallback behavior. It is still not a complete contract:
  it embeds `hold:mayor` in generic Go, while
  `engdocs/contributors/hold-label-conventions.md` defines hold labels as
  project data conventions rather than SDK behavior.
- Tier-1 crash recovery can repeatedly reclaim long-held work. The observed
  symptom was tracked by `ga-vxkuqm`; that bead is superseded by the broader
  architecture decision below.

The remaining product requirement is therefore not “add one more label
filter.” It is “define and enforce a role-neutral paused-work contract across
every automatic work-selection path.”

## Work packages

| Bead | Route | Depends on | Deliverable |
|---|---|---|---|
| `ga-5736js` | architect (`needs-architecture`) | — | A role-neutral contract covering ambient routing, explicit assignment, recovery tiers, and control-ready cache/fallback behavior; includes a verdict on PR #4787 and the older superseded PRs. |
| `ga-x9kptu` | validator (`needs-tests`) | `ga-5736js` | RED regression tests that exercise production entry points, not only generated query strings, across every path named by the architecture decision. |
| `ga-0b2tc0` | builder (`ready-to-build`) | `ga-5736js`, `ga-x9kptu` | The approved implementation, green tests, role-neutral Go, and a merge-authority handoff that reconciles the stale PRs. |

Dependency flow:

```text
ga-5736js architecture
        |
        v
ga-x9kptu production-path tests
        |
        v
ga-0b2tc0 implementation and PR reconciliation
```

## Acceptance rollup

The initiative is complete when:

1. The architecture decision identifies a generic data or configuration
   contract for paused work and preserves Gas City's zero-hardcoded-roles
   invariant.
2. The contract explicitly covers ambient routed work, deliberately assigned
   work, Tier-1 crash recovery, Tier-2/3 ready selection, and control-ready
   cache plus fallback execution.
3. Tests demonstrate a RED failure on the relevant baseline and drive the
   production entry points for every path in scope.
4. The implementation makes those tests pass without provider-specific,
   beads-backend-specific, or role-specific logic leaking into generic paths.
5. Merge authority receives an explicit disposition for PRs #3950, #4775,
   #4782, and #4787 so no superseded verdict can merge later by accident.
6. Task-scoped tests, `make test-fast-parallel`, and `go vet ./...` pass with
   their evidence recorded on the implementation bead.

## Risks and controls

- **Premature merge:** PR #4787 is open and green but conflicts with the
  settled role-neutrality invariant. Notify the mayor and hold merge action
  until `ga-5736js` records the contract and disposition.
- **False-positive verification:** String-level query assertions previously
  passed while production bypassed the edited query. `ga-x9kptu` requires
  production-entry-point tests and recorded RED evidence.
- **Duplicate work:** The original implementation and relands remain useful as
  evidence, not as parallel build targets. `ga-vxkuqm` is superseded by
  `ga-5736js`; the new builder starts only after architecture and tests close.
- **Scope drift:** The architect decides automatic-versus-explicit assignment
  semantics. The builder must not broaden the change for symmetry.

## Handoff

`ga-5736js` is the only immediately actionable package. The validator and
builder packages are dependency-blocked and should not begin until their
prerequisites close. All three carry their routing labels and measurable
acceptance criteria in bead notes.

## Planning-system note

The repository moved internal PM artifacts to `engdocs/plans` in `512b066e8`.
The active PM prompt still names `docs/plans`; open bead `ga-f74ph9.1` owns that
pack-level correction. The public `docs/` tree rejects unpublished engineering
plans through `TestEveryDocsPageIsPublished`, so this artifact follows the
current repository decision and is committed from `engdocs/plans`.
