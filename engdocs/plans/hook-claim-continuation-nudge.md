# Hook Claim Continuation Nudge

Date: 2026-06-30

PM intake source:
- Root bead: ga-7n7vth
- Architecture decision: ga-npfasd
- Designer handoff mail: gm-wisp-nl00zob
- Diagram: /tmp/designer-ga-7n7vth/ga-7n7vth-hook-nudge-flow.png

The expected `docs/architecture/`, `docs/rules/`, and `docs/designs/`
directories were not present at intake time. This plan uses the architecture
decision and designer context embedded in the beads.

## Goal

Make self-claimed pool `graph.v2` workflow roots continue into step 1 without
operator nudges. The fix must enqueue a per-session continuation nudge when
`gc hook --claim` newly claims a workflow root and pre-assigns continuation
siblings.

Because the source bead is labeled `source:actual-designer`, all downstream
work routes directly to builder with `ready-to-build`.

## Work Packages

| Bead | Title | Routing | Dependencies |
| --- | --- | --- | --- |
| ga-7n7vth.1 | As a contributor, hook-claim continuation nudge behavior has regression coverage | ready-to-build -> gascity/builder | none |
| ga-7n7vth.2 | As a pool operator, a self-claimed graph.v2 root auto-advances without manual nudges | ready-to-build -> gascity/builder | ga-7n7vth.1 |
| ga-7n7vth.3 | As an ACP pool operator, continuation nudge dispatcher requirements are visible | ready-to-build -> gascity/builder | ga-7n7vth.2 |
| ga-7n7vth.4 | As a maintainer, hook-claim propulsion changes ship with regression validation | ready-to-build -> gascity/builder | ga-7n7vth.2, ga-7n7vth.3 |

## Acceptance Summary

`ga-7n7vth.1` is complete when focused tests cover the pool workflow-root
enqueue path, verify the exact nudge target/source/message/delivery semantics,
prove non-root step claims do not enqueue the continuation nudge, and cover
retry or re-find idempotence.

`ga-7n7vth.2` is complete when `gc hook --claim` enqueues exactly one
`hook-claim-continuation` nudge for a newly claimed workflow root with assigned
continuation siblings, targets the concrete claiming session name, uses
wait-idle delivery, starts the nudge poller, pokes the controller, logs enqueue
failure non-fatally, and leaves singleton, on-demand named, zero-step, step-bead,
and re-found claim behavior unchanged.

`ga-7n7vth.3` is complete when the enqueue site has the ACP dispatcher comment
from the design, and gh#3554 or release guidance documents the in-flight ceiling
sizing rule: `max_concurrent_formulas x (1 + max_parallel_steps)`.

`ga-7n7vth.4` is complete when focused hook/nudge tests pass, singleton and
on-demand formula root behavior is validated as unchanged, the pool graph.v2
root path is shown to advance without manual `gc session nudge`, and the builder
records `go test ./cmd/gc` plus `go vet ./...` results or a justified narrower
scope.

## Dependency Graph

ga-7n7vth.1 -> ga-7n7vth.2 -> ga-7n7vth.3 -> ga-7n7vth.4

ga-7n7vth.2 -> ga-7n7vth.4

## Out Of Scope

- New schema, metadata keys, or bead types.
- A standing Go driver loop, timeout heuristic, or stalled-work judgment.
- Role-specific behavior or hardcoded role names.
- New config defaults for ACP sessions.
- Reworking the sling-time template-targeted nudge path beyond what is needed
  to keep existing tests passing.

## Risk

The main product risk is over-enqueueing: step beads and idempotent re-finds can
also surface continuation assignment state, but only the workflow root latch
needs the new nudge. The builder should keep the trigger structural:
newly claimed bead, `gc.kind=workflow`, and at least one assigned continuation
sibling.

The main operator risk is ACP transport silence. The approved mitigation is a
source comment plus public guidance that ACP pool sessions require
`daemon.nudge_dispatcher = supervisor` for queued nudge delivery.
