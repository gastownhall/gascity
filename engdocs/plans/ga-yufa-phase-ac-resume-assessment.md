# Plan: ga-yufa Phase A/C resume assessment

> Owner: `gascity/pm` - Created: 2026-07-13
> Source decision bead: `ga-igcny0`
> Operator decision mail: `gm-wisp-met7x`
> Decomposed into: 1 architecture bead

## Context

Mayor decided to lean RESUME for the ga-yufa Phase A/C performance work that
was reviewed PASS but never reached `origin/main`. Phase D (`ga-yufa.4`) stays
blocked until the real Phase A/C fixes are either landed and soaked or formally
declared superseded with evidence.

The current tracker history is not reliable enough to route builders directly:
several closed beads represent reviewed implementation work, but dependency
closure did not prove merge ancestry. Architect must re-check current
`origin/main` and identify the valid resume path first.

## Children

| ID | Title | Routing label | Routes to | Depends on |
| --- | --- | --- | --- | --- |
| `ga-igcny0.1` | Assess resume path for stranded ga-yufa Phase A/C perf slices | `needs-architecture` | `gascity/architect` | - |

## Acceptance Rollup

The architecture handoff is complete when `ga-igcny0.1` records:

- Which Phase A/C reviewed-PASS slices are already ancestors of current
  `origin/main` and which are absent.
- The explicit stranded slice set, including source bead, branch/commit where
  available, review status, and supersession evidence.
- A divergence classification for each absent slice: `resume-clean`,
  `resume-with-rework`, `superseded`, or `unsalvageable`.
- The recommended resume/rebuild order and dependency graph for salvageable
  slices.
- Any superseded or unsalvageable slice evidence clear enough for
  mayor/operator review.

## Routing Rationale

This routes to `gascity/architect` because the next decision is architectural:
determine whether validated but stale implementation slices still compose with
current main, and define the safe dependency order before implementation is
routed. PM should not decide which code branches are technically salvageable.

## Risks

- Reusing closed dependency status as the unblock signal would repeat the
  original failure mode. Main ancestry must be checked directly.
- Routing builders before the divergence assessment could create another
  stale stacked-branch loop.
- Phase D must remain blocked until Phase A/C are actually present on main and
  live-soak evidence exists.

## Out of Scope

- Rebuilding or rebasing any implementation branch.
- Changing the ga-yufa technical design.
- Declaring Phase D ready for build.
