---
title: Formula Files
description: Structure and placement of Gas City formula files.
---

Gas City resolves formula files from PackV2 formula layers and stages the
winning formula files into `.beads/formulas/` with
[`ResolveFormulas`](https://github.com/gastownhall/gascity/blob/main/cmd/gc/formula_resolve.go).

Formula instantiation happens via the CLI or the store interface:

- `gc formula cook <name>` creates a molecule (every step materialized as a bead)
- `gc sling <target> <name> --formula` creates a wisp (lightweight, ephemeral)
- `Store.MolCook(formula, title, vars)` creates a molecule or wisp programmatically
- `Store.MolCookOn(formula, beadID, title, vars)` attaches a molecule to an
  existing bead

## Minimal Formula

```toml
formula = "pancakes"
description = "Make pancakes"
version = 1

[[steps]]
id = "dry"
title = "Mix dry ingredients"
description = "Combine the flour, sugar, and baking powder."

[[steps]]
id = "wet"
title = "Mix wet ingredients"
description = "Combine eggs, milk, and butter."

[[steps]]
id = "cook"
title = "Cook pancakes"
description = "Cook on medium heat."
needs = ["dry", "wet"]
```

## Common Top-Level Keys

| Key | Type | Purpose |
|---|---|---|
| `formula` | string | Unique formula name used by `gc formula cook`, `gc sling --formula`, and `Store.MolCook*` |
| `description` | string | Human-readable description |
| `version` | integer | Optional formula version marker |
| `extends` | []string | Optional parent formulas to compose from |

## Step Fields

Each `[[steps]]` entry represents one task bead inside the instantiated
molecule.

| Key | Type | Purpose |
|---|---|---|
| `id` | string | Step identifier; unique within the formula |
| `title` | string | Short step title |
| `description` | string | Step instructions shown to the agent |
| `needs` | []string | Step IDs that must complete before this step is ready |
| `condition` | string | Equality expression (`{{var}} == value` or `!=`) — step is excluded when false |
| `children` | []step | Nested sub-steps; parent acts as a container dependency |
| `loop` | object | Static loop expansion: `count` iterations at compile time |
| `check` | object | Runtime retry: `max_attempts` with a `check` script after each attempt |
| `timeout` | duration string | Default timeout for this step's `check` script; `check.check.timeout` takes precedence |

## Graph.v2 Review Quorum Formula

The core pack includes `mol-review-quorum`, a Gas City-owned review quorum
formula scaffold. It is a `graph.v2` formula that fans out exactly two reviewer
lanes and then routes synthesis for their durable outputs:

- lane one, with ID, provider, model, and dispatch target supplied by formula
  variables
- lane two, with ID, provider, model, and dispatch target supplied by formula
  variables

Lane IDs, providers, model targets, and dispatch targets are configured through
the required formula variables `lane_one_id`, `lane_one_provider`,
`lane_one_model`, `lane_one_target`, `lane_two_id`, `lane_two_provider`,
`lane_two_model`, and `lane_two_target`. The synthesis dispatch target is
configured through `synthesis_target`.
Reviewer lanes use retry semantics with
`on_exhausted = "soft_fail"` for transient provider failures so synthesis can
continue with degraded coverage when one lane exhausts its retry budget.

Reviewer and synthesis steps must persist structured JSON state for future
automation. The lane output contract includes `verdict`, `summary`,
`findings_count`, `findings`, `evidence`, `usage`,
`read_only_enforcement`, `mutations_delta`, `failure_class`, and
`failure_reason`.

Read-only enforcement is baseline-relative: reviewers compare the after state
against the mutation baseline they recorded before review with
`git status --porcelain=v1 -z`. Pre-existing dirty state and pre-existing
untracked files are not reviewer-created mutations.

`internal/reviewquorum` defines the durable Go contract and finalizer, but the
current formula synthesis step is still agent-executed and does not call
`reviewquorum.Finalize` directly. `dx-review` is a future compatibility
consumer for this durable output shape; it does not own the lifecycle of
`mol-review-quorum`.

## Variable Substitution

Formula descriptions can use `{{key}}` placeholders. Variables are supplied as
`key=value` pairs when the formula is instantiated, for example:

```bash
gc sling worker deploy --formula --var env=prod
```

## Convergence-Specific Fields

Convergence uses a formula subset defined in
[`internal/convergence/formula.go`](https://github.com/gastownhall/gascity/blob/main/internal/convergence/formula.go).

| Key | Type | Purpose |
|---|---|---|
| `convergence` | bool | Must be `true` for convergence loops |
| `required_vars` | []string | Variables that must be supplied at creation time |
| `evaluate_prompt` | string | Optional prompt file for the controller-injected evaluate step |

## Where Formulas Come From

PackV2 formula discovery is convention-based:

- a pack's reusable formulas live in `formulas/`
- a city pack's own `formulas/` layer wins over imported pack formulas
- rig-level imports can provide rig-specific formulas
- imported pack formulas keep their pack provenance during resolution

Legacy fields such as `[formulas].dir` and `[[rigs]].formulas_dir` may still
appear in the config schema for migration compatibility. New packs should use
the PackV2 `formulas/` directory convention instead of declaring formula
directories in TOML.

For the current formula-resolution behavior, see
Architecture: Formulas & Molecules (`engdocs/architecture/formulas`).
