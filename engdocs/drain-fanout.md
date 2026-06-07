# Drain Fan-Out Guide

> **Quick reference** — `drain` is the canonical fan-out primitive for `graph.v2`
> formulas. One drain step scatters an input convoy's members into individual
> item convoys and runs a per-item formula against each one.

## Quick Reference

```toml
# graph.v2 formula — scatter convoy members across a per-item formula
[[steps]]
id = "process-members"
title = "Process each member"

[steps.drain]
context  = "separate"    # one item convoy per member (see Context below)
formula  = "my-item-formula"
member_access = "read"   # or "exclusive"
max_units = 50           # safety cap (1–100)
on_item_failure = "continue"  # or "skip_remaining"
```

## When to Use `drain`

| Situation | Use |
|-----------|-----|
| New `graph.v2` formula needs fan-out | `drain` ← **canonical** |
| `graph.v2` formula already uses `gc.output_json` | Leave as-is (grandfathered); migrate at next major refactor |
| `graph.v1` formula | Upgrade to `graph.v2` first, then use `drain` |

**Decision rule:** If you are writing a new `graph.v2` formula that needs to
scatter work, use `drain`. Do not introduce `gc.output_json` in new `graph.v2`
formulas.

## `drain` Field Reference

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `context` | string | Yes | — | `"separate"` creates one item convoy per member; `"shared"` materializes one single-lane item at a time with continuation affinity |
| `formula` | string | Yes | — | Name of the `graph.v2` formula to run for each item convoy |
| `member_access` | string | No | — | `"read"` for read-only access; `"exclusive"` records a per-member reservation before materializing item work |
| `max_units` | int | No | unlimited | Safety cap. Caps one drain expansion to prevent accidental runaway materialization (1–100 in v0) |
| `on_item_failure` | string | No | — | `"continue"` keeps running remaining items when one fails; `"skip_remaining"` stops |
| `continuation_group` | string | No | — | Shared execution group suffix; valid only with `context = "shared"` |
| `[steps.drain.item]` | table | No | — | Per-item execution controls |
| `[steps.drain.item].single_lane` | bool | No | `false` | Required `true` for shared drains |

## Minimal `graph.v2` Example

```toml
# scatter-reviews.formula.toml
formula  = "scatter-reviews"
version  = 1
contract = "graph.v2"
type     = "workflow"

[[steps]]
id    = "review-each"
title = "Review each member"

[steps.drain]
context       = "separate"
formula       = "review-one"
member_access = "read"
max_units     = 50
on_item_failure = "continue"
```

The item formula (`review-one`) is an ordinary `graph.v2` formula. It receives
`convoy_id` pointing to a one-member item convoy. It does not need any
drain-specific template variables — it inspects convoy membership directly.

## `gc.output_json` — Legacy Fan-Out (Grandfathered)

`gc.output_json` is the **legacy** fan-out mechanism used by `graph.v1`
formulas and early `graph.v2` formulas written before `drain` was available.

**Rule:** Existing callers of `gc.output_json` are grandfathered. Do not
remove or change them without explicit migration planning. New `graph.v2`
formulas must use `drain` instead.

`gc validate` warns on `gc.output_json` in `graph.v2` formulas to surface
the opportunity for migration, but the warning does not fail validation
or break CI. Grandfathered callers can keep running.

## Frequently Asked Questions

**Q: Do I need to change existing formulas that use `gc.output_json`?**

No. Existing callers are grandfathered. `gc validate` will warn, but the
warning is informational only. Migrate them at your next major refactor
opportunity, not as part of routine work.

**Q: Can I mix `drain` and `gc.output_json` in the same formula?**

No. Use one or the other. A `graph.v2` formula should use `drain` exclusively.

**Q: What does `context = "shared"` do?**

Shared drains materialize one single-lane item at a time with continuation
affinity. This is useful when item work requires exclusive sequential access
to a shared resource. Shared drains require `[steps.drain.item].single_lane = true`.
Separate drains (`context = "separate"`) are the common case.

**Q: Can drain item formulas use drain themselves?**

Not in v0. Nested drains are not supported. Item formulas are ordinary
`graph.v2` formulas without a drain step.

**Q: What happens when an item fails?**

Depends on `on_item_failure`:
- `"continue"` — remaining items keep running; the drain control step closes
  with a partial-success outcome after all items finish.
- `"skip_remaining"` — no new items start after the first failure.

**Q: Is there a `max_units` limit?**

In v0, `max_units` must be between 1 and 100. If unset, the drain expands
to all convoy members. Always set `max_units` in production formulas to
prevent accidental runaway materialization.

**Q: Where does the item formula find the work item?**

The item formula receives `convoy_id` pointing to a one-member item convoy.
The item formula inspects convoy membership (not special metadata) to find
the work bead.

## See Also

- [`engdocs/design/convoy-first-formulas-and-drain-v0.md`](design/convoy-first-formulas-and-drain-v0.md) — design spec for drain v0
- [`internal/formula/types.go`](https://github.com/gastownhall/gascity/blob/main/internal/formula/types.go) — `DrainSpec` and `DrainItemSpec` struct definitions
- [`internal/dispatch/fanout.go`](https://github.com/gastownhall/gascity/blob/main/internal/dispatch/fanout.go) — `gc.output_json` / `gc.fanout_state` runtime (legacy)
