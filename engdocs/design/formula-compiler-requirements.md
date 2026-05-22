# Formula Compiler Requirements

| Field | Value |
|---|---|
| Status | Implemented |
| Date | 2026-05-20 |
| Issue | [gastownhall/gascity#1760](https://github.com/gastownhall/gascity/issues/1760) |
| Replaces | Formula `contract = "graph.v2"` as the user-facing compiler requirement |

This document defines the small authoring surface for formulas that need a
minimum formula compiler capability.

## Problem

Formula v2 currently uses a legacy contract field:

```toml
formula = "code-review-loop"
contract = "graph.v2"
```

That field is doing one real job: saying the formula needs a compiler with
formula-v2 capability. It should be expressed as a requirement, not as a
runtime contract or compiler selector.

## Decision

Formula files may declare requirements in a top-level `[requires]` table:

```toml
formula = "code-review-loop"

[requires]
formula_compiler = ">=2.0.0"
```

`requires.formula_compiler` is a semver comparator string. It declares the
minimum formula compiler capability needed to parse and compile the formula.
It does not select a compiler implementation.

Gas City will initially expose a hardcoded formula compiler capability
constant. The active host capability is:

```text
min(binary formula compiler capability, city enabled formula compiler capability)
```

`daemon.formula_v2` defaults to true. A city that explicitly sets
`[daemon] formula_v2 = false` is saying it only has compiler capability `1.x`,
even if the binary can compile v2 formulas. A formula that requires
`formula_compiler = ">=2.0.0"` must fail before any durable work is written in
that city.

## Semantics

### Accepted default

If `[requires]` is omitted, the formula declares no host capability
requirements.

```toml
formula = "simple-review"
```

This is vacuously satisfied. The formula stays on the legacy compiler contract
unless it also uses the legacy `contract = "graph.v2"` alias.

### Accepted v2 requirement

```toml
[requires]
formula_compiler = ">=2.0.0"
```

This requires formula compiler capability 2 or newer.

### Legacy alias

During migration, this legacy form is accepted as an alias for
`formula_compiler = ">=2.0.0"`:

```toml
contract = "graph.v2"
```

If both `contract = "graph.v2"` and `[requires] formula_compiler = ">=2.0.0"`
are present, they are consistent and valid.

If both are present but disagree, compilation fails before durable work is
written.

### Composition through extends

Formula requirements are inherited safety constraints, not overrideable config.
The effective requirement set for a resolved formula is the conjunction of:

- the child formula's direct `[requires]` entries
- every resolved parent formula's effective requirements
- requirements implied by legacy parent or child `contract = "graph.v2"`

A child may strengthen a parent requirement by adding another comparator, but it
cannot weaken or erase the parent constraint. Multiple parents all contribute
their constraints. If the combined constraints do not overlap, resolution fails
before durable work with `formula.compiler_requirement_conflict` and names the
formula sources that contributed the incompatible requirements.

### Unknown requirements

Unknown keys under `[requires]` fail validation.

```toml
[requires]
state_store = ">=2.0.0"
```

This is rejected. Gas City should not silently ignore requirement axes it does
not understand.

## Validation Rules

`internal/formula` owns requirement validation.

Validation must run before any durable work is written, including molecule root
beads, child step beads, order-run roots, workflow metadata, or convergence
records.

Minimum rules:

1. `[requires]` must be a top-level TOML table.
2. `formula_compiler`, when present, must be a string.
3. `formula_compiler` must parse as a standard semver comparator expression.
4. The active host compiler capability must satisfy the comparator.
5. Unknown keys under `[requires]` are rejected.
6. `contract = "graph.v2"` is accepted as a deprecated alias for
   `formula_compiler = ">=2.0.0"`.
7. Conflicting legacy and new declarations are rejected.
8. Parent and child formula requirements compose by conjunction through
   `extends`; child formulas cannot weaken or erase parent requirements.
9. Non-overlapping composed requirements are rejected with
   `formula.compiler_requirement_conflict`.

Gas City should use a standard semver comparator implementation rather than a
custom parser.

## Error Contract

Keep diagnostics simple for now. Requirement failures must include a stable
machine-readable code and an actionable message.

Example unsatisfied requirement:

```text
formula.compiler_requirement_unsatisfied: formula requires formula_compiler >=2.0.0, but this city has formula compiler capability 1.x because [daemon] formula_v2 is disabled
```

Example unknown requirement:

```text
formula.requirement_unknown: unknown formula requirement "state_store"; supported requirements: formula_compiler
```

Example invalid comparator:

```text
formula.compiler_requirement_invalid: formula_compiler must be a semver comparator, for example ">=2.0.0"
```

Example inherited requirement conflict:

```text
formula.compiler_requirement_conflict: formula "child" has non-overlapping formula_compiler requirements: <2.0.0 from formula "legacy-parent" [requires]; >=2.0.0 from formula "v2-parent" [requires]
```

These codes are enough for CLI, controller, order, and test assertions. Do not
build a broader typed diagnostic system until there is a concrete consumer that
needs it.

## Doctor Surface

`gc doctor` includes a formula requirements check over visible city and rig
formula layers. It reports deprecated `contract = "graph.v2"` aliases with
migration guidance, v2-only formula constructs that lack an explicit compiler
requirement, cities that disable `formula_v2` while visible formulas require
compiler v2, invalid or unknown requirement axes, and inherited requirement
conflicts that would otherwise only appear when cooking a formula.

## Compatibility

New formulas that only use `[requires]` may be invisible to old Gas City
versions that only understand `contract = "graph.v2"`. That is acceptable.

First-party formulas may keep both declarations during migration when old
binaries still need to read them:

```toml
formula = "code-review-loop"
contract = "graph.v2"

[requires]
formula_compiler = ">=2.0.0"
```

No pack-level minimum version mechanism is required by this design. Add one
later only if real compatibility pressure proves it is needed.

Formula `version = ...` is not part of the compiler-capability contract and is
no longer a supported syntax switch. Author new formulas with `[requires]`
instead.

## Non-Goals

- Cataloging every formula-v2 syntax feature.
- Defining per-formula compiler supported-syntax matrices.
- Adding runtime compiler selection.
- Adding multiple compiler implementations.
- Defining pack-level semver floors.
- Building a typed diagnostic framework.
- Designing future requirement axes before they exist.

## Implementation Shape

The implementation should be small:

1. Add a hardcoded current formula compiler capability constant.
2. Parse `[requires]` in `internal/formula`.
3. Normalize legacy `contract = "graph.v2"` into the same requirement model.
4. Validate requirements before compile/apply paths create durable work.
5. Add focused tests for accepted, rejected, and host-disabled cases.

The design intentionally stops there.
