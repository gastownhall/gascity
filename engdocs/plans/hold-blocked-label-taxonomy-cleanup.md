# Hold and Blocked Label Taxonomy Cleanup

Owner: `gascity/pm`
Created: 2026-07-14
Source bead: `ga-tug8ry`

## Goal

Consolidate the overlapping hold/blocked label family without losing
operational meaning or touching unrelated work. The cleanup starts with an
architecture decision because the current labels mix at least three concepts:
human/external holds, upstream blockers, and ordinary dependency/status
blocking.

Tracker import: no tracker skill was installed in this worktree, so no external
tracker issues were imported.

## Live Census

Queried with `bd label list-all` and exact `bd list --all --include-infra
--label ...` calls on 2026-07-14.

| Label | Count | Current usage summary |
| --- | ---: | --- |
| `arch-hold` | 2 | Both closed deployer beads: `ga-8nt0so`, `ga-khldol`. |
| `blocked` | 4 | Three closed historical beads and one blocked deployer bead: `ga-rrl2aq`. |
| `blocked-by-operator` | 1 | One blocked deployer bead: `ga-rrl2aq`. |
| `blocked-on-external` | 2 | Two blocked SEC-003 baseline beads: `ga-7psli2.3.3`, `ga-7psli2.3.2`. |
| `blocked-on-upstream` | 1 | One open mayor-routed review/deploy disposition bead: `ga-bxq5`. |
| `blocked-prereq` | 1 | One closed historical PR rebase bead: `ga-wa8qmq`. |
| `human-hold` | 6 | Five live/open-or-blocked mayor-routed PR #3579 beads plus one closed deploy bead. |
| `hold:mayor` | 1 | One closed worker-nudge bead: `ga-rncghg.1`. |
| `hold:external` | 0 | No current live usage yet; introduced by the deploy-gate treadmill design. |

## PM Disposition

Do not bulk-migrate labels from the PM seat. The live footprint is small, but
the meanings are not interchangeable. The safest plan is:

1. Architect records the canonical taxonomy and migration policy.
2. Builder applies only the approved mechanical label migration.
3. Builder documents the accepted rules for future agents/operators.
4. Validator reruns the census and verifies the result.

The PR #3579 `human-hold` cluster remains mayor-routed unless the mayor changes
that disposition. This plan is about label semantics, not resolving that PR.

## Work Packages

| Bead | Title | Routing | Gate |
| --- | --- | --- | --- |
| `ga-tug8ry.1` | Define canonical hold and blocked label taxonomy | `gascity/architect` | `needs-architecture` |
| `ga-tug8ry.2` | As a maintainer, I can migrate approved legacy hold labels without losing context | `gascity/builder` | `ready-to-build` |
| `ga-tug8ry.3` | As an operator, I can find the canonical hold label rules | `gascity/builder` | `ready-to-build` |
| `ga-tug8ry.4` | As a PM, I can verify legacy hold label drift stays resolved | `gascity/validator` | `needs-tests` |

## Dependency Graph

```text
ga-tug8ry.1 (taxonomy decision)
  -> ga-tug8ry.2 (approved migration)
  -> ga-tug8ry.3 (operator/agent documentation)

ga-tug8ry.2 -> ga-tug8ry.4 (verification)
ga-tug8ry.3 -> ga-tug8ry.4 (verification)
```

`ga-tug8ry.2` and `ga-tug8ry.3` can run in parallel after the architecture
decision. `ga-tug8ry.4` waits for both.

## Acceptance Summary

`ga-tug8ry.1` is complete when every legacy label has an explicit disposition:
migrate to a canonical `hold:<value>` label, keep as intentionally distinct, or
retire with no migration. The decision must distinguish bead status, dependency
edges, and hold labels, and must preserve current mayor-held PR #3579 routing
unless mayor explicitly changes it.

`ga-tug8ry.2` is complete when builder reruns the census, applies only the
architecture-approved label changes to exact bead IDs, preserves notes, status,
dependencies, and routing metadata, and records before/after counts plus any
skipped beads. If live state has materially drifted from the approved policy,
builder stops and routes back to architect.

`ga-tug8ry.3` is complete when documentation names the allowed labels, explains
when to use status or dependency edges instead, lists retired labels and their
replacement/no-op rule, and covers both `hold:external` and `hold:mayor`
without adding role-specific SDK behavior.

`ga-tug8ry.4` is complete when validator reruns the exact label census,
confirms the results match the architecture policy and builder after-counts,
confirms the documentation covers every allowed label, and files a follow-up if
an automated convention check is warranted but out of this slice.

## Risks

The main risk is collapsing labels that currently encode different operational
meanings. Routing the first step to architect avoids that. The second risk is
accidentally disrupting mayor-owned human holds, especially PR #3579; all
migration work must preserve routing and ownership unless a mayor decision says
otherwise. The third risk is recurrence through agents inventing a new label;
the documentation and validator verification steps address that.
