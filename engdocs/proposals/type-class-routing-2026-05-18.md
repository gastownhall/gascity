---
title: "gc hook type-class routing with shadow migration and unrouted-bead lint"
anchor_commit: 9f954698c456f36b90aea14f5d8bbf9f498f2e44
draft_date: 2026-05-18
status: proposal — awaiting acceptance
forked_from: gc-240h9q (parent bug: pool workers claiming session-type and other non-work beads from `gc hook`)
parent_bead: gascity-lh7
---

## Context

`gc hook` is the pool worker's demand-detection query. It runs the agent's
`EffectiveWorkQuery` and returns a single ready bead the worker should claim.
The current default query (`internal/config/config.go:2335-2424`) has three
tiers — in-progress assigned, ready assigned, ready unassigned routed — and
applies exactly one type exclusion: `--exclude-type=epic`.

That exclusion is incomplete. The bd type registry contains many bead
types that are not executable work but DO appear in `bd ready` because
they are open and unblocked: `session` (agent-lifecycle records),
`wisp` (ephemeral molecules), `convoy` (sling-batch wrappers),
`molecule` (workflow containers), and `message` (mail). In May 2026,
two pool workers (`claude-10` / `gc-2hxjf5` and `claude-1` / `gc-yb05im`)
each claimed a session-type bead (`gc-h0kxgf`, a sleep record for agent
`claude-1`) within a 90-minute window, analyzed it, and drained without
doing any real work. The incident is filed as `gc-240h9q`.

A naive whitelist fix (`--exclude-type=session --exclude-type=wisp
--exclude-type=convoy ...`) closes the immediate bug but creates a new
silent failure mode: any future bd built-in type, any operator-defined
custom type, any rename in bd upstream, drops out of the worker query
without alerting anyone. Stranding is the worst kind of failure because
nobody notices. Beads sit ready, no agent claims them, no log line
fires.

The user (Corey, in mayor session `e63742fa-6530-436b-adbe-38af8eedc028`,
2026-05-18) rejected the whitelist approach explicitly and asked for a
durable design. The stated requirements:

1. Type-class routing where each bd type carries an execution-class
   metadata field.
2. Haiku-pool tier is opt-in only — never a silent fallback for unknown
   or missing class.
3. Unknown-class beads warn the operator (debounced) and route to the
   default executable pool so work still gets done.
4. Cumulative cost-class-miss spend is tracked separately so aggregate
   drift surfaces even when individual warnings are debounced.
5. A `gc doctor` lint detects ready beads stranded for >24h without a
   matching agent query.
6. A `gc doctor` lint warns when bd types appear without a class
   declaration.
7. Shadow-mode migration runs the new query alongside the old for a
   review period before enforcement.
8. An inverse-list alert fires when a ready bead matches no agent's
   `work_query` selector at all.

This document presents three iterations of the design. Iteration 1 is a
rough sketch likely to have flaws. Iteration 2 surfaces those flaws via
self-review and addresses them. Iteration 3 is the final design with all
eight safety mechanisms wired in, plus a decomposition into independently
slingable implementation beads. Earlier iterations are preserved verbatim
so the reviewer can audit the evolution.

---

## Iteration 1 — rough sketch

The shape of the fix: add a `class` field per bd type in config, and have
`EffectiveWorkQuery` translate that registry into the right
`--exclude-type` flags for each agent.

### Config schema

Extend the existing `[bd.types]` section in `pack.toml` / `city.toml` to
carry per-type class metadata:

```toml
[[bd.type_class]]
type = "task"
class = "executable"

[[bd.type_class]]
type = "bug"
class = "executable"

[[bd.type_class]]
type = "session"
class = "supervisor-managed"

[[bd.type_class]]
type = "wisp"
class = "hook-closed"

[[bd.type_class]]
type = "molecule"
class = "hook-closed"

[[bd.type_class]]
type = "convoy"
class = "hook-closed"

[[bd.type_class]]
type = "message"
class = "mail-managed"
```

Built-in defaults baked into Go for the standard bd types:

| Type           | Default class       | Rationale                                        |
| -------------- | ------------------- | ------------------------------------------------ |
| task           | executable          | The primary work-unit type.                      |
| bug            | executable          | Defects are worker-actionable.                   |
| feature        | executable          | Net-new functionality.                           |
| chore          | executable          | Maintenance tasks.                               |
| story          | executable          | Same as task in practice.                        |
| spike          | executable          | Time-boxed investigation work.                   |
| decision       | executable          | A worker captures the decision.                  |
| step           | executable          | Molecule child steps are worker-claimed.         |
| epic           | excluded            | Containers — no executable spec.                 |
| milestone      | excluded            | Same as epic.                                    |
| session        | supervisor-managed  | Agent lifecycle records; controller-driven.      |
| wisp           | hook-closed         | Ephemeral molecules; auto-closed on hook fire.   |
| molecule       | hook-closed         | Workflow containers; child beads carry the work. |
| convoy         | hook-closed         | Sling-batch wrappers; auto-resolved.             |
| message / mail | mail-managed        | Inbox flow, not worker queue.                    |
| event          | excluded            | Append-only log entries.                         |

### Agent config

Each agent declares which classes it claims:

```toml
[[agent]]
name = "claude"
accepts_classes = ["executable"]

[[agent]]
name = "claude-haiku-pool"
accepts_classes = ["cheap"]
```

Default for agents that don't declare `accepts_classes`: `["executable"]`.

### EffectiveWorkQuery rewrite

The current three-tier shell command continues to fire, but the
`--exclude-type=epic` flag is replaced with a generated set built from
the type-class registry. For an agent declaring
`accepts_classes = ["executable"]`, the generator collects every type
whose class is NOT "executable" and emits `--exclude-type=X` for each.

The shell template becomes something like:

```sh
EXCLUDE_FLAGS="--exclude-type=session --exclude-type=wisp --exclude-type=convoy ..."
bd ready --metadata-field gc.routed_to=<target> --unassigned $EXCLUDE_FLAGS --json --limit=1
```

The generator runs at config-load time, so the exclude flags are baked
into the agent's effective query string.

### Open issues with iter 1

This is a sketch. It probably has gaps. Self-review in iter 2.

---

## Iteration 2 — self-review and corrections

Reviewing iter 1 honestly turns up several real problems.

### Problem 1: The registry default isn't itself fail-safe

If a new bd type appears at runtime (operator creates a bead with
type `incident`, say, and there's no `[[bd.type_class]]` entry for it),
the iter-1 generator has no row to consult. The natural behavior is to
silently drop the type from the exclude list — which means the worker
WILL claim it. That's the opposite of the user's requirement: unknown
types should warn AND route to a default executable pool, not silently
become executable.

**Correction:** The runtime registry must have an explicit "unknown
class" rule. Any bd type that does not appear in the type_class registry
is treated as `class = "unknown"`. The query for `accepts_classes =
["executable"]` then has two modes:

- Strict mode (`accepts_classes = ["executable"]` only): excludes both
  non-executable AND unknown classes. Beads with unknown class go
  unclaimed.
- Permissive mode (`accepts_classes = ["executable", "unknown"]`):
  includes unknown so work still flows, but the routing layer fires a
  debounced warn event when it routes an unknown-class bead.

The user wants permissive default — unknown-class work should still get
done — so the default executable pool must declare
`accepts_classes = ["executable", "unknown"]` and the warn-on-route
telemetry covers the cost-visibility gap.

### Problem 2: The shell template can't compose dynamic exclude lists at config-load time without breaking the worker boundary

Iter 1 talks about "generating" the exclude flags at config-load time
and baking them into the agent's effective query string. But the
generator needs to read the live registry, and the registry must include
overrides from rig-level config layers. That means the agent's
EffectiveWorkQuery has to be re-generated whenever rig config changes —
which is a config-cache invalidation problem we'd rather not invent.

**Correction:** Generate the query string ONCE per worker session. The
controller hands the worker its effective query at startup (via env or
prompt template), and the worker's `gc hook` invocation runs the
already-resolved query. Cache invalidation happens at session boundary,
not at runtime. This matches the existing pattern in
`config.go:Agent.EffectiveWorkQuery` — that function is called when the
agent is materialized, not on every hook call.

### Problem 3: bd's `--exclude-type` semantics need verification

The iter-1 design assumes `bd ready --exclude-type=A --exclude-type=B`
excludes both A and B. The bd help confirms it: "Exclude issue types
from results (comma-separated or repeatable, e.g.,
--exclude-type=convoy,epic)." So the syntax works.

But: what does `bd ready` do if EVERY type is excluded? It returns
empty, which is fine. What if `--exclude-type=` is passed an unknown
type? bd treats it as a no-op (just doesn't filter anything by that
name), so passing a stale type name from a misconfigured registry is
safe.

### Problem 4: Shadow mode as iter 1 imagined it is incoherent

Iter 1 mentions a shadow flag but doesn't say what it shadows. If the
new query and old query both run in parallel and both drive claiming,
you get double-claims. If one runs in audit-only, which one drives
claiming?

**Correction:** Shadow mode operates as follows.

- `work_query.shadow = true`: the worker's actual `gc hook` invocation
  uses the OLD query (the current `--exclude-type=epic`-only form).
  Separately, the reconciler runs the NEW query against the same store
  and computes the diff: beads the old query would have surfaced that
  the new query excludes. Each diff entry is logged as an event
  (`work_query.shadow.diff`) and surfaced via `gc doctor`.
- `work_query.shadow = false` (default after migration): the worker uses
  the NEW query. The reconciler runs the OLD query in audit and logs
  beads the new query excluded but the old would have surfaced. Operator
  reviews to confirm exclusions remain correct.

The transition is: ship with `shadow = true`, observe `shadow.diff`
events for N days (suggested N=7), confirm the diff is only the intended
non-work types, flip the city-level flag to `shadow = false`. The audit
direction reverses but the visibility stays.

### Problem 5: "Default pool" isn't a known concept in the SDK

The user's stated requirement is that unknown-class beads "route to the
default executable pool." But the SDK has ZERO hardcoded roles, so
there's no built-in "default pool" — every agent role is configuration.

**Correction:** The routing destination for unknown-class beads is
determined by the existing `gc.routed_to` metadata. The operator slings
unknown-class beads to whichever pool template they want (typically the
main `<rig>/claude` pool). The "default" status comes from the fact that
the main pool's `accepts_classes` includes `"unknown"`. No new
infrastructure for "default pool" — just an opt-in flag on whichever
agent the operator picks.

### Problem 6: Debounce state survival

Iter 1 didn't talk about where the debounce state lives. If it's
in-memory per controller process, restarting the controller resets the
window and re-fires every warning. If it's on disk, we have a state file
to maintain, which contradicts the "no status files — query live state"
principle.

**Correction:** Debounce state is derived from the event log itself. The
event bus is append-only and persistent. The warn-emit code checks
whether a `work_query.unknown_class` event for this type has fired
within the last N minutes; if yes, skip. The event log is the source of
truth — no separate debounce state file.

### Problem 7: Cumulative cost tracker needs token attribution

The cost-class-miss tracker needs to attribute model spend to specific
beads. Gas City's session-lifecycle telemetry records token counts per
session, but not per bead claimed during that session. A worker can
claim multiple beads in one session; the token total per session doesn't
decompose cleanly per bead.

**Correction:** Attribute conservatively. The tracker records cumulative
token spend on sessions that claimed at least one unknown-class bead.
That over-counts (token spend on properly-classified beads claimed by
the same session also gets attributed) but it's a usable signal — when
unknown-class telemetry shows N% of pool sessions, the over-counted cost
gives an upper bound on drift. For finer attribution, future work could
add per-claim token markers (out of scope for this design).

### Problem 8: Inverse-list alert needs a clear emitter

"Live operator signal" is vague. Where does it fire from? When?

**Correction:** The reconciler already scans `bd ready` periodically for
demand detection. Extend that scan to compute the inverse: for each
ready bead, check whether ANY agent's `EffectiveWorkQuery` would match
it. If none would, emit `work_query.unrouted_ready` with the bead ID.
Surface in `gc doctor --check=unrouted-ready-beads`. Frequency matches
the existing demand-detection cycle (no new schedule).

---

## Iteration 3 — final design with safety mechanisms and decomposition

### 3.1 Type-class registry

#### Config schema

A new TOML table per type lives in the bd-section of pack.toml /
city.toml:

```toml
[[bd.type_class]]
type = "task"
class = "executable"

[[bd.type_class]]
type = "session"
class = "supervisor-managed"

# ... etc for each type
```

#### Built-in defaults

Embedded in Go (a const map in `internal/config/type_class.go`):

```go
// BuiltinTypeClass maps each known bd type to its default class.
// Operator config can override per type via [[bd.type_class]] entries.
var BuiltinTypeClass = map[string]string{
    // Executable work (worker pool claims these)
    "task":         "executable",
    "bug":          "executable",
    "feature":      "executable",
    "chore":        "executable",
    "story":        "executable",
    "spike":        "executable",
    "decision":     "executable",
    "step":         "executable",
    "spec":         "executable",
    "merge-request": "executable",

    // Excluded (no executable spec)
    "epic":      "excluded",
    "milestone": "excluded",
    "event":     "excluded",

    // Supervisor-managed (controller drives, not workers)
    "session": "supervisor-managed",
    "agent":   "supervisor-managed",
    "role":    "supervisor-managed",
    "rig":     "supervisor-managed",

    // Hook-closed (auto-resolved by lifecycle hooks)
    "wisp":     "hook-closed",
    "molecule": "hook-closed",
    "convoy":   "hook-closed",
    "gate":     "hook-closed",

    // Mail-flow (inbox, not worker queue)
    "message": "mail-managed",
}
```

#### Resolution order

`config.ResolveTypeClass(typeName)` returns:

1. The operator's `[[bd.type_class]]` entry if one exists for `typeName`.
2. Else `BuiltinTypeClass[typeName]` if present.
3. Else `"unknown"`.

### 3.2 Agent accepts_classes

Add a new field to `config.Agent`:

```go
// AcceptsClasses lists the type_classes this agent will claim.
// Default if empty: ["executable"].
// To accept unknown-class beads (cost-flagged but routed), include
// "unknown" explicitly.
AcceptsClasses []string `toml:"accepts_classes"`
```

The default executable pool should set
`accepts_classes = ["executable", "unknown"]` so unknown-class work
flows, with telemetry. A dedicated cheap-tier pool sets
`accepts_classes = ["cheap"]` and only claims types explicitly classified
as `cheap`. The `"cheap"` class is operator-declared per type — there's
no Haiku fallback unless the operator declares specific types as
`class = "cheap"`.

Per-agent-config addition affects:

- `config.Agent` struct
- `config.AgentPatch` (override layer)
- `config.AgentOverride` (override layer)
- `applyAgentPatch`, `applyAgentOverride` (merge functions)
- `poolAgents` deep-copy in `cmd/gc/pool.go`
- `TestAgentFieldSync` will catch struct drift

### 3.3 EffectiveWorkQuery rewrite

The new generator takes the agent's `AcceptsClasses` and produces the
exclude-type list at config-resolution time (when the agent is
materialized, before the worker session starts). For each entry in the
type-class registry whose class is NOT in `AcceptsClasses`, the
generated query includes `--exclude-type=<typeName>`.

Skeleton (replacing lines 2369-2391 of `config.go`):

```go
func (a *Agent) EffectiveWorkQuery() string {
    if a.WorkQuery != "" {
        return a.WorkQuery
    }

    excludeFlags := buildExcludeFlags(a.AcceptsClasses)
    target := a.QualifiedName()
    if a.PoolName != "" {
        target = a.PoolName
    }

    return `sh -c '` +
        // Tier 1: in_progress assigned (crash recovery)
        `for id in "$GC_SESSION_ID" "$GC_SESSION_NAME" "$GC_ALIAS"; do ` +
        `[ -z "$id" ] && continue; ` +
        `r=$(bd list --status in_progress --assignee="$id" ` +
        excludeFlags + ` --json --limit=1 2>/dev/null); ` +
        `[ -n "$r" ] && [ "$r" != "[]" ] && printf "%s" "$r" && exit 0; ` +
        `done; ` +
        // Tier 2: ready assigned (pre-assigned)
        // Tier 3: ready unassigned routed_to (pool)
        // ... same shape, all use excludeFlags
        `printf "[]"'`
}

func buildExcludeFlags(acceptsClasses []string) string {
    accepted := make(map[string]bool, len(acceptsClasses))
    for _, c := range acceptsClasses {
        accepted[c] = true
    }
    if len(accepted) == 0 {
        accepted["executable"] = true
    }

    var excluded []string
    for typeName, class := range allKnownTypeClasses() {
        if !accepted[class] {
            excluded = append(excluded, typeName)
        }
    }
    sort.Strings(excluded)

    flags := make([]string, len(excluded))
    for i, t := range excluded {
        flags[i] = "--exclude-type=" + t
    }
    return strings.Join(flags, " ")
}
```

`allKnownTypeClasses()` merges built-in defaults with operator overrides.

The legacy workflow-control path (lines 2393-2427) gets the same
treatment — same exclude-flag generator, just kept in its existing
branch.

### 3.4 Unknown-class telemetry: debounced warn + cumulative tracker

#### Warn event

When a worker claims a bead whose resolved class is `"unknown"`, the
worker's claim handler (or a controller-side reconciler observer) emits
an event:

```
type: work_query.unknown_class
payload:
  bead_id:    "gascity-xyz"
  bead_type:  "incident"
  agent:      "gascity/claude"
  session_id: "gc-2um4pp"
  ts:         "2026-05-18T20:00:00Z"
```

The event subscriber (in `internal/doctor` or a new
`internal/typeclass` package) maintains the debounce window. Before
emitting an operator-facing warning, the subscriber queries the event
log for prior `work_query.unknown_class` events with the same
`bead_type` within the debounce window (default 1h, configurable as
`bd.type_class.warn_debounce`). If none, emit a warning; if yes,
skip. Either way the underlying event is still recorded — the debounce
is only on the operator-facing escalation, not on the raw signal.

Operator warning surface: a `gc doctor` line and an entry in the
city's `events` stream so the dashboard can show it.

#### Cumulative cost tracker

Subscribed alongside the warn emitter. For each
`work_query.unknown_class` event, the tracker records the session
that processed it. At session-end (`session.lifecycle.closed` event),
the tracker reads the session's token totals from existing telemetry
and adds them to a per-class cumulative counter
(`unknown_class_total_input_tokens`, `unknown_class_total_output_tokens`).
Persistence is in the event log (the totals are derivable by replaying
events).

`gc doctor` surface:

```
type-class drift:
  unknown-class beads routed (24h): 14
  unknown-class types seen (24h): incident, experiment
  estimated cost on unknown-class sessions (24h):
    input  tokens: 8_421_330 (~$0.83 at Haiku rates, ~$25.30 at Opus rates)
    output tokens:   312_440
  recommendation: declare class for these types in [[bd.type_class]]
```

### 3.5 `gc doctor --check=unrouted-ready-beads`

A doctor check that walks `bd ready` and for each bead computes whether
any agent's `EffectiveWorkQuery` selector would match it. The "match"
test is: does the bead's resolved class appear in some agent's
`AcceptsClasses`? If no AND the bead has been in ready state for >24h
(configurable as `bd.type_class.stranding_threshold`), the check flags
it and emits a `work_query.unrouted_ready` event with a wisp escalation
(create a `wisp` bead routed to `<rig>/witness` so a supervisor sees
it).

Implementation: add to `internal/doctor/checks.go` registry. Reuses the
existing doctor cron path so no new schedule. Check name:
`unrouted-ready-beads`.

### 3.6 `gc doctor` type-class declaration lint

Walks all bd types that have appeared in the store (query
`bd list --types` or equivalent) and for each, checks
`config.ResolveTypeClass(typeName)`. If the resolution falls through to
`"unknown"` AND no prior `type_class.missing_declaration` event for
this type exists in the last 30 days, emit one and surface a warning in
`gc doctor`:

```
type-class registry:
  type "incident" appears in store but has no class declaration
    add to pack.toml:
      [[bd.type_class]]
      type = "incident"
      class = "executable"  # or other class
```

First-appearance-only warnings prevent flooding. The 30-day re-warn
window allows operators to revisit if they ignored the first warning.
This is separate from the unknown-class warn (3.4) — that one fires on
routing, this one fires on doctor sweep.

### 3.7 Shadow-mode migration

#### Flag

A new boolean lives at the `city.toml` level (not per-agent — the city
is the migration unit):

```toml
[bd.type_class]
work_query_shadow = true  # true = use old query, audit new; false = use new query, audit old
warn_debounce = "1h"
stranding_threshold = "24h"
```

#### Worker behavior

If `work_query_shadow = true`, every agent's `EffectiveWorkQuery`
returns the pre-rewrite shell template (current code, only excludes
`epic`). Workers behave as before. The new exclusion logic is
NOT applied.

#### Audit behavior

A controller-side reconciler observer runs both queries against the
same store periodically (suggested every demand-detection cycle, ~30s).
For each diff bead, emit a `work_query.shadow_diff` event:

```
type: work_query.shadow_diff
payload:
  bead_id:        "gascity-abc"
  bead_type:      "session"
  bead_class:     "supervisor-managed"
  old_query_match: true
  new_query_match: false
  agent:          "gascity/claude"
  ts:             "..."
```

Operator inspects these via `gc doctor --check=shadow-diff` or the
dashboard's events tab. If the diff list contains only beads the
operator intended to exclude, the migration is safe to flip.

#### Migration runbook

Shipped in `engdocs/proposals/type-class-routing-2026-05-18.md` (this
doc) under a future appendix or in the `gc-240h9q` close note:

1. Land all implementation beads with `work_query_shadow = true` as
   the default in new cities and a no-op for existing cities (their
   `city.toml` doesn't have the section yet, so default applies).
2. Run for N days (suggested N=7). Operator monitors
   `gc doctor --check=shadow-diff`.
3. When diff is clean (only intended exclusions), operator sets
   `work_query_shadow = false` in `city.toml`.
4. Audit direction reverses: new query drives, old query audits. If
   the new query later starts excluding too much (e.g., a new bd
   built-in type was assumed non-executable but is actually work),
   the audit catches it.
5. After 30 days of clean enforced operation, remove the shadow
   subsystem entirely (a future cleanup bead, not in this design's
   scope).

### 3.8 Inverse-list alert

A reconciler observer computes, on each demand-detection cycle, the set
of currently-ready beads. For each ready bead, it walks the live agent
roster and checks whether any agent's `AcceptsClasses` contains the
bead's resolved class. If none do, the observer emits
`work_query.unmatched_ready` (distinct from `unrouted_ready` —
`unmatched` means no agent in the world would claim it; `unrouted`
means it has matching agents but they aren't claiming for some other
reason).

Surface via `gc doctor --check=unmatched-ready-beads` and the events
stream. This is the live signal — fires within one demand-detection
cycle of a misclassified bead appearing.

### 3.9 Decomposed implementation beads

Eight beads, each independently slingable to a pool worker. All target
the gascity rig at `origin/main`. Dependencies are encoded such that
beads A and F can land first in parallel; B unblocks the rest.

| Bead ID (to assign)  | Title                                                                                   | Depends on        |
| -------------------- | --------------------------------------------------------------------------------------- | ----------------- |
| TC-A                 | type-class registry — config schema + built-in defaults + resolver                      | (none)            |
| TC-B                 | EffectiveWorkQuery — generate exclude-type flags from registry                          | TC-A              |
| TC-C                 | Agent `accepts_classes` field + Haiku-pool opt-in routing                               | TC-A, TC-B        |
| TC-D                 | Unknown-class telemetry — debounced warn event + cumulative cost-miss tracker           | TC-B              |
| TC-E                 | `gc doctor --check=unrouted-ready-beads` lint with wisp escalation                      | TC-B              |
| TC-F                 | `gc doctor` type-class declaration lint (first-appearance warning per type)              | TC-A              |
| TC-G                 | Shadow-mode migration — `work_query_shadow` flag + diff observer + diff doctor check    | TC-B              |
| TC-H                 | Inverse-list alert — `gc doctor --check=unmatched-ready-beads` + reconciler observer    | TC-B              |

#### Recommended sling order

1. **TC-A** (foundation — registry + resolver). All others depend on it.
2. **TC-B** (work-query rewrite). Replaces the current `--exclude-type=epic`
   default. Must land before any consumer of the new selector logic.
3. **TC-G** (shadow mode). Land immediately after B, default
   `work_query_shadow = true` so the new logic doesn't take effect
   until operator review. Other consumers (C, D, E, H) operate on the
   new logic but in shadow they're observing rather than acting.
4. **TC-F** (declaration lint). Independent of B; can land in parallel
   with TC-G to start gathering "missing class" signal early.
5. **TC-D, TC-E, TC-H** (telemetry + doctor checks). Parallelizable after
   B is in. These produce the operator-facing signals the migration
   review needs.
6. **TC-C** (Haiku-pool opt-in). Last because it requires the operator
   to declare specific types as `class = "cheap"` and configure the
   Haiku pool with `accepts_classes = ["cheap"]`. Lands when there's
   actual cheap-class demand to route.

#### Each bead must satisfy the quality bar

The implementation beads filed under this design must include:

- A concrete acceptance criteria list (testable assertions).
- A design field explaining the rationale for the chosen approach.
- A notes field with PII status, working-tree expectations,
  cross-references to companion work, no open questions.

The mayor should file these via `gc bd --rig gascity create --validate`
and link them with dep edges via `gc bd --rig gascity dep add`. This
worker (`gascity/claude-1`) files them as part of this bead's
acceptance criteria.

### 3.10 What this design does NOT cover

These are intentional exclusions to keep the scope tight.

- **Per-bead class override.** A bead could carry its own `gc.class`
  metadata that overrides the type-class registry for that specific
  bead. Useful for one-off "this incident is actually a `cheap` task"
  reclassifications. Out of scope for the initial design; can be added
  later if needed.
- **Class hierarchies.** No `executable` includes `cheap` semantics; if
  an agent wants both, it lists both explicitly. Keeps resolution
  trivial.
- **Per-rig class registry.** Right now, the type-class registry is
  city-level (one source of truth across rigs). If two rigs disagree
  about whether `incident` is `executable` or `unknown`, the city
  config wins. Per-rig overrides could be added later as a layer in
  resolution order between operator and built-in.
- **Token-attribution-per-claim refinement.** The cost tracker
  over-counts (any session that touched at least one unknown-class
  bead is counted in full). Per-claim attribution requires
  instrumenting bd-claim hooks to emit per-bead token markers — a
  larger piece of telemetry work, out of scope here.
- **Removing the shadow subsystem after migration.** A cleanup bead
  filed separately, ~30 days after the city flips to
  `work_query_shadow = false`.

### 3.11 Acceptance criteria roll-up

The design as a whole satisfies the eight required safety mechanisms:

| # | Requirement                                                                        | Where addressed |
| - | ---------------------------------------------------------------------------------- | --------------- |
| 1 | type_class metadata on type registry with sensible defaults for built-in bd types  | 3.1             |
| 2 | Haiku-pool tier ONLY for explicitly classified-as-cheap types                      | 3.2             |
| 3 | Unknown-class-miss → debounced WARN + route to default pool                        | 3.2, 3.4        |
| 4 | Cumulative cost-class-miss tracking surfaced in `gc doctor`                        | 3.4             |
| 5 | `gc doctor --check=unrouted-ready-beads` with wisp escalation                       | 3.5             |
| 6 | Type-registry doctor lint (warn on missing class)                                  | 3.6             |
| 7 | Shadow-mode migration (`work_query.shadow = true`) with N-day review               | 3.7             |
| 8 | Inverse-list alert for ready beads matching no work_query selector                 | 3.8             |

### 3.12 Open questions flagged for human review

These are not silent decisions — the design extends past the user's
literal asks in a few places. Each is called out explicitly.

1. **Shadow-mode default for new cities.** The design defaults
   `work_query_shadow = true` on first introduction so existing cities
   pick up the safety net without behavior change. New cities also
   inherit `true` unless the operator sets it. Alternative: default
   `false` for net-new cities (no legacy query to compare). Recommended
   default is `true` for symmetry and predictability.

2. **`unrouted_ready` vs `unmatched_ready` distinction.** The design
   distinguishes "no matching agent exists" (3.8) from "matching agent
   exists but isn't claiming for some other reason" (3.5). The user's
   asks for these are slightly different (`unrouted-ready-beads` and
   "inverse-list alert"), so the design treats them as separate
   checks. If the operator finds the two names confusing, they can be
   merged into one check that reports both kinds of stranding with a
   subtype tag.

3. **Where the cost tracker lives.** The design places it in
   `internal/typeclass/` (a new package). Could alternatively live in
   `internal/doctor/` as a doctor-only subsystem, or
   `internal/events/` if treated as pure event-bus subscriber. Picking
   a home is an implementation-bead decision; this design doc just
   says "subscribed to event bus, surfaces in doctor."

4. **`gc.class` per-bead override.** Section 3.10 explicitly excludes
   this. If reviewers want it in scope, the design extends easily
   (resolution order becomes per-bead override → operator config →
   built-in default → unknown).

5. **Class-name vocabulary.** The design uses
   `executable / excluded / supervisor-managed / hook-closed /
   mail-managed / cheap / unknown`. These names are stable design
   choices; operators can add their own custom classes. If reviewers
   prefer different names (e.g., `worker / container / lifecycle / 
   ephemeral / mail / low-cost / unset`), the rename is mechanical.

---

## Evolution summary (for the cover mail)

Iteration 1 sketched a config-driven exclude-list generator with per-type
class metadata and per-agent `accepts_classes`. Iteration 2 surfaced
eight concrete weaknesses: missing fail-safe for unrecognized types, an
unclear shadow-mode semantic, missing default-pool definition, debounce
state outside the event bus, weak cost attribution, and an unclear
inverse-list emitter. Iteration 3 fixed each with explicit mechanisms —
unknown-class is its own routable class with opt-in by the default pool;
shadow mode reverses audit direction across the flip; the event bus IS
the debounce store; cost tracking conservatively attributes per session;
inverse-list runs in the existing demand-detection cycle. The final
design carries all eight required safety mechanisms and decomposes into
eight implementation beads with explicit dependencies and a recommended
sling order.
