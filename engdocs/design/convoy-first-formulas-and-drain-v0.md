---
title: Convoy-First Formulas And Drain V0
status: Implemented V0
source: https://github.com/gastownhall/gascity/issues/1709
---

# Convoy-First Formulas And Drain V0

## Summary

Issue #1709 asks for graph formulas to operate on convoys rather than
individual beads. This v0 makes `contract = "graph.v2"` the hard switch:
targeted graph.v2 formulas receive the reserved system variable `convoy_id`,
not `issue` or `bead_id`.

When the target is already a convoy, that convoy is passed through whole. When
the target is a normal bead, Gas City creates a visible real convoy containing
that one bead, then passes that convoy ID as `convoy_id`.

This v0 also adds a graph.v2-only `drain` step for scatter work. Drain v0
supports separate drains and shared single-lane drains. The control bead
snapshots the input convoy membership, creates visible drain-unit convoys, and
starts ordinary graph.v2 item formulas against those unit convoys. Item formulas
also receive only `convoy_id`.

## Goals

- Make every targeted graph.v2 formula invocation think in terms of an input
  convoy.
- Preserve the single-bead operator entry point by normalizing it to a visible
  one-item convoy.
- Fail fast on graph.v2 formulas or caller vars that use legacy bead-scoped
  names: `issue` and `bead_id`.
- Add a minimal drain primitive that scatters a fixed convoy snapshot into
  generated one-member convoys.
- Keep item formulas ordinary graph.v2 formulas, with no special `item_bead_id`
  or drain-only template variables.
- Keep generated one-item and drain-unit convoys ordinary convoy containers;
  formulas inspect membership instead of special per-bead metadata.

## Non-Goals

- Arbitrary parallel shared-context drain is not in v0. Shared drains must use
  `[steps.drain.item].single_lane = true` and are materialized one item at a
  time.
- Drain setup and teardown syntax is not in v0. Formula authors can place
  normal graph.v2 steps before and after a drain step.
- Scripted shredders are not in v0. The only v0 shredder is one-by-one.
- Typed drain events, API/SSE drain projections, `gc trace` rendering, and
  cleanup of system-created convoys are follow-up work.
- Multi-controller, cross-process uniqueness is not solved in v0. The current
  implementation serializes graph.v2 root and drain materialization with
  process-local keyed locks. Store-level unique create-or-get primitives remain
  future work before running multiple materializing controllers against the
  same store.

## Graph.v2 Invocation Contract

`contract = "graph.v2"` opts a formula into convoy-first invocation.

Targeted invocation behavior:

- If the target bead has `type = "convoy"`, use that bead ID as `convoy_id`.
- Otherwise create a one-item convoy for the target bead and use that convoy ID
  as `convoy_id`.
- Do not inject `issue`.
- Do not stamp graph.v2 workflow roots with `gc.source_bead_id`.
- Do not write `workflow_id` back to the original source bead.
- Do not auto-close the input convoy or the original source bead when the
  graph.v2 workflow completes.
- If a legacy source-workflow root is still live for the target bead, reject
  the launch with the existing source-workflow conflict response. Graph.v2 roots
  do not create new legacy source links, but v0 does not silently run beside an
  already-live legacy workflow for the same bead.

Targetless graph.v2 formulas are allowed only when they do not reference
`convoy_id` and do not contain a drain step. Targetless formulas that need the
input convoy fail before work is created.

The shared invocation helper lives in `internal/graphv2` and is used by the
implemented runtime entry points:

- formula-backed sling on an explicit target
- default formula sling
- batch/convoy sling before container expansion
- `gc formula cook --attach`
- formula-backed order dispatch

Convergence wisps are not a graph.v2 entry point in v0 because convergence does
not yet carry an explicit input convoy target. Convergence rejects graph.v2
formulas rather than guessing from the convergence root bead.

Non-graph formulas keep their existing bead-scoped behavior and may continue to
use `issue`.

## Reserved Variables

Graph.v2 reserves:

- `convoy_id`
- `issue`
- `bead_id`

`convoy_id` is injected by the runtime for targeted graph.v2 invocations. A
caller cannot supply or override it through CLI vars, rig formula vars, order
vars, inherited vars, or formula `[vars]`.

`issue` and `bead_id` are never valid in graph.v2 formulas. There is no
compatibility mapping from either name to a convoy member.

Validation runs after formula resolution and `description_file` loading. Missing
description files are hard errors. The scanner covers descriptions, notes,
titles, conditions, labels, metadata, drain fields, children, loop bodies, and
template steps.

## Automatic One-Item Convoys

Automatic one-item convoys are visible real convoy beads created for non-convoy
graph.v2 targets.

Metadata:

```text
gc.synthetic = true
```

Rules:

- The convoy tracks exactly one source bead through the canonical `tracks`
  relation.
- The convoy does not inherit arbitrary source metadata, routing, molecule,
  workflow, assignee, or label state.
- Repeated invocations against the same source bead create separate one-item
  convoys. A caller that wants stable grouping or dedupe should create and
  target an explicit convoy.
- No graph.v2 code branches on a special singleton kind or source-bead metadata.

## Graph.v2 Root Identity

Targeted graph.v2 workflow roots are stamped with:

```text
gc.input_convoy_id = <input convoy>
gc.graphv2_root_key = graphv2-root:<input_convoy_id>:<formula_name>:<vars_hash>:<scope>
```

The vars hash excludes the reserved `convoy_id` value, so the same formula and
non-reserved var set dedupe for the same input convoy. The default scope is
`default`; callers with a stable controller/order scope can pass a non-default
scope.

If a live root already exists for the same key, materialization returns that
root. `--force` closes the existing graph.v2 root with:

```text
gc.outcome = skipped
gc.failure_reason = graphv2_force_replaced
```

then creates a replacement root. Closed roots are not reused.

## Drain Syntax

Drain is a graph.v2-only step block.

```toml
[[steps]]
id = "review-members"
title = "Review members"

[steps.drain]
context = "separate"
formula = "review-one-convoy"
member_access = "read"
max_units = 50
on_item_failure = "continue"
```

V0 rules:

- `context` may be omitted or set to `separate`.
- `context = "shared"` requires `[steps.drain.item].single_lane = true`.
- `formula` is required and must name a graph.v2 item formula.
- `member_access` may be omitted or set to `read` or `exclusive`.
- `max_units` may be omitted. If set, it must be between 1 and 100.
- `on_item_failure` may be omitted or set to `continue`.
- `skip_remaining` skips unmaterialized later rows after the first failed item
  in a shared drain.
- `continuation_group` is valid only with `context = "shared"`.

A drain step may use `id`, `title`, `description`, `description_file`, `notes`,
`needs`, `depends_on`, labels, and non-`gc.kind` metadata. It may not combine
with normal executable routing fields such as `assignee`, `expand`, `gate`,
`loop`, `on_complete`, `check`, `retry`, children, or timeout.

The compiled drain step becomes a controller-owned control bead with:

```text
gc.kind = drain
gc.drain_context = separate | shared
gc.drain_formula = <item formula>
gc.drain_member_access = read | exclusive
gc.drain_on_item_failure = continue | skip_remaining
```

User agents do not execute the drain control bead.

## Drain Expansion

The control dispatcher processes `gc.kind = "drain"` beads.

Expansion flow:

1. Read the graph.v2 workflow root from `gc.root_bead_id`.
2. Read the parent input convoy from the root's `gc.input_convoy_id`.
3. If `gc.drain_manifest.v1` already exists, replay that persisted snapshot.
4. Otherwise list active convoy members, excluding closed members.
5. Reject unresolved or cross-store tracked members.
6. Reject more than the configured `max_units`.
7. Persist `gc.drain_manifest.v1` before creating generated work.
8. Create or reuse one visible drain-unit convoy per manifest row.
9. Create or reuse one graph.v2 item workflow root per drain-unit convoy.
10. Add a dependency from the drain control to each item root.
11. Mark the drain `expanded`.
12. On later dispatcher passes, close the drain after every item root is
    terminal.

The manifest is the canonical membership snapshot for one drain run. If convoy
membership changes after the manifest is written, replay still uses the original
rows.

Manifest metadata:

```text
gc.drain_state = expanding | expanded | completing | succeeded | failed
gc.drain_parent_convoy_id = <input convoy>
gc.drain_count = <row count>
gc.drain_manifest.v1 = <JSON DrainManifestV1>
```

`DrainManifestV1` is an internal typed structure serialized as JSON metadata.
Rows contain:

```text
index
member_id
unit_key
unit_convoy_id
item_root_key
item_root_id
status
outcome_bead_id
outcome_kind
failure_reason
```

## Drain-Unit Convoys

Each generated unit convoy is visible and tracks exactly one original member.

Metadata:

```text
gc.synthetic = true
gc.synthetic_kind = drain-unit-convoy
gc.parent_convoy_id = <input convoy>
gc.drain_control_id = <drain control bead>
gc.drain_index = <0-based index>
gc.drain_count = <total generated drain units>
gc.drain_member_id = <original member bead>
gc.drain_member_access = read | exclusive
gc.drain_unit_key = drain-unit:<control>:<index>:<member>
```

Drain-unit convoys stay open after item work succeeds. They are lineage and
input containers, not completion signals.

## Item Formula Contract

The item formula named by `[steps.drain].formula` must be graph.v2. The drain
runtime instantiates it with:

```text
convoy_id = <generated drain-unit convoy>
```

The item formula receives no `item_bead_id`, `drain_index`, `drain_count`, or
underlying member variable. When it needs the underlying member in one-by-one
mode, it reads `gc.drain_member_id` from its input convoy.

Item workflow roots are stamped with:

```text
gc.input_convoy_id = <generated drain-unit convoy>
gc.drain_control_id = <drain control bead>
gc.drain_index = <0-based index>
gc.drain_count = <total generated drain units>
gc.drain_member_id = <original member bead>
gc.drain_member_access = read | exclusive
gc.item_root_key = drain-item-root:<control>:<index>:<member>
gc.graphv2_root_key = graphv2-root:<unit convoy>:<item formula>:<vars hash>:drain=<control>:<member>
```

Drain completion is based on item workflow roots, not drain-unit convoys or
original member beads.

Successful rows use `gc.outcome_bead_id` from the item root when present;
otherwise the item root itself is the default outcome bead.

## Migration

Out-of-tree non-graph formulas remain bead-scoped. Bundled and example
formulas are all convoy-native graph.v2 formulas (#2941 completed the
migration that this v0 started for `mol-scoped-work` and
`mol-review-quorum`); a whole-pack test in `internal/bootstrap/` keeps it
that way.

Existing graph.v2 formulas must migrate:

- Replace `{{issue}}` and `{{bead_id}}` with `{{convoy_id}}` when the formula
  operates on the input convoy.
- When a graph.v2 formula needs per-member work, use a `drain` step and make
  the item formula convoy-native too.
- Do not rely on graph.v2 success to close the input convoy or the original
  target bead.

Deprecated compat alias (#2941, one release): a graph.v2 formula that still
declares `vars.issue` or references `{{issue}}` is accepted with a
deprecation warning, and the runtime resolves `issue` to the single tracked
member of the input convoy (erroring when the convoy tracks more or fewer
than one member). `bead_id` has no compat mapping and is rejected outright.
Caller-supplied `--var issue=` values remain rejected — the value is always
runtime-derived.

Core formula changes in this v0:

- `mol-scoped-work` receives `convoy_id`; when it needs a single underlying
  bead, it reads the convoy membership and requires exactly one tracked member.
- `mol-review-quorum` receives `convoy_id` and no longer requires a bead-scoped
  `subject` value from graph.v2 callers.

#2941 applied the same single-member derivation pattern to `mol-do-work`,
`mol-polecat-base` and its variants (`mol-polecat-commit`,
`mol-polecat-report`, gastown's `mol-polecat-work`), `mol-prompt-synth`, and
gastown's `mol-review-leg`.

## Acceptance Criteria

- Targeted graph.v2 invocation on a bead creates a visible one-item convoy and
  injects `convoy_id`.
- Targeted graph.v2 invocation on a convoy passes that convoy whole and does
  not expand members before formula materialization.
- Targetless graph.v2 formulas that reference `convoy_id` or contain drain fail
  before creating work.
- Caller attempts to provide `convoy_id`, `issue`, or `bead_id` fail before
  creating graph.v2 work. Formula declarations/references of `convoy_id` and
  `bead_id` outside the contract fail too; formula usage of `issue` is
  accepted for one release as a deprecation-warned compat alias (#2941).
- Graph.v2 roots use `gc.input_convoy_id` and `gc.graphv2_root_key`, not
  `gc.source_bead_id`.
- Repeated graph.v2 launches against a bare bead create separate one-item
  convoys and therefore separate graph.v2 root keys. Reuse applies when the
  caller targets the same explicit convoy and non-reserved var set.
- Forced graph.v2 launch closes the existing root and creates a replacement.
- Separate drain snapshots active convoy members once and replays the same
  snapshot after later membership changes.
- Drain creates one system-created drain-unit convoy and one item graph.v2 root per
  manifest row.
- Drain item roots receive only `convoy_id` as their system variable (plus
  the deprecated `issue` alias resolved to the unit's tracked member during
  the #2941 compat window).
- Drain succeeds only after every item root is closed successfully, and fails
  after every reachable item root is terminal if any item root fails.
- Graph.v2 item formulas receive only `convoy_id` and inspect convoy membership
  or drain metadata as needed.

## Follow-Up Work

- Store-level atomic create-or-get and metadata CAS primitives for
  multi-controller graphv2 materialization.
- Shared-context drain using hard continuation-group/session-affinity
  enforcement.
- Scripted shredders that split one input convoy into caller-defined sub-convoys.
- Typed events, API/SSE projection, dashboard lineage, and `gc trace` rendering.
- System-created one-item and drain-unit cleanup command.
- Rollback cleanup guidance for downgrading to binaries that do not understand
  `gc.synthetic=true` convoys or `gc.kind=drain` control beads.
