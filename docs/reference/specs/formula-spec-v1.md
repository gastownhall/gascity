---
title: Gas City Formula Specification — v1
description: Authoritative specification for the formulas v1 contract.
---

| Field | Value |
|---|---|
| Status | Authoritative specification |
| Last verified | 2026-06-12 |
| Contract | `formula_compiler 1.0` (default — no declaration required) |
| Primary implementation | `internal/formula`, `internal/molecule` |
| Concept model | [Six Primitives](/getting-started/how-gas-city-works) (authoritative — where Formula fits in the taxonomy) |
| User-facing guide | [Understanding Formulas](/guides/understanding-formulas) |
| Tutorial | [Formulas tutorial](/tutorials/05-formulas) |

This document specifies the **v1** formula contract: the file format a
formula author writes, how the v1 compiler turns it into a molecule of
beads, and what happens to the molecule after instantiation. It is
self-contained: the authoring surface shared with formulas v2 is specified
here in full.

v1 and v2 are peer contracts; both are supported. v1 is the **default**: a
formula that declares no compiler requirement compiles under v1. The v2
contract is specified separately in the
[Formula Specification — v2](/reference/specs/formula-spec-v2). Section 5
specifies the v1-side behaviors v2 has not yet absorbed: the `gc converge`
command accepts only v1 formulas, and v1 container-dependency semantics have
a tracked v2 gap
([#3451](https://github.com/gastownhall/gascity/issues/3451)).

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

Under the v1 contract the materialized shape is a **molecule**: a
parent-child tree of beads.

```text
formula (TOML)
  → compiled recipe (flat list + dependency edges)
    → molecule container root      (type "molecule")
    + child step beads             (parent-child edges to their container;
                                    blocking edges from needs/depends_on)
```

A `phase = "vapor"` formula without `pour` materializes a **wisp** instead:
a single root bead (type `task`, `gc.kind = "wisp"`) with no step beads
(section 2). Vapor is a v1-era materialization shortcut from when bead
writes were expensive; it remains specified for compatibility, but new
formulas should use the v2 contract rather than reasoning in phases.

v1 has **no runtime engine**. Conditions and loops resolve at cook time;
after instantiation the molecule is inert data. No orchestrator component
advances it — agents work the molecule through their hooks, inside their
own sessions, and the bead store's dependency edges sequence the steps.
The orchestrator's control dispatcher executes only control beads, which v1
compilation never emits.

The execution model is the structural difference from v2:

| | v1 | Formulas v2 |
|---|---|---|
| Compiled shape | Parent-child molecule tree under a `molecule` container root | Flat graph: `task` root plus step beads linked only by blocking dependency edges |
| Runtime engine | None. Conditions and loops resolve at cook time; afterwards the molecule is inert data | The orchestrator's control dispatcher executes every control bead — check and retry evaluation, fan-out, drain, scope checks, workflow-finalize |
| Who advances work | Agents working hooked beads, inside their own sessions | The orchestrator drives orchestration outside any agent session; agents only run plain work beads |
| Agent fan-out | The molecule is typically worked by the one agent it is slung to; spreading steps across agents is manual routing | Step beads are independently routable; per-step routing intent resolves at dispatch, and `drain` / `on_complete` fan out across agents or pools at runtime |
| Root visibility | The container root is the molecule's handle; default-typed steps are stamped type `step` and excluded from `Ready()` (section 2) | Step beads are independently Ready-visible and routable; the root surfaces only when the workflow completes |
| Dependency semantics | A dependency on a parent gates on the parent and its children (container dependencies, section 1.3) | No parent-child edges; a dependency on a parent gates only on the parent step bead |

A minimal v1 formula — note the absence of any contract declaration:

```toml
formula = "pancakes"
description = "Make pancakes from scratch"

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

This section specifies the full authoring surface. The file format is
shared with formulas v2; every construct below parses under both
contracts. Constructs marked **v2-only** (`check`, `retry`, `drain`,
`on_complete`, `timeout`, and reserved `gc.*` step metadata)
require the explicit v2 declaration — using them in a v1 formula must fail
compilation (section 5).

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
| `requires` | table | Host capability requirements. `formula_compiler` (a semver comparator) is the only axis; unknown axes fail with `formula.requirement_unknown` (section 5). Typically absent from a v1 formula; a constraint that capability 1.0.0 satisfies keeps the formula on v1 |
| `contract` | string | Deprecated v2 opt-in. Only valid value: `"graph.v2"`; anything else fails validation. Declaring it moves the formula to the v2 contract (section 5) |
| `extends` | []string | Parent formulas to compose from (section 1.7) |
| `vars` | table | Template variable declarations (section 1.4) |
| `steps` | []table | Work items to create (section 1.3) |
| `type` | string | `workflow` (default), `expansion`, or `aspect` |
| `phase` | string | v1-era materialization hint: `"liquid"` (pour) or `"vapor"` (wisp). `phase = "vapor"` without `pour` compiles a root-only recipe — steps are not materialized as beads. Kept for compatibility; not a design surface for new formulas |
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
marked **v2-only** require the explicit v2 declaration; a v1 formula that
uses them must fail to compile with the error quoted below the table.
Their semantics are specified in the
[v2 specification, section 3](/reference/specs/formula-spec-v2#3-runtime).

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
| `metadata` | table | — | String key/value pairs copied to the cooked bead. `gc.*` keys are reserved for the runtime; several force the v2 declaration (section 5) |
| `depends_on` | []string | — | Step IDs this step blocks on; must reference known IDs |
| `needs` | []string | — | Simpler alias for `depends_on`; both are real and merged during cooking |
| `condition` | string | — | Compile-time include/exclude (section 1.5) |
| `children` | []step | — | Nested sub-steps; IDs share the formula-wide namespace. A step with `children` compiles to an `epic` container (section 2) |
| `assignee` | string | — | Default assignee; supports `{{var}}` |
| `expand` | string | — | Inline an expansion formula here (the step is replaced by its template steps) |
| `expand_vars` | table | — | Variable overrides for the inline expansion |
| `loop` | table | — | Iteration container (section 1.6) |
| `waits_for` | string | — | Fanout gate: `all-children`, `any-children`, or `children-of(step-id)`; the referenced step must exist (sections 2 and 4) |
| `gate` | table | — | Async wait condition `{type, id, timeout}` (sections 2 and 4) |
| `check` | table | v2-only | Inline run/check verification loop |
| `retry` | table | v2-only | Transient retry loop |
| `drain` | table | v2-only | Scatter the input convoy into unit convoys |
| `on_complete` | table | v2-only | Runtime fan-out over step output |
| `tally` | table | v2-only | Removed from the SDK; authored formulas must not use it |
| `timeout` | duration string | v2-only | Max duration for a `check` script; requires `check` |

Compiling a v1 formula that uses any v2-only construct must fail with:

```text
requires: formulas that use graph-only constructs must declare [requires] formula_compiler = ">=2.0.0" or the deprecated contract = "graph.v2" explicitly
```

<Note>
Unknown step keys are silently ignored. A typo like `dependson` produces no
diagnostic — the dependency simply vanishes.
</Note>

**Container dependencies.** v1 compiles containment: every step is linked
to its enclosing container — the molecule root, or the parent step for
`children` — by a parent-child edge, and a step with `children` is
promoted to issue type `epic` (section 2). Because v1 containers close
only when their members are done (the molecule root is auto-closed only
when every transitive descendant is terminal, and epic closure in the bead
store follows the epic's children), a blocking dependency on a container
waits for the container's entire subtree, not just the container bead.
This is the v1 container dependency semantic. The v2 compiler creates no
parent-child edges, so the same dependency under v2 gates only on the
named step bead.

<Warning>
The bead store restricts blocking edges across the task/epic boundary:
materialization fails with `tasks can only block other tasks, not epics`
(or its inverse) when exactly one endpoint is an `epic`. Since a step with
`children` always compiles to an `epic`, a default-typed step cannot
`needs` a parent that has `children` — the formula validates but `gc
formula cook` fails when the edge is written. Make the dependent a
container too (give it `children`), or depend on the parent's terminal
child instead.
</Warning>

### 1.4. Variables

Declare variables in a top-level `[vars]` table. Two forms exist: a string
shorthand that sets only the default, and a table form with validation.

```toml
formula = "deploy"
description = "Deploy {{env}} from {{branch}}"

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

Here `worker` is a pack-supplied agent name, not a platform built-in; targets in
these examples are illustrative names a pack declares.

**Injected names.** v1 reserves no variable names; the v2
reserved-variable rules (`convoy_id`, `bead_id`) do not apply. On a
targeted invocation (`gc sling <target> <bead-id> --on <formula>`), the
router injects `issue` — the target bead's ID — plus the routing variables
`rig_name`, `binding_name`, and `binding_prefix`, and an automatic
`base_branch` / `target_branch` when the formula references them.
Precedence, highest first: explicit `--var` > rig `formula_vars` >
routing-injected values > formula-level `default`s. Under v2 the `issue`
injection is replaced by the reserved `{{convoy_id}}` derivation, and
`issue` survives there only as a deprecated compat alias.

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
expands its `body` steps in place — the container step itself is replaced
by the expanded iterations. Exactly one of `count`, `until`, or `range` is
required, and `body` must be non-empty.

| Mode | Keys | Expansion |
|---|---|---|
| `count` | `count = N` | Compile time: the body is expanded N times, with iteration N+1 chained after iteration N |
| `range` | `range = "start..end"`, optional `var` | Compile time: bounds support integers, `+ - * / ^`, parentheses, and `{var}` substitution; `var` exposes the iteration value as `{var}` in body steps |
| `until` | `until = "<condition>"`, `max = N` (required) | Compile time: one iteration is expanded and its first body step is labeled with the condition and `max` budget for runtime re-execution — which no current runtime performs (section 4) |

A range loop (the same shape works with `count`):

```toml
formula = "hanoi"

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

`gc formula show hanoi` renders the chained iterations:

```text
Formula: hanoi

Steps (3):
  ├── hanoi.moves.iter1.move: Move 1
  ├── hanoi.moves.iter2.move: Move 2 [needs: hanoi.moves.iter1.move]
  └── hanoi.moves.iter3.move: Move 3 [needs: hanoi.moves.iter2.move]
```

An until loop expands a single iteration and records the condition and
`max` budget as a `loop:` label on the first body step:

```toml
formula = "poll-until"

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

Steps (1):
  └── poll-until.poll.iter1.probe: Probe the endpoint
```

<Warning>
Two caveats. First, the re-run never happens: the `until` label is written
but nothing consumes it — neither the v1 cook path nor the v2 control
dispatcher — so an `until` loop runs exactly one iteration (section 4).
v1 has no runtime re-execution mechanism at all; orchestrator-driven
re-execution requires the v2 `check` construct
([v2 specification, section 3.1](/reference/specs/formula-spec-v2#31-check)).
Second, `until` does not use the `{{var}} == value` step-condition syntax:
`until = "{{ready}} == yes"` fails with `unrecognized condition format`.
The grammar is the runtime condition evaluator's:
`probe.status == 'complete'`, `step.output.field == value`,
`children(x).all(status == 'complete')`, `steps.complete >= 3`.
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
  validated as a set (section 5). A v1 child extending a v2 parent
  inherits the v2 requirement and stops being a v1 formula.
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
- A v1 formula skips an unresolvable `description_file` best-effort: the
  reference is left unconsumed and any inline `description` is kept, with
  no diagnostic. (v2 formulas fail fast instead.)

### 1.9. Validation

A formula must satisfy these structural rules; violating any is a
validation error. The rules apply to the shared file format; constructs
marked v2-only in section 1.3 are additionally rejected in a v1 formula
with the section 1.3 compile error.

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
- `timeout` must be a positive Go duration and requires `check` — both
  v2-only; a bare `timeout` in a v1 formula fails with a message routing
  convergence gate scripts to `convergence.gate_timeout` / `--gate-timeout`.
- Loops (validated during control-flow expansion): `body` non-empty;
  exactly one of `count` / `until` / `range`; `max` required with `until`.

The v2-only constructs carry their own shape rules (`check` / `retry` /
`drain` / `on_complete` field constraints); `tally` was removed and now
fails fast if authored. The remaining construct constraints are specified
with the constructs in the
[v2 specification](/reference/specs/formula-spec-v2#19-validation).

## 2. Compilation

Compilation is a fixed pipeline shared with v2: load → resolve `extends` →
control-flow expansion (loops, branches, gates) → advice → inline
expansion → compose expand/map → aspects → condition filtering →
standalone expansion → requirement merge and the explicit-declaration
check → host requirement validation → recipe. The graph-specific stages
(reserved-symbol validation, retry and check transforms, graph control
injection) do not apply to a v1 formula: the explicit-declaration check
rejects their inputs first.

The v1 compiler must emit a molecule tree:

- **Namespaced step IDs.** Every step gets the recipe ID
  `<formula>.<step>`; nested children extend the path
  (`<formula>.<parent>.<child>`). The namespaced ID is stamped on each
  cooked step bead as `gc.step_ref` metadata.
- **Parent-child containment.** Every step carries a parent-child edge to
  its container — the molecule root for top-level steps, the enclosing
  step for `children`. A step with `children` is promoted to issue type
  `epic`.
- **Blocking edges.** `needs` / `depends_on` compile to `blocks` edges;
  `waits_for` compiles to a readiness-blocking `waits-for` edge plus a
  `gate:<value>` label on the cooked step (section 4).
- **Gate synthesis.** A `[steps.gate]` table synthesizes a gate bead
  (recipe ID `<formula>.gate-<step>`, type `gate`, title
  `Gate: <type> <id>`) that is a child of the step's container, plus a
  `blocks` edge from the gated step to it: the step stays blocked until
  the gate bead is closed.
- **Root.** The recipe root is type `molecule`, priority 2. Its title is
  the formula name, or the `{{title}}` placeholder when a `title` var is
  declared; its description is the formula description, or `{{desc}}` when
  a `desc` var is declared.
- **Root-only wisps.** When the formula sets `phase = "vapor"` without
  `pour`, or has no steps, the recipe is root-only: the root is type
  `task` stamped `gc.kind = "wisp"`, and steps are not materialized as
  beads — the root bead itself is the work, carrying only the formula's
  title and description. Authors of root-only formulas should put the
  work instructions in the formula `description`. Setting `pour = true`
  forces full materialization (checkpoint recovery) regardless of phase.
- **Step-type coercion at instantiation.** Non-root step beads typed
  `task` (the default) are stamped type `step` so `Ready()` and `bd ready`
  skip formula scaffolding — the molecule root is the actionable unit.
  Other explicit types (`bug`, `epic`, ...) are preserved.
- **Root stamping.** Non-batch instantiation stamps `gc.formula_hash`
  (SHA-256 of the raw formula file bytes) and `gc.formula_source` on the
  root; `gc formula version-check <bead-id>` compares the stored hash
  against the on-disk recipe to detect formula drift since spawn.

**Preview.** `gc formula show <name>` renders the compiled recipe. For the
minimal formula of section 0 (five authored steps render as five — v1
appends no finalize step):

```text
Formula: pancakes
Description: Make pancakes from scratch

Steps (5):
  ├── pancakes.dry: Mix dry ingredients
  ├── pancakes.wet: Mix wet ingredients
  ├── pancakes.combine: Combine wet and dry [needs: pancakes.dry, pancakes.wet]
  ├── pancakes.cook: Cook the pancakes [needs: pancakes.combine]
  └── pancakes.serve: Serve [needs: pancakes.cook]
```

A step with `children` renders with an `(epic)` marker:

```text
  ├── feature.build: Build the feature (epic)
  ├── feature.build.api: Implement the API
```

`gc formula cook pancakes` materializes the molecule; the created count is
steps + root, and each bead gets an independent store ID:

```text
Root: mc-8qi
Created: 6
pancakes -> mc-8qi
pancakes.combine -> mc-2x7
pancakes.cook -> mc-mjm
pancakes.dry -> mc-pzz
pancakes.serve -> mc-gzg
pancakes.wet -> mc-k1b
```

`gc formula cook <name> --attach <bead-id>` grafts the compiled recipe
under an existing bead as a sub-DAG; the attach target gains a blocking
dependency on the sub-DAG root, so it cannot close until the sub-DAG
completes.

`gc sling <target> <formula> --formula` instantiates the formula as an
ephemeral wisp and routes the root bead to the target:

```text
Slung formula "pancakes" (wisp root mc-98o) → worker
```

A root-only vapor formula previews and cooks to a single bead:

```text
Formula: patrol
Description: Patrol loop worked from the root bead
Phase: vapor
Root only: true

Steps (1):
  └── patrol.scan: Scan for stale work
```

```text
Root: mc-lbd
Created: 1
patrol -> mc-lbd
```

## 3. Runtime

v1 has no runtime engine. The orchestrator's control dispatcher executes
control beads only, and v1 compilation emits none — after instantiation no
orchestrator component advances the molecule.

- **Agents advance work.** The molecule is worked by the agent it is
  slung or hooked to; the bead store's blocking edges sequence the steps.
  Neither the `molecule` container root nor its default-typed `step` beads
  surface through `Ready()` (section 2) — the molecule routes as a unit by
  being assigned to an agent directly, which is also why scale-from-zero
  pools cannot wake for stepped molecules (section 5).
- **Targeted invocations.** `gc sling <target> <bead-id> --on <formula>`
  injects the `issue` variable and routing variables specified in
  section 1.4. Formula slings do not create an auto-convoy; convoys, where
  they exist, track members through non-blocking `tracks` edges that never
  gate readiness.
- **Completion.** The molecule root is auto-closed by the bd close hook
  when every transitive descendant is terminal (close reason
  `molecule autoclose: all step children closed`). Container dependents
  unblock at that point (section 1.3).
- **Garbage collection.** A core-pack cleanup exec order (cooldown
  trigger, 30m interval) closes stale wisps whose parents or roots are
  closed, purges old closed molecule data, and closes TTL-expired beads.

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
- **Gate `type` vocabulary.** `[steps.gate]` synthesizes a real gate bead
  that blocks its step until the gate bead is closed (manually or by an
  external watcher), but the `type` values `gh:run`, `gh:pr`, `timer`,
  `human`, and `mail` are doc-comment vocabulary in
  `internal/formula/types.go` — the parser never validates them and no
  bundled watcher acts on them. Zero bundled formulas use `gate`.
- **`waits_for` gate modes.** `waits_for` compiles to a readiness-blocking
  `waits-for` dependency edge plus a recorded gate mode, but no current
  component interprets the `all-children` / `any-children` distinction.
  Zero bundled formulas use `waits_for`.
- **`vars.<name>.type`.** The variable `type` field (`string`, `int`,
  `bool`) is parsed into the variable definition
  (`internal/formula/types.go`) but never enforced; only `required`,
  `enum`, and `pattern` are validated at instantiation.

## 5. Conformance And Compatibility

### Contract selection

v1 is the default contract. The host's formula compiler capability is
`2.0.0` when `[daemon] formula_v2` is enabled (the default) and `1.0.0`
when it is disabled; a formula compiles under v1 whenever no declared
constraint rejects capability 1.0.0.

| Declaration state | Contract |
|---|---|
| No `contract` key, no `formula_compiler` requirement | v1 (default) |
| `[requires] formula_compiler` constraint that capability 1.0.0 satisfies (for example `">=1.0.0"`) | v1 |
| `[requires] formula_compiler = ">=2.0.0"`, or the deprecated `contract = "graph.v2"` | v2 — out of this contract; see the [v2 specification](/reference/specs/formula-spec-v2#5-conformance-and-compatibility) |

`formula_compiler` is the only `[requires]` axis. The value must be a
semver comparator; violations fail with
`formula.compiler_requirement_invalid: formula_compiler must be a semver
comparator, for example ">=2.0.0"`, and unknown axes fail with
`formula.requirement_unknown: unknown formula requirement "<key>";
supported requirements: formula_compiler`.

`[requires]` composes through `extends` and through composed expansion and
aspect formulas as a safety constraint: a child inherits every parent
requirement and may only add tighter constraints, so a v1 formula that
composes in v2-requiring material stops being v1. Non-overlapping
constraint sets must fail before any durable work is written
(`formula.compiler_requirement_conflict`).

**`[daemon] formula_v2` interplay.** Disabling the host switch lowers the
compiler capability to 1.0.0 and makes v2 formulas fail to compile; it
must not change the behavior of v1 formulas, which compile identically
under either setting.

### v2-only constructs are rejected

A v1 formula that uses `check`, `retry`, `drain`, `on_complete`, or
reserved `gc.*` step metadata (the control
and structural `gc.kind` values, `gc.scope_name`, `gc.scope_role`,
`gc.scope_ref`, `gc.continuation_group`, `gc.on_fail`) must fail to
compile with:

```text
requires: formulas that use graph-only constructs must declare [requires] formula_compiler = ">=2.0.0" or the deprecated contract = "graph.v2" explicitly
```

This check runs after expansions and aspects materialize, so composed-in
constructs trigger it too. A drain step additionally fails parse-time
validation in a v1 formula:
`<step>.drain: drain steps must declare the formulas v2 contract
([requires] formula_compiler = ">=2.0.0")`.

The `gc doctor` check `formula-requirements` reports, per city and rig
formula layer: parse failures, deprecated `contract = "graph.v2"` opt-ins,
v2-only constructs in formulas without an explicit v2 requirement, and
host capability mismatches.

### Convergence currently requires v1

For iterate-until-verified semantics, prefer a v2 check loop
([v2 spec section 3.1](/reference/specs/formula-spec-v2#31-check));
`gc converge` is the pre-v2 command for this pattern and accepts only v1
formulas.

`gc converge` loops instantiate ordinary v1 formulas — there are no
convergence-specific formula keys. Top-level `convergence`,
`required_vars`, or `evaluate_prompt` keys in formula TOML are not decoded
(unknown keys are silently ignored). The evaluate prompt is supplied at
creation time with `gc converge create --evaluate-prompt`, stored as bead
metadata `convergence.evaluate_prompt`, and injected into the cook as the
`evaluate_prompt` variable. Convergence wisps must reject v2 formulas:

```text
convergence wisps do not support v2 formula "<name>"; use a v1 formula until convergence has an explicit input convoy target
```

### Ready-visibility and pools

A stepped v1 molecule's container root is not Ready-visible work, so it
cannot wake a scale-from-zero pool. `gc sling --formula` must reject a
stepped v1 formula routed to a multi-session target — any agent with a
namepool, or whose `max_active_sessions` is unset or not 1:

```text
formula "<name>" root is a molecule container, not Ready-visible work; scale-from-zero pools will not wake for this wisp. Convert the formula to phase="vapor"/root-only or formulas v2 before routing it to a pool
```

Order dispatch applies the same predicate but only warns for pool orders:

```text
warning: pool order "<order>" uses formula "<name>" whose root is a molecule container, not Ready-visible work; scale-from-zero pools will not wake for this wisp. Convert the formula to phase="vapor"/root-only or formulas v2 before routing it to a pool.
```

Root-only v1 wisps (`phase = "vapor"` without `pour`) pass the predicate:
the root bead is itself Ready-visible work and may be routed to pools.

### Deprecated surfaces

| Surface | Status | Replacement |
|---|---|---|
| `contract = "graph.v2"` | Deprecated opt-in that moves a formula to the v2 contract; `gc doctor` warns: `deprecated contract = "graph.v2"; use [requires] formula_compiler = ">=2.0.0"` | `[requires] formula_compiler = ">=2.0.0"` (for formulas that should be v2) |
| `<name>.formula.toml` / `<name>.formula.json` | Deprecated spellings (section 1.1) | `formulas/<name>.toml` |
| JSON `labels` step key | Deprecated JSON spelling (section 1.3) | TOML `tags` |
