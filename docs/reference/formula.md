---
title: Formula Files
description: Structure and placement of Gas City formula files.
---

Gas City resolves formula files from configured formula layers and stages the
winning `*.formula.toml` files into `.beads/formulas/` with
[`ResolveFormulas`](https://github.com/gastownhall/gascity/blob/main/cmd/gc/formula_resolve.go).

Formula instantiation happens through the store interface:

- `Store.MolCook(formula, title, vars)` creates a new molecule or wisp
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
| `formula` | string | Unique formula name used by `gc mol create` and `Store.MolCook*` |
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

## Variable Substitution

Formula descriptions can use `{{key}}` placeholders. Variables are supplied as
`key=value` pairs when the formula is instantiated, for example:

```bash
gc sling --formula deploy --var env=prod worker
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

- City-level layers are resolved from `[formulas].dir`
- Rig-local overrides come from `[[rigs]].formulas_dir`
- Pack formulas participate through pack composition and formula layers

For the current formula-resolution behavior, see
Architecture: Formulas & Molecules (`engdocs/architecture/formulas`).
