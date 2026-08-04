---
title: Hold and Blocked Label Conventions
description: The canonical hold/blocked label taxonomy for this project's own bd tracker — which label to use, when to use status or a dependency edge instead, and what happened to the old ad hoc labels.
---

## Why this exists

Before 2026-07-14 this repo's bd tracker had accumulated at least 8 overlapping
ad hoc "hold"-family labels with unclear, possibly inconsistent semantics
(`arch-hold`, `blocked`, `blocked-by-operator`, `blocked-on-external`,
`blocked-on-upstream`, `blocked-prereq`, `human-hold`/`human`, `on-hold`),
alongside the one label that already followed the sanctioned convention,
`hold:mayor`. `ga-tug8ry` audited and consolidated them down to two canonical
values; `ga-tug8ry.2` migrated every live bead onto the result. This page is
the durable reference so nobody reinvents another ad hoc hold label — if
you're about to pause a bead and reach for a new label name, stop and use one
of the two values below instead.

Full rationale and the live census this decision was based on:
`bd show ga-tug8ry.1` (the decision) and `bd show ga-tug8ry.2` (the
migration record, including before/after counts).

## Three orthogonal "not ready" mechanisms

A bead can be "not simply ready to work" for three structurally different
reasons. Pick the mechanism that matches *why* you're pausing it, not just
"it's blocked":

| Mechanism | How to set it | Meaning |
|---|---|---|
| Dependency edge | `bd dep add <a> <b>` | Bead A cannot start until bd-tracked bead B closes. Gates `bd ready`. Computed from real edges, not a manual claim. |
| Bead status | `bd update <id> --status blocked` | "I cannot currently proceed," with no further structure about why or who must act. |
| `hold:<value>` label | `bd set-state <id> hold=<value> --reason "..."` | "I am paused pending a specific actor or condition." Structured, audited (files an event bead), and names the *who*. |

These are orthogonal and combine freely — a bead can be `status=blocked`
**and** `hold:external` at the same time. Use a dependency edge when the
blocker is itself a bd bead; use `status=blocked` when nothing more specific
applies; use `hold:<value>` only when a specific actor or external condition
is the actual reason you're paused.

## Canonical `hold:<value>` values

Only two values are canonical. Don't introduce a third without a new
architecture decision — see `ga-tug8ry.1` for the reasoning that narrowed
the taxonomy to these two.

- **`hold:mayor`** — the required next actor is the mayor. Covers both
  mayor-initiated pauses and automation-escalated-to-mayor cases; both are
  the same operational state ("nothing proceeds until the mayor acts") and
  share one value rather than being split in two.
- **`hold:external`** — the required next actor or condition is outside this
  bd instance's control (an external repo's maintainers, an upstream PR
  merge, etc.). Established by `ga-h7hnpt`.

Set either with the sanctioned command — never with a plain `bd label add`:

```bash
bd set-state <id> hold=mayor --reason "why, and who/what unblocks it"
bd set-state <id> hold=external --reason "why, and who/what unblocks it"
```

`bd set-state` removes any existing label in the `hold:` dimension, adds the
new one, and files an audit event bead. It does **not** touch `status`,
`owner`, or `metadata` — update those separately (or add a dependency edge)
if they also need to change.

## Retired labels

These labels are legacy. If you see one on a live bead, treat it as drift
worth a bug report, not a pattern to follow.

| Legacy label | Replace with | Notes |
|---|---|---|
| `blocked-by-operator` | `hold:mayor` | "Operator" meant the human operator/mayor seat. |
| `blocked-on-upstream` | `hold:mayor` | Means "next step in our own merge pipeline," not an external repo — despite the name, this is not a `hold:external` synonym. |
| `human-hold`, bare `human` | `hold:mayor` | Both named the same "next actor is mayor" state as a bare label. Caution: a bare `human` label can also appear alone for an unrelated reason (a human merge/PR action needed) that is not a hold state at all — check the bead's own context before assuming `human` implies a hold. |
| `blocked-on-external` | `hold:external` | Direct predecessor of `hold:external`; carry forward any `blocker_scope`/`external_blocker`/`external_pr`/`pr`/`repo` metadata unchanged. |
| `blocked` | none — use native `status=blocked` | Redundant with the bead's own `Status` field; keeping both invites drift between them. |
| `arch-hold` | none — owned by the `maintainer-pr-review` pack | Not a generic bd hold; it's that pack's own gate, cleared via `gc maintainer-pr-review clear-hold`. It only looked like one of ours because it lacks the `mpr-` prefix its sibling `mpr-human-hold` carries. |
| `blocked-prereq` | none today; if it recurs, use a dependency edge (prerequisite is a bd bead) or `hold:external` with PR numbers recorded in metadata (prerequisite is bare GitHub PR numbers) | Historical: blocked on specific GitHub PRs merging first, with no corresponding bd bead. |
| `on-hold` | none — already superseded | Any bead needing this should already carry the canonical `hold:mayor`/`hold:external` in its place. |

**Explicitly out of scope — do not migrate these, they mean something
different:**

- `mpr-human-hold` and other `mpr-*` labels — owned end-to-end by the
  `maintainer-pr-review` pack, with its own metadata namespace and its own
  clearing tool. Not a generic bd hold label.
- `build-blocker`, `ci-blocker`, `pre-push-blocker`, `push-blocking`,
  `test-blocker` — a different semantic axis ("pipeline stage X is red
  because of me"), not "I am waiting on decision-maker Y."
- `needs-mayor` / `needs-mayor-decision` — a routing/queue-placement label
  (parallel to `needs-architecture`, `needs-design`, `needs-pm`,
  `ready-to-build`), not a pause-state label. It may legitimately co-occur
  with `hold:mayor`.

## This is a data convention, not SDK behavior

Nothing in this page requires or implies special-casing any role name in Go.
`hold:mayor` and `hold:external` are plain label values in this project's own
bd data, chosen and enforced by convention — this document, PR review, and
`bd set-state`'s dimension semantics — not by SDK code. Gas City's "ZERO
hardcoded roles" invariant is unaffected: nothing under `internal/` or
`cmd/gc/` branches on the literal label value `hold:mayor` or `hold:external`.

## See also

- `bd show ga-tug8ry.1` — the architecture decision: full live census,
  per-label disposition rationale, and a label-flow diagram.
- `bd show ga-tug8ry.2` — the migration record: before/after counts and the
  beads intentionally skipped (bare `human` used for an unrelated reason).
- [Beads architecture](../architecture/beads.md) — the generic `Label` and
  `Store` mechanism this convention is built on.
