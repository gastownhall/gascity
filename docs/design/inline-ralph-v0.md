# Inline Ralph v0

| Field | Value |
|---|---|
| Status | Draft |
| Date | 2026-03-19 |
| Author(s) | Codex |
| Issue | — |
| Supersedes | — |

Small design for a first prototype of first-class workflow beads with a
single Ralph-style retry loop.

This is intentionally narrow. It does not attempt the generalized
work/resource scheduler from
the generalized-work-resource-reconciler-v0 design.

## Problem

Today, formula-backed work is still fundamentally session-first:

- formulas are instantiated as wisps
- a single agent session tends to walk the formula
- retries/checks are not modeled as first-class reusable workflow beads

We want a tiny slice that proves the opposite direction:

- the workflow is compiled into durable beads up front
- the unit of execution is an ordinary `run` bead
- validation is an ordinary `check` bead
- retries append new attempt beads instead of reopening old work

## Goals

1. Compile one user-authored step into a stable logical step plus first-class
   `run/check` attempt beads.
2. Keep the work inline. Do not require a second run formula such as
   `mol-do-work`.
3. Drop step-local target selection. Placement should come from sling-time
   context or a formula var.
4. Route `run` work to normal agents and `check` work to a dedicated checker
   lane.
5. Keep retry append under one owner.

## Non-Goals

- General work/resource scheduling
- Intelligence tiers or provider choice
- Inference-based checks
- Cross-model transcript reuse
- Arbitrary graph expansion beyond one Ralph loop shape

## Formula Shape

V0 adds one new step sub-table: `steps.ralph`.

```toml
formula = "mol-demo-ralph"
version = 1
pour = true

[vars.run_target]
description = "Optional override for where run attempts should be routed"
default = ""

[[steps]]
id = "implement"
title = "Implement widget"
description = """
Make the code changes for the widget.
"""

[steps.ralph]
max_attempts = 3

[steps.ralph.check]
mode = "exec"
path = ".gascity/checks/widget.sh"
timeout = "2m"
```

Important properties:

- The original step body is the work.
- There is no `run_formula`.
- There is no step-local `target`.
- Placement comes from instantiation context, not the step schema.

## Compiled Beads

For a Ralph step `implement`, the compiler creates:

- a logical step bead `implement`
- ordinary bead `implement.run.1`
- ordinary bead `implement.check.1`

For a graph-style formula root, the compiler also creates:

- a plain workflow head bead with `gc.kind=workflow`

The workflow head is an ordinary bead, not a special molecule primitive.

Downstream `needs = ["implement"]` depend on the logical step bead, not on an
individual attempt bead.

This keeps stable step identity even as attempts are appended later.

## Metadata Keys

V0 keeps the metadata surface intentionally small.

### Ralph container bead

```text
gc.kind=ralph
gc.step_id=<step-id>
gc.max_attempts=<n>
```

### Run attempt bead

```text
gc.kind=run
gc.step_id=<step-id>
gc.attempt=<n>
```

The run attempt bead inherits the original step's:

- title
- description
- labels

### Check attempt bead

```text
gc.kind=check
gc.step_id=<step-id>
gc.attempt=<n>
gc.check_mode=exec
gc.check_path=<repo-relative-or-absolute-script>
gc.check_timeout=<duration>
```

### Placement metadata

Placement is not stored on the step definition.

At sling/cook time, the workflow head or attached parent bead may receive:

```text
gc.run_target=<qualified-agent-or-pool>
```

This value may come from:

- the sling target selected by the human
- a formula var such as `run_target`

If both exist, sling-time context wins.

## Target Resolution

`run` beads do not store their own target in V0.

Instead, the Ralph runtime resolves a run target by walking up the bead
parent chain:

1. current bead metadata
2. parent metadata
3. workflow root / attached work bead metadata

The first `gc.run_target` found is used.

If no run target is found, the attempt fails closed.

## Routing Rules

V0 uses bead kind, not step-local target fields, to choose the lane.

### Run beads

When a ready bead has `gc.kind=run`:

1. resolve `gc.run_target`
2. route internally with:

```bash
gc sling <target> <bead-id> --no-formula --no-convoy
```

Important:

- internal dispatch must use `--no-formula`
- internal dispatch must use `--no-convoy`

The compiled attempt bead is already the runnable unit. It must not receive
an additional default sling formula.

### Check beads

When a ready bead has `gc.kind=check`:

1. route to the checker lane
2. use the same internal shape:

```bash
gc sling checker <bead-id> --no-formula --no-convoy
```

The checker lane may be:

- a fixed `checker` agent
- a checker pool

Either way, `check` is routed by kind, not by per-step target config.

## Checker Lane

The checker lane should be a normal Gas City work lane whose command is a
script, not an LLM.

Its job is only:

1. read the claimed `check` bead
2. load `gc.check_path` and `gc.check_timeout`
3. run the check in the bead's `work_dir` when present
4. write outcome metadata
5. close the `check` bead

V0 keeps retry append out of the checker lane. The checker executes checks;
the Ralph runtime owns graph mutation.

## Pass / Fail Transitions

### Pass

1. `run.n` closes
2. `check.n` becomes ready
3. checker runs the script
4. checker records pass outcome and closes `check.n`
5. Ralph runtime closes the container bead
6. downstream steps blocked on the container bead unblock normally

### Fail with budget left

1. `run.n` closes
2. checker records fail outcome and closes `check.n`
3. Ralph runtime reads:
   - `gc.attempt=n`
   - container `gc.max_attempts`
4. if `n < max_attempts`, append:
   - `run.(n+1)`
   - `check.(n+1)`
5. new attempt beads are siblings under the same container
6. old attempt beads remain as durable history

### Fail with budget exhausted

1. checker records fail outcome and closes `check.n`
2. Ralph runtime sees `n == max_attempts`
3. Ralph runtime marks the container bead failed
4. downstream work stays blocked because the logical step never completed

## Outcome Metadata

Closed state alone is not enough to distinguish pass from fail. V0 therefore
stores explicit outcome metadata.

On `check` beads:

```text
gc.outcome=pass|fail
gc.exit_code=<n>
```

On Ralph container beads:

```text
gc.outcome=pass|fail
```

V0 rule:

- checker always closes the `check` bead after execution
- outcome is always read from metadata

## Ownership Split

V0 deliberately splits responsibilities:

- compiler: create the initial container + attempt beads
- checker lane: execute deterministic checks
- Ralph runtime: append retries and close the logical container

This keeps retry semantics under one owner while still reusing the normal
Gas City work-routing model for both `run` and `check`.

## Implementation Notes

Likely implementation seams:

- `/data/projects/gascity/internal/formula/types.go`
- `/data/projects/gascity/internal/molecule/molecule.go`
- `/data/projects/gascity/cmd/gc/cmd_ralph.go`
- `/data/projects/gascity/internal/convergence/condition.go`

## Open Questions

1. Should the checker lane be a fixed agent or a pool in V0?
2. Should `gc.run_target` be written only on the workflow root, or also copied
   onto the Ralph container for easier lookup?
3. Do we want a dedicated internal route helper instead of shelling out to
   `gc sling` for attempt dispatch?
