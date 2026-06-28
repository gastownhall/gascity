---
title: Gas City Formula Specification — v2
description: Authoritative specification for the formulas v2 contract.
---

| Field | Value |
|---|---|
| Status | Authoritative specification |
| Last verified | 2026-06-12 |
| Contract | `formula_compiler >=2.0.0` (deprecated alias: `contract = "graph.v2"`) |
| Primary implementation | `internal/formula`, `internal/graphv2`, `internal/dispatch`, `internal/molecule` |
| Concept model | [How Gas City Works](/getting-started/how-gas-city-works) — where a formula (the HOW) sits among the six primitives |
| User-facing guide | [Understanding Formulas](/guides/understanding-formulas) |
| Tutorial | [Formulas tutorial](/tutorials/05-formulas) |

This document specifies the **formulas v2** contract: the file format a
formula author writes, how the v2 compiler turns it into a graph of beads,
and what the orchestrator's control dispatcher does with the compiled graph at
runtime. It is self-contained: the authoring surface shared with v1 is
specified here in full, and graph-only constructs state their declaration
requirement where they appear.

v1 and v2 are peer contracts; both are supported. The v1 contract is
specified separately in the
[Formula Specification — v1](/reference/specs/formula-spec-v1). v2 is not a strict
superset of v1 — section 5 specifies the differences that keep v1 the
contract `gc converge` accepts today, and the tracked container-dependency
gap ([#3451](https://github.com/gastownhall/gascity/issues/3451)).

The key words "must", "must not", "required", "shall", "shall not",
"should", "should not", and "may" are to be interpreted as normative
requirements unless the paragraph is explicitly marked as non-normative.

## 0. Concept And Data Model

### 0.1. Concept

A formula is a TOML file specifying *how* work should be carried out — its
steps, their ordering and dependencies, and the control flow around them. A
formula is not the work itself (a bead is a unit of work) nor a grouping of
work (a convoy is a graph of related work); it is the reusable method that
*produces* work when applied. The method is defined independently of how its
work is stored; the data model below is the *current implementation* of that
storage, not part of the definition of a formula.

### 0.2. Data Model

Compilation produces a **recipe**: a flattened, validated list of steps and
dependency edges. Instantiation (`gc formula cook`, `gc sling --formula`, or
the Go API `molecule.Cook` / `molecule.CookOn` / `molecule.Attach` in
`internal/molecule`) materializes the recipe into the bead store.

Under the v2 contract the materialized shape is a **workflow**:

```text
formula (TOML)
  → compiled recipe (flat, topologically ordered)
    → workflow root bead          (type "task", gc.kind = "workflow")
    + step beads                  (independently routable work, blocking deps only)
    + control beads               (orchestrator-owned: check, retry, fanout,
                                   drain, scope-check, workflow-finalize)
```

Execution responsibility is split by bead kind:

- **The orchestrator executes every control bead.** The control dispatcher in
  `internal/dispatch` evaluates check and retry budgets, expands fan-outs,
  scatters drains, enforces scope failure policy, and finalizes the
  workflow. No agent participates in control execution.
- **Agents execute only plain work beads.** Step beads are
  independently Ready-visible and routable, so different steps of one
  workflow may be worked by different agents, pools, or providers.

The execution model is the structural difference from v1:

| | v1 | Formulas v2 |
|---|---|---|
| Compiled shape | Parent-child molecule tree under a `molecule` container root | Flat graph: `task` root plus step beads linked only by blocking dependency edges |
| Runtime engine | None. Conditions and loops resolve at cook time; afterwards the molecule is inert data | The orchestrator's control dispatcher executes every control bead — check and retry evaluation, fan-out, drain, scope checks, workflow-finalize |
| Who advances work | Agents working hooked beads, inside their own sessions | The orchestrator drives orchestration outside any agent session; agents only run plain work beads |
| Agent fan-out | The molecule is typically worked by the one agent it is slung to; spreading steps across agents is manual routing | Step beads are independently routable; per-step routing intent resolves at dispatch, and `drain` / `on_complete` fan out across agents or pools at runtime |
| Root visibility | The container root is the molecule's handle | The root blocks on `workflow-finalize` and only becomes Ready when the workflow completes (section 2) |

A minimal v2 formula:

```toml
formula = "pancakes"
description = "Make pancakes from scratch"

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "dry"
title = "Mix dry ingredients"
description = "Combine flour, sugar, baking powder, salt in a large bowl."

[[steps]]
id = "wet"
title = "Mix wet ingredients"
description = "Whisk eggs, milk, and melted butter together."

[[steps]]
id = "combine"
title = "Combine wet and dry"
description = "Fold wet ingredients into dry. Do not overmix."
needs = ["dry", "wet"]

[[steps]]
id = "cook"
title = "Cook the pancakes"
description = "Heat griddle to 375F. Pour 1/4 cup batter per pancake."
needs = ["combine"]

[[steps]]
id = "serve"
title = "Serve"
description = "Stack pancakes on a plate with butter and syrup."
needs = ["cook"]
```

## 1. File Format

This section specifies the full authoring surface. Every construct here is
accepted under the v2 contract. Constructs marked **graph-only** (`check`,
`retry`, `drain`, `on_complete`, `timeout`, and reserved `gc.*`
step metadata) additionally require the explicit contract declaration of
section 5 — they are rejected without it.

### 1.1. File Naming And Layers

Formula files live in `formulas/` directories:

| Filename | Status |
|---|---|
| `formulas/<name>.toml` | Canonical |
| `formulas/<name>.formula.toml` | Accepted deprecated spelling; the `.formula` infix is not part of the formula name |
| `formulas/<name>.formula.json` | Loader-only deprecated fallback; excluded from symlink staging |

Formula directories are collected into layers, ordered lowest to highest
priority:

| Layer | Directory |
|---|---|
| 1 | `formulas/` directories from city packs (imported packs) |
| 2 | The city's own `formulas/` directory |
| 3 | `formulas/` directories from rig packs |
| 4 | The rig's `formulas_dir` (rig-local override declared on `[[rigs]]`; relative paths resolve against the city directory) |

Resolution must be last-wins across layers: the highest-priority layer
containing a formula name wins. Within a single layer, `<name>.toml` beats
`<name>.formula.toml`, which beats `<name>.formula.json`.

At city start, init, rig add, and supervisor start, the resolver symlinks
each winning file into `<scope>/.beads/formulas/` under both `<name>.toml`
and the deprecated `<name>.formula.toml` alias. Real (non-symlink) files
already present in `.beads/formulas/` are never overwritten.

City-level `[formulas].dir` is not valid configuration. Authoring it is a
hard config error:

```text
[formulas].dir is no longer supported; use the well-known formulas/ directory
```

and the `gc doctor` check `v2-formulas-dir` reports any remaining
declaration as an error.

> **Compatibility:** Builds predating the shared last-wins resolver
> (issue #2027, fixed by #2028) resolved `gc formula show`/`cook`/`sling`
> first-wins, letting imported pack formulas shadow same-name city
> overrides. If an override does not take effect, inspect
> `gc formula show <name>` against the layer you expect to win.

### 1.2. Top-Level Keys

| Key | Type | Purpose |
|---|---|---|
| `formula` | string | Required. Unique formula name used by `gc formula cook`, `gc sling --formula`, and `molecule.Cook`/`CookOn` |
| `description` | string | Human-readable description; supports `{{var}}` substitution |
| `requires` | table | Host capability requirements. `formula_compiler` (a semver comparator) is the only axis; unknown axes fail with `formula.requirement_unknown` (section 5) |
| `contract` | string | Deprecated v2 opt-in. Only valid value: `"graph.v2"`; anything else fails validation. Prefer `[requires]` (section 5) |
| `extends` | []string | Parent formulas to compose from (section 1.7) |
| `vars` | table | Template variable declarations (section 1.4) |
| `steps` | []table | Work items to create (section 1.3) |
| `type` | string | `workflow` (default), `expansion`, or `aspect` |
| `phase` | string | Legacy v1 materialization mechanics, not a v2 authoring choice: `"liquid"` (pour) or `"vapor"`. `phase = "vapor"` without `pour` compiles a root-only recipe — steps are not materialized as beads. This selects how v1 stores a run's work (see the section 0.1 hedge: the storage shape is implementation, not part of the definition of a formula); it is accepted for compatibility and must not be used to design new formulas |
| `pour` | bool | Materialize each step as a bead row (checkpoint recovery). Default `false`. Monotonic through `extends`: any ancestor's `pour = true` sticks |
| `catalog` | table | `{name, description}` opting the formula into workflow-catalog discovery (`gc formula catalog`) |
| `template` | []table | Expansion template steps for `type = "expansion"` formulas (`{target}` / `{target.description}` placeholders) |
| `compose` | table | Advanced composition rules: `bond_points`, `hooks`, `expand`, `map`, `branch`, `gate`, `aspects` (section 1.7) |
| `advice` | []table | Advanced before/after/around step transformations applied during cooking |
| `pointcuts` | []table | Advanced target patterns for `type = "aspect"` formulas |

Unknown top-level keys are silently ignored, with one exception: unknown
keys inside `[requires]` are hard errors (section 5).

### 1.3. Steps

Each `[[steps]]` entry becomes one bead in the instantiated recipe. Rows
marked graph-only require the explicit v2 declaration (section 5); a
formula that uses them without it must fail to compile.

| Key | Type | Declaration | Purpose |
|---|---|---|---|
| `id` | string | — | Required. Unique across the whole formula, including `children` |
| `title` | string | — | Required unless `expand` is set; supports `{{var}}` substitution |
| `description` | string | — | Step instructions shown to the agent; supports `{{var}}` |
| `description_file` | string | — | Path to a file whose contents replace `description` (section 1.8) |
| `notes` | string | — | Additional notes; supports `{{var}}` |
| `type` | string | — | Issue type: `task`, `bug`, `feature`, `epic`, `chore` (conventional vocabulary; not validated) |
| `priority` | int | — | 0–4; out-of-range values are rejected |
| `tags` | []string | — | Labels applied to the created bead. The TOML key is `tags`; deprecated JSON formulas spell it `labels` — a TOML `labels` key is silently ignored |
| `metadata` | table | — | String key/value pairs copied to the cooked bead. `gc.*` keys are reserved for the runtime; several force the v2 declaration (section 2) |
| `depends_on` | []string | — | Step IDs this step blocks on; must reference known IDs |
| `needs` | []string | — | Simpler alias for `depends_on`; both are real and merged during cooking |
| `condition` | string | — | Compile-time include/exclude (section 1.5) |
| `children` | []step | — | Nested sub-steps; IDs share the formula-wide namespace |
| `assignee` | string | — | Default assignee; supports `{{var}}` |
| `expand` | string | — | Inline an expansion formula here (the step is replaced by its template steps) |
| `expand_vars` | table | — | Variable overrides for the inline expansion |
| `loop` | table | — | Iteration container (section 1.6) |
| `waits_for` | string | — | Fanout gate: `all-children`, `any-children`, or `children-of(step-id)`; the referenced step must exist (sections 2 and 4) |
| `gate` | table | — | Async wait condition `{type, id, timeout}` (sections 2 and 4) |
| `check` | table | graph-only | Inline run/check verification loop (section 3.1) |
| `retry` | table | graph-only | Transient retry loop (section 3.2) |
| `drain` | table | graph-only | Scatter the input convoy into unit convoys (section 3.3) |
| `on_complete` | table | graph-only | Runtime fan-out over step output (section 3.4) |
| `tally` | table | graph-only | Removed from the SDK; authored formulas must not use it (section 3.4) |
| `timeout` | duration string | graph-only | Max duration for this step's `check` script; requires `check`; `check.check.timeout` takes precedence |

<Note>
Unknown step keys are silently ignored. A typo like `dependson` produces no
diagnostic — the dependency simply vanishes.
</Note>

### 1.4. Variables

Declare variables in a top-level `[vars]` table. Two forms exist: a string
shorthand that sets only the default, and a table form with validation.

```toml
formula = "deploy"
description = "Deploy {{env}} from {{branch}}"

[requires]
formula_compiler = ">=2.0.0"

[vars]
branch = "main"

[vars.env]
description = "Deployment environment"
required = true
enum = ["dev", "staging", "prod"]

[[steps]]
id = "deploy"
title = "Deploy {{env}}"
```

Table-form fields:

| Field | Type | Purpose |
|---|---|---|
| `description` | string | What the variable is for; shown by `gc formula show` |
| `default` | string | Value used when none is provided. An explicit empty string is a valid default |
| `required` | bool | The variable must be provided at instantiation. Declaring `required = true` together with a `default` fails validation: `vars.<name>: cannot have both required:true and default` |
| `enum` | []string | Allowed values; enforced at instantiation |
| `pattern` | string | Regex the value must match; enforced at instantiation |
| `type` | string | Parsed but not enforced (section 4) |

`{{key}}` placeholders substitute into descriptions, titles, notes,
assignee, and metadata values. Values are supplied as `key=value` pairs at
instantiation:

```bash
gc sling worker deploy --formula --var env=prod
```

**Reserved names.** A v2 formula must not declare vars named `convoy_id` or
`bead_id` — validation fails with
`vars.<name>: formulas v2 reserved variable cannot be declared` — and
callers must not supply them
(`formulas v2 reserved variable "<name>" cannot be supplied by the caller`).
`issue` is tolerated as a deprecated compat alias:

- `{{convoy_id}}` — the input convoy of a targeted invocation. References
  must be spelled exactly `{{convoy_id}}` (any other spelling fails with
  `convoy_id requires a targeted formulas v2 invocation`), and any
  reference forces a targeted invocation (`gc sling --on <formula>` or
  `gc formula cook --attach`); an untargeted invocation fails with
  `v2 formula "<name>" requires a target convoy`. Enforcement happens at
  invocation time (section 3).
- `{{bead_id}}` — forbidden: `bead_id is not available in v2 formulas; use
  convoy_id`.
- `{{issue}}` — deprecated one-release compat alias resolving to the single
  tracked member of the input convoy; cook and sling print warnings:
  `deprecated in formulas v2 and removed next release; migrate to the
  convoy_id work-bead derivation (gastownhall/gascity#2941)`.

### 1.5. Conditions

A step `condition` is a compile-time include/exclude filter evaluated
during compilation, before any beads exist. The grammar is:

| Form | Meaning |
|---|---|
| `{{var}}` | Include when the value is truthy |
| `!{{var}}` | Include when the value is falsy |
| `{{var}} == value` | Include on equality (quotes around `value` are stripped) |
| `{{var}} != value` | Include on inequality |

Falsy values are: empty string, `false`, `0`, `no`, `off`. Everything else
is truthy. Excluded steps are removed from the recipe along with their
dependency edges.

This grammar applies only to step `condition`. Loop `until` conditions use
the runtime condition evaluator's grammar instead (section 1.6).

### 1.6. Loops

A step with a `[steps.loop]` table becomes an iteration container that
expands its `body` steps. Exactly one of `count`, `until`, or `range` is
required, and `body` must be non-empty.

| Mode | Keys | Expansion |
|---|---|---|
| `count` | `count = N` | Compile time: the body is expanded N times, with iteration N+1 chained after iteration N |
| `range` | `range = "start..end"`, optional `var` | Compile time: bounds support integers, `+ - * / ^`, parentheses, and `{var}` substitution; `var` exposes the iteration value as `{var}` in body steps |
| `until` | `until = "<condition>"`, `max = N` (required) | Compile time: one iteration is expanded and its first body step is labeled with the condition and `max` budget for runtime re-execution — which no current runtime performs (section 4) |

A range loop (the same shape works with `count`):

```toml
formula = "hanoi"

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "moves"
title = "Tower moves"

[steps.loop]
range = "1..3"
var = "move_num"

[[steps.loop.body]]
id = "move"
title = "Move {move_num}"
```

`gc formula show hanoi` renders the chained iterations (the
`workflow-finalize` step comes from the v2 contract — section 2):

```text
Formula: hanoi

Steps (4):
  ├── hanoi.moves.iter1.move: Move 1
  ├── hanoi.moves.iter2.move: Move 2 [needs: hanoi.moves.iter1.move]
  ├── hanoi.moves.iter3.move: Move 3 [needs: hanoi.moves.iter2.move]
  └── hanoi.workflow-finalize: Finalize workflow [needs: hanoi.moves.iter3.move]
```

An until loop expands a single iteration and records the condition and
`max` budget as a `loop:` label on the first body step:

```toml
formula = "poll-until"

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "poll"
title = "Poll for completion"

[steps.loop]
until = "probe.status == 'complete'"
max = 5

[[steps.loop.body]]
id = "probe"
title = "Probe the endpoint"
```

```text
Formula: poll-until

Steps (2):
  ├── poll-until.poll.iter1.probe: Probe the endpoint
  └── poll-until.workflow-finalize: Finalize workflow [needs: poll-until.poll.iter1.probe]
```

<Warning>
Two caveats. First, the re-run never happens in the current release: the
`until` label is written but nothing consumes it, so an `until` loop runs
exactly one iteration (section 4). Prefer `check` (section 3.1) for
orchestrator-driven re-execution. Second, `until` does not use the
`{{var}} == value` step-condition syntax: `until = "{{ready}} == yes"`
fails with `unrecognized condition format`. The grammar is the runtime
condition evaluator's: `probe.status == 'complete'`,
`step.output.field == value`, `children(x).all(status == 'complete')`,
`steps.complete >= 3`.
</Warning>

### 1.7. Composition And Inheritance

`extends` composes a child formula from one or more parents:

- Child steps replace parent steps with the same ID whole-step (no
  field-level merge), preserving the parent's position; new child steps
  append.
- Parent vars are inherited; child declarations override.
- `phase` is taken from the child, else the first parent that declares one.
- `pour` is monotonic: any ancestor's `pour = true` sticks; a child cannot
  opt out.
- `contract` and `requires` come from the child, else the first parent —
  and requirement constraints from *every* parent are still collected and
  validated as a set (section 5).
- Circular `extends` chains must fail
  (`circular extends detected: a -> b -> a`).

Expansion formulas (`type = "expansion"`) declare `template` steps with
`{target}` / `{target.description}` placeholders; a step's `expand` key
inlines an expansion formula in place of the step, with `expand_vars`
overriding variables for that expansion.

<Warning>
The formula resolved through `extends` drops `advice` and `pointcuts`
entirely — including the child's own. And when both parent and child
declare `compose` rules, the merge keeps only `bond_points`, `hooks`,
`expand`, and `map`; `branch`, `gate`, and `aspects` rules from both sides
are dropped. Do not rely on full inheritance of composition rules.
</Warning>

### 1.8. Description Files

Use `description_file` when a step's instructions should live in a separate
Markdown file instead of inline TOML. Non-asset paths resolve relative to
the formula file.

Paths using `../assets/...` resolve through the same low-to-high formula
layer order as the formula itself (section 1.1). A bundled formula can
reference:

```toml
description_file = "../assets/workflows/review/local-review.md"
```

and a city or higher-priority pack can override only that prose by
providing the same asset path next to its formula layer. The formula
structure and step IDs remain inherited from the lower-priority pack; only
the description file content is shadowed. Other relative or absolute paths
that happen to contain an `assets` segment still use normal
formula-relative or absolute resolution. Description file reads use the
same configured source as formula reads, so a parser pinned to a git ref
also reads committed description file content from that ref.

Two behaviors are normative:

- Files larger than 4096 bytes are not inlined. The step's description is
  replaced by a generated pointer that directs the agent to read the file
  at its resolved path.
- A v2 formula must fail fast on an unresolvable `description_file`. (v1
  formulas skip it best-effort and keep any inline `description`.)

### 1.9. Validation

A formula must satisfy these structural rules; violating any is a
validation error:

- `formula` name is required.
- `contract`, if set, must be `"graph.v2"`
  (`contract: invalid value "<value>" (must be graph.v2)`).
- `type` must be `workflow`, `expansion`, or `aspect`.
- A var must not combine `required = true` with a `default`.
- Step `id` is required and globally unique, including `children`.
- Step `title` is required unless `expand` is set.
- `priority` must be 0–4.
- `depends_on` / `needs` entries must reference known step IDs (including
  children).
- `waits_for` must match `all-children`, `any-children`, or
  `children-of(step-id)` with the referenced step present
  (`waits_for has invalid value "<value>" (must be all-children,
  any-children, or children-of(step-id))`).
- `on_complete`: `for_each` and `bond` must be set together; `for_each`
  must start with `output.`; `parallel` and `sequential` are mutually
  exclusive.
- `tally` is removed; formulas that still declare `[steps.tally]` fail with
  `steps.tally was removed from the SDK`.
- `check`: `max_attempts` ≥ 1; the inner `check` table is required with
  `mode = "exec"` (the only supported checker) and a non-empty `path`.
  Unexpected keys fail:
  `step.check: unsupported key "<key>" (expected max_attempts or check)` /
  `step.check.check: unsupported key "<key>" (expected mode, path, or
  timeout)`.
- `retry`: `max_attempts` ≥ 1; `on_exhausted` must be `hard_fail` or
  `soft_fail`.
- `drain`: see section 3.3 for field constraints and incompatibilities.
- `timeout` must be a positive Go duration and requires `check`.
- Loops (validated during control-flow expansion): `body` non-empty;
  exactly one of `count` / `until` / `range`; `max` required with `until`.

Construct-combination restrictions are specified with each construct in
section 3.

## 2. Compilation

Compilation is a fixed pipeline: load → resolve `extends` →
reserved-symbol validation → control-flow expansion (loops, branches,
gates) → advice → inline expansion → compose expand/map → aspects →
condition filtering → standalone expansion → requirement merge and the
explicit-declaration check → retry transform → check transform → host
requirement validation → graph validation → graph control injection →
recipe. Requirement constraints contributed by composed expansion and
aspect formulas are merged before validation (section 5).

The v2 compiler must emit a flat, topologically ordered graph:

- **Blocking dependency edges only.** Step beads carry `blocks` edges from
  `needs` / `depends_on` (and readiness-blocking `waits-for` edges from
  `waits_for`). The compiler creates no parent-child edges between graph
  steps; nesting in `children` affects ID namespacing and validation, not
  runtime hierarchy.
- **`workflow-finalize` is appended.** A control step with ID
  `workflow-finalize` (kind `workflow-finalize`) is added depending on
  every sink step, so it becomes Ready exactly when all other work is
  terminal.
- **The root blocks on the finalize step.** The workflow root bead is made
  to depend on `workflow-finalize` (or, when a recipe has no finalize step,
  on every step whose `gc.kind` is not one of the generated `run`, `check`,
  `retry-run`, `retry-eval`, or `spec` kinds).
  Consequence: the root is never Ready-visible while the workflow runs and
  only surfaces when the workflow completes. Step beads — not the root —
  are the Ready-visible work that wakes agents and pools.
- **Non-blocking `tracks` edges to the root.** Batch instantiation connects
  every non-root node to the root with a `tracks` edge so cascade deletion
  from the root discovers all workflow beads without making the root a
  readiness blocker.
- **Step beads keep routable types.** Non-root graph step beads are not
  coerced to scaffolding types, so `Ready()` surfaces them for worker
  claim and each step is independently routable.
- **Cycles are rejected**: `v2 formula "<name>" contains a dependency
  cycle`.

**Root stamping.** The recipe root is type `task` with
`gc.kind = "workflow"` plus a `gc.formula_contract` marker recording the
contract identifier (the same literal value accepted by the deprecated
`contract` key). Sling additionally stamps the root with
`gc.input_convoy_id` (targeted invocations), `gc.graphv2_root_key` (an
idempotency key deduplicating repeat instantiations of the same workflow),
and `gc.graphv2_vars.v1` (runtime variable snapshot). Non-batch
instantiation stamps `gc.formula_hash` (SHA-256 of the raw formula file
bytes) and `gc.formula_source` on the root; `gc formula version-check
<bead-id>` compares the stored hash against the on-disk recipe to detect
formula drift since spawn.

**`gc.kind` vocabulary.** The compiler and dispatcher reserve these values
of the `gc.kind` step-metadata key (all hyphenated):

| Group | Values | Author may set |
|---|---|---|
| Control kinds (dispatched by the orchestrator) | `retry`, `ralph`, `check`, `retry-eval`, `fanout`, `drain`, `scope-check`, `workflow-finalize` | No — compiler/orchestrator-owned; authored values are not validated and produce unspecified dispatcher behavior |
| Structural kinds (compiled into graphs, never dispatched) | `scope`, `cleanup`, `run`, `retry-run` | `scope` and `cleanup` only (section 3.5) |
| Root kinds | `workflow`, `wisp` | No — stamped by the compiler |
| Sidecar | `spec` | No — generated step-spec sidecars |

A drain step must not set `gc.kind` at all (section 3.3). Authoring any of
the reserved kind values, or any of the scope keys `gc.scope_name`,
`gc.scope_role`, `gc.scope_ref`, `gc.continuation_group`, `gc.on_fail`,
forces the explicit v2 declaration (section 5). The compiler itself writes
`gc.on_fail = "abort_scope"` on check body members and nested retry
children (section 3.5).

**Dispatch routing intent.** A step's `gc.run_target` metadata is
compile-time routing intent: at dispatch the router resolves it into
`gc.routed_to`, the sole persisted routing key, overriding the convoy-wide
default for that step. Per-dispatch provider options ride `opt_*` step
metadata (for example `opt_model`), validated against the provider's
options schema at spawn; `gc.model` is a deprecated spelling that the
`gc doctor` check `work-option-metadata-migration` migrates to `opt_model`.

**Gates and waits_for.** A `[steps.gate]` table synthesizes a sibling gate
bead (type `gate`, title `Gate: <type> <id>`) and a `blocks` edge from the
gated step to it: the step stays blocked until the gate bead is closed.
`waits_for` records a `gate:<value>` label on the cooked step bead and
compiles a `waits-for` dependency edge on the spawner step, treated as
readiness-blocking. The gate `type` vocabulary and the `waits_for` mode
distinction have no runtime consumer (section 4).

**Preview.** `gc formula show <name>` renders the compiled recipe. For the
minimal formula of section 0 (five authored steps render as six):

```text
Formula: pancakes
Description: Make pancakes from scratch

Steps (6):
  ├── pancakes.dry: Mix dry ingredients
  ├── pancakes.wet: Mix wet ingredients
  ├── pancakes.combine: Combine wet and dry [needs: pancakes.dry, pancakes.wet]
  ├── pancakes.cook: Cook the pancakes [needs: pancakes.combine]
  ├── pancakes.serve: Serve [needs: pancakes.cook]
  └── pancakes.workflow-finalize: Finalize workflow [needs: pancakes.serve]
```

`gc formula cook pancakes` materializes the recipe; every step gets an
independent bead ID (not a child-suffix of the root), and the created
count is steps + finalize + root:

```text
Root: mc-79s
Created: 7
pancakes -> mc-79s
pancakes.combine -> mc-265
pancakes.cook -> mc-nia
pancakes.dry -> mc-b8g
pancakes.serve -> mc-k3q
pancakes.wet -> mc-0ez
pancakes.workflow-finalize -> mc-9vb
```

`gc sling <target> <formula> --formula` starts the workflow and routes the
step graph:

```text
Started workflow mc-btj (formula "pancakes") → mayor
```

## 3. Runtime

**Invocation.** `gc sling --formula` / `--on` and `gc formula cook` /
`--attach` normalize a v2 invocation before instantiation. A formula that
references `{{convoy_id}}` (or the deprecated `{{issue}}`) or contains a
drain step requires a target convoy; an untargeted invocation fails with
`v2 formula "<name>" requires a target convoy`. Targeted invocations
inject `convoy_id`, resolve the deprecated `issue` alias to the single
tracked convoy member, and stamp the root as specified in section 2. The
reserved-variable rules of section 1.4 are enforced at this point.

**Control dispatch.** The orchestrator's control dispatcher processes every
open control bead by `gc.kind`: `retry`, `ralph`, `check`, `retry-eval`,
`fanout`, `drain`, `scope-check`, and `workflow-finalize`. An
unknown control kind is a hard dispatcher error. Structural kinds
(`scope`, `cleanup`, `run`, `retry-run`) are never dispatched. Control
execution requires only the orchestrator; no user-configured agent role
participates.

### 3.1. Check

`[steps.check]` wraps a step in an inline run/check verification loop:
after each iteration closes, the orchestrator runs the configured script;
pass closes the step, fail with budget left spawns the next iteration,
exhaustion closes the step as failed.

| Key | Purpose |
|---|---|
| `max_attempts` | Total run/check attempts including the first (≥ 1) |
| `check.mode` | Checker implementation; only `"exec"` is supported |
| `check.path` | Repo-relative or absolute script path to execute |
| `check.timeout` | Script execution bound (e.g. `"2m"`); takes precedence over the step's `timeout` |

```toml
formula = "checked"

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "implement"
title = "Implement the feature"

[steps.check]
max_attempts = 3

[steps.check.check]
mode = "exec"
path = "scripts/verify.sh"
timeout = "2m"
```

Materialization: the compiler emits a spec sidecar (`<step>.spec`, kind
`spec`, carrying the serialized step definition), the first iteration
(`<step>.iteration.1`) as the agent-visible work, and keeps the original
step ID as the control bead (kind `ralph`) blocking on the live iteration:

```text
Formula: checked

Steps (4):
  ├── checked.implement.spec: Step spec for Implement the feature (spec)
  ├── checked.implement.iteration.1: Implement the feature
  ├── checked.implement: Implement the feature [needs: checked.implement.iteration.1]
  └── checked.workflow-finalize: Finalize workflow [needs: checked.implement]
```

The step `timeout` applies as a general bound on the check script; a
`check.timeout` takes precedence when both are set. A check step with
`children` wraps each iteration in a scope bead whose members default to
`gc.on_fail = "abort_scope"` (section 3.5).

`check` must not be combined with `loop`, `on_complete`, `gate`, `expand`,
`assignee`, or `retry`.

### 3.2. Retry

`[steps.retry]` wraps a step in a transient-failure retry loop. Where
`check` verifies output with a script, `retry` re-runs attempts the
orchestrator classifies as transient failures.

| Key | Purpose |
|---|---|
| `max_attempts` | Total attempts including the first (≥ 1) |
| `on_exhausted` | Terminal outcome when the budget is exhausted: `hard_fail` (default) or `soft_fail` |

```toml
formula = "retry-fetch"

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "fetch"
title = "Fetch the dataset"

[steps.retry]
max_attempts = 3
on_exhausted = "soft_fail"
```

```text
Formula: retry-fetch

Steps (4):
  ├── retry-fetch.fetch.spec: Step spec for Fetch the dataset (spec)
  ├── retry-fetch.fetch.attempt.1: Fetch the dataset
  ├── retry-fetch.fetch: Fetch the dataset [needs: retry-fetch.fetch.attempt.1]
  └── retry-fetch.workflow-finalize: Finalize workflow [needs: retry-fetch.fetch]
```

The control bead keeps the original step ID (kind `retry`); attempts are
`<step>.attempt.N`. The orchestrator classifies each closed attempt:

- **pass** — the control closes `gc.outcome = pass`, copying the attempt's
  `gc.output_json` and non-`gc.*` metadata upward.
- **hard** — the control closes `gc.outcome = fail` with
  `gc.final_disposition = hard_fail`; no further attempts.
- **transient** — with budget left, the orchestrator spawns the next
  attempt; at `max_attempts`, exhaustion applies `on_exhausted`:
  `hard_fail` closes the control `gc.outcome = fail` and
  `gc.final_disposition = hard_fail`; `soft_fail` closes it
  `gc.outcome = pass` with `gc.final_disposition = soft_fail` so
  downstream work continues with degraded coverage.

`retry` must not be combined with `check`, `loop`, `on_complete`, `gate`,
`expand`, or `children`.

### 3.3. Drain

`[steps.drain]` is the canonical v2 fan-out: it scatters the input convoy
into one-member unit convoys and runs an item formula per unit. A drain
step forces a targeted invocation, and the item formula must itself
declare the v2 contract — both at invocation
(`drain item formula "<item>" for v2 formula "<parent>" must declare the
formulas v2 contract ([requires] formula_compiler = ">=2.0.0")`) and again
at runtime.

| Key | Purpose |
|---|---|
| `formula` | Required. Item formula run per unit convoy; `{{templated}}` names are rejected in v0 |
| `context` | `separate` (default — all item roots created in parallel) or `shared` (one single-lane item root at a time) |
| `member_access` | `read` (default) or `exclusive` (records a per-member `gc.exclusive_drain_reservation`; fails if another drain owns it) |
| `max_units` | Cap on expansion: 1–100 in v0; default and hard cap 100. Exceeding the cap closes the drain failed with `gc.failure_reason = limit_exceeded` |
| `on_item_failure` | `skip_remaining` or `continue`; defaults to `continue` for separate drains, `skip_remaining` for shared. A shared skip marks remaining items skipped with reason `previous_item_failed` |
| `continuation_group` | Shared execution group suffix; valid only with `context = "shared"` |
| `item.single_lane` | Must be `true` for shared drains |

```toml
formula = "review-batch"
description = "Run a work formula for each convoy member"

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "scatter"
title = "Process every member"

[steps.drain]
formula = "mol-do-work"
context = "separate"
max_units = 20
```

A drain without the v2 declaration fails validation:
`<step>.drain: drain steps must declare the formulas v2 contract
([requires] formula_compiler = ">=2.0.0")`.

`drain` must not be combined with `assignee`, `expand`, `gate`, `loop`,
`on_complete`, `check`, `retry`, `children`, `timeout`, or an authored
`gc.kind` metadata value.

### 3.4. On-Complete And Tally

`[steps.on_complete]` is a runtime for-each fan-out over the step's
structured output: when the step completes, a formula is instantiated for
each element of a collection in its output. The compiler marks such steps
`gc.output_json_required` and injects a `<step>-fanout` control step (kind
`fanout`) carrying `gc.for_each`, `gc.bond`, the fan-out mode
(`parallel` | `sequential`), and any bond variable bindings.

| Key | Purpose |
|---|---|
| `for_each` | Path to the iterable collection in step output; must start with `output.` |
| `bond` | Formula instantiated per item. `for_each` and `bond` must be set together |
| `vars` | Variable bindings per iteration; `{item}`, `{item.field}`, and `{index}` placeholders |
| `parallel` / `sequential` | Run bonded work concurrently (default) or one at a time; mutually exclusive |

`[steps.tally]` was removed from the SDK. Use pack-level workflow logic to
aggregate fan-out results if you need a synthetic pass/fail decision. Any
formula that still declares `[steps.tally]` fails fast with
`steps.tally was removed from the SDK`.

<Note>
`drain` is the graph-native canonical fan-out. Fan-out via authored
`gc.output_json_required` step metadata is deprecated in v2 formulas:
`gc lint` warns `gc.output_json is deprecated; use drain in v2 formulas
(see: engdocs/drain-fanout.md)`. The runtime keys themselves remain live —
workers write `gc.output_json`, and the compiler still sets
`gc.output_json_required` for `on_complete` steps and check output sinks.
</Note>

### 3.5. Scopes And Failure Policy

A **scope** groups steps under a durable scope body with shared failure
policy. The scope body is an authored step with
`gc.kind = "scope"`, a `gc.scope_name`, and `gc.scope_role = "body"`;
members reference it with `gc.scope_ref = "<body-step-id>"` and a
`gc.scope_role` (`setup`, `member`, `teardown`); cleanup steps use
`gc.kind = "cleanup"`. The compiler injects a `<step>-scope-check` control
(kind `scope-check`, role `control`) for steps carrying a `gc.scope_ref`.

```toml
formula = "scoped"

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "body"
title = "Worktree scope"
needs = ["implement"]
metadata = { "gc.kind" = "scope", "gc.scope_name" = "worktree", "gc.scope_role" = "body" }

[[steps]]
id = "implement"
title = "Implement the change"
metadata = { "gc.scope_ref" = "body", "gc.scope_role" = "member", "gc.on_fail" = "abort_scope" }

[[steps]]
id = "cleanup"
title = "Tear down the worktree"
needs = ["body"]
metadata = { "gc.kind" = "cleanup", "gc.scope_ref" = "body", "gc.scope_role" = "teardown" }
```

**Failure policy.** `gc.on_fail = "abort_scope"` is the only specified
value of `gc.on_fail`. When a scoped member fails, the orchestrator skips
all remaining open scope members, propagates non-`gc.*` member metadata
onto the scope body (so diagnostics survive), and closes the body with
`gc.outcome = fail`. The worker-result contract for `abort_scope` members
is **fail-closed**: a member that closes with `gc.outcome = fail` counts
as failed, and so does a member that declared `abort_scope` and closed
with a missing or unknown `gc.outcome` — only `pass` and `skipped` do not
abort the scope. Retry-managed attempt subjects are exempt; their contract
violations are classified by retry evaluation as transient retries instead.
The compiler writes `gc.on_fail = "abort_scope"` by default on check body
members and nested retry children.

**Workflow finalize.** When `workflow-finalize` becomes Ready, the
orchestrator aggregates the outcomes of its blockers into a single
pass/fail, closes the workflow root with that outcome (root first, so a
crash retries finalization), closes generated spec sidecars, and — on pass
only — propagates closure across the `gc.source_bead_id` chain. Failures
intentionally leave parent source beads open for investigation.

## 4. Accepted But Inert

This specification is normative for implemented behavior. The constructs
in this section are accepted by the parser and compiler but have **no
runtime consumer** in the current release. Authors should not rely on
them; they are documented to prevent silent surprise.

- **Until-loop re-execution.** Compiling an `until` loop validates the
  condition and writes a `loop:{"until":...,"max":...}` label on the first
  body step (the loop expander in `internal/formula/controlflow.go`), but
  no component — neither the v1 cook path nor the v2 control dispatcher —
  reads that label. An `until` loop therefore runs exactly one iteration.
  Use `check` (section 3.1) for orchestrator-driven re-execution.
- **Gate `type` vocabulary.** `[steps.gate]` synthesizes a real gate bead
  that blocks its step until the gate bead is closed (manually or by an
  external watcher), but the `type` values `gh:run`, `gh:pr`, `timer`,
  `human`, and `mail` are doc-comment vocabulary in
  `internal/formula/types.go` — the parser never validates them and no
  bundled watcher acts on them. Zero bundled formulas use `gate`.
- **`waits_for` gate modes.** `waits_for` compiles to a readiness-blocking
  `waits-for` dependency edge plus a recorded gate mode, but no current
  dispatcher logic interprets the `all-children` / `any-children`
  distinction. Zero bundled formulas use `waits_for`.
- **`vars.<name>.type`.** The variable `type` field (`string`, `int`,
  `bool`) is parsed into the variable definition
  (`internal/formula/types.go`) but never enforced; only `required`,
  `enum`, and `pattern` are validated at instantiation.

## 5. Conformance And Compatibility

### Opt-in surface

| Declaration | Where | Status |
|---|---|---|
| `[requires] formula_compiler = ">=2.0.0"` | formula TOML | Canonical v2 opt-in |
| `contract = "graph.v2"` | formula TOML | Deprecated opt-in. `gc doctor` warns: `deprecated contract = "graph.v2"; use [requires] formula_compiler = ">=2.0.0"` |
| `[daemon] formula_v2` | city.toml | Host switch, default `true`. When `false` the host compiler capability is 1.0.0 and v2 formulas fail to compile |

The canonical declaration:

```toml
[requires]
formula_compiler = ">=2.0.0"
```

`formula_compiler` is the only `[requires]` axis. The value must be a
semver comparator; violations fail with
`formula.compiler_requirement_invalid: formula_compiler must be a semver
comparator, for example ">=2.0.0"`, and unknown axes fail with
`formula.requirement_unknown: unknown formula requirement "<key>";
supported requirements: formula_compiler`.

### Explicit declaration rule

Graph-only constructs — `check`, `retry`, `drain`, `on_complete`, and
reserved `gc.*` step metadata (the
section 2 kind values, `gc.scope_name`, `gc.scope_role`, `gc.scope_ref`,
`gc.continuation_group`, `gc.on_fail`) — require an explicit declaration.
Compiling without one must fail with:

```text
requires: formulas that use graph-only constructs must declare [requires] formula_compiler = ">=2.0.0" or the deprecated contract = "graph.v2" explicitly
```

This check runs after expansions and aspects materialize, so composed-in
constructs trigger it too.

### Requirement composition

`[requires]` composes through `extends` as a safety constraint. A child
inherits every parent requirement — including requirements implied by the
deprecated `contract = "graph.v2"` — and may only add tighter constraints.
Constraints contributed by composed expansion and aspect formulas are
merged the same way. Non-overlapping constraint sets must fail before any
durable work is written:

```text
formula.compiler_requirement_conflict: formula "<name>" has non-overlapping formula_compiler requirements: <constraint> from <source>; ...
```

An unsatisfied constraint fails as:

```text
formula.compiler_requirement_unsatisfied: formula requires formula_compiler <constraint> from <source>, but this city has formula compiler capability 1.0.0 because [daemon] formula_v2 is disabled
```

and compiling a v2 formula with the host switch off fails as:

```text
formula "<name>" requires formula compiler v2 but formula_v2 is disabled; enable [daemon] formula_v2 or lower the formula requirements
```

### Doctor and lint

The `gc doctor` check `formula-requirements` reports, per city and rig
formula layer: parse failures (error), deprecated `contract = "graph.v2"`
opt-ins (warning, message quoted above), missing explicit v2 requirements
for graph-only constructs (error), and host mismatches including disabled
`formula_v2` (error). Its fix hint is:

```text
replace deprecated contract = "graph.v2" with [requires] formula_compiler = ">=2.0.0"; enable [daemon] formula_v2 or lower requirements; fix invalid requirements and parent/child conflicts
```

`gc lint` warns on the deprecated `gc.output_json` fan-out in v2 formula
steps (section 3.4). Warnings do not fail lint.

### Differences from v1

Both contracts are supported. Two v1-side behaviors have no v2 equivalent
yet; neither is a design commitment:

- **`gc converge` accepts only v1 formulas** and rejects v2:

  ```text
  convergence wisps do not support v2 formula "<name>"; use a v1 formula until convergence has an explicit input convoy target
  ```

  The v2 construct for iterate-until-verified semantics is the check loop
  (section 3.1); `gc converge` is the pre-v2 command for this pattern.

- **Container dependencies do not gate on children.** Under v1, a step that
  `needs` a parent waits for all of that parent's children; the v2 compiler
  creates no parent-child edges, so the same dependency gates only on the
  parent step itself. This is a tracked gap
  ([#3451](https://github.com/gastownhall/gascity/issues/3451)), not a
  contract guarantee — until it lands, enumerate the children explicitly in
  `needs`.

Conversely, routing a formula to a scale-from-zero pool requires a
Ready-visible surface, which the v1 container materialization lacks; `gc sling`
rejects them. The remedy in the error text below is migration to formulas v2;
the `phase="vapor"`/root-only alternative it names is legacy v1 materialization
mechanics (section 1.2), not a v2 authoring choice:

```text
formula "<name>" root is a molecule container, not Ready-visible work; scale-from-zero pools will not wake for this wisp. Convert the formula to phase="vapor"/root-only or formulas v2 before routing it to a pool
```

### Deprecated surfaces

| Surface | Status | Replacement |
|---|---|---|
| `contract = "graph.v2"` | Deprecated opt-in; `gc doctor` warns | `[requires] formula_compiler = ">=2.0.0"` |
| `<name>.formula.toml` / `<name>.formula.json` | Deprecated spellings (section 1.1) | `formulas/<name>.toml` |
| JSON `labels` step key | Deprecated JSON spelling (section 1.3) | TOML `tags` |
| Authored `gc.output_json_required` fan-out | Deprecated; `gc lint` warns (section 3.4) | `drain` (section 3.3) |
| `{{issue}}` variable | Deprecated one-release compat alias (section 1.4) | `{{convoy_id}}` derivation |
| `gc.model` step metadata | Deprecated; `gc doctor` migrates (section 2) | `opt_model` |
