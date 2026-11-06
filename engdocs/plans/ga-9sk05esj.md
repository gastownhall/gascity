# Plan: expose session pool membership to pack scripts

- Root: `ga-9sk05esj`
- Generated: 2026-11-06
- Source reviewed: `2f9493f0c45`
- Branch: `docs/ga-9sk05esj-session-pool-membership`

Pack scripts need to distinguish a pool-managed session from a named session
that uses the same template. `gc session list --json` currently omits that
information on both its local and API-served routes. This followup came from
the review of PR #7034 and was not fixed by that PR.

## Evidence and boundaries

- `cmd/gc/cmd_session.go` defines a shared `sessionListJSONRow` and local/API
  builders that omit pool membership. Commit `2a7681066bb` (PR #8712)
  established the shared document; preserve its compatibility guarantees.
- `internal/api/handler_sessions.go` includes `pool` and `agent_kind`, but
  derives them from the configured template. That grouping does not by itself
  distinguish individual named and pool sessions of the same template.
- The session domain already projects identity and origin into typed
  `session.Info` fields. Its existing membership semantics are authoritative.
  Field naming and projection placement remain engineering decisions under the
  existing domain, worker, and typed wire boundaries.
- The originating review run is `20261105T161033Z` under the PR #7034 review
  directory. Its multi-member named-session finding explains why counting
  members is insufficient.
- No tracker importer is installed. Step `ga-49q4igsg` completed as a no-op;
  the root records `tracker_sync: none`.

## Acceptance

1. Scripts can identify actual pool-managed sessions through explicit JSON
   data, including a collapsed singleton pool, while excluding named/manual
   sessions of the same template. Consumers need no member-count, alias, or
   name-suffix guesses. Preserve the session domain's existing rules.
2. Equivalent records agree on membership across local and API-served reads.
   Older-server responses without the signal have documented and tested
   unknown/unsupported behavior; template grouping is never silently treated
   as individual membership.
3. Preserve the existing envelope and schema compatibility, other row fields,
   filters, summaries, empty lists, table output, closed history, provider
   semantics, and remote redaction.
4. Record a failing regression before production changes and passing focused
   Bazel evidence afterward. Cover the mixed named/pool template, collapsed
   singleton pool, both read routes, and absent older-server data at the
   smallest owning layers, reusing existing test owners.
5. Document the public field contract and a selection example at the
   appropriate source; regenerate derived references and clients as needed.
6. Required repository and relevant acceptance gates pass. API/schema changes
   also require the dashboard gates and local serve proof in
   `internal/api/AGENTS.md`. Complexity has zero growth against the pinned
   base under the pack's full complexity-zero-growth protocol. This feature
   does not edit `engdocs/complexity-baseline.json`.

## Work package and routing

| Bead | User story | Intake | Handoff |
| --- | --- | --- | --- |
| `ga-1snja6e3` | As a pack author, distinguish pool-managed sessions from named sessions in session-list JSON | `needs-tests` | validator, then builder on the same bead |

The child's notes carry both acceptance and `Build:` scope: expose the
existing membership truth through typed local/API session-list projections,
including the necessary contract documentation and generated artifacts.

This is one bounded defect. Keep one work item for tests and implementation,
as permitted by the decompose step's 1–10-child exit criterion. A second
build bead would duplicate the scope. No UI design is needed. A genuinely
new membership policy requires an architect ruling rather than an implicit
transport-specific rule.

## Dependency graph

```text
ga-9sk05esj (PM planning record)
  <- discovered-from: ga-1snja6e3
     needs-tests -> validator -> ready-to-build -> builder
```

The discovery edge is provenance, not a blocker. There are no prerequisite
work beads and no blocking edges to add; the one-node work DAG is acyclic.
Both records live in the gascity store. No rollup-ship bead is needed for this
single scope.

## Verification and completion

The downstream child owns RED/GREEN behavior evidence and implementation
checks. PM changes only this plan and bead records, verifies docs sync and
repository gates, commits the plan on its bead-specific branch, and rechecks
that its exact path is tracked and clean before closing the planning root.
The handoff slings the child to `gascity/validator` with `--nudge`, reads the
route back, and verifies the context mail with `gc mail peek`.

Nudge/backstop behavior, pack routing policy, and the underlying lifecycle
rules are outside this followup's scope.
