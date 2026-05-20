# Formula Compiler Requirements v0

| Field | Value |
|---|---|
| Status | Proposed |
| Date | 2026-05-09 |
| Author(s) | Codex |
| Issue | [gastownhall/gascity#1760](https://github.com/gastownhall/gascity/issues/1760) |
| Supersedes | `contract = "graph.v2"` as the user-facing formula compiler requirement |

Design for replacing the formula `contract` field with an explicit
requirements table that declares the minimum formula compiler capability a
formula needs.

## Problem

Formula v2 currently uses:

```toml
formula = "code-review-loop"
version = 2
contract = "graph.v2"
```

This mixes three separate ideas:

- formula artifact revision
- formula file/compiler compatibility
- the current graph compiler implementation name

After review, formula artifact revision should not live on individual formula
files. The existing integer `version` field should not be repurposed into
formula semver. Formulas are distributed through packs, and pack version, pack
ref, or pack commit SHA is the durable way to identify the exact authored
workflow a consumer is using.

That leaves the current `contract` field. Its real purpose is not to describe a
runtime contract and not to select a compiler at execution time. It is a
minimum requirement: this formula uses constructs that require a compiler with
formula-v2 capability.

## Goals

1. Give formulas a standard way to declare minimum compiler requirements.
2. Make clear that formulas do not select the compiler implementation.
3. Remove formula-level artifact versioning from the design.
4. Preserve pack-level version/ref/SHA as the consistency mechanism for formula
   consumers.
5. Keep the current `[daemon] formula_v2` feature flag as the operator opt-in
   for formula-v2 compilation.
6. Provide a migration path from `contract = "graph.v2"` without breaking
   existing formulas immediately.

## Non-Goals

- Versioning individual formulas independently from their containing pack.
- Adding runtime compiler selection.
- Adding multiple formula compiler implementations.
- Changing formula layer resolution or filename-based shadowing.
- Changing pack semver, pack imports, or remote pinning behavior.

## Decision

<!-- REVIEW: added per attempt-94-global-synthesis -->

This document is the canonical design artifact for the formula compiler
requirements migration. Review, release, and implementation automation must
fail closed if the configured design path
`engdocs/design/formula-compiler-requirements.md` is absent, points at a
different document such as `engdocs/proposals/formula-migration.md`, or cannot
be snapshotted with a stable content hash. A design-review attempt output must
record the configured source path, the snapshotted source path, the source
content hash, and the actual persona-synthesis directory; it may not silently
fall back to a context file.

<!-- REVIEW: added per attempt-100-missing-synthesis -->

If the current design-review attempt does not produce `synthesis.md`, the apply
step may not infer a new verdict from persona inventory, `gc.output_json`, or
source-bead status alone. It must carry forward the last valid synthesis path
and verdict, record the missing current synthesis in the apply summary, and keep
`design_review.workflow_status=iterating` until a real global synthesis is
written.

Attempt 94 review closure is binding on the implementation plan:

| Review finding | Binding contract |
|---|---|
| Canonical artifact | This file is restored as the authoritative requirements design; `engdocs/proposals/formula-migration.md` is context only and must be updated or marked superseded before user-visible diagnostics ship. |
| Fail-closed compatibility | Unknown axes, unsupported future minima, malformed values, `contract` conflicts, disabled host capability, legacy-only roots without accepted artifacts, `GC_NATIVE_FORMULA=false`, and any `bd` probe all fail before durable writes unless the row is explicitly a validation-only probe. |
| Caller inventory | A checked-in per-occurrence manifest covers Go callers, prompt templates, first-party formulas, examples, docs snippets, API/dashboard/generated TypeScript, convergence, fanout, orders, source-workflow scans, and legacy `bd` materialization commands. Every durable writer moves through `CompileResult` to `AcceptedCompileArtifact`; every root reader uses the shared workflow-root predicate. |
| Rollout sequencing | First-party graph formulas stay dual-declared until `[pack] requires_gc` floors are enforced, old-reader fixtures pass, public notice and release evidence exist, and Phase 8 requires-only conversion is explicitly open. Phase 8 source conversion and Phase 9 parser alias removal are separate gates with separate rollback units. |
| Parser and validation matrix | Raw presence-preserving capture, byte-exact grammar fixtures, TOML/JSON shape rows, duplicate handling, future capability rows, v2-only construct detection before filtering, caller-path zero-write fixtures, generated count locks, and diagnostic ordering are required before any producer writes from requirements data. |
| Diagnostics | `HostCapabilities` is an explicit input at every compile entry point. CLI, Huma/OpenAPI, dashboard TypeScript, Event Bus payloads, and release reports project the same typed diagnostic schema; order/controller/convergence grouping is stored in producer/read-model state or append-only summary events, never by mutating prior Event Bus facts. |
| Release evidence | Alias drain, active-root repair, minimum binary floor, compatibility corpus, external-support status, legacy-version reports, first-party inventory, docs checks, public notice, and repair-root dry runs are JSON artifacts with `schema_version`, `generated_at`, owner, command, exit contract, and consumer fields. Markdown summaries are not release evidence. |
| Pack provenance | Pack revision, ref, SHA, content hash, dirty state, transitive source evidence, and `[pack] requires_gc` come from packman resolver/lockfile provenance. If that evidence is unavailable, pack-floor enforcement, requires-only conversion, and alias removal fail closed. |
| Docs and terminology | Formula `[requires]`, pack `requires_gc`, host capability, compiler capability, compiler implementation, schema/artifact version, formula `version`, and pack revision have one glossary and one docs/examples inventory. Generated help, OpenAPI, dashboard types, PackV2 docs, and stale-guidance CI must update in the same branch that exposes the corresponding surface. |

<!-- REVIEW: added per attempt-102-global-synthesis -->

Attempt 102 review closure adds these binding corrections:

| Finding | Binding correction |
|---|---|
| Alias rollout | Phase 8 is only first-party requires-only conversion. Phase 9 is parser alias removal. Both consume the same alias-window artifact digest, alias-drain report, compatibility corpus, min-floor report, external-support report, docs-check report, and active-root report. Missing, stale, conflicting, or rollback-ineligible evidence exits non-zero and blocks the gate. Runtime accepted-alias counts are durable artifact/report data, never process-local counters. |
| Caller inventory | Ignored runtime `.gc/` trees are not mandatory CI scan inputs. CI scans tracked sources and checked-in generated fixtures only; local `.gc/system/packs` scans are optional operator evidence unless a deterministic temp-city materialization step writes a checked-in manifest artifact. |
| Requirement surfaces | Formula `[requires]`, pack `[[pack.requires]]`, and pack `[pack].requires_gc` are separate surfaces with separate owners, docs rows, stale-guidance matchers, and diagnostics. "Formula v2", "graph workflow", "compiler capability 2", and host `[daemon] formula_v2` cannot be used interchangeably. |
| Compile authority | Durable writers must use the canonical `internal/formula` preflight and accepted-artifact API. Bare `Compile(...)`, `*Recipe`, `Recipe.GraphWorkflow`, root metadata strings, raw requirement fields, direct host-capability globals, and caller-built compile metadata are not write authority. |
| Diagnostics | Wire diagnostics are presence-aware typed structs, not `omitempty`-pruned best-effort JSON. Grouped diagnostic state requires store singleton/CAS capability or an explicit unsupported-store diagnostic. Order failure compatibility, rollup/status routes, dashboard-less CLI status, burst budgets, and launch exit semantics are part of the executable contract. |
| Pack ecosystem | Every legacy `contract` spelling accepted by the current compiler remains a deprecated alias during the compatibility window unless external-support evidence proves no supported consumer depends on it. Resolver/import provenance, direct-pack validation identity, and old-reader pack-load fixtures are Phase 2/7 deliverables. |
| Parser matrix | Named suites cover nested misplaced requirement tables, dotted-key syntax, dotted/bracket collisions, inline-table conflicts, combined-defect precedence, condition-disabled constructs, JSON loader policy, unknown axes paired with otherwise satisfied known requirements, integer boundaries, and `CompilerCapability(0)`. Explicit `>=1` is provenance only; `ContentHash` hashes canonical resolved source bytes as defined below. |
| Convergence | The migration is anchored to the live `cmd/gc/convergence_store.go:pourWisp -> molecule.Cook -> internal/formula` path. Retired subset parsers are deleted or quarantined, every evaluate-prompt check maps to `ValidateProjection` or is explicitly retired, and convergence v2 policy is decided by accepted artifact validation before durable writes. |

Add a top-level `[requires]` table to formula files:

```toml
formula = "code-review-loop"

[requires]
formula_compiler = ">=2"
```

`requires.formula_compiler` declares the minimum formula compiler capability
needed to parse and compile the formula. It is a requirement expression, not a
selector.

The active Gas City binary decides which compiler path to use. For v0, the
mapping is simple:

| Requirement | Meaning |
|---|---|
| omitted | Formula only requires the default compiler capability |
| `>=2` | Formula requires formula-v2 graph compiler capability |

If `[daemon] formula_v2 = false`, a formula requiring compiler capability `>=2`
must fail with an actionable error that tells the operator to enable
`[daemon] formula_v2` or use a formula that does not require compiler v2.

## Why `requires`

`requires` is the standard package/config term for constraints that must be
satisfied by the host toolchain. It communicates the important boundary:

- the formula declares what it needs
- the host decides whether it can satisfy that need
- the formula does not choose the compiler at execution time

This is more precise than `contract`, which sounds like an interface agreement,
and more precise than `schema`, which suggests the physical file format.

## Formula Artifact Versioning

Formula files should not carry their own semver field.

Consumers who need consistency should pin the containing pack:

- by semver when using a released pack version
- by tag or branch when appropriate
- by commit SHA when exact reproducibility matters

This keeps the existing pack model as the single distribution and revision
boundary. A formula's identity remains its resolved formula name and winning
file in the formula layer stack. Its authored revision is the pack revision
that provided that winning file.

## Proposed TOML Surface

### Default compiler capability

```toml
formula = "simple-review"

[[steps]]
id = "review"
title = "Review the change"
description = "..."
```

No `[requires]` table means the formula can compile with the default formula
compiler capability.

### Formula-v2 compiler capability

```toml
formula = "code-review-loop"

[requires]
formula_compiler = ">=2"

[[steps]]
id = "review"
title = "Review the change"
description = "..."

[steps.retry]
max_attempts = 3
on_exhausted = "soft_fail"
```

The exact v2-only fields are validated by the formula package. If a formula uses
v2-only constructs without declaring `requires.formula_compiler = ">=2"`,
validation should fail with a direct message that names the missing requirement.

## Requirement Expression

V0 supports only the expression forms needed for current formula compiler
capabilities. The parser must reject every other value instead of accepting a
future range optimistically.

| Expression | Meaning |
|---|---|
| omitted | Default compiler capability, equivalent to `>=1` |
| `>=1` | Default compiler capability |
| `>=2` | Formula-v2 graph compiler capability |

Resolved public syntax choices:

- Exact `formula_compiler = "2"` is invalid syntax in v0.
- `">= 2"`, `" >=2"`, `">=2 "`, `">=2.0"`, and `">=2.1"` are invalid syntax
  in v0.
- `">=3"` is syntactically recognizable as a future monotonic minimum, but it
  is unsupported until the binary implements compiler capability 3.
- `requires.formula_compiler` must be a TOML string. Integer, float, boolean,
  array, table, and dotted-table values fail validation.
- An omitted `[requires]` table and an empty `[requires]` table both mean the
  default compiler capability.
- Unknown keys under `[requires]` fail validation. `formula_compiler` is the
  only supported requirement axis in v0.

Future compiler capability expressions can be added only by extending this
grammar and its validation matrix in the same change.

The parser must preserve raw TOML key names, types, and source positions until
diagnostics are built. Decoding directly into the final typed `Formula` shape is
not sufficient for `[requires]` because it can erase whether an author supplied
an integer, a dotted table, an unknown key, or an empty table.

The parser therefore has two stages. Stage one captures raw requirement,
legacy `contract`, and legacy `version` fields into the `RawRequirementField`
contract defined below before typed formula decoding. Stage two converts only
the accepted raw fields into `NormalizedRequirements`. Diagnostics always point
at the raw field, never at a lossy decoded zero value.

## Parser And Validation Contract

<!-- REVIEW: added per parser-validation-semantics -->

Requirement validation is owned by `internal/formula` and runs before any
caller creates beads, wisps, or workflow roots. The deterministic diagnostic
order is:

1. TOML decoding errors.
2. Unknown `[requires]` keys and invalid TOML types.
3. Malformed `requires.formula_compiler` syntax.
4. Syntactically valid but unsupported future `requires.formula_compiler`
   expressions.
5. Legacy `contract` normalization and `contract`/`requires` conflicts.
6. Missing compiler requirement for v2-only constructs.
7. Host capability failures such as `[daemon] formula_v2 = false`.

Accepted TOML shape matrix:

| TOML shape | Normalized result | Diagnostic |
|---|---|---|
| no `[requires]` table | default capability, source `omitted` | none |
| empty `[requires]` table | default capability, source `omitted` | none |
| `[requires] formula_compiler = ">=1"` | default capability, source `requires` | none |
| `[requires] formula_compiler = ">=2"` | compiler capability 2, source `requires` | none when host satisfies it |
| `[requires] formula_compiler = ""` | none | `formula.compiler_requirement_invalid_syntax` |
| `[requires] formula_compiler = "2"` | none | `formula.compiler_requirement_invalid_syntax` |
| `[requires] formula_compiler = ">= 2"` | none | `formula.compiler_requirement_invalid_syntax` |
| `[requires] formula_compiler = ">=2 "` or `" >=2"` | none | `formula.compiler_requirement_invalid_syntax` |
| `[requires] formula_compiler = ">=2.0"` or `">=2.1"` | none | `formula.compiler_requirement_invalid_syntax` |
| `[requires] formula_compiler = ">=3"` | none | `formula.compiler_requirement_unsupported_future` |
| `[requires] formula_compiler = 2`, `2.0`, `true`, `[]`, or `{}` | none | `formula.requirement_invalid_type` |
| `[requires.formula_compiler]` dotted or nested table | none | `formula.requirement_invalid_type` |
| `[requires] unknown_axis = "x"` | none | `formula.requirement_unknown_axis` |

<!-- REVIEW: added per DR53-raw-requirement-source-contract -->

Raw requirement capture is an API contract, not an implementation detail. The
compiler first builds a `RawRequirementField` table from the original bytes
before typed decoding:

```go
type RawRequirementField struct {
    Format     string // toml or json
    Path       string
    Key        string
    RawValue   []byte
    Shape      string
    Line       int
    Column     int
    Duplicate  bool
}
```

For TOML, `github.com/BurntSushi/toml` v1.6.0 remains the typed decoder and
syntax-error authority. Its `MetaData` is used for key presence and decoded
type checks only; it is not treated as a stable source-position API for valid
fields. `internal/formula` owns a small byte scanner for the `[requires]`,
legacy `contract`, and legacy `version` fields that records raw source bytes,
line, column, table shape, dotted/nested/inline shape, and duplicate candidates
before typed decoding. Duplicate keys or tables rejected by the TOML parser
remain parser-boundary errors with the parser's position. Valid files cannot
reach `NormalizeRequirements` unless every requirement, legacy alias, and
legacy version field has a `RawRequirementField`; a missing raw row is an
internal invariant diagnostic and a test failure.

JSON formulas are in scope for this migration while any JSON formula loader is
enabled. JSON decoding therefore uses a token-level raw pass before typed
unmarshal. It records JSON pointer keys, raw member bytes, value shape, and
duplicate object members. Duplicate JSON members are not silently last-writer
wins; they are fixture-locked parser-boundary failures or structured duplicate
diagnostics, depending on the JSON reader's ability to report source position.
If the implementation retires JSON formulas before this feature lands, the
same change must delete the JSON matrix rows, remove the loader, and update the
docs; until then JSON cannot bypass raw requirement capture.

V2-only construct registry:

| Construct | TOML locations scanned | Trigger predicate |
|---|---|
| Step `check` | `[[steps]]`, `children`, `loop.body`, expansion `template` | field is present, even if disabled by conditions |
| Legacy internal `ralph` | `[[steps]]`, `children`, `loop.body`, expansion `template` | field is present |
| Step `retry` | `[[steps]]`, `children`, `loop.body`, expansion `template` | retry table or retry metadata is present |
| Step `on_complete` | `[[steps]]`, `children`, `loop.body`, expansion `template` | on-complete formula/action is present |
| Workflow-control metadata key | step metadata, child metadata, loop body metadata, expansion template metadata | key is present in the generated workflow-control metadata registry |
| Workflow-control metadata value | same metadata locations | `gc.kind` or another registry-declared control value is present byte-exactly as registered |
| Authored step `expand` | root steps, children, loop bodies, inline expansion templates | decoded `Step.Expand` field is present and materializes a subgraph contribution |
| `expand_vars` | same locations as authored step `expand`, plus compose expansion/map vars | decoded expansion-var field is present and affects graph materialization |
| Expansion/aspect contribution | `compose.expand`, `compose.map`, `compose.aspects`, transitive imports | contributed formula or template contains any construct in this registry |

<!-- REVIEW: added per DR41-validation-registry -->
<!-- REVIEW: added per DR48-workflow-control-registry -->

Workflow-control metadata is a generated registry, not a prose-only list. The
source of truth is `internal/formula/testdata/workflow_control_metadata.yaml`,
and the v2-only construct registry imports it when building matrix rows:

```yaml
schema_version: 1
keys:
  - key: gc.kind
    owner: formula
    generated_by: [workflow_root, scope_step, source_spec, retry_step, ralph_step, fanout_step, check_step, workflow_finalize_step]
    min_compiler_capability: 2
  - key: gc.scope_name
    owner: controller
    generated_by: [scope_step, scope_check_step]
    min_compiler_capability: 2
  - key: gc.scope_role
    owner: controller
    generated_by: [scope_step, scope_check_step, workflow_finalize_step]
    min_compiler_capability: 2
  - key: gc.scope_ref
    owner: controller
    generated_by: [scope_step, retry_step, ralph_step, workflow_finalize_step]
    min_compiler_capability: 2
  - key: gc.continuation_group
    owner: controller
    generated_by: [continuation_step, retry_step, fanout_step]
    min_compiler_capability: 2
  - key: gc.on_fail
    owner: controller
    generated_by: [scope_step, retry_step, ralph_step]
    min_compiler_capability: 2
  - key: gc.control_for
    owner: controller
    generated_by: [scope_check_step, fanout_step, check_step]
    min_compiler_capability: 2
  - key: gc.for_each
    owner: fanout
    generated_by: [fanout_step]
    min_compiler_capability: 2
  - key: gc.bond
    owner: fanout
    generated_by: [fanout_step]
    min_compiler_capability: 2
  - key: gc.bond_vars
    owner: fanout
    generated_by: [fanout_step]
    min_compiler_capability: 2
  - key: gc.fanout_mode
    owner: fanout
    generated_by: [fanout_step]
    min_compiler_capability: 2
  - key: gc.output_json_required
    owner: controller
    generated_by: [fanout_source_step, ralph_run_step, ralph_scope_step]
    min_compiler_capability: 2
  - key: gc.spec_for
    owner: formula
    generated_by: [source_spec]
    min_compiler_capability: 2
  - key: gc.spec_for_ref
    owner: formula
    generated_by: [source_spec]
    min_compiler_capability: 2
  - key: gc.step_id
    owner: controller
    generated_by: [scope_step, retry_step, ralph_step, scope_check_step]
    min_compiler_capability: 2
  - key: gc.step_ref
    owner: controller
    generated_by: [workflow_control_step]
    min_compiler_capability: 2
  - key: gc.ralph_step_id
    owner: ralph
    generated_by: [ralph_step, ralph_run_step, ralph_check_step]
    min_compiler_capability: 2
  - key: gc.attempt
    owner: retry
    generated_by: [retry_step, ralph_step]
    min_compiler_capability: 2
values:
  gc.kind:
    - value: workflow
      owner: formula
      generated_by: [workflow_root]
      min_compiler_capability: 2
    - value: scope
      owner: controller
      generated_by: [scope_step, ralph_scope_step]
      min_compiler_capability: 2
    - value: cleanup
      owner: controller
      generated_by: [cleanup_step]
      min_compiler_capability: 2
    - value: scope-check
      owner: controller
      generated_by: [scope_check_step]
      min_compiler_capability: 2
    - value: workflow-finalize
      owner: controller
      generated_by: [workflow_finalize_step]
      min_compiler_capability: 2
    - value: retry
      owner: retry
      generated_by: [retry_step]
      min_compiler_capability: 2
    - value: retry-run
      owner: retry
      generated_by: [retry_step]
      min_compiler_capability: 2
    - value: retry-eval
      owner: retry
      generated_by: [retry_step]
      min_compiler_capability: 2
    - value: ralph
      owner: ralph
      generated_by: [ralph_step]
      min_compiler_capability: 2
    - value: run
      owner: ralph
      generated_by: [ralph_run_step]
      min_compiler_capability: 2
    - value: check
      owner: controller
      generated_by: [check_step, ralph_check_step]
      min_compiler_capability: 2
    - value: fanout
      owner: fanout
      generated_by: [fanout_step]
      min_compiler_capability: 2
    - value: spec
      owner: formula
      generated_by: [source_spec]
      min_compiler_capability: 2
  gc.fanout_mode:
    - value: parallel
      owner: fanout
      generated_by: [fanout_step]
      min_compiler_capability: 2
    - value: sequential
      owner: fanout
      generated_by: [fanout_step]
      min_compiler_capability: 2
```

<!-- REVIEW: added per attempt-79-decoded-construct-predicates -->

Construct predicates are expressed as decoded field paths, not prose. The
construct registry fixture stores rows like:

```yaml
- id: retry
  decoded_paths:
    - Formula.Steps[].Retry
    - Formula.Steps[].Children[].Retry
    - Formula.Steps[].Loop.Body[].Retry
    - Compose.Expand[].Template.Steps[].Retry
  trigger: present
  min_compiler_capability: 2
```

For metadata constructs, the decoded path points to the metadata field and the
trigger references a row in `workflow_control_metadata.yaml`. For contribution
constructs, the row names the traversal rule (`root`, `child`, `loop_body`,
`inline_expansion`, `compose_expand`, `compose_map`, `aspect`,
`transitive_import`) that brought the decoded field into the final recipe.
The generator fails when a field path is present in prose but missing from the
fixture, or when a fixture path no longer exists in the decoded formula types.

The registry must enumerate every workflow-control `gc.*` key and every
generated `gc.kind` value used by scope, cleanup, retry, Ralph continuation,
fanout, run/check, and workflow-finalize steps. CI fails when a generated
control metadata key or value is added without a registry row, owner, generator
name, matrix suite, and at least one positive and one missing-requirement
fixture. Ad hoc `strings.HasPrefix("gc.")` checks are not allowed to define
formula-v2 semantics.

The registry is seeded from the current graph-control generators in
`internal/formula/graph.go`, `internal/formula/fragment.go`,
`internal/formula/retry.go`, `internal/formula/ralph.go`,
`internal/formula/source_spec.go`, `internal/graphroute/graphroute.go`, and
the first-party graph formula fixtures under `internal/bootstrap/packs/`,
checked-in generated system-pack manifests, `examples/`, and
`internal/testfixtures/`. Generated
values are byte-exact: no case folding, prefix matching, substring matching,
or optimistic acceptance of unregistered `gc.*` keys is allowed. Historical
whitespace trimming applies only inside the shared legacy workflow-root
predicate, not inside the formula-v2 construct registry.

CI owns the registry count locks:

| Lock | Source | Required assertion |
|---|---|---|
| `workflow_control_key_count` | generated YAML key rows | every current graph-control metadata key has one owner and at least one positive plus one missing-requirement fixture |
| `workflow_control_value_count` | generated YAML value rows | every current `gc.kind` and other enum-like control value has byte-exact fixtures |
| `workflow_control_generator_count` | named generator inventory | every generator that writes registered metadata is represented in the registry |
| `workflow_control_callsite_count` | grep-derived writer manifest | every production writer of a registered key is either a formula generator or a durable writer that consumes an accepted artifact |

`needs`, `depends_on`, `children`, `loop`, `gate`, `condition`,
`compose.expand`, `compose.map`, and `compose.aspects` are not v2-only by
themselves in this design. They require compiler v2 only when the resolved
formula, expansion, aspect, or nested body also contains one of the v2-only
constructs above.

<!-- REVIEW: added per attempt-79-step-expand-semantics -->

The `step.expand` decision is deliberately split from `compose.expand`.
Authored `Step.Expand` and `expand_vars` are v2-only because they are decoded
fields that directly materialize subgraph topology from a step. `compose.expand`,
`compose.map`, and `compose.aspects` are traversal mechanisms: the traversal is
v1-compatible until the contributed template or formula contains a registered
v2-only construct. Matrix rows therefore use `construct_identity=step_expand`
only for decoded authored step expansion, and use
`contribution_path=compose_expand|compose_map|aspect|transitive_import` for
constructs discovered while traversing contributed formulas. The generator
fails if one row tries to use `construct_identity=step_expand` to mean both the
authored field and the compose traversal.

Normalization rules:

- Requirements are normalized after formula layer resolution and inheritance,
  before control-flow, expansion, retry, Ralph, and graph-control transforms.
- Parent requirements are inherited like the current `contract` field. A child
  may raise the requirement, but it may not lower a parent requirement.
- A child, expansion, aspect, or import that explicitly declares a lower
  requirement than the inherited or contributing context emits
  `formula.compiler_requirement_conflict` with both source locations. A default
  or empty `[requires]` in a contributed formula inherits when the formula has
  no independent v2-only constructs; it does not lower the compiled maximum.
- Inline children and `loop.body` inherit the containing formula's normalized
  requirement because they have no independent formula header.
- Expansion and aspect formulas are parsed and normalized independently. The
  compiled root requirement is the maximum requirement of the root formula and
  every expansion or aspect that contributed steps.
- If a formula, expansion, or aspect contains a v2-only construct but does not
  declare `requires.formula_compiler = ">=2"` or legacy `contract = "graph.v2"`,
  validation fails at that source formula with a diagnostic that names the
  missing requirement.

The validation matrix is generated from
`internal/formula/testdata/compiler_requirements_matrix.yaml`; the table below
is a rendered excerpt, not a hand-maintained checklist. The fixture file is the
normative source and every row names:

- requirement source (`omitted`, `requires`, `contract`, `dual`)
- raw TOML shape and raw value kind
- legacy `contract` state
- legacy `version` state (`omitted`, `1`, `2`, or invalid type)
- v2 construct registry entry and `min_compiler_capability`
- construct location, contribution path, and caller path
- host compiler capability
- normalized requirement and source attribution
- ordered diagnostics with source path, key, value, and severity

<!-- REVIEW: added per parser-validation-contract-v21 -->

The fixture is an executable contract, not a sample list. Its checked-in schema
is:

```yaml
schema_version: 1
dimensions:
  source_format: [toml, json]
  requirement_shape:
    [omitted, empty_table, string, integer, float, boolean, array, inline_table, dotted_table, nested_table, duplicate_table, duplicate_scalar_key, array_of_tables, top_level_requires_scalar, top_level_requires_array, unknown_axis]
  requirement_value:
    [omitted, ">=1", ">=2", "", "2", ">= 2", " >=2", ">=2 ", ">=2.0", ">=2.1", ">=3"]
  contract_value: [omitted, graph.v2, graph_v2_whitespace_variant, graph_v2_case_variant, graph.v1, other]
  version_value: [omitted, 1, 2, string, invalid_type]
  host_capability: [1, 2, invalid]
  construct_identity:
    [none, check, ralph, retry, on_complete, workflow_metadata_key, workflow_metadata_value, step_expand, expand_vars]
  construct_location:
    [root_step, child_step, loop_body_step, inline_expansion_in_step, inline_expansion_in_child, inline_expansion_in_loop_body, compose_expand_template, compose_map_template, aspect_advice]
  contribution_path:
    [root_file, child_body, loop_body, inline_expansion, compose_expand, compose_map, aspect, transitive_import]
  caller_path:
    [validate_only, show_preview, cook_root, cook_attach, sling_cli_launch, sling_cli_attach, sling_api_launch, order_dispatch, controller_validation, retry_continuation, on_complete_continuation, fanout_parent, fanout_fragment, convergence_create, convergence_retry, convergence_next_iteration, convoy_source_scan, dashboard_preview]
  workflow_control_metadata_registry: internal/formula/testdata/workflow_control_metadata.yaml
suites:
  - id: caller-preflight
    owner: internal/formula
    coverage_intent: every durable writer has one fatal diagnostic row that asserts zero writes
    row_kinds: [positive, negative, unsupported, impossible]
    dimension_owners:
      caller_path: internal/formula/testdata/caller_paths.yaml
      contribution_path: internal/formula/testdata/contribution_paths.yaml
    count_lock: 18
rows:
  - id: requires-v2-host-disabled
    input:
      source_format: toml
      source_path: formulas/root.toml
      requirement_shape: string
      requirement_value: ">=2"
      contract_value: omitted
      version_value: omitted
      host_capability: 1
      construct_identity: retry
      construct_location: root_step
      contribution_path: root_file
      caller_path: cook_root
    expect:
      normalized:
        formula_compiler: 2
        source: requires
        source_path: formulas/root.toml
        source_key: requires.formula_compiler
      diagnostics:
        - order: 1
          code: formula.compiler_requirement_unsatisfied
          severity: error
          source_path: city.toml
          source_key: daemon.formula_v2
          source_value: "false"
      diagnostic_count: 1
```

<!-- REVIEW: added per DR33-validation-matrix-bounded -->

The generator does not expand the full cross-product. The `dimensions` block is
the vocabulary, while executable suites select bounded combinations with named
coverage intent. CI locks the generated case count for each suite; adding a
requirement axis, construct, raw shape, or caller path must either add cases to
the owning suite with an updated count or declare a checked `unsupported` or
`impossible` row. This keeps the matrix auditable instead of turning it into an
unreviewable product of every dimension.

Normative suites:

| Suite | Covers | Count lock |
|---|---|---|
| `grammar` | omitted, empty, accepted byte-exact strings, rejected strings, unsupported future `>=3`, and invalid host capability | exact generated rows plus golden diagnostic order/count |
| `raw-shape` | TOML duplicate table, duplicate scalar key, dotted table, nested table, inline table, array, bool, integer, float, parser-boundary failures, and JSON equivalents | exact generated rows with TOML source position or JSON pointer expectations |
| `legacy-alias` | legacy `contract`, dual declarations, conflicts, unsupported contract values, legacy `version`, and warning placement | exact generated rows and warning/fatal precedence |
| `construct-registry` | every v2-only construct identity and decoded-field classification | at least one positive and one missing-requirement row per registry entry |
| `contribution-traversal` | root, child, loop body, inline expansion, compose expansion/map, aspect, and transitive import propagation | pairwise rows across construct location and contribution path |
| `caller-preflight` | validate/show previews plus every durable writer path | one fatal row per durable writer proving zero writes |
| `projection-parity` | CLI, API, API-routed CLI, dashboard, and Event Bus projections | golden rows for diagnostic field preservation, ordering, and count |
| `condition-state` | authored-present, materialized, and condition-disabled v2 constructs | exact missing-requirement rows for disabled steps, disabled retry branches, order gates, and skipped compose branches |
| `misplaced-requirements` | non-top-level `[requires]`, `[steps.requires]`, nested/dotted requirement tables, dotted/bracket collisions, and inline-table conflicts | parser-boundary or `formula.requirement_invalid_type` rows with source positions |
| `combined-defect` | independent defects in one valid source file | ordered diagnostics and exact `diagnostic_count` for unknown axes, malformed values, conflicts, missing requirements, warnings, and host checks |
| `json-loader-policy` | every JSON requirement shape while the JSON loader exists | JSON pointer attribution, duplicate-member behavior, caller-preflight parity, and zero-write rows for durable JSON callers |
| `future-boundary` | unknown axes with satisfied known requirements, unsupported future minima, integer boundaries, overflow strings, and `CompilerCapability(0)` | fail-closed diagnostics; no defaulting, wrapping, or behavior branch on zero capability |

Each suite records row-kind counts separately:

```yaml
count_locks:
  grammar:
    positive: "<generated accepted grammar rows>"
    negative: "<generated rejected grammar rows>"
    unsupported: "<generated future-capability rows>"
    impossible: 0
  raw-shape:
    positive: "<generated valid raw shape rows>"
    negative: "<generated wrong-shape and duplicate rows>"
    unsupported: "<generated JSON/TOML loader-unsupported rows>"
    impossible: "<generated impossible row count>"
```

Literal integers are required once the generator exists; expression strings are
allowed only in the design fixture that seeds the implementation. CI compares
the generated row counts by suite and `row_kind`, so adding a shape, grammar
edge, construct, caller, or source format cannot be hidden inside a total count.
Every rejected edge named in this document has at least one row with exact
expected source key, line, column, diagnostic code, diagnostic order, and
`diagnostic_count`.

<!-- REVIEW: added per attempt-78-matrix-count-derivation -->

The `caller-preflight` count lock of `18` is derived from concrete durable and
preview callers, not from a prose total:

| # | Caller path | Durable boundary |
|---|---|---|
| 1 | `validate_only` | none; diagnostic parity only |
| 2 | `show_preview` | none; must not open a bead store |
| 3 | `cook_root` | root, child, dependency, hook, workflow-root metadata |
| 4 | `cook_attach` | attached child, dependency, hook, attachment metadata |
| 5 | `sling_cli_launch` | root/wisp, child, convoy, hook, route metadata |
| 6 | `sling_cli_attach` | attached molecule, dependency, hook |
| 7 | `sling_api_launch` | root/wisp, child, hook, HTTP diagnostic body |
| 8 | `order_dispatch` | fired metadata, wisp root, child beads |
| 9 | `controller_validation` | producer diagnostic group only; no protected writes |
| 10 | `retry_continuation` | retry-run bead, retry metadata, dependency, hook |
| 11 | `on_complete_continuation` | attached molecule, continuation metadata, hook |
| 12 | `fanout_parent` | fanout state, child fragments, convoy links |
| 13 | `fanout_fragment` | fragment child, convoy, continuation metadata |
| 14 | `convergence_create` | convergence root, iteration, hook, artifact ref |
| 15 | `convergence_retry` | retry metadata, iteration, replacement child |
| 16 | `convergence_next_iteration` | iteration bead, dependency, hook |
| 17 | `convoy_source_scan` | no formula writes; shared predicate parity |
| 18 | `dashboard_preview` | no writes; generated-client diagnostic parity |

The generator reads this list from `internal/formula/testdata/caller_paths.yaml`
and fails if a durable writer exists in the caller manifest without one
preflight row. Splitting a caller into launch and attach, parent and fragment,
or create and retry is required because each touches different protected
boundaries.

<!-- REVIEW: added per attempt-90-parser-caller-manifest-binding -->

The table above is the seed vocabulary only. The checked implementation cannot
preserve `18` as a hand-maintained proof. The `caller-preflight` suite is
generated from the repository-wide caller manifest described below, and the
generated count is the number of manifest rows whose `durable_write_boundary`
or `preview_projection` field is non-empty. CI fails when a manifest row can
write or project formula behavior and has no generated preflight matrix row,
or when a preflight row does not point back to exactly one manifest row.

The manifest-to-matrix join key is `caller_manifest_id`. Every matrix row in
the `caller-preflight` suite stores:

```yaml
caller_manifest_id: cmd-gc-order-dispatch-filter-001
source_kind: go | dashboard_ts | pack_formula | example | tutorial | prompt_template | generated_prompt
occurrence_kind: compile_call | metadata_filter | durable_writer | shell_probe | prompt_producer
durable_boundaries: [root, child, dependency, hook, convoy, retry_metadata, fanout_state, convergence_state, workflow_root_metadata, fired_order_metadata, artifact_ref]
zero_write_fixture: internal/formula/testdata/zero_write/order-dispatch-disabled-host.yaml
```

`TestCallerManifestDrivesPreflightMatrix` compares the manifest, matrix rows,
zero-write fixtures, and static raw-consumer scan in one assertion. A caller
can move from `raw_consumer` to `accepted_artifact_only` only when its manifest
row, matrix row, replacement API, and zero-write fixture change in the same
PR. Prompt-template and tutorial rows are first-class producer candidates:
any first-party prompt or example that can instruct an agent to run
`gc bd mol wisp`, `gc bd mol burn`, `gc bd mol cook`, `gc bd mol cook-on`, or
an equivalent molecule/materialization command must either be deleted,
rewritten to a native compile/accept producer, or covered by a requires-only
blocking diagnostic before first-party requires-only formulas are distributed.

Construct detection runs before condition filtering. `if`, `when`, retry
conditions, disabled steps, skipped compose branches, and order gates do not
hide a v2-only authored construct from requirement validation. The validator
classifies three states separately:

| State | Meaning | Requirement effect |
|---|---|---|
| `authored_present` | raw field exists in a decoded formula, template, import, aspect, or generated prompt source | requires the registered minimum compiler capability |
| `materialized` | graph expansion selected the construct for this compile | requires the registered minimum compiler capability and contributes to projection |
| `condition_disabled` | construct is authored but would not execute for the current run | still requires the registered minimum compiler capability because an old compiler may fail to parse or project it |

Every triggering construct has source attribution independent of the final
graph transform:

```go
type ConstructTriggerAttribution struct {
    ConstructID      string
    DecodedPath      string
    ContributionPath string
    SourceFormat     string
    SourcePath       string
    SourceKey        string
    SourceValue      string
    Line             int
    Column           int
    ConditionState   string
    ManifestID       string
}
```

The compiler emits `formula.compiler_requirement_missing` at the
`ConstructTriggerAttribution` source, not at the root formula by default. If
several constructs trigger the same missing requirement, diagnostics are
ordered by source path, line, column, construct id, then decoded path, and the
matrix fixture records the full `diagnostic_count`. Fixture families must
cover multiple triggering constructs in one formula, imported/aspect
contributions, condition-disabled constructs, `expand_vars`, retry/check/
`on_complete`, workflow metadata, convergence projections, JSON formulas, and
conflicting legacy aliases in the same source.

Unregistered `gc.*` metadata is not a v2-only workflow-control construct by
prefix. It is either ordinary user metadata or a hard validation diagnostic
depending on the owning metadata registry row. The registry fixture must name
every compiler-owned workflow-control key and every generated `gc.kind` value.
`TestWorkflowControlMetadataRegistryComplete` reflects over generated
workflow-control writers and fails when a key or enum value is produced without
a registry row, source attribution, matrix fixture, owner, and release-phase
expiry for any legacy spelling.

Raw-shape and combined-defect rows are explicit:

| Input shape or defect set | Suite | First diagnostic / count rule |
|---|---|---|
| top-level `requires = ">=2"` | `raw-shape` | `formula.requirement_invalid_type`, `diagnostic_count=1` |
| top-level `requires = []` or `[{}]` | `raw-shape` | `formula.requirement_invalid_type`, `diagnostic_count=1` |
| `[[requires]]` array of tables | `raw-shape` | TOML parser boundary or fixture-locked duplicate/type diagnostic |
| duplicate `formula_compiler` scalar keys | `raw-shape` | TOML parser boundary; no last-writer-wins normalization |
| `[requires.formula_compiler]` nested or dotted table | `raw-shape` | `formula.requirement_invalid_type`, `diagnostic_count=1` |
| unknown axis plus unsupported future `>=3` | `combined-defect` | unknown axis first, future diagnostic second if same valid source permits both |
| unknown axis plus accepted `formula_compiler = ">=2"` and satisfied host | `combined-defect` | unknown axis remains fatal; accepted known axes do not mask unsupported axes |
| malformed requirement plus disabled host | `combined-defect` | requirement syntax/type diagnostic only; host satisfaction does not run |
| conflicting dual declaration plus legacy warning | `combined-defect` | conflict fatal plus deprecation warning with exact `diagnostic_count` |
| `[steps.requires]`, `[compose.expand.requires]`, or `[requires.formula_compiler]` plus no top-level `[requires]` | `misplaced-requirements` | non-top-level requirement tables never satisfy the root; invalid table diagnostic or missing-requirement diagnostic is fixture-locked |
| dotted key `requires.formula_compiler = ">=2"` plus bracket table `[requires]` | `misplaced-requirements` | TOML parser-boundary collision when the parser rejects it, otherwise structured duplicate/collision diagnostic; no last writer wins |

JSON support is named, not implicit. The implementation either keeps
`source_format=json` rows and adds `internal/formula/json_loader.go` ownership,
pointer attribution, duplicate-member tests, and Phase 2 docs examples, or
retires the JSON formula loader before user-visible diagnostics ship and
removes every JSON row from the matrix in the same PR. A JSON formula cannot
remain enabled as an untested bypass.

Every construct registry row cross-walks to the matrix identity that consumes
it:

| Registry entry | `construct_identity` | Axis minimum parser |
|---|---|---|
| step `check` | `check` | `formula_compiler` owner grammar |
| legacy internal `ralph` | `ralph` | `formula_compiler` owner grammar |
| retry table or metadata | `retry` | `formula_compiler` owner grammar |
| `on_complete` | `on_complete` | `formula_compiler` owner grammar |
| registered workflow-control metadata key | `workflow_metadata_key` | `formula_compiler` owner grammar |
| registered workflow-control metadata enum value | `workflow_metadata_value` | `formula_compiler` owner grammar |
| authored `step.expand` field | `step_expand` | `formula_compiler` owner grammar |
| `expand_vars` | `expand_vars` | `formula_compiler` owner grammar |

`AxisMinimum.Minimum` strings are parsed only by the owning axis grammar. No
registry generator may parse `">=2"` with ad hoc string splitting, numeric
coercion, trimming, or a generic semver helper.

<!-- REVIEW: added per attempt-80-construct-registry-executability -->

Compose traversal is not a construct identity. `compose.expand`, `compose.map`,
`compose.aspects`, and transitive imports appear only in `contribution_path`;
the construct identity remains the decoded feature found inside the contributed
formula or template. A row with `contribution_path=compose_expand` and
`construct_identity=step_expand` is valid only when the contributed template
contains an authored `Step.Expand` field. A row that uses `step_expand` merely
because compose traversal happened is invalid fixture input.

Construct registry invariants are generated from typed axis parsers:

| Invariant | Failure |
|---|---|
| `AxisMinimum.Minimum` parses through the `formula_compiler` grammar to exactly `Capability` | generator error; the row is not emitted |
| `min_compiler_capability` equals the parsed capability for every registry row | `TestConstructRegistryMinimumMatchesCapability` fails |
| every registry row has at least one matrix row by `construct_identity`, `construct_location`, and `contribution_path` | `TestConstructRegistryRowsCoveredByMatrix` fails |
| every matrix row names a checked `coverage_intent_id` from the suite registry, not free text | matrix schema validation fails |
| every suite count lock is a literal integer after generation | `TestCompilerRequirementMatrixCountsLocked` fails |

The matrix schema keeps human-readable `coverage_intent` for review, but CI
keys on `coverage_intent_id`. The checked ids live in
`internal/formula/testdata/coverage_intents.yaml`, so caller-path vocabulary,
contribution-path vocabulary, and suite ownership cannot drift independently.

Every suite row carries `row_kind`, `coverage_intent`, `dimension_owner`, and
`zero_write_assertion` when the caller can mutate state. The generator rejects a
durable caller row that lacks an explicit zero-write assertion for roots,
children, hooks, dependencies, convoys, retry metadata, fanout state,
convergence state, or workflow-root metadata. The required fixture families are:
workflow-control metadata keys and generated `gc.kind` values; parent/child
inheritance; expansion/aspect/import contribution conflicts; malformed,
unsupported-future, Unicode, and NUL-containing requirement values; legacy
`contract` and `version` edge cases; and generated control metadata produced by
scope, retry, fanout, Ralph continuation, and workflow-finalize formulas.

The raw-shape suite is the only owner of parser-boundary behavior. Duplicate
TOML tables or scalar keys that the TOML library rejects remain plain parser
errors with no structured formula diagnostic. Valid TOML/JSON values with the
wrong decoded shape become structured formula diagnostics. JSON cases use JSON
pointers as source keys and must be paired with the nearest TOML equivalent.
A reviewer must be able to add a new requirement axis or v2-only construct and
see exactly which suite counts, diagnostic fixtures, docs rows, and tests must
change.

<!-- REVIEW: added per DR55-raw-scanner-property-contract -->

Raw capture has named differential tests:

| Test | Contract |
|---|---|
| `TestRawRequirementScannerMatchesTOMLMetadata` | for every valid TOML fixture, raw key presence and decoded value shape match `toml.MetaData`, while raw bytes and source positions come only from the scanner |
| `TestRawRequirementScannerDuplicateScalarKeys` | duplicate scalar keys, duplicate `[requires]` tables, and `[[requires]]` arrays of tables stop at the parser boundary or emit the fixture-locked duplicate diagnostic; no last-writer-wins normalization |
| `FuzzRawRequirementScannerSourceAttribution` | random comments, whitespace, Unicode, control bytes, inline tables, dotted tables, nested tables, arrays, and malformed numeric strings never produce accepted requirements unless they exactly match the v0 grammar |
| `TestJSONRequirementScannerDuplicateMembersFatal` | duplicate JSON object members are fatal whenever the JSON reader can report them; otherwise the JSON loader is marked unsupported for requirement-bearing formulas before durable writes |
| `TestRawScannerPositionRoundTrip` | every structured diagnostic uses the scanner's byte-oriented line/column and escaped source value, not rendered parser text |

The matrix seed PR must replace placeholder count expressions with literal
integers for the grammar, raw-shape, JSON duplicate-member, construct-registry,
contribution-traversal, projection-parity, and caller-preflight suites. The
generator self-test prints the formula used to derive each count, but the
checked fixture stores the literal count so accidental row loss is visible in
review.

<!-- REVIEW: added per validation-matrix-independent-dimensions -->

The dimensions are intentionally independent. `construct_identity` says what
semantic feature requires compiler capability 2. `construct_location` says
where that feature appeared in the parsed formula tree. `contribution_path`
says how the construct reached the final compiled recipe. `caller_path` says
which production or preview path consumed the compile output. The generator
must not collapse those axes into a single "formula source" label because that
would hide bypasses such as a `step.expand` inside a loop body, `expand_vars`
on an inline expansion, an aspect that contributes retry metadata, or a
transitive import that raises the maximum requirement.

Unsupported and impossible rows are checked fixtures, not comments. Examples:

| Row kind | Example | Required assertion |
|---|---|---|
| `unsupported` | JSON formula contains dotted-table equivalent for `[requires.formula_compiler]` | emits the same diagnostic code as the TOML invalid-type row, using a JSON pointer source key |
| `unsupported` | `expand_vars` contains a non-string compile-time value | emits a formula-var diagnostic before durable writes |
| `impossible` | `construct_location=aspect_advice` with `contribution_path=root_file` | generator rejects the row as invalid fixture input |
| `impossible` | `caller_path=convoy_source_scan` with a source-only requirement parse error | convoy scan never compiles source formulas and must use metadata predicates only |

Generator self-tests fail closed. They must prove every decoded field in
`internal/formula.Formula`, `Step`, inline expansion, compose expansion/map,
aspect advice, and import contribution is either registered with
`min_compiler_capability`, explicitly v1-compatible, or rejected as unsupported.
They must also prove each caller path has at least one fatal-diagnostic fixture
whose assertion includes "zero durable writes" when that caller can create
beads, wisps, convoys, dependencies, hooks, retry metadata, convergence state,
or root metadata.

<!-- REVIEW: added per attempt-50-parser-edge-closure -->

The grammar and raw-shape suites must include byte-exact edge rows for signed
strings (`">=+2"`), zero and lower-than-supported strings (`">=0"` and
`">=-1"`), leading-zero strings (`">=02"`), overflow-sized digit strings,
tab/newline/carriage-return variants around the operator or value,
Unicode lookalike operators and digits, Unicode whitespace, escaped NUL and
other control characters, TOML integers at zero/negative/overflow boundaries,
and JSON-only shapes such as `null`, object, nested object, duplicate object
member when the JSON reader can report it, and array. None of these rows may
be accepted by trimming, numeric coercion, Unicode normalization, or
best-effort parsing.

<!-- REVIEW: added per attempt-54-requirement-matrix-closure -->

Exact matrix rows are required for the edges reviewers called out:

| Input | Row kind | Diagnostic |
|---|---|---|
| `formula_compiler = ">=0"` | negative | `formula.compiler_requirement_invalid_syntax` |
| `formula_compiler = ">=-1"` | negative | `formula.compiler_requirement_invalid_syntax` |
| `formula_compiler = ">=01"` and `">=02"` | negative | `formula.compiler_requirement_invalid_syntax` |
| `formula_compiler = ">=1.0"`, `">=2.0"`, or `">=2.1"` | negative | `formula.compiler_requirement_invalid_syntax` |
| `formula_compiler = ">=+2"` | negative | `formula.compiler_requirement_invalid_syntax` |
| `formula_compiler = ">="` or an overflow-sized ASCII integer | negative or unsupported according to parser boundary | never accepted; diagnostic code and parser boundary are fixture-locked |
| strings with NUL, control bytes, Unicode whitespace, Unicode operators, or Unicode digits | negative | `formula.compiler_requirement_invalid_syntax` with escaped source value |
| TOML `[[requires]]`, `[requires.formula_compiler]`, nested table, inline table, array, integer, float, bool | negative | `formula.requirement_invalid_type` or TOML parser error as specified by raw-shape suite |
| JSON malformed syntax or duplicate object member | parser-boundary | no structured formula diagnostic unless the JSON reader can provide a stable duplicate-member source |
| JSON `requires.formula_compiler` as object, array, number, bool, or null | negative | TOML-equivalent wrong-type diagnostic with JSON pointer source |
| `formula_compiler = ">=3"` plus unknown `requires.state_store` | negative | `formula.requirement_unknown_axis` before unsupported future capability |
| transitive import contributes retry/check/fanout without a declared `>=2` requirement | negative | one `formula.compiler_requirement_missing` at the contributing source formula |
| root has `>=1`, child/aspect/import contributes `>=2` | positive when inherited maximum is valid | normalized root requirement is `>=2` with source attribution for the raising contribution |

Suite count locks are derived from concrete fixture generators, not prose. The
`grammar` suite count is the number of accepted byte strings plus every
listed rejected byte string and unsupported future string. The `raw-shape`
suite count is the decoded TOML/JSON shape vocabulary multiplied only by the
fixture-owned source formats. The `construct-registry` count is the registered
construct rows times the required positive and missing-requirement cases. The
`caller-preflight` count is the durable-writer manifest rows whose end state is
`accepted_artifact_only`. CI prints the derived formula and the resulting
literal count so reviewers can see why a count changed.

`check` is a v2-only construct in this design. Any existing reference doc,
tutorial, fixture, or generated example that presents `check` as valid for
default-capability formulas is stale and must either dual-declare
`contract = "graph.v2"` plus `[requires] formula_compiler = ">=2"` during the
alias window or be rewritten as a v1-compatible example. The first
implementation PR must enumerate first-party formulas, examples, tutorials,
and fixtures containing `check`, `retry`, `on_complete`, workflow-control
metadata, fanout, expansion, aspect, or convergence syntax and classify each
as `dual-declare`, `rewrite-v1`, or `delete-stale-fixture`.

<!-- REVIEW: added per legacy-version-bypass -->

Legacy `version` is never a compiler requirement. The matrix must cross
`version` omitted, `version = 1`, and `version = 2` with every v2-only
construct and with omitted, empty-table, `>=1`, and `>=2` requirement states.
All rows where a v2-only construct appears with only omitted/default
requirements fail with `formula.compiler_requirement_missing`, regardless of
whether legacy `version` is absent, `1`, or `2`. `version = 2` is preserved
only as legacy metadata and may produce `formula.version_deprecated` on
validation/display surfaces; it does not imply graph capability.

If `version = 2` appears in a source formula that also contains v2-only syntax
but lacks `contract = "graph.v2"` or `[requires] formula_compiler = ">=2"`,
validation emits `formula.compiler_requirement_missing` as the fatal
diagnostic and `formula.version_misuse` as a warning on validation/display
surfaces. The warning's remediation points to `[requires]`; it never changes
runtime behavior and is not emitted on successful launch paths.

Fixtures that currently expect `version = 1` plus a v2-only construct such as
`[steps.check]` to compile as a legacy molecule are invalid under this design.
They must be removed or rewritten to dual-declare `contract = "graph.v2"` and
`[requires] formula_compiler = ">=2"` during the alias window. CI fails if any
fixture, generated matrix row, or golden diagnostic preserves a path where
legacy `version` satisfies or suppresses `requires.formula_compiler`.

`go generate ./internal/formula` regenerates the rendered matrix and the golden
diagnostic fixtures. CI fails when the generated Markdown table, golden
diagnostics, and fixture rows disagree.

<!-- REVIEW: added per attempt-73-validation-matrix-contract -->

Validation matrix generation has named files, generated tests, and self-tests:

| Artifact | Path or test | Contract |
|---|---|---|
| source matrix | `internal/formula/testdata/compiler_requirements_matrix.yaml` | checked-in row schema, dimensions, suite count locks, and explicit positive/negative/unsupported/impossible rows |
| generated Go cases | `internal/formula/compiler_requirements_matrix_generated_test.go` | produced by `go generate ./internal/formula`; never edited by hand |
| golden diagnostics | `internal/formula/testdata/golden/compiler_requirements/*.json` | one ordered diagnostic projection per row that names source path/key/value/line/column and `diagnostic_count` |
| generator | `internal/formula/internal/matrixgen` | validates schema, rejects unknown dimensions, expands bounded suites, and writes generated tests plus rendered Markdown |
| generator self-tests | `TestCompilerRequirementMatrixSchema`, `TestCompilerRequirementMatrixCountsLocked`, `TestCompilerRequirementMatrixGoldenDiagnostics`, `TestCompilerRequirementMatrixGeneratedTestsInSync` | fail when a row, count lock, generated Go case, or golden output is missing or stale |
| public `check` compatibility | suite `legacy-check-compatibility` | rows for current public `check` syntax, old examples, dual declarations, missing `>=2`, and host-disabled diagnostics |
| JSON policy | suite `json-requirements` | JSON remains supported while the JSON loader exists; duplicate members, pointer attribution, wrong-type parity, and host-disabled rows are required |

The row schema is stable enough for review:

```yaml
id: string
row_kind: positive | negative | unsupported | impossible
coverage_intent: string
source:
  format: toml | json
  path: string
  body_fixture: string
  requirement_path: string
  raw_value: string
  raw_shape: string
legacy:
  contract: omitted | graph.v2 | graph.v1 | other
  version: omitted | 1 | 2 | string | invalid_type
construct:
  identity: string
  location: string
  contribution_path: string
caller:
  path: string
  durable_boundaries: [root, child, dependency, hook, convoy, retry_metadata, fanout_state, convergence_state, artifact_ref]
host:
  formula_compiler: 1 | 2 | invalid
expect:
  normalized_requirement: string
  diagnostics: []
  diagnostic_count: integer
  zero_write_assertion: string
```

Every generated Go test name includes the row id and suite, for example
`TestCompilerRequirementsMatrix/grammar/requires-v2-host-disabled`. A matrix
PR is incomplete while a fixture can change parser behavior without changing a
row count, generated Go case, and golden diagnostic.

Rendered validation matrix excerpt:

| `contract` | legacy `version` | `[requires] formula_compiler` | V2-only construct | Host `formula_v2` | Result |
|---|---|---|---|---|---|
| omitted | omitted, `1`, or `2` | omitted or empty table | no | either | default capability, optional `formula.version_deprecated` on validation/display |
| omitted | omitted, `1`, or `2` | `>=1` | no | either | default capability, optional `formula.version_deprecated` on validation/display |
| omitted | omitted, `1`, or `2` | `>=2` | no or yes | true | graph capability, no error except optional `formula.version_deprecated` |
| omitted | omitted, `1`, or `2` | `>=2` | no or yes | false | `formula.compiler_requirement_unsatisfied` |
| omitted | omitted, `1`, or `2` | omitted, empty table, or `>=1` | yes | either | `formula.compiler_requirement_missing` |
| `graph.v2` | omitted, `1`, or `2` | omitted | no or yes | true | graph capability plus `formula.contract_deprecated`, optional `formula.version_deprecated` on validation/display |
| `graph.v2` | omitted, `1`, or `2` | `>=2` | no or yes | true | graph capability, source `dual`, plus `formula.contract_deprecated`, optional `formula.version_deprecated` |
| compatibility-corpus accepted whitespace/case alias | omitted, `1`, or `2` | omitted or `>=2` | no or yes | true | graph capability plus `formula.contract_deprecated`; diagnostics preserve raw alias spelling |
| `graph.v2` | omitted, `1`, or `2` | `>=1` | no or yes | either | `formula.compiler_requirement_conflict` |
| other value | any | any | any | any | `formula.compiler_requirement_unsupported` on `contract` |
| omitted | any | invalid syntax such as `2`, `>= 2`, `>=2.0`, or empty string | any | any | `formula.compiler_requirement_invalid_syntax` |
| omitted | any | unsupported future minimum such as `>=3` | any | any | `formula.compiler_requirement_unsupported_future` |
| omitted | any | wrong TOML type, dotted table, or nested table | any | any | `formula.requirement_invalid_type` |
| omitted | any | unknown `[requires]` key | any | any | `formula.requirement_unknown_axis` |

If several defects are present, diagnostic ordering follows the list above. For
example, an unknown requirement axis is reported before a missing v2 compiler
requirement, and a conflict between `contract` and `[requires]` is reported
before host capability satisfaction.

Combined-defect precedence examples:

| Input defects | First diagnostic |
|---|---|
| malformed TOML plus unsupported requirement | TOML decoding error |
| unknown `[requires]` key plus v2-only construct without `>=2` | `formula.requirement_unknown_axis` |
| wrong TOML type plus legacy `contract = "graph.v2"` | `formula.requirement_invalid_type` |
| unsupported future `formula_compiler = ">=3"` plus disabled host | `formula.compiler_requirement_unsupported_future` |
| `contract = "graph.v2"` plus `formula_compiler = ">=1"` plus disabled host | `formula.compiler_requirement_conflict` |
| `version = 1` or `version = 2` plus v2-only construct without `>=2` | `formula.compiler_requirement_missing` |
| missing `>=2` for v2-only construct plus disabled host | `formula.compiler_requirement_missing` |

Diagnostic count and ordering rules:

- Invalid TOML/JSON syntax stops formula validation because raw source
  locations are unavailable.
- For a valid raw file, all independent requirement diagnostics are emitted,
  sorted by precedence group, then source path, line, column, source key, and
  diagnostic code.
- Host satisfaction runs only when normalization produced a usable requirement
  and no fatal requirement syntax, type, axis, or conflict diagnostic exists for
  that source formula.
- Missing-requirement diagnostics are emitted once per source formula that owns
  the undeclared v2-only construct, not once per call site that imported it.
- `formula.contract_deprecated` and `formula.version_deprecated` warnings are
  appended after fatal diagnostics but keep their own source order when there
  are no fatal diagnostics.
- `formula.version_misuse` is appended after the paired
  `formula.compiler_requirement_missing` diagnostic and before generic
  `formula.version_deprecated` when both warnings would apply to the same
  source field.
- The matrix row's `diagnostic_count` is authoritative; tests fail if code
  short-circuits, duplicates warnings, or adds downstream diagnostics not named
  by the row.

The implementation must add table-driven tests for accepted strings, rejected
strings, invalid TOML types, unknown `[requires]` keys, empty tables, legacy
contract compatibility, conflicts, missing requirements, inherited
requirements, expansion/aspect aggregation, loop bodies, children, and
unsupported future requirements.

Raw file decode failures are classified before formula validation:

| Raw input failure | Diagnostic class |
|---|---|
| invalid TOML syntax, duplicate table, or invalid array/table structure | plain TOML parse/decode error; no formula diagnostic code |
| valid TOML with wrong `[requires]` value type | structured `formula.requirement_invalid_type` |
| valid TOML with malformed byte-exact string such as `">= 2"` or `"2"` | structured `formula.compiler_requirement_invalid_syntax` |
| valid TOML with syntactically valid future minimum such as `">=3"` | structured `formula.compiler_requirement_unsupported_future` |
| valid TOML with unknown `[requires]` key | structured `formula.requirement_unknown_axis` |
| valid TOML plus conflicting legacy `contract` | structured `formula.compiler_requirement_conflict` |

The v0 accepted grammar is byte-exact: only omitted, `">=1"`, and `">=2"` are
accepted. The parser may recognize `>=<integer>` only to emit the
machine-distinguishable `formula.compiler_requirement_unsupported_future`
diagnostic for future minimums above the host binary's known maximum. No
trimming, semver parsing, numeric coercion, or optimistic future-range
acceptance is allowed in v0.

<!-- REVIEW: added per DR41-edge-fixtures -->

Fixture-level edge decisions:

| Edge | Decision |
|---|---|
| `>=1` | accepted as default capability, source `requires`, and not a graph workflow |
| integer boundaries | TOML integers, negative numbers, zero, and values larger than Go `int64` are wrong type or TOML parse errors; none are coerced |
| Unicode strings | any value other than byte-exact ASCII `>=1`, `>=2`, or recognizable ASCII `>=<integer>` is invalid syntax |
| NUL-containing strings | invalid syntax with escaped source value in diagnostics; no truncation |
| test-only normalized requirements | `_test.go` or `internal/formula/testonly`; production constructors remain unexported |
| glossary `schema` wording | reserved for physical file/API schema, never compiler capability |
| dashboard remediation fields | generated from typed `FormulaDiagnostic.Remediation`, not copied from message text |
| transitive ref collisions | PackV2 validation fails unless the lockfile resolves a single immutable revision and records the collision |
| `safe_automatic_edit` | advisory only in v0; no command applies edits automatically |

V2-only construct registry completeness is CI-enforced. The registry fixture
enumerates every syntax location the decoder can produce for steps, children,
loop bodies, expansion templates, aspects, and imported formulas. A
reflection-backed unit test fails when a new decoded formula field or
metadata-controlled workflow construct is added without one of these outcomes:
registered as v2-only, explicitly classified as v1-compatible, or rejected as
unsupported input. This prevents silent graph-only syntax from bypassing
`requires.formula_compiler`.

## Canonical Compile Result

<!-- REVIEW: added per canonical-requirement-ownership -->

`internal/formula` is the only package that may interpret raw `contract`,
`requires.formula_compiler`, `version`, or v2-only construct strings. All
behavioral callers must consume a normalized compile result.

The required internal shape is:

```go
type CompilerCapability int

const (
    CompilerCapabilityDefault CompilerCapability = 1
    CompilerCapabilityV2      CompilerCapability = 2
)

type RequirementSource string

const (
    RequirementSourceOmitted  RequirementSource = "omitted"
    RequirementSourceRequires RequirementSource = "requires"
    RequirementSourceContract RequirementSource = "contract"
    RequirementSourceDual     RequirementSource = "dual"
)

type DiagnosticSeverity string

const (
    DiagnosticSeverityError   DiagnosticSeverity = "error"
    DiagnosticSeverityWarning DiagnosticSeverity = "warning"
)

type DiagnosticSourceKind string

const (
    DiagnosticSourceFormulaRequirement DiagnosticSourceKind = "formula_requirement"
    DiagnosticSourceHostCapability     DiagnosticSourceKind = "host_capability"
    DiagnosticSourcePackRequirement    DiagnosticSourceKind = "pack_requirement"
    DiagnosticSourceLegacyAlias        DiagnosticSourceKind = "legacy_alias"
    DiagnosticSourceParserBoundary     DiagnosticSourceKind = "parser_boundary"
    DiagnosticSourceInternalInvariant  DiagnosticSourceKind = "internal_invariant"
)

type DiagnosticSource struct {
    Kind   DiagnosticSourceKind
    Path   string
    Key    string
    Value  string
    Line   int
    Column int
}

type DiagnosticAttribution struct {
    Primary     DiagnosticSource
    Requirement *DiagnosticSource
    Host        *DiagnosticSource
    Pack        *DiagnosticSource
}

type NormalizedRequirements struct {
    formulaCompiler CompilerCapability
    source          RequirementSource
    sourceFormula   string
    sourcePath      string
    deprecated      bool
}

type Diagnostic struct {
    Code                  string
    Severity              DiagnosticSeverity
    Attribution           DiagnosticAttribution
    Formula               string
    SourcePath            string
    SourceKey             string
    SourceValue           string
    SourceKind            DiagnosticSourceKind
    RequirementSourceKind DiagnosticSourceKind
    NormalizedRequirement string
    HostSourceKind        DiagnosticSourceKind
    HostCapability        string
    Message               string
    Remediation           string
    OnceKey               string
}

type HostCapabilitySourceKind string

const (
    HostCapabilitySourceOmittedDefault       HostCapabilitySourceKind = "omitted_default"
    HostCapabilitySourceExplicitFormulaV2    HostCapabilitySourceKind = "explicit_formula_v2"
    HostCapabilitySourceDeprecatedGraphWorkflows HostCapabilitySourceKind = "deprecated_graph_workflows"
    HostCapabilitySourceTestOverride         HostCapabilitySourceKind = "test_override"
)

type HostCapabilitySource struct {
    Kind             HostCapabilitySourceKind
    Path             string
    Key              string
    RawValue         string
    Line             int
    Column           int
    ConfigGeneration uint64
    ReloadID         string
}

type HostCapabilityInput struct {
    FormulaCompiler CompilerCapability
    Source          HostCapabilitySource
}

type HostCapabilities struct {
    formulaCompiler  CompilerCapability
    source           HostCapabilitySource
}

type CompileOptions struct {
    Vars             map[string]string
    HostCapabilities HostCapabilities
    ValidateRuntimeVars bool
}

type CompileIdentity struct {
    FormulaName                   string
    SourceIdentity                string
    ContentHash                   string
    HostCapabilities              HostCapabilities
    SearchPathsHash               string
    OptionsHash                   string
    VarsHash                      string
    BindingIdentityHash           string
    ArtifactVersion               int
    RequirementsSchemaVersion     int
    ProjectionSnapshotVersion     int
}

type CompileResult struct {
    Recipe       *Recipe
    Requirements NormalizedRequirements
    GraphWorkflow bool
    Diagnostics  []Diagnostic
    Provenance   Provenance
    Projection   PreviewProjectionSnapshot
}
```

<!-- REVIEW: added per DR53-diagnostic-attribution-contract -->

`DiagnosticAttribution` is the single reusable source-attribution object for
formula diagnostics. The older flat `SourcePath`, `RequirementSourcePath`, and
`HostSourcePath` fields in CLI/API/event structs are projections from this
object for wire compatibility and display. No caller, dashboard panel, event
producer, or release report may reconstruct host or requirement attribution
from message text, root metadata strings, or side channels.

The attribution rules are fixed:

| Diagnostic | Primary source | Requirement source | Host source |
|---|---|---|---|
| formula source syntax/type/axis errors | offending formula field | same field when it is a requirement | omitted |
| legacy `contract` warning/conflict | legacy `contract` field | legacy `contract` or dual source | omitted |
| unsatisfied host capability | host capability field | formula requirement field | host capability field |
| pack floor failure | `[pack] requires_gc` or pack acquisition source | formula requirement when available | active binary/version source |
| internal invalid host value | typed host capability constructor source | omitted | invalid host value |

Every diagnostic fixture asserts `Attribution.Primary.Kind`, `Path`, `Key`,
and `Value`; unsatisfied-host fixtures also assert both `Requirement` and
`Host`. This is what CLI stderr, Huma JSON, generated TypeScript, dashboard
state, registered Event Bus payloads, warning `OnceKey`, and release reports
derive from.

<!-- REVIEW: added per host-capability-satisfaction -->

The public compile path may keep a convenience `Compile(...) (*Recipe, error)`
wrapper, but callers that branch on graph behavior or project diagnostics must
use `CompileResult`, and callers that create workflow roots or stamp durable
metadata must use an `AcceptedCompileArtifact`. `Recipe` may carry the
normalized fields for instantiation, but it must not force consumers to
re-parse raw TOML strings.

`CompileResult` is a preview and inspection object. It is not proof that the
compile is acceptable for a durable write because a caller can still ignore
fatal diagnostics, skip host-capability satisfaction, or reuse the result with
different vars/search paths. Durable producers must first exchange a
`CompileResult` for a compiler-minted accepted artifact, described below.

`HostCapabilities` is an explicit input, not package-global state. Existing
`SetFormulaV2Enabled`/`IsFormulaV2Enabled` shims may remain only as temporary
wrappers that build `CompileOptions` at the CLI/API edge. They may not be read
inside requirement normalization or satisfaction logic.

`HostCapabilities` has one authoritative capability field plus a structured
source object. The legacy `[daemon] formula_v2` boolean is translated at the
CLI/API/controller edge into `CompilerCapabilityDefault` or
`CompilerCapabilityV2`; it is not carried as a second boolean inside the formula
package because that would allow contradictory host state. The source object
preserves the config path, key, raw value, source kind, source position, config
generation, and reload id that produced the normalized capability.

<!-- REVIEW: added per DR48-host-capability-provenance -->

Omitted/default `formula_v2 = false`, explicit `formula_v2 = false`, explicit
`formula_v2 = true`, deprecated `graph_workflows = true` promotion, and
test-only overrides are distinct provenance cases even when two cases normalize
to the same capability. Runtime behavior uses only the normalized
`FormulaCompiler` value, but diagnostics, accepted artifacts, release reports,
and migration hints preserve the source object so operators can tell whether to
add `formula_v2`, remove `graph_workflows`, change an explicit false value, or
ignore a test-only override. `graph_workflows` promotion is allowed only in the
config edge adapter and must emit a deprecation diagnostic on
validation/display surfaces; it must not become a second host-capability input
inside `internal/formula`.

Valid host capability values are exactly `CompilerCapabilityDefault` and
`CompilerCapabilityV2` in v0. `NewHostCapabilities(input)` is the only
production constructor. A legacy `formula_v2` adapter may exist at the config
edge, but its input is still structured `HostCapabilityInput`; no production
API accepts only `(enabled bool, source string)` as capability provenance.
`HostCapabilitiesForTest` is available only from `internal/formula/testonly` or
`_test.go` files and must set `Source.Kind=test_override`. The fields are
unexported so callers cannot bypass provenance, config-generation, source-kind,
or value validation with struct literals. Accessors such as
`FormulaCompiler()`, `Source()`, `SourceKind()`, and `ConfigGeneration()` are
read-only projections. Any invalid value or incomplete source object is
rejected by `CheckRequirements` as an internal configuration error diagnostic
and never panics. Future values such as `CompilerCapability(3)` are unsupported
until the same change adds typed parser support, diagnostics, metadata, docs,
and tests. Satisfaction is computed for every call from
`CompileOptions.HostCapabilities`; it is not cached on formula identity,
process-global config, or pack resolution state.

<!-- REVIEW: added per attempt-78-host-capability-provenance -->

Host-capability provenance has golden fixtures across CLI, API, API-routed CLI,
dashboard, Event Bus failure payloads, accepted artifacts, and release reports:

| Fixture | Normalized capability | Required source object |
|---|---|---|
| omitted default | `1` | `Kind=omitted_default`, config path, key `daemon.formula_v2`, raw value `""`, generation |
| explicit false | `1` | `Kind=explicit_formula_v2`, source path/key/value `false`, line, column, generation |
| explicit true | `2` | `Kind=explicit_formula_v2`, source path/key/value `true`, line, column, generation |
| deprecated alias promotion | `2` | `Kind=deprecated_graph_workflows`, source key `daemon.graph_workflows`, raw value, generation, deprecation diagnostic |
| test override | `1` or `2` | `Kind=test_override`, test file path or fixture id, never accepted from production config |
| config reload | current value | new generation and reload id; old producer groups keep the old generation |
| host downgrade same identity | current value `1`, artifact value `2` | current source object in diagnostic state and original source object in the accepted artifact |

`TestHostCapabilityProvenanceParity` is generated from these fixtures. It
fails if any surface drops source kind, raw value, line/column when available,
config generation, reload id, host capability, normalized requirement, or the
same-identity artifact-reuse distinction.

<!-- REVIEW: added per attempt-80-host-authority-guards -->

Host capability authority is guarded inside `internal/formula`, not only at
call sites. `TestNoFormulaPackageGlobalCapabilityReads` scans production files
in `internal/formula` and fails on reads of legacy process-global state such as
`formula_v2`, `graph_workflows`, `IsFormulaV2Enabled`, or
`SetFormulaV2Enabled`, except for explicitly named edge adapters that build a
`HostCapabilityInput` and return immediately to callers outside normalization.
Those adapter rows must name an owner, expiry phase, and replacement test in
`internal/formula/testdata/host_capability_adapter_allowlist.yaml`.

`RequirementSource` is provenance-only. A separate static guard,
`TestRequirementSourceProvenanceOnly`, fails when production code outside
diagnostic construction, warning persistence, migration reports, compatibility
reports, fixtures, or docs branches on `RequirementSource` constants. Runtime
behavior must branch on normalized capability values and accepted-artifact
validation results only.

Property tests close the two authority escape hatches:

| Property | Assertion |
|---|---|
| `CompilerCapability(0)` non-escape | fuzzed raw fields, host inputs, test overrides, artifact loads, and metadata reads never produce a normalized requirement, host capability, artifact identity, or wire diagnostic that treats capability `0` as valid |
| same-identity host downgrade write intent | a disabled current host can authorize only operations whose accepted artifact, root artifact ref, non-host identity, projection snapshot, and write intent match; any changed source, vars, options, search path, binding, projection version, or write boundary fails before durable writes |

Graph-apply enablement follows the same lifetime rule. Package globals such as
`SetGraphApplyEnabled` and `IsGraphApplyEnabled` may remain only as temporary
edge shims while caller migration is in progress. Durable graph-apply
decisions must flow through per-operation compile options, accepted artifact
validation, and write intent. A graph-apply global cannot authorize root,
child, dependency, hook, fanout, or convergence writes and cannot override a
disabled or stale host capability captured in the accepted artifact.

`NormalizedRequirements` is constructible only inside `internal/formula`.
Callers outside the package use accessor methods such as
`FormulaCompiler()`, `Source()`, `SourcePath()`, and `Deprecated()`. Test-only
constructors live in `_test.go` files or behind a `testonly` helper. Production
callers may not synthesize normalized requirements from raw TOML, root
metadata, or command flags.

Required formula-domain APIs:

```go
type PreflightInput struct {
    Source           ResolvedFormulaSource
    HostCapabilities HostCapabilities
    Vars             map[string]string
    Options          CompileOptions
    WriteIntent      *CompileWriteIntent // nil for preview/report commands
}

type PreflightResult struct {
    CompileResult    *CompileResult
    AcceptedArtifact *AcceptedCompileArtifact
    Diagnostics      []Diagnostic
    Provenance       Provenance
    Projection       PreviewProjectionSnapshot
}

func PreflightFormula(ctx context.Context, input PreflightInput) (*PreflightResult, error)
func CompileWithResult(ctx context.Context, name string, searchPaths []string, opts CompileOptions) (*CompileResult, error)
func CheckRequirements(req NormalizedRequirements, host HostCapabilities) []Diagnostic
func AcceptCompileResult(result *CompileResult, identity CompileIdentity) (AcceptedCompileArtifact, []Diagnostic)
func ValidateAcceptedArtifact(artifact AcceptedCompileArtifact, intent CompileWriteIntent) []Diagnostic
func NewHostCapabilities(input HostCapabilityInput) (HostCapabilities, []Diagnostic)
func RootMetadataFacts(metadata map[string]string) (RootMetadataFacts, []Diagnostic)
```

`PreflightFormula` is the canonical entry point for new behavioral callers. It
receives resolver-owned `ResolvedFormulaSource`, active `HostCapabilities`,
compile vars/options, and an optional write intent. Preview/report callers get
only a `CompileResult`, diagnostics, provenance, and preview projection.
Durable callers get an `AcceptedCompileArtifact` only when fatal diagnostics
are absent and the write intent validates. `CompileWithResult` may remain as an
internal or compatibility wrapper, but it cannot authorize writes by itself.

`CheckRequirements` returns diagnostics and never creates beads, writes
metadata, or mutates global state. `AcceptCompileResult` is the only function
that can mint an `AcceptedCompileArtifact`; it calls or verifies requirement
satisfaction, rejects any fatal diagnostic, verifies the compile identity, and
copies provenance from the compiler-owned result. Every caller that can create
a root, wisp, attached molecule, expansion fragment, order wisp, or convergence
instance must use `PreflightFormula` with a write intent or call
`CompileWithResult` and `AcceptCompileResult` in that exact order before the
first durable write. Unit tests must prove two compiles in the same process can
evaluate the same formula against different host capabilities deterministically,
including concurrent compiles with host capability `1` and `2`.

Legacy `Compile(...) (*Recipe, error)` is preview compatibility only after this
migration starts. It returns a recipe suitable for display or v1 execution
paths that do not write graph state, but it does not expose accepted artifacts,
diagnostic authority, host provenance, or workflow-root facts. Static guards
fail when a durable writer receives a bare `*Recipe`, `Recipe.GraphWorkflow`,
raw metadata predicate result, or caller-built `NormalizedRequirements` as a
write precondition.

`GraphWorkflow` is true only when the accepted normalized compiler capability
is `CompilerCapabilityV2`; it is false for default-capability formulas even if
arbitrary metadata contains legacy-looking strings. `Recipe.GraphWorkflow` is a
read-only projection of the compile result for execution code that already
receives a recipe. Mutable recipe metadata is a serialization boundary only and
must not be used as a caller-side compiler API.

<!-- REVIEW: added per DR41-graph-semantics -->

The semantic rule is fixed: `requires.formula_compiler = ">=2"` declares that
the formula may materialize graph-workflow topology. It is not merely a host
provenance or diagnostic capability flag. A formula with capability `2` is
compiled through the compiler-owned graph workflow path, and every durable
writer must learn graph behavior from `CompileResult.GraphWorkflow`, accepted
artifact accessors, or `internal/sourceworkflow.WorkflowRootFacts`. No other
package may infer graph workflow semantics from raw TOML, `Recipe` metadata,
root metadata strings, or legacy helper names.

The compiler-owned generic step projections are:

```go
type SourceValue[T any] struct {
    Value      T
    SourcePath string
    SourceKey  string
    Line       int
    Column     int
}

type CompiledStep struct {
    ID              SourceValue[string]
    Title           SourceValue[string]
    Kind            SourceValue[string]
    Retry           *CompiledRetryPolicy
    RuntimeVars     []CompiledRuntimeVar
    Metadata        []CompiledMetadataField
    GraphCapability CompilerCapability
}

type CompiledRuntimeVar struct {
    Name        SourceValue[string]
    Required    SourceValue[bool]
    Default     SourceValue[string]
    Description SourceValue[string]
}
```

These projections are read-only compiler output. `Recipe.GraphWorkflow`,
`internal/sourceworkflow`, convergence, fanout, API globals, and molecule
instantiation code consume them or the accepted artifact; they do not keep a
parallel graph-contract subset parser.

<!-- REVIEW: added per DR53-accepted-projection-snapshot -->
<!-- REVIEW: added per attempt-73-preview-accepted-boundary -->

The compiler emits a preview projection first, then seals a structurally
different accepted projection inside the accepted artifact. Preview projection
types are allowed only on validation, display, and dry-run paths:

```go
type PreviewProjectionSnapshot struct {
    SnapshotVersion       int
    Requirements          NormalizedRequirements
    Diagnostics           []Diagnostic
    Provenance            Provenance
    Steps                 []CompiledStep
    RuntimeVars           []CompiledRuntimeVar
    RetryPolicies         []CompiledRetryPolicy
    WorkflowControlFields []CompiledMetadataField
    Convergence           CompiledConvergenceProjection
    SourceAttributions    []DiagnosticSource
    ContentHash           string
    VarsHash              string
    OptionsHash           string
    SearchPathsHash       string
    BindingIdentityHash   string
}
```

`AcceptedProjectionSnapshot` is minted only by `AcceptCompileResult` after
fatal diagnostics, identity fields, host satisfaction, provenance, and proof
identity all validate. The accepted snapshot carries the same compiler-owned
facts plus the accepted identity hash and write-intent schema version so
durable callers cannot accidentally pass a preview as authority:

```go
type AcceptedProjectionSnapshot struct {
    SnapshotVersion       int
    AcceptedIdentityHash  string
    WriteIntentSchema     int
    Requirements          NormalizedRequirements
    Diagnostics           []Diagnostic
    Provenance            Provenance
    Steps                 []CompiledStep
    RuntimeVars           []CompiledRuntimeVar
    RetryPolicies         []CompiledRetryPolicy
    WorkflowControlFields []CompiledMetadataField
    Convergence           CompiledConvergenceProjection
    SourceAttributions    []DiagnosticSource
    ContentHash           string
    VarsHash              string
    OptionsHash           string
    SearchPathsHash       string
    BindingIdentityHash   string
}
```

This is the only data durable convergence, fanout, retry, scope-check,
workflow-finalize, and missing-child repair may use when they need formula
facts after the original source may have moved or disappeared.
`CompileWithResult` populates the preview snapshot from the fully decoded,
inherited, expanded, and source-attributed compiler output.
`AcceptCompileResult` copies and seals it into `AcceptedCompileArtifact`.
`ProjectAcceptedFormula`,
`ValidateProjection`, active-root repair, fanout fragment repair, retry, and
workflow-finalize paths receive only the accepted artifact or a read-only view
of this snapshot. They may not reopen formula source, call a subset parser, or
fall back to root metadata when the snapshot is present.

Snapshot identity is checked with the accepted artifact identity. A mismatch in
formula name, content hash, vars/options/search-path hash, host capability,
config generation, pack/import binding identity, artifact version, or snapshot
version makes the artifact display-only for that binary and blocks
graph-specific writes. Fixtures cover missing source, voluntary/default
requirements, host downgrade after root creation, concurrent repair attempts,
unsupported snapshot schema, and zero-write failure at every durable boundary.

Workflow-root query authority is split deliberately and has one persistence
owner:

| Owner | Authority |
|---|---|
| `internal/formula` | Defines normalized requirement semantics, graph-workflow facts, and exact metadata key/value meanings |
| `internal/sourceworkflow` | Sole owner of workflow-root bead-store query criteria and post-fetch predicates |
| CLI/API/order/convoy/dashboard callers | Call the shared predicate/query helper only; no direct metadata filters |

Required `internal/sourceworkflow` APIs:

```go
type WorkflowRootKind string

const (
    WorkflowRootAny              WorkflowRootKind = "any"
    WorkflowRootCanonicalOnly    WorkflowRootKind = "canonical_only"
    WorkflowRootLegacyOnly       WorkflowRootKind = "legacy_only"
    WorkflowRootDualStamped      WorkflowRootKind = "dual_stamped"
    WorkflowRootKnownGraphOnly   WorkflowRootKind = "known_graph_only"
    WorkflowRootUnknownFutureOnly WorkflowRootKind = "unknown_future_only"
    WorkflowRootNeedsMigration   WorkflowRootKind = "needs_migration"
)

type SourceWorkflowScope struct {
    SourceBeadID string
    WorkflowID   string
    IncludeClosed bool
}

type WorkflowRootCriteria struct {
    Kind          WorkflowRootKind
    Scope         SourceWorkflowScope
    IncludeLegacy bool
    IncludeClosed bool
}

type WorkflowRootCapabilityKind string

const (
    WorkflowRootNotWorkflow       WorkflowRootCapabilityKind = "not_workflow"
    WorkflowRootDefaultCapability WorkflowRootCapabilityKind = "default_capability_workflow"
    WorkflowRootKnownGraph        WorkflowRootCapabilityKind = "known_graph_workflow"
    WorkflowRootUnknownFuture     WorkflowRootCapabilityKind = "unknown_future_capability_workflow"
)

type WorkflowRootFacts struct {
    Kind                      WorkflowRootCapabilityKind
    FormulaName               string
    CompilerCapability        int
    Requirement               string
    RequirementSource         string
    RequirementsSchemaVersion int
    AcceptedArtifactVersion   int
    MinimumReaderCapability   int
    FutureAxes                []string
    UnsupportedAxes           []string
    ArtifactRef               string
    Diagnostics               []formula.Diagnostic
}

type WorkflowRootSnapshot struct {
    ID        string
    Status    beads.Status
    SourceID  string
    Metadata  map[string]string
    Facts     WorkflowRootFacts
}

func ClassifyWorkflowRoot(metadata map[string]string) WorkflowRootFacts
func IsWorkflowRootMetadata(metadata map[string]string) bool
func IsGraphWorkflowMetadata(metadata map[string]string) bool
func ListWorkflowRoots(store beads.Store, criteria WorkflowRootCriteria) ([]WorkflowRootSnapshot, error)
func AcceptedCompileArtifactRef(metadata map[string]string) (string, bool)
```

`internal/formula.RootMetadataFacts` may classify the formula-owned metadata
keys, but it does not query beads and it is not the workflow-root predicate.
The `internal/sourceworkflow` package owns the predicate and query helper for
all callers. New packages may not create a successor owner without first
deleting or moving the old one in the same change.

<!-- REVIEW: added per DR55-sourceworkflow-typed-api -->

`ListWorkflowRoots` is the migration boundary. Callers are not allowed to build
their own `beads.ListQuery` for workflow roots, even when the current filter
looks like a direct metadata match. The package may use `beads.ListQuery`
internally for efficient first-pass selection, but it must always run the
typed post-fetch predicate and return `WorkflowRootSnapshot` values whose
`Facts` came from `ClassifyWorkflowRoot`. This prevents each caller from
inventing subtly different exact, trim, case-fold, closed-history, or
source-scope semantics.

Phase 0 of caller migration must commit executable parity fixtures under
`internal/sourceworkflow/testdata/workflow_root_parity/` before any caller is
moved. The fixtures cover canonical-only, dual-stamped, legacy-only,
graph-v2-only, closed-history, source-scoped, whitespace-variant, and
case-variant stores. For each store, the golden output names the root ids
returned by `ListWorkflowRoots`, the `WorkflowRootFacts.Kind` for each row, and
the callers whose previous behavior must match: sling attach, convoy/source
workflow scans, API workflow-root projections, order dispatch, convergence
repair, and dashboard read models. Case variants are fixtures specifically so
the compatibility rule stays narrow: canonical metadata is exact, while the
legacy `gc.formula_contract` fallback trims only historical surrounding
whitespace and never case-folds.

<!-- REVIEW: added per DR33-typed-root-facts -->

Boolean predicates are compatibility helpers over `WorkflowRootFacts`, not the
primary model. Callers that need behavior choose from the typed kind:
non-workflow, default-capability workflow, known graph workflow, or unknown
future capability workflow. Old binaries that see valid canonical workflow
metadata with an unsupported compiler capability classify it as
`unknown_future_capability_workflow`, keep it visible for observation and
operator cleanup, and refuse to perform graph-specific child, fanout,
continuation, retry, or new-compile behavior they do not understand.

<!-- REVIEW: added per DR41-old-reader-contract -->

Every new workflow root carries a v0-readable durable requirements contract in
metadata even when the full accepted artifact is stored elsewhere:

| Metadata key | Meaning |
|---|---|
| `gc.formula_requirements_schema_version` | integer schema version for formula requirement root metadata; v0 writes `1` |
| `gc.formula_accepted_artifact_version` | accepted artifact serialization version used for this root |
| `gc.formula_min_reader_capability` | minimum formula compiler capability an old binary must understand before graph-specific writes are allowed |
| `gc.formula_requirement_axes` | comma-separated axis manifest, for example `formula_compiler` |
| `gc.formula_unsupported_axes` | comma-separated axes seen but not understood by this reader |

Old binaries do not need to parse future axis semantics to stay safe. If the
schema version, accepted artifact version, minimum reader capability, compiler
capability, or axis manifest exceeds what the binary understands, typed facts
classify the root as `unknown_future_capability_workflow`. Observation,
read-only API/dashboard projection, and cleanup that does not synthesize graph
semantics may continue; retry, fanout, continuation, missing-child repair,
child creation, `on_complete`, and any new compile fail closed before writes.

The query helper must expose typed criteria for all persistence scans that
currently need workflow roots: any workflow root, graph workflow roots,
canonical-only roots, legacy-only roots during the alias window, and roots that
need migration warnings. Tests must prove SQL/API/order/convoy selectors match
the same rows as the in-memory predicate.

Host capability plumbing is explicit at every production entry point:

| Entry point | Capability source and lifetime |
|---|---|
| `gc sling`, `gc formula`, `gc order` | loaded once from the active city config for that command invocation |
| HTTP/API handlers | derived from the request's resolved city config and passed through Huma handlers to formula adapters |
| order dispatch loop | snapshotted at the start of each order scan tick; one tick uses one snapshot |
| dispatch fanout | inherited from the parent dispatch operation and re-used for every fragment compile in that fanout transaction |
| convergence create/retry/speculative wisp | snapshotted before convergence validation and used until either zero writes happen or the accepted compile artifact is persisted |
| reconciler/controller tick | snapshotted per tick; existing accepted roots use persisted metadata, new formulas use the tick snapshot |

<!-- REVIEW: added per attempt-79-host-capability-edge-adapter -->

Host capability construction has one shared edge adapter. The implementation
adds an edge-only package, `internal/formulahost`, with:

```go
func FromCityConfig(cfg *config.City, source HostConfigSource) (formula.HostCapabilities, []formula.Diagnostic)
```

`FromCityConfig` is the only production code that translates
`cfg.Daemon.FormulaV2`, promoted `cfg.Daemon.GraphWorkflows`, omitted defaults,
config-generation ids, reload ids, source path, raw value, line, and column
into `formula.HostCapabilityInput`. CLI commands, API handlers, order dispatch,
controller ticks, convergence, fanout, tests that exercise production paths,
and any future pack-validation command call this adapter before compiling.
`cmd/gc/feature_flags.go:applyFeatureFlags` becomes a temporary compatibility
shim that calls the adapter and may set legacy globals only for callers that
have not yet migrated; it is not allowed to be the source of truth after
caller sub-phase 4a. Static guard `TestNoFormulaHostCapabilityBypass` fails on
new production calls to `formula.SetFormulaV2Enabled`,
`formula.IsFormulaV2Enabled`, `molecule.SetGraphApplyEnabled`, or boolean-only
host constructors outside the adapter, compatibility shim, and tests.

Caller inventory and required replacement behavior:

| Surface | Current risk | Required behavior |
|---|---|---|
| `internal/formula` parser, validation, and graph transforms | Raw `Contract` and `declaresGraphV2Contract` are load-bearing | Replace with `NormalizedRequirements` and `GraphWorkflow` |
| `gc formula show` | Preview can compile through legacy wrappers and omit structured diagnostics | Use `CompileWithResult` for display, keep runtime-var validation display semantics, and print/provide diagnostics without writing |
| `gc formula cook` | Root cook can bypass host checks before bead creation | Require `AcceptedCompileArtifact` before `molecule.Cook` writes root or children |
| `gc formula cook --attach` | Attached molecule can create child beads from a raw `Recipe` | Require `AcceptedCompileArtifact` before `molecule.Attach` writes the sub-DAG, dependency, hook, or attachment metadata |
| `internal/molecule` cook/cook-on/attach and graph apply | Root metadata can be stamped from raw contract or preview result | Stamp from `AcceptedCompileArtifact` only |
| `cmd/gc/cmd_sling.go` and `internal/sling` routing helpers | Graph routing and workflow attachment can branch on `gc.formula_contract` | Use shared workflow-root predicates backed by normalized metadata |
| `cmd/gc/cmd_order.go` and `cmd/gc/order_dispatch.go` | Order wisps can emit divergent errors | Preflight and accept a compile artifact; emit the shared diagnostic event on failure |
| `cmd/gc` helpers that shell out to `bd` or legacy formula probes | Shell-out output can bypass typed compiler diagnostics and durable preflight | Treat shell-outs as validation-only probes; no shell-out result may authorize root, child, hook, convoy, retry, or fanout writes |
| Retry and `on_complete` continuation launchers | Continuations can compile formulas after a root already exists | Use the parent host snapshot or current host snapshot as specified by the entry point and accept the target artifact before retry-run or continuation metadata writes |
| Ralph continuation and workflow-control steps | Generated control metadata can imply graph behavior without a compiler-owned result | Generate control metadata only from accepted artifacts and registered workflow-control metadata rows |
| `internal/dispatch/fanout.go` and fanout expansion fragments | Runtime fragment compilation can happen after a durable root exists | Compile each fragment through `CompileWithResult` before fanout writes any child, convoy, or continuation metadata |
| `molecule.Instantiate` and fragment instantiation helpers | Generic instantiation can stamp roots or children from a raw `Recipe` | Require an accepted artifact or accepted fragment wrapper for every root, child, dependency, and hook write |
| `cmd/gc/cmd_convoy_dispatch.go` and convoy cleanup | Graph-only roots can be discovered by legacy metadata only | Use shared workflow-root predicate that accepts new and legacy metadata during migration |
| `internal/api` workflow-root read models | Duplicate predicates can diverge from CLI/sourceworkflow scans | Use `internal/sourceworkflow.ListWorkflowRoots` and typed facts for every workflow-root API projection |
| `internal/api` global formula/order/control projections | API globals can reassemble graph state from root metadata | Project compiler requirement, graph kind, and diagnostics from typed facts only |
| `internal/api/handler_sling.go`, formula endpoints, order feeds, and convoy projections | HTTP status and dashboard-facing errors can diverge | Project `Diagnostic` without hand-written JSON or string parsing |
| `internal/convergence` create/retry/next iteration | Subset validation can drift from full compiler semantics or reuse stale artifacts | Accept a compiler artifact, project typed convergence fields, and reuse only persisted artifacts whose identity matches the existing root |
| Legacy graph-contract helpers | Helpers can preserve alternate `graph.v2` authority after callers migrate | Delete or reduce to parser/metadata compatibility shims covered by the raw-consumer allowlist |
| Dashboard generated types | UI can infer graph state from legacy metadata | Use API-projected typed fields and diagnostics |

<!-- REVIEW: added per caller-migration-executable -->

Executable call-site migration:

| Current file/helper | Target API | Required tests |
|---|---|---|
| `internal/formula/compile.go`: `isGraphWorkflow`, `declaresGraphV2Contract`, package-global `formulaV2Enabled` | `NormalizeRequirements`, `CheckRequirements`, and `CompileWithResult` with explicit `HostCapabilities` | `TestCompileWithResultHostCapabilitiesArePerCall`, concurrent enabled/disabled host tests |
| `internal/formula/types.go`: `requiresExplicitGraphContract`, `metadataRequiresGraphContract` | V2-only construct registry that returns source formula/path/key diagnostics | Parser matrix tests for every registry entry and nested location |
| `cmd/gc/cmd_formula.go`: `newFormulaShowCmd` | `CompileWithResult` in preview mode; no accepted artifact because it never writes | golden show diagnostics with required vars preserved |
| `cmd/gc/cmd_formula.go`: `newFormulaCookCmd` root and `--attach` branches | `CompileWithResult` -> `AcceptCompileResult` -> `molecule.CookAccepted` or `molecule.AttachAccepted` | root and attach tests prove disabled host writes no root, child, dependency, hook, or metadata |
| `internal/molecule.Cook`, `CookOn`, `Attach`, graph-apply build path | Accept `AcceptedCompileArtifact`; stamp recipe/root metadata from artifact requirements and provenance | Cook/attach/graph-apply tests assert canonical keys plus legacy key during alias window and compile-artifact refs when metadata is too large |
| `internal/sling.isGraphSlingFormula`, `validateSlingFormulaRuntimeVars`, `AttachFormula`, `LaunchFormula` | One preflight returning `AcceptedCompileArtifact`; use `GraphWorkflow` and shared diagnostics for conflict and runtime-var paths | CLI and `internal/sling` tests for enabled, disabled, force replacement, and no partial root on unsatisfied host |
| `cmd/gc/cmd_sling.go` graph decoration | Use accepted artifact provenance and canonical workflow-root metadata before route decoration | Existing graph sling tests updated to assert canonical metadata and no raw-contract branch |
| `cmd/gc/cmd_order.go` and `cmd/gc/order_dispatch.go` formula order cook | Preflight and accept artifact; emit one registered failure event on fatal diagnostics; continue later orders | Order tests for unsatisfied host, deprecation warning suppression, and successful later order |
| `cmd/gc` or internal helpers invoking `bd` for formula behavior | Remove from runtime authorization path; keep only explicit release-validation probes with typed native parity | static guard fails on durable writer paths that consume shell-out output; probe tests are non-interactive and validation-only |
| retry, `on_complete`, Ralph continuation, and workflow-control generated steps | `CompileWithResult` -> `AcceptCompileResult` before retry-run, continuation, or generated-control metadata writes | continuation tests prove disabled host writes no retry-run bead, hook, or continuation metadata |
| `internal/dispatch/fanout.go`: `CompileExpansionFragment` and any continuation fanout helper | Accept a parent artifact and compile/accept fragment formulas through the same host snapshot before durable fanout writes | Fanout tests prove disabled host capability creates zero child beads, convoys, continuation beads, or metadata |
| `internal/molecule.Instantiate` and fragment instantiation helpers | Accepted root/fragment wrappers that embed `AcceptedCompileArtifact` | instantiate tests prove raw `Recipe` and caller-built metadata cannot authorize root, child, dependency, or hook writes |
| `cmd/gc/cmd_convoy_dispatch.go` source-workflow scans | Use `internal/sourceworkflow.IsWorkflowRoot` backed by canonical metadata first, legacy fallback second | Convoy tests with canonical-only, legacy-only, dual-stamped, and mixed-store roots |
| `internal/sourceworkflow.IsWorkflowRoot` and `ListLiveRoots` | Keep `internal/sourceworkflow` as the sole persistence predicate that calls formula metadata helpers | Predicate parity tests shared by sling, convoy, API, and dispatch |
| `internal/graphroute.IsCompiledGraphWorkflow` | Read `Recipe.GraphWorkflow` or `CompileResult.GraphWorkflow`, not root metadata strings | Graph route tests prove metadata changes do not affect compiled result semantics |
| `internal/api/handler_sling.go` and `handler_formulas.go` | Project `Diagnostic` into Huma response structs; never parse stderr/error strings | API tests for HTTP 400 diagnostic payloads and generated OpenAPI/type updates |
| `internal/api/orders_feed.go`, `handler_convoy_dispatch.go`, and workflow-root read-model helpers | Use shared workflow-root predicate for closed and open roots | API projection tests for canonical-only roots and legacy fallback; static guard against direct metadata filters |
| `internal/convergence/create`, retry, next-iteration, speculative wisp adapters | Preflight and accept artifact before any convergence bead/wisp write; keep convergence-only validation as post-compile domain checks | Convergence tests for disabled host capability with zero durable writes and artifact reuse after host downgrade |
| Dashboard generated types and state | Consume typed API diagnostics and workflow requirement fields | Dashboard tests for diagnostic rendering without metadata/string inference |

The migration is not complete while any non-test production code outside
`internal/formula` or the shared persistence predicate branches on
`Contract`, `declaresGraphV2Contract`, `Requires.FormulaCompiler`, or
`gc.formula_contract`.

<!-- REVIEW: added per attempt-78-no-bypass-gate -->

The transitional no-bypass gate blocks requires-only graph formulas until every
durable writer is behind compiler acceptance. A first-party source PR may add
`[requires] formula_compiler = ">=2"` during the alias window, but it may not
remove `contract = "graph.v2"` or distribute requires-only graph formulas while
any row below is not proven:

| Gate row | Blocking proof |
|---|---|
| `Store.MolCook` and `Store.MolCookOn` | deleted, unexported behind accepted-artifact wrappers, or static-guarded so they cannot authorize requires-only writes |
| bd-backed materialization | native compile/accept runs before any `bd` validation probe; probe output cannot create roots or children |
| exec-store `mol-cook` / `mol-cook-on` | validation-only compatibility path or removed script operation; no script result authorizes durable writes |
| convergence transition | shadow compile with `CompileWithResult` before every legacy subset-parser write until `ProjectAcceptedFormula` is the only durable path |
| fanout and order dispatch | all selected parent/fragment/order formulas accepted before fired metadata, fanout state, convoy, or child writes |
| API sling and dashboard/generated-client projection | Huma handlers project typed diagnostics and generated clients consume those fields; no metadata/string inference |
| raw TOML and metadata consumers | raw-consumer allowlist contains only parser, compatibility writer, shared predicate, tests, fixtures, and docs |

The gate is checked by `TestRequiresOnlyGraphFormulasCannotBypassAcceptance`.
The test loads one requires-only graph fixture and drives CLI launch, API
launch, formula order dispatch, convergence create/retry, fanout fragment
expansion, and dashboard preview. Rejected cases assert zero protected writes;
preview cases assert typed diagnostics only. The first-party inventory and
minimum-floor report must remain blocking while this test is absent or failing.

<!-- REVIEW: added per attempt-54-durable-writer-end-states -->

Every caller-manifest row has exactly one end state:

| End state | Meaning | Allowed examples |
|---|---|---|
| `accepted_artifact_only` | production path can write durable state only after `AcceptedCompileArtifact` validation | molecule cook/attach, sling launch, formula orders, fanout, convergence create/retry, retry/on-complete, workflow-finalize |
| `unexported_internal_helper` | helper remains only behind an accepted-artifact entry point and is not callable from CLI/API/controller packages | molecule child materialization internals and graph-control metadata writers |
| `preview_only` | helper can inspect or project diagnostics but cannot open a bead store or write metadata | `gc formula show`, formula preview API, dashboard preview state |
| `test_only` | helper exists only in `_test.go` or `internal/formula/testonly` | accepted-artifact constructors and normalized-requirement builders |
| `compatibility_writer` | helper writes legacy alias metadata only from an accepted artifact during the alias window | dual root metadata writer and legacy `gc.formula_contract` stamp |
| `deleted` | raw helper or subset parser is removed with its manifest row | legacy graph-contract predicates after caller migration |
| `temporary_delegator` | compatibility shim delegates one call deep to the compiler-owned API and has an expiry phase | convergence preview shim before deletion |

Durable-writer inventory is per symbol, not per subsystem:

| Symbol or family | End state | Blocking assertion |
|---|---|---|
| `internal/molecule.Cook`, `CookOn`, `Attach`, `Instantiate` | `accepted_artifact_only` public wrapper plus `unexported_internal_helper` internals | zero roots, children, dependencies, hooks, convoys, and metadata on fatal diagnostics |
| sling launch and attach helpers | `accepted_artifact_only` | no partial root, hook, convoy, or route metadata when host is unsatisfied |
| formula-backed order dispatch | `accepted_artifact_only` | order remains unfired and no wisp/root is written on fatal diagnostics |
| retry, Ralph, scope-check, and workflow-finalize writers | `accepted_artifact_only` for mutation, `preview_only` for status | same-identity artifact validation before retry-run, check, finalizer, dependency, or hook writes |
| fanout fragment expansion | `accepted_artifact_only` for parent and every fragment | all fragments accepted before first fragment child, convoy, or continuation write |
| convergence create/retry/next-iteration/missing-child/speculative wisp | `accepted_artifact_only` | convergence state, iteration, replacement child, retry metadata, and wisp writes remain unchanged on preflight failure |
| API formula/sling preview endpoints | `preview_only` | Huma response can return typed diagnostics but cannot create beads |
| compatibility root metadata dual-stamper | `compatibility_writer` | accepts only compiler-owned artifact data and expires at alias removal |
| legacy raw graph-contract helpers | `deleted` or `temporary_delegator` | static guard proves no production behavior branches on raw values outside owners |

Legacy helper retirement is explicit:

| Helper | Decision | Expiry |
|---|---|---|
| `declaresGraphV2Contract` | delete; replacement is `NormalizeRequirements` plus construct registry | caller sub-phase 4a |
| `requiresExplicitGraphContract` | delete; replacement is registry-driven missing-requirement diagnostics | caller sub-phase 4a |
| `metadataRequiresGraphContract` | delete or move into shared workflow-root predicate as legacy metadata fallback only | caller sub-phase 4e |
| `isGraphWorkflow` / `isGraphSlingFormula` | replace with `CompileResult.GraphWorkflow` or `WorkflowRootFacts.Kind` | caller sub-phases 4a-4b |
| `formulaV2Enabled`, `IsFormulaV2Enabled`, `SetFormulaV2Enabled` | edge-only shims that build `HostCapabilities`; no reads inside `internal/formula` | removed after CLI/API/controller callers pass explicit host capability |
| convergence subset wrappers | one-call `temporary_delegator` to compiler projection for preview, then `deleted` | caller sub-phase 4f |

<!-- REVIEW: added per caller-manifest-grep-derived -->

The first implementation PR must commit a grep-derived manifest at
`engdocs/release/formula-compiler-caller-manifest.md` or a machine-readable
equivalent under `docs/release/`. It is seeded from the current tree with
targeted searches for `CompileWithoutRuntimeVarValidation`,
`CompileExpansionFragment`, `SetFormulaV2Enabled`, `IsFormulaV2Enabled`,
`gc.formula_contract`, `contract = "graph.v2"`, `ValidateForConvergence`,
`molecule.Cook`, `molecule.Attach`, `molecule.Instantiate`, `GraphWorkflow`,
`gc.fanout_state`, `gc.continuation_group`, `gc.kind`, `exec.Command("bd"`,
API formula/global projections, legacy graph-contract helpers, and
workflow-root metadata filters. Each row names the
owner package, current helper, durable-write
ability, migration target, and blocking test. At minimum it contains these
rows:

| Row | Current location | Durable write? | Migration owner/test |
|---|---|---|---|
| formula show preview | `cmd/gc/cmd_formula.go:newFormulaShowCmd` | no | CLI golden diagnostics and no-store-open test |
| formula cook root | `cmd/gc/cmd_formula.go:newFormulaCookCmd`, `internal/molecule.Cook` | yes | accepted-artifact root cook zero-write test |
| formula cook attach | `cmd/gc/cmd_formula.go:newFormulaCookCmd --attach`, `internal/molecule.Attach` | yes | accepted-artifact attach zero-write dependency/hook test |
| sling launch | `internal/sling.InstantiateSlingFormula`, `cmd/gc/cmd_sling.go` | yes | no partial root/child/hook on unsatisfied host |
| graph attachment predicate | `internal/sling.IsGraphWorkflowAttachment` | reads durable metadata | shared `internal/sourceworkflow` predicate parity test |
| order cook | `cmd/gc/cmd_order.go`, `cmd/gc/order_dispatch.go` | yes | grouped event and successful-later-order tests |
| durable shell-out bridge | any `bd` or legacy formula probe invocation reachable from launch/order/fanout/convergence | yes if it authorizes writes | validation-only probe or deletion plus static guard |
| retry and on-complete continuation | retry/on-complete helpers and generated continuation metadata writers | yes | accepted-artifact continuation zero-write tests |
| Ralph/control continuation | Ralph continuation and generated `gc.kind` control writers | yes | workflow-control registry and generated metadata tests |
| fanout fragment | `internal/formula.CompileExpansionFragment`, `internal/dispatch/fanout.go` | yes | compile all fragments before first fragment write |
| molecule instantiate | `internal/molecule.Instantiate` and fragment instantiation helpers | yes | accepted-artifact instantiate tests |
| convergence subset | `internal/convergence/formula.go:ValidateForConvergence` and create/retry callers | yes | projection and artifact-reuse tests |
| API sling/formulas | `internal/api/handler_sling.go`, `handler_formulas.go` | launch yes, preview no | typed Huma diagnostic and OpenAPI in-sync tests |
| API globals and graph helpers | API global projection helpers and legacy graph-contract helpers | reads durable metadata | typed facts projection tests and static guard |
| API workflow roots | `internal/api/orders_feed.go`, `handler_convoy_dispatch.go`, workflow-root read models | reads durable metadata | shared predicate static guard and projection fixtures |
| convoy/source workflow | `cmd/gc/cmd_convoy_dispatch.go` | reads durable metadata | canonical-only/legacy-only/dual-stamped selector test |
| dashboard state | generated TS and dashboard panels | no direct write | generated-client compile and no-metadata-inference tests |

<!-- REVIEW: added per DR53-per-occurrence-caller-manifest -->

The manifest is per occurrence, not per file. One Go file can contribute
several rows when it contains multiple predicates, metadata filters, durable
write sites, or helper calls. Each row records:

```yaml
- id: cmd-gc-order-dispatch-filter-001
  file: cmd/gc/order_dispatch.go
  line_pattern: 'gc.formula_contract'
  occurrence_kind: metadata_filter
  current_behavior: selects graph formula-backed orders
  durable_write_boundary: order fired metadata and wisp root creation
  end_state: accepted_artifact_only
  replacement_api: internal/sourceworkflow.ListWorkflowRoots
  owner: cmd/gc
  expiry_phase: 4c
  rollback_control: disable formula-backed order dispatch before fired metadata or wisp writes
  blocking_test: TestOrderDispatchUsesListWorkflowRoots
```

The generator fails when two raw predicate sites in one file collapse into one
manifest row or when a row lacks `line_pattern`, `occurrence_kind`,
`durable_write_boundary`, `end_state`, `replacement_api`, owner, expiry phase,
`rollback_control`, and blocking test. CI compares the raw-consumer scan to the
manifest after every caller migration sub-phase. A raw predicate/filter site
may be deleted only when the same change removes its manifest row and adds or
updates the replacement test.

The caller manifest is a Phase 0 acceptance artifact for the migration, not a
later bookkeeping report. Before any production caller moves to accepted
artifacts, the repository must contain a generated manifest row for each
occurrence that can read formula requirement metadata, call a compile helper,
authorize a durable write, invoke a runner-based `bd` probe, or project
workflow-root state. "Each occurrence" means sling preflight and sling
instantiation are separate rows; graph apply, molecule cook, molecule attach,
order dispatch, convergence create, convergence retry, fanout fragment
expansion, retry, `on_complete`, workflow-finalize, scope-check repair,
missing-child repair, API projection, dashboard projection, and convoy scans
are all separate rows even when they share a file.

The manifest generator must record one durable-write boundary per row:
`root`, `child`, `dependency`, `hook`, `convoy`, `tracking`, `retry_metadata`,
`fanout_state`, `convergence_state`, `workflow_root_metadata`,
`fired_order_metadata`, or `artifact_ref`. A zero-write fixture for a row must
snapshot every boundary the row can touch and prove a fatal compile diagnostic
changes none of them.

The manifest is updated in every caller-migration PR. A row can be removed only
when the static guard proves no production code path remains for that raw
consumer and the removal commit names the replacement test.

CI must include a static guard that fails on new behavioral uses of raw
`contract = "graph.v2"`, `declaresGraphV2Contract`, `Requires.FormulaCompiler`,
or `gc.formula_contract` outside the parser, compatibility metadata writer,
shared workflow-root predicate, tests, fixtures, and docs.

<!-- REVIEW: added per attempt-80-repository-wide-caller-inventory -->

The caller manifest is repository-wide. Its generator scans Go production
code, first-party packs, examples, tutorials, generated prompt templates, and
workflow-pack prompt snippets before any requires-only formula is distributed.
The mandatory CI scan inputs are tracked or generated-from-tracked sources:
`internal/graphroute`, `internal/sling`, `internal/api`,
`internal/dispatch`, `internal/molecule`, `internal/convergence`, `cmd/gc`,
`internal/bootstrap/packs`, `examples`, `docs/tutorials`, `docs/reference`,
workflow prompt template source directories, checked-in generated prompt
fixtures, and formula snippets embedded in Markdown. Omitting one of those
tracked inputs is a schema error, not an empty result.

Ignored runtime materialization under `.gc/system/packs` is not a mandatory CI
input. CI may cover materialized workflow-pack prompts only through a
deterministic fixture step such as
`gc workflows fixtures materialize-system-packs --from-tracked-sources --out internal/formula/testdata/generated/system-packs-manifest.json`.
That step must run in a temporary city, record the command, source revisions,
content hashes, generated file list, and digest, and commit the manifest or
golden prompts it produces. Local scans of `.gc/system/packs` are optional
operator/release evidence and must be labeled `runtime_observation`; absence
of that ignored directory in a clean checkout is not a schema failure.

Prompt and formula command references are producer candidates even when no Go
code shells out to the command. The manifest must classify every first-party
prompt, formula, tutorial, or example that can instruct an agent to run
`gc bd mol wisp`, `gc bd mol burn`, `gc bd mol cook`, `gc bd mol cook-on`, or
any equivalent molecule/materialization command. Each row chooses exactly one
outcome before first-party requires-only distribution:

| Outcome | Meaning |
|---|---|
| `rewritten_native` | prompt/formula now routes through a native compile/accept producer before durable writes |
| `blocked_until_phase` | command remains documented only for legacy dual-declared formulas and the inventory names the blocking phase |
| `validation_only` | command can inspect or report but is fixture-proven unable to authorize roots, children, hooks, convoys, retry, fanout, or convergence writes |
| `deleted` | prompt/formula command reference is removed with a deletion test that proves no generated prompt still emits it |
| `legacy_classified` | retained only for old dual-declared roots with an owner, expiry phase, and explicit unsupported requires-only diagnostic |

Production guards are also repository-wide. `TestNoNewFormulaRawConsumers`
and `TestDurableWritersRequireAcceptedArtifact` run against every production
Go package, generated command helper, and first-party prompt/formula command
inventory row. Allowlist entries are narrow: they name the file or prompt
source, line pattern, symbol or command, owner package, expiry phase,
replacement API, and blocking test. A package-level or directory-level
allowlist is invalid because it can hide a second raw consumer in the same
file.

Retired helper deletion tests are mandatory. Each deleted helper or prompt
command reference has a paired test such as
`TestDeclaresGraphV2ContractDeleted`, `TestValidateForConvergenceNotImported`,
`TestNoLegacyMolProducerPromptReferences`, or
`TestNoBDMolRuntimeAuthorization`. The tests fail if a replacement helper with
the same behavior appears under a new name, if a prompt can still emit the
legacy producer command, or if a generated first-party formula can still reach
a raw `Recipe`/`CompileResult` durable write path.

<!-- REVIEW: added per compiler-authority-enforcement -->

The guard starts from a checked-in allowlist, not from an aspirational grep
comment. The initial migration commit creates
`internal/formula/testdata/raw_consumer_allowlist.yaml` with this shape:

```yaml
schema_version: 1
allowed:
  - package: internal/formula
    file: internal/formula/compile.go
    symbols: [NormalizeRequirements, writeCompatibilityMetadata]
    reason: parser and alias writer own raw compatibility fields
  - package: internal/sourceworkflow
    file: internal/sourceworkflow/predicate.go
    symbols: [IsWorkflowRootMetadata, ClassifyWorkflowRoot]
    line_patterns: ["gc.formula_contract", "gc.formula_compiler_requirement"]
    reason: shared predicate owns legacy metadata fallback during alias window
  - package: internal/sourceworkflow
    file: internal/sourceworkflow/list.go
    symbols: [ListWorkflowRoots]
    line_patterns: ["ClassifyWorkflowRoot"]
    reason: shared predicate owns legacy metadata fallback during alias window
```

`go test ./internal/formula ./internal/sourceworkflow ./cmd/gc` runs
`TestNoNewFormulaRawConsumers`, which scans production Go files for raw
`Contract`, `declaresGraphV2Contract`, `Requires.FormulaCompiler`,
`Version`, `gc.formula_contract`, `gc.formula_compiler_requirement`, and
`gc.formula_compiler_capability` reads. Additions fail unless the same change
updates this allowlist with an exact file, symbol, line pattern, owner, expiry
condition, and test that proves the read is parser, compatibility-writer, or
shared-predicate logic. Globs, package-wide rows, and directory-wide rows are
invalid unless every matched file is enumerated in the generated expansion and
each match has its own exact symbol plus line pattern. The allowlist is
report-only only in rollout phase 1; it becomes
blocking before any durable producer switches to accepted compile artifacts.
From caller sub-phase 4a onward, once `CompileResult`, `AcceptedCompileArtifact`, and the
shared workflow-root predicate exist, the guard blocks new production raw
consumers. Existing consumers may remain only in the checked allowlist, and each
allowlist row must name an owner, expiry phase, and replacement test. Every
later caller sub-phase must remove or narrow at least the rows it owns before it
can be considered complete.

Durable writer APIs must make the accepted compile identity impossible to
skip. New or modified APIs that can write roots, children, wisps, dependencies,
convoys, hooks, retry metadata, continuation metadata, or convergence state may
not accept a bare `*formula.Recipe`, `*formula.CompileResult`, or caller-built
`NormalizedRequirements` as proof of acceptance. They must accept
`formula.AcceptedCompileArtifact` or an entry-point-specific struct that embeds
that artifact and records the operation-specific write intent. Tests must prove
a caller cannot construct durable metadata from raw `contract`, raw
`[requires]`, legacy `version`, `gc.formula_*` metadata, global formula flags,
or a preview-only `CompileResult` without going through the compiler-owned
acceptance function.

<!-- REVIEW: added per attempt-90-durable-writer-authority -->

Accepted artifacts are the only authority at durable writer boundaries. This
is enforced from the repository-wide caller manifest, not by manually auditing
the current Go signatures. For every manifest row whose
`durable_boundaries[]` is non-empty, `TestAllDurableWritersRequireAcceptedArtifact`
loads the row, resolves the Go symbol or prompt/generated-command producer,
and proves the replacement entry point receives one of:

- `formula.AcceptedCompileArtifact`
- an operation wrapper that embeds `formula.AcceptedCompileArtifact`
- a parent/fragment pair whose fields both embed accepted artifacts
- an explicit `preview_only` row that cannot open a bead store or mutate
  metadata

The same test fails when a production durable path accepts or consumes any of
these as authorization: raw `Recipe`, raw `CompileResult`, caller-built
metadata maps, root metadata strings, `bd` shell-out output, dashboard
TypeScript projection data, generated prompt text, tutorial/example commands,
or compatibility probe output. A compatibility probe may still produce release
evidence, but it cannot be in the call graph before root, child, dependency,
hook, convoy, retry, fanout, convergence, workflow-root, order-fired, or
artifact-ref writes.

Multi-write entries have all-or-nothing fixtures:

| Entry | Boundaries snapshotted before fatal diagnostic |
|---|---|
| root cook / wisp launch | root, child, dependency, hook, convoy, workflow-root metadata, artifact ref |
| attach / `on_complete` | attached child, dependency, hook, attachment metadata, continuation metadata |
| order dispatch | fired metadata, order-run/tracking bead, wisp root, child, hook, convoy, route metadata, last-run/cooldown history |
| retry/Ralph/scope-check/finalize | retry metadata, retry-run/check/finalizer bead, dependency, hook, outcome metadata |
| fanout | parent fanout state, every fragment child, convoy link, continuation metadata |
| convergence | root, iteration, retry metadata, replacement child, missing-child state, pending wisp, hook, dependency, convoy |
| repair command | immutable artifact payload, root artifact-ref metadata, migration metadata, and every graph-control boundary |

Every zero-write fixture includes a store snapshot before and after, plus a
positive accepted-artifact row proving the same path still writes when the
artifact is valid. A producer that writes several boundaries in one operation
cannot claim coverage from a single root-only assertion.

<!-- REVIEW: added per attempt-50-durable-write-guards -->

This is enforced by blocking tests, not code-review convention:

| Guard | Fails when | Owner |
|---|---|---|
| `TestNoRecipeGraphWorkflowSynthesis` | production code outside `internal/formula` assigns `Recipe.GraphWorkflow` or derives it from raw metadata | `internal/formula` |
| `TestDurableWritersRequireAcceptedArtifact` | a function that can write roots, children, dependencies, hooks, convoys, retry metadata, fanout state, or convergence state accepts only `*Recipe`, `*CompileResult`, or caller-built metadata | `internal/molecule` with per-package fixtures |
| `TestNoRawFormulaMetadataReadsOutsideOwners` | production code outside `internal/formula` and `internal/sourceworkflow` reads `gc.formula_*`, raw `contract`, or raw `[requires]` for behavior | `internal/formula` |
| `TestNoConvergenceSubsetParserUse` | production convergence code imports the legacy subset parser, reopens formula TOML, or reconstructs required vars outside the projection | `internal/convergence` |
| `TestRawConsumerAllowlistExpiry` | an allowlist row has no owner, replacement test, and rollout-phase expiry, or survives after its owning sub-phase is complete | `internal/formula` |

The first implementation PR seeds these guards in report mode with the current
manifest. Caller sub-phase 4a turns new violations blocking; each caller-migration
sub-phase must remove or narrow the rows it owns before completion. A PR that
adds a durable writer or graph-control mutator must add its accepted-artifact
signature and zero-write fixture in the same change.

Durable preflight contract:

| Entry point | Must complete before first durable write | Writes forbidden on fatal diagnostic |
|---|---|---|
| Root molecule or wisp launch | `CompileWithResult` -> `AcceptCompileResult` -> `ValidateAcceptedArtifact` | root bead, child beads, root metadata, convoy, hook |
| Attached molecule | compile/accept attached formula with current host capability and validate artifact against attach intent | attached child bead, dependency, hook, attachment metadata |
| Formula-backed order | compile/accept selected formula before marking the order fired | wisp root, child beads, order fired metadata |
| Retry or `on_complete` that starts a formula | compile/accept target formula before retry/on-complete state mutation | retry-run bead, attached molecule, continuation metadata |
| Fanout fragment | compile/accept every fragment formula in the fanout transaction and validate every artifact before fragment expansion | fragment child beads, convoy links, `gc.fanout_state`, continuation group metadata |
| Convergence create/retry/speculative wisp | compile/accept before convergence-specific validation writes | convergence root, iteration bead, missing-child state, retry metadata |

No production path may accept a pre-parsed `NormalizedRequirements` argument
from a caller as proof of satisfaction. Passing a full `CompileResult` is
allowed only between formula package helpers and `AcceptCompileResult`; durable
writers must receive the accepted artifact.

Fanout preflight is transactional. For an existing same-identity graph root,
fanout first loads and validates the persisted parent accepted artifact; it
does not recompile the parent merely because a fanout operation is pending.
If the parent formula identity, vars, options, search paths, pack binding,
projection snapshot, or content hash changed, the parent is a new compile and
must satisfy the current host capability before fanout writes anything. Every
new or changed selected fragment is then compiled and accepted using the same
host-capability snapshot and vars/options identity for this fanout
transaction. Only after the parent artifact and every fragment artifact
validate may the dispatcher mutate `gc.fanout_state`, create fragment
children, link convoys, or write continuation metadata. If any parent or
fragment validation fails, the whole fanout returns the shared diagnostic and
leaves the store unchanged. A zero-write fixture covers an artifact-stamped
parent with pending fanout after `[daemon] formula_v2` is disabled: the parent
artifact is reused, unchanged fragments continue, and any new fragment compile
fails without child, convoy, continuation, or fanout-state writes.

<!-- REVIEW: added per DR48-active-root-artifact-gate -->

Active graph-control roots have the same accepted-artifact requirement as new
launches. Any path that mutates graph-specific durable state for an existing
root must first load the root's accepted artifact, validate it against the
operation intent, and use only compiler-owned projections from that artifact.
Runtime mutators do not auto-stamp artifacts onto legacy-only roots. If a root
is legacy-only and has no artifact reference, graph-specific mutation returns a
typed diagnostic that names the required `gc formula repair-root-artifact`
command. If the source is missing, the host is downgraded, the accepted
identity does not match, or projection validation fails, the operation fails
closed and writes nothing.

The active-root rule covers every graph-control appender and repair path:

| Path | Required artifact source | Writes forbidden until validation passes |
|---|---|---|
| retry control and retry-run creation | root artifact or legacy-root migration compile | retry metadata, retry-run bead, retry-eval bead, dependency, hook |
| Ralph retry and next iteration | root artifact plus Ralph step projection | run bead, check bead, attempt metadata, scoped child, hook |
| scope-check, workflow-finalize, and check controls | root artifact and registered workflow-control metadata rows | outcome metadata, generated child, dependency, hook, finalizer state |
| fanout expansion and fanout repair | parent artifact plus every fragment artifact in the same transaction | `gc.fanout_state`, fragment children, convoy links, continuation metadata |
| `on_complete` and continuation formulas | parent artifact plus accepted target formula artifact | continuation bead, attached molecule, hook, dependency |
| missing-child repair | root artifact and compiler projection for the missing child | replacement child, dependency, hook, repair marker |
| convergence retry, next iteration, missing-child repair, and speculative wisp | persisted convergence artifact or accepted recompile when identity changes | iteration bead, retry metadata, missing-child state, speculative root, child |
| order dispatch for a formula root | accepted selected formula artifact | fired metadata, wisp root, child beads, convoy, hook |

<!-- REVIEW: added per attempt-73-active-root-repair-modes -->

Every active-root caller has exactly one repair mode:

| Caller path | Legacy-only source present and host enabled | Source missing, host downgraded, unsupported future schema, or conflicting artifact |
|---|---|---|
| retry control and retry-run creation | `operator_repair_required` | `read_only_fail_closed` |
| Ralph retry and next iteration | `operator_repair_required` | `read_only_fail_closed` |
| scope-check, workflow-finalize, and check controls | `operator_repair_required` | `read_only_fail_closed` |
| fanout expansion and fanout repair | `operator_repair_required` | `read_only_fail_closed` |
| `on_complete` and continuation formulas | `operator_repair_required` for parent root; new target formula still compiles against current host | `read_only_fail_closed` |
| missing-child repair | `operator_repair_required` | `read_only_fail_closed` |
| convergence retry, next iteration, missing-child repair, and speculative wisp | `operator_repair_required` | `read_only_fail_closed` |
| order dispatch for an existing formula root | `operator_repair_required` before graph-specific mutation | `read_only_fail_closed` |
| `gc formula repair-root-artifact` | command-owned repair after operator invocation; not automatic runtime repair | `read_only_fail_closed` with typed diagnostic |

`auto_repair` is intentionally unused by runtime callers in v0. Introducing it
later requires a separate design because it would let background producers
modify root artifact authority without an operator action.

The zero-write assertion is literal. A failing active-root preflight leaves no
new bead, no changed metadata, no new dependency edge, no hook mutation, no
convoy link, no cached partial artifact, and no generated control child. Tests
must snapshot the store before each failing case and compare beads,
dependencies, hooks, convoys, and graph-control metadata after the operation.

<!-- REVIEW: added per durable-artifact-identity-v21 -->

When a durable producer accepts a compiler-minted artifact before writing
beads, the accepted artifact identity is part of the contract:

```go
type AcceptedArtifactIdentity struct {
    CompileID                  string
    FormulaName                string
    SourceIdentity             string
    ContentHash                string
    HostCapabilities           HostCapabilities
    SearchPathsHash            string
    OptionsHash                string
    VarsHash                   string
    BindingIdentityHash        string
    ArtifactVersion            int
    RequirementsSchemaVersion  int
    ProjectionSnapshotVersion  int
    MinimumReaderCapability    CompilerCapability
}

type AcceptedCompileArtifact struct {
    identity                   AcceptedArtifactIdentity
    artifactVersion            int
    requirementsSchemaVersion  int
    projectionSnapshotVersion  int
    compileID                  string
    formulaName                string
    hostCapabilities           HostCapabilities
    searchPathsHash            string
    optionsHash                string
    varsHash                   string
    bindingIdentityHash        string
    contentHash                string
    provenance                 Provenance
    requirements               NormalizedRequirements
    diagnostics                []Diagnostic
    projection                 AcceptedProjectionSnapshot
    minimumReaderCapability    CompilerCapability
    createdBy                  string
    createdAt                  time.Time
    proof                      acceptedCompileProof
}
```

<!-- REVIEW: added per DR33-accepted-artifact-proof -->

The unexported `proof` field is intentional and is not a marker-only field:

```go
type acceptedCompileProof struct {
    nonce        [16]byte
    identityHash string
    accepted     bool
}
```

The proof identity hash is byte-for-byte deterministic. `AcceptCompileResult`
constructs a canonical length-prefixed binary record with these fields in this
order: proof nonce, artifact version, requirements schema version, projection
snapshot version, write-intent schema version, formula name, source identity,
content hash, host compiler capability, host source kind, config generation,
search paths hash, options hash, vars hash, binding identity hash, provenance
identity hash, minimum reader capability, and the SHA-256 of the accepted
projection snapshot. Strings are UTF-8 bytes prefixed by unsigned varint
lengths; integers are fixed-width big-endian; absent optional fields encode as
zero length. The hash is `sha256("gc.accepted-formula-artifact.v1" || record)`
lowercase hex. JSON, Go struct formatting, map iteration order, timestamps,
absolute cache paths, and diagnostic display strings are never hash inputs.

Code outside `internal/formula` cannot construct a non-zero accepted proof, and
the zero value of `AcceptedCompileArtifact` is invalid. Durable writer entry
points must call the formula-owned validation method before writing:

```go
type CompileWriteIntent struct {
    DurableProducer           string
    Operation                 string
    CurrentHostCapabilities   HostCapabilities
    Identity                  AcceptedArtifactIdentity
    RootArtifactRef           string
    ExistingRootIdentityHash  string
    SameIdentityReuse         bool
}

func ValidateAcceptedArtifact(artifact AcceptedCompileArtifact, intent CompileWriteIntent) []Diagnostic
```

`ValidateAcceptedArtifact` fails closed when `proof.accepted` is false,
the compile ID is empty, the proof identity hash does not match the stored
identity fields, any fatal diagnostic is present, or the write intent disagrees
with formula/source identity, search paths, compile options, vars, content
hash, binding identity, artifact version, requirements schema version,
projection snapshot version, or provenance. New or changed formula operations
also require the current host capability to satisfy the artifact's normalized
requirements. Same-identity reuse for an already-created artifact-stamped root
is the only exception: a later disabled host does not invalidate the persisted
artifact when every non-host identity field still matches the root and the
operation uses only the accepted projection snapshot.

Callers may store and reload accepted artifacts only through
formula-owned serialization that revalidates the proof identity against the
persisted payload. They may not reconstruct an artifact from root metadata or
a `CompileResult`.

The only exported readers on the artifact return copied values; no caller can
mutate requirements, diagnostics, provenance, or identity after acceptance.

Artifact identity is byte-level and versioned:

| Field | Hash input / rule |
|---|---|
| `compileID` | SHA-256 over artifact version, requirements schema version, formula name, source identity, content hash, host capability, search paths hash, options hash, vars hash, and provenance identity |
| `SearchPathsHash` | ordered formula search path after pack/import resolution, including binding names, layer priorities, and winning/losing source identities; absolute cache staging paths are excluded |
| `OptionsHash` | compile options that can affect expansion, runtime-var validation, graph transforms, compatibility alias projection, diagnostic projection, and accepted artifact serialization |
| `VarsHash` | compile-time vars only, after typed redaction; secret values are omitted or HMAC-hashed by policy, runtime-only vars are not included |
| `ContentHash` | canonical resolved source bytes and transitive contribution manifest before runtime substitution and graph transforms |
| `HostCapabilities` | typed capability values plus source attribution and config generation |
| `Provenance` | resolver-owned source identity, pack/import binding identity, lockfile entry, requested ref/constraint, locked revision, dirty state, and content hash |
| `BindingIdentityHash` | author-editable pack/import binding graph, including binding names, parent bindings, lockfile keys, and winning/losing layer identities |
| `ProjectionSnapshotVersion` | accepted projection schema used by convergence, fanout, retry, and repair without source access |

A caller may submit a `CompileResult` for acceptance only when these identities
match the operation it is about to perform. Otherwise it must compile again
with current options before writing. Cross-version reuse is fail-closed:
readers may display older artifact versions, but no durable graph-specific
write may use an artifact whose `artifactVersion`,
`requirementsSchemaVersion`, `projectionSnapshotVersion`,
`minimumReaderCapability`, or identity hash is unsupported by the binary.

Production code cannot mint artifacts for tests. Test-only artifact
constructors live in `internal/formula/testonly` or `_test.go` files and are
excluded from production builds. A static guard fails when production code
outside `internal/formula` assigns `AcceptedCompileArtifact` fields directly,
constructs `acceptedCompileProof`, or deserializes artifact payloads without
calling the formula-owned loader.

Required negative tests:

| Case | Assertion |
|---|---|
| zero-value artifact passed to root cook, attach, order, fanout, or convergence writer | validation diagnostic and zero durable writes |
| caller-built struct or stale deserialized payload with no accepted proof | validation diagnostic and zero durable writes |
| accepted artifact with stale host capability, search paths, options, vars, or content hash | validation diagnostic and zero durable writes |
| accepted artifact whose provenance/content hash no longer matches the selected formula | validation diagnostic and zero durable writes |
| accepted artifact containing any fatal diagnostic | validation diagnostic and zero durable writes |
| host-disabled compile result submitted for acceptance | no artifact minted; durable writer cannot be called without failing validation |
| artifact version, requirements schema version, or minimum reader capability newer than this binary | display-only projection allowed, graph-specific writes fail closed with zero durable writes |

Retention semantics:

- Root, convergence, order-wisp, fanout, retry, and `on_complete` producers
  persist an accepted artifact whenever the full provenance or diagnostic set
  cannot fit in root metadata.
- The artifact is immutable and retained at least as long as the root bead or
  workflow history that references it.
- Garbage collection may delete an artifact only after no live or archived
  root metadata references `gc.formula_compile_artifact`.
- Reusing an artifact after a host downgrade is allowed only for already-created
  roots whose metadata references that artifact. Selecting or compiling a new
  formula after the downgrade must run `CompileWithResult` against the current
  host capability and can fail with zero writes.

<!-- REVIEW: added per attempt-54-host-downgrade-reuse-contract -->

Host-downgrade continuation is a narrow same-identity accepted-artifact reuse
mode, not a second way to satisfy requirements. It is available only when all
of these checks pass:

| Check | Required match |
|---|---|
| root reference | root metadata names the same accepted artifact ref being validated |
| formula/source identity | formula name, source identity, content hash, pack/import binding identity, and provenance identity match the accepted artifact |
| compile inputs | vars hash, options hash, search-path hash, and projection snapshot version match the accepted artifact |
| artifact support | artifact version, requirements schema version, projection snapshot version, and minimum reader capability are supported by this binary |
| diagnostics | accepted artifact contains no fatal diagnostics and its proof identity validates |
| operation scope | operation mutates only graph state already represented by the accepted projection snapshot |

The current host capability is still recorded in diagnostics and producer
state, but it is not compared to the artifact's original host capability in
same-identity reuse mode. Any operation that changes formula identity, vars,
options, source content, search paths, binding identity, or projection version
is a new compile and must satisfy the current host before writing.

Host-downgrade fixtures are required for root retry, workflow-finalize,
scope-check, fanout repair, convergence retry, missing-child repair, and
dashboard/API projection:

| Fixture | Expected result |
|---|---|
| artifact-stamped same identity with host disabled | graph-specific mutation allowed after artifact/projection validation |
| same root with changed vars/options/source/binding | current-host compile attempted and fails closed while disabled |
| unsupported artifact or projection schema | read-only visibility plus diagnostic; zero graph-specific writes |
| legacy-only root with source present and host disabled | observation only; repair command fails before artifact stamping |
| concurrent retry and create on same root | compare-and-swap permits one same-identity mutation; loser reloads and revalidates before writing |
| dashboard/API read during downgrade | root remains visible with original requirement, current host state, artifact support state, and remediation |

Entry-point wrappers are thin and explicit:

```go
type RootLaunchCompile struct {
    Accepted formula.AcceptedCompileArtifact
    RootKind  string
}

type FanoutFragmentCompile struct {
    Parent   formula.AcceptedCompileArtifact
    Fragment formula.AcceptedCompileArtifact
    TargetID string
}
```

The wrapper may add write-specific context, but it must not weaken the accepted
artifact contract or allow callers to substitute raw metadata for compiler
acceptance.

## Compatibility With `contract`

<!-- REVIEW: added per migration-compatibility-contract -->

`contract = "graph.v2"` is a deprecated compatibility alias for:

```toml
[requires]
formula_compiler = ">=2"
```

V0 behavior:

- If only `contract = "graph.v2"` is present, compile as
  `requires.formula_compiler = ">=2"` and emit `formula.contract_deprecated`.
- If the current released compiler accepts legacy source spellings such as
  surrounding whitespace or case variants of `graph.v2`, those spellings remain
  accepted deprecated aliases during the compatibility window and emit the same
  warning with the original source spelling preserved in diagnostics.
- If both are present and agree, compile and emit
  `formula.contract_deprecated`.
- If both are present and disagree, fail validation with
  `formula.compiler_requirement_conflict`.
- If `contract` has any value outside the checked compatibility alias set,
  fail validation.
- The warning must preserve the source spelling and point to the exact
  replacement:

```text
contract = "graph.v2" is deprecated; use [requires] formula_compiler = ">=2"
```

Compatibility matrix:

| Formula source | Old binary that only knows `contract` | New binary with `formula_v2 = false` | New binary with `formula_v2 = true` |
|---|---|---|---|
| No requirement and no v2-only constructs | Works | Works | Works |
| `contract = "graph.v2"` | Works when old graph support is enabled | Fails with `formula.compiler_requirement_unsatisfied` | Works with deprecation warning |
| Dual `contract` plus `[requires] formula_compiler = ">=2"` | Works because old binary reads `contract` | Fails with `formula.compiler_requirement_unsatisfied` | Works with deprecation warning |
| `[requires] formula_compiler = ">=2"` only | Not supported for v2 formulas; old binary can miss the requirement | Fails with `formula.compiler_requirement_unsatisfied` | Works |
| Unsupported future requirement such as `>=3` | Not supported | Fails with `formula.compiler_requirement_unsupported_future` | Fails with `formula.compiler_requirement_unsupported_future` |

<!-- REVIEW: added per attempt-78-compatibility-corpus-edges -->

The compatibility corpus fixture-locks exact source spellings:

| Source edge | Expected result |
|---|---|
| `contract = "graph.v2"` | accepted alias with `formula.contract_deprecated` |
| `contract = " graph.v2"`, `"graph.v2 "`, `"Graph.V2"`, `"GRAPH.V2"` when accepted by the current released compiler | accepted deprecated alias during the compatibility window; diagnostics preserve raw spelling and normalize only the requirement |
| `contract = "graph.v1"` or any other string | `formula.compiler_requirement_unsupported` |
| omitted `[requires]` and no v2-only construct | default capability |
| empty `[requires]` and no v2-only construct | default capability with empty-table provenance |
| explicit `[requires] formula_compiler = ">=1"` and no v2-only construct | default capability with `requires` provenance |
| omitted, empty, or `>=1` plus a v2-only construct | `formula.compiler_requirement_missing` |
| dual `contract = "graph.v2"` plus `>=2` | accepted as source `dual` with deprecation warning |
| dual `contract = "graph.v2"` plus omitted, empty, or `>=1` | `formula.compiler_requirement_conflict` |
| dual `contract = "graph.v2"` plus malformed or future requirement | requirement syntax/future diagnostic before host satisfaction |

Legacy root metadata is a different compatibility surface: the shared
workflow-root predicate may trim historical surrounding whitespace in
`gc.formula_contract`, but source formula alias parsing follows the checked
compatibility corpus. The matrix has separate rows for source alias parsing and
persisted root metadata fallback so the two rules cannot drift. Narrowing the
source alias set requires saved external-support evidence that no supported
first-party, external, SHA-pinned, transitive, or shadowed consumer still relies
on the spelling being removed.

Runtime compatibility decision:

The live production runtime uses the native Go formula compiler only. Sling,
API sling, formula-backed orders, convergence, fanout, controller ticks, and
dashboard/API preview paths must not shell out to `bd` to decide formula
requirements or graph-workflow state. The historical `bd`/`GC_NATIVE_FORMULA`
path is a migration compatibility probe only: it may run in CI or explicit
operator validation, but no durable runtime entry point may depend on it after
this design lands. If the probe is still present, first-party graph formulas
remain dual-declared until the probe is removed or proven to parse `[requires]`
identically.

<!-- REVIEW: added per compatibility-corpus-v21 -->

This design supersedes conflicting rollback language in
`engdocs/proposals/formula-migration.md`, especially any text that treats
`GC_NATIVE_FORMULA=false` as a production runtime fallback after requirement
normalization ships. That proposal must be updated or marked superseded in the
same PR stack that introduces user-visible `[requires]` diagnostics. The only
permitted rollback after user-visible diagnostics ship is to restore
dual-declared formula sources and dual root metadata, or to disable the new
producer path before it writes durable state.

Per-entry-point behavior is therefore fixed:

| Entry point | Old or legacy-only `contract` formula | Dual-declared formula | Requires-only formula | Host `formula_v2=false` |
|---|---|---|---|---|
| `gc sling --formula` and API sling | native compiler accepts alias and warns | native compiler accepts and warns | native compiler accepts if host satisfies | returns typed diagnostic before root/child/hook writes |
| formula-backed orders | native compiler accepts alias and records bounded warning | same | same | records one grouped order compile-failure event for the scan tick; no wisp or fired metadata |
| convergence create/retry/speculative wisp | native compiler accepts alias before convergence validation | same | same | returns typed diagnostic before convergence root, retry metadata, missing-child state, or wisp writes |
| fanout fragments and `on_complete` formulas | native compiler accepts alias before fragment mutation | same | same | returns typed diagnostic before fragment children, convoy links, or continuation metadata |
| validation/report commands | reports alias, dual, or requires source | reports dual source | reports requires source | may still validate syntax and reports unsatisfied host separately |
| `bd` compatibility probe | must handle alias or dual through `contract` | must handle dual through `contract` | not a supported first-party distribution until minimum binary floor | not used for runtime writes |

First-party built-in and example graph formulas must stay dual-declared until
the minimum supported Gas City binary understands `[requires]` and either the
native compiler path is the only production path or the `bd` shell-out path is
proven to accept equivalent requirements. External SHA-pinned formulas that use
legacy `contract = "graph.v2"` remain valid through the alias window.

JSON formula policy:

TOML remains the canonical authoring surface. If an existing JSON formula
loader is enabled for legacy formulas, it must normalize through the same raw
requirement pipeline instead of bypassing it.

| JSON shape | Behavior |
|---|---|
| no `contract` and no `requires` | default compiler capability |
| `"contract": "graph.v2"` | deprecated alias for capability 2 with `formula.contract_deprecated` |
| `"requires": {"formula_compiler": ">=2"}` | JSON equivalent of the TOML `[requires]` table |
| both JSON `contract` and JSON `requires` agree | source `dual`, same warning as TOML |
| invalid JSON `contract` value | `formula.compiler_requirement_unsupported` with a JSON-pointer source key |
| malformed JSON `requires`, non-string value, unknown axis, or unsupported string | same structured diagnostic code as the equivalent TOML row |
| host disabled for any graph-capability JSON formula | `formula.compiler_requirement_unsatisfied` before durable writes |

First-party JSON formulas may not become requires-only until the minimum binary
floor artifact says all supported readers understand the JSON `requires`
object, or the JSON formula loader is formally retired.

The alias window is evidence-based with a calendar floor. The release captain
for this migration is the only role that may declare a gate satisfied, and the
declaration must cite the saved artifacts below. The parser may remove legacy
`contract` support only after all of these are true:

1. Every first-party pack, example, test fixture, and tutorial has shipped with
   `[requires]` for at least two completed minor releases and at least 60
   calendar days after the first published release containing dual declarations.
2. `gc formula validate --all-packs --legacy-contract-report` reports zero
   first-party `legacy_only` formulas and zero first-party `dual_declared`
   formulas.
3. `docs/release/formula-compiler-external-support.json` has no active support
   row that still requires parser alias support for an externally pinned pack,
   branch, tag, SHA, registry source, local path, transitive import, or shadowed
   formula.
4. The compatibility artifact records exact versions or SHAs for every old
   reader in the supported set and proves those readers either no longer need
   alias parsing or are outside support.
5. CI's stale-guidance check rejects new docs that teach `contract` as the
   canonical surface.

`docs/release/formula-compiler-compatibility.yaml` is the executable source of
truth for active old readers. PR descriptions, comments, or release notes do
not count unless the YAML names the exact binary version or SHA and the corpus
result. Unknown external packs are treated as supported legacy consumers until
`docs/release/formula-compiler-external-support.json` marks them expired or
not-needed with a saved inventory. SHA-pinned external packs keep alias support
unless the artifact names a compatibility branch, LTS binary, or explicit
maintainer opt-out path.

The external migration report is typed evidence, not prose. Each support row in
`docs/release/formula-compiler-external-support.json` records `source_class`,
`pack_name`, `source`, `requested_ref`,
`locked_revision`, `content_hash`, `binding_path`, `maintainer_contact`,
`support_strategy`, `support_expires_after_release`, `support_expires_at`,
`evidence_artifact`, and `status`. Alias removal fails while any active or
unexpired row lacks an immutable revision, content hash, contact/opt-out
record, or saved migration evidence.

<!-- REVIEW: added per attempt-79-external-discovery-outreach -->

External discovery and outreach are executable workflow inputs:

| Field | Meaning |
|---|---|
| `discovery_method` | `installed_pack_scan`, `lockfile_scan`, `registry_query`, `known_partner`, `support_request`, or `manual_report` |
| `maintainer_contact_status` | `unknown`, `contacted`, `acknowledged`, `opted_out`, `unreachable`, or `not_needed` |
| `public_notice_url` | stable release-note, discussion, mailing-list, or docs URL announcing the alias window |
| `notice_sent_at` | timestamp for direct maintainer notice when contact exists |
| `support_window_closes_at` | timestamp derived from the alias-window clock and any explicit extension |
| `migration_path` | `dual_declare`, `compat_branch`, `lts_binary`, `pin_pack_revision`, `maintainer_opt_out`, or `not_needed` |
| `last_validated_at` | timestamp and artifact path for the latest validation run |

The discovery command writes a JSON companion,
`docs/release/formula-compiler-external-support.json`, with one row per
external source and every field above. Alias removal is blocked while a row is
`unknown`, `contacted` without elapsed support window, `unreachable` without a
published public notice and immutable lock/cache evidence, or missing
`last_validated_at`. A SHA-pinned external pack can leave support only through
maintainer opt-out, a compatibility branch/LTS plan, or a validation artifact
that proves the pinned object no longer needs legacy `contract`.

<!-- REVIEW: added per attempt-80-external-support-schema -->

The canonical external-support artifact is
`docs/release/formula-compiler-external-support.json`; any Markdown summary is a
rendered human checklist generated from that JSON. The JSON schema is:

```json
{
  "schema_version": 1,
  "generated_at": "2026-05-12T00:00:00Z",
  "release_captain": "name",
  "alias_window": {
    "start_release": "v0.x.y",
    "start_date": "2026-05-12",
    "minimum_completed_minor_releases": 2,
    "minimum_calendar_days": 60,
    "support_window_closes_at": "2026-07-11T00:00:00Z",
    "public_notice_url": "https://...",
    "public_notice_published_at": "2026-05-12T00:00:00Z"
  },
  "rows": []
}
```

Each `rows[]` item must include `row_id`, `status`, `source_class`,
`discovery_method`, `pack_name`, `source`, `requested_ref`, `locked_revision`,
`content_hash`, `binding_path`, `parent_binding`, `lockfile_key`,
`maintainer_contact`, `maintainer_contact_status`, `notice_sent_at`,
`public_notice_url`, `migration_path`, `edit_target`, `evidence_artifact`,
`last_validated_at`, `expires_after_release`, `support_window_closes_at`, and
`blocks_alias_removal`. The alias-removal gate consumes this JSON directly and
fails on missing fields, duplicate `row_id`, malformed timestamps, unknown enum
values, non-immutable `locked_revision` for a row marked pinned, missing
content hash, stale `last_validated_at`, or a Markdown checklist that is not
generated from the JSON digest.

Unknown and unreachable consumers have deterministic expiry semantics. An
`unknown` row starts active support and blocks alias removal until a public
notice URL is published, the alias-window start artifact records that URL, the
later of two completed minor releases or 60 calendar days has elapsed, and the
latest discovery run is non-stale. An `unreachable` row may expire only with
immutable lock/cache evidence or a public notice plus saved acquisition logs;
network failure alone is exit `1`, not an expiry decision.

`formula.migration.pin_pack_revision` is a migration hint for consumers, not a
formula edit. It tells a pack consumer to pin an import, registry source, or
lockfile entry to an immutable revision that is known to contain the dual or
requires-compatible formula. Its JSON includes `edit_target`:

| `edit_target` | Meaning |
|---|---|
| `consumer_import_ref` | edit the consumer's `[imports.<binding>]` ref or version constraint |
| `consumer_lockfile` | refresh the consumer lockfile to the named immutable revision |
| `upstream_pack_branch` | ask the upstream maintainer to publish a compatibility branch or tag |
| `forked_pack_source` | consumer must fork or mirror because the original pinned SHA cannot be edited |
| `none` | evidence shows no pin change is needed |

For SHA-pinned packs, the original source object is immutable. Validation must
not suggest editing that SHA in place. The remediation either keeps alias
support, points to an upstream compatibility branch/LTS binary, records a
maintainer opt-out, or names a consumer-side import/lockfile edit target. A
hint that lacks this edit-target distinction is malformed and blocks alias
removal.

`[pack] requires_gc` compatibility is part of the same evidence bundle. The
external-support row records the exact `requires_gc` string, the active binary
version used by the comparator, the comparator result, and whether the parser
accepted the grammar before formula validation. Alias removal cannot rely on a
pack-floor row whose `requires_gc` was parsed by an older or ad hoc grammar.

<!-- REVIEW: added per migration-gates-measurable -->

Additional compatibility gates:

| Scenario | Required behavior before first-party formulas become requires-only |
|---|---|
| Old binary reading old pack | No change; legacy `contract` formulas keep working as today |
| Old binary reading dual-declared pack | Works because `contract` remains present and old binaries ignore `[requires]` |
| Old binary reading requires-only graph pack | Not a supported first-party distribution until the minimum binary floor is enforced |
| New binary reading old external SHA-pinned pack | Works through the `contract` alias and emits bounded deprecation diagnostics |
| New binary with `formula_v2=false` reading any graph-capability formula | Fails before durable writes with `formula.compiler_requirement_unsatisfied` |
| Mixed-version controllers sharing a bead store | New roots are dual-stamped until all supported readers use canonical keys |
| Native compiler path | Uses `CompileWithResult` and canonical diagnostics |
| `GC_NATIVE_FORMULA=false` or `bd` shell-out path | Optional validation-only probe; if marked active, preserve dual `contract` declarations or prove byte-level `[requires]` parity before source conversion |

<!-- REVIEW: added per alias-window-executable-gates -->

Alias-window gates are executable reports:

| Report or fixture | Must cover | Blocks |
|---|---|---|
| `--legacy-contract-report` | legacy-only, dual-declared, requires-only, empty `[requires]`, invalid `contract`, and source path/provenance | first-party requires-only conversion while first-party legacy-only rows remain; alias removal while first-party legacy-only or dual-declared rows remain |
| `--legacy-version-report` | `version` omitted, `1`, `2`, string, wrong type, and coexistence with v2-only constructs | diagnostics rollout when `version` still implies compiler behavior anywhere |
| inherited requirement conflict fixture | parent raises to `>=2`, child attempts default or `>=1`, expansion/aspect raises beyond root | caller migration when conflicts do not produce deterministic source-attributed diagnostics |
| first-party dual-declaration guard | built-in packs, examples, tutorials, and test fixtures during alias window | release when a first-party graph formula drops `contract` before the floor allows it |
| external pinned-pack inventory | local path, git SHA, tag, branch, registry source, transitive import, and shadowed formula | alias removal while any supported external path still depends on legacy `contract` |
| old-reader/probe corpus | latest supported old binaries, current binary, and optional active probe | first-party requires-only conversion while old readers are still in support |

`gc formula validate --all-packs --legacy-contract-report` is a release-gate
command, not best-effort text. It returns JSON by default when `--json` is set
with this shape:

```json
{
  "legacy_only": 0,
  "dual_declared": 0,
  "requires_only": 0,
  "unsupported": [],
  "items": []
}
```

Each item records formula name, source path, pack binding, import source, ref,
locked revision or local hash, requirement source, and whether the formula is
first-party or external. Exit code `0` means no first-party legacy-only formulas
were found; exit code `2` means first-party legacy-only formulas remain, and
the alias-removal subcommand treats first-party `dual_declared > 0` as the same
blocking release-gate failure. Exit code `1` is reserved for I/O or
malformed-formula failures. The minimum binary floor is recorded in the release
checklist and `docs/reference/config.md` before any first-party requires-only
conversion lands.

<!-- REVIEW: added per DR53-first-party-external-classification -->

First-party versus external classification is deterministic and fixture-locked:

| Classification | Predicate |
|---|---|
| `first_party_builtin` | resolver source is a Gas City bundled pack under `internal/bootstrap/packs/` or a checked-in generated system-pack manifest for the checked-out revision |
| `first_party_example` | source is under `examples/`, runnable tutorials, or docs snippets that the release gate executes |
| `first_party_fixture` | source is under `internal/testfixtures/`, formula compiler testdata, workflow fixtures, or Ralph demo formulas shipped with the repository |
| `external_git_or_registry` | source was resolved through a user import, git URL, registry source, branch, tag, semver constraint, or SHA |
| `external_local` | source was resolved from a user local path outside first-party roots |
| `external_transitive` | source entered through a first-party or external pack import whose child source is not first-party |
| `shadowed_external` | losing layer is external even when the winning formula is first-party |

Every report item carries `classification`, `classification_reason`,
`source_root`, `binding_path`, `parent_binding`, and `lockfile_key`. Unreadable
inventories, ambiguous roots, symlink escapes, missing binding identity, or
missing lockfile evidence fail closed as `unreadable_inventory`; they do not
default to external success.

Requires-only conversion and alias removal are two executable gates:

```bash
gc formula validate --all-packs --requires-only-conversion-gate --json
```

The conversion gate runs legacy-contract, compatibility-corpus, min-floor,
first-party inventory, active-root, provenance, stale-guidance, and
external-support checks. It returns exit `0` only when first-party
`legacy_only == 0`, pack floors are enforced, the supported old-reader set is
above the minimum floor, active roots are repaired/drained/waived, and the
alias-window artifact says conversion is open. First-party `dual_declared > 0`
is expected before conversion and does not block this gate. Exit `2` means the
conversion window is not open; exit `1` means evidence is unreadable or
malformed.

Alias removal is stricter:

```bash
gc formula validate --all-packs --alias-removal-gate --json
```

The command runs legacy-contract, legacy-version, compatibility-corpus,
first-party inventory, external-support, stale-guidance, and active-root report
checks. It returns exit `0` only when first-party `legacy_only == 0`,
first-party `dual_declared == 0`, unsupported future requirements are zero for
all supported first-party packs, external support is expired or not-needed, and
the inventory was fully readable. Exit `2` is a release-gate block with typed
counts; exit `1` is I/O or malformed input. The same command is cited in alias
removal criteria, rollout gates, and release checklists so they cannot drift.

<!-- REVIEW: added per attempt-73-alias-removal-report-schema -->

The alias-removal gate JSON report has a fixed top-level schema:

```json
{
  "schema_version": 1,
  "generated_at": "2026-05-11T00:00:00Z",
  "gate_command": "gc formula validate --all-packs --alias-removal-gate --json",
  "release": {
    "tag": "v0.x.y",
    "date": "2026-05-11",
    "rollback_window_open": true,
    "rollback_window_closes_at": "2026-07-10T00:00:00Z"
  },
  "baseline_binary": {
    "tag": "v0.x.y",
    "digest": "sha256:...",
    "source": "docs/release/formula-compiler-compatibility.yaml"
  },
  "input_artifacts": {
    "alias_window_start": {
      "path": "docs/release/formula-compiler-alias-window-start.json",
      "sha256": "..."
    },
    "alias_drain": {
      "path": "docs/release/formula-compiler-alias-drain.json",
      "sha256": "..."
    },
    "minimum_floor": {
      "path": "docs/release/formula-compiler-min-floor.json",
      "sha256": "..."
    },
    "active_roots": {
      "path": "docs/release/formula-compiler-active-roots.json",
      "sha256": "..."
    },
    "compatibility_corpus": "internal/formula/testdata/compat_corpus/report.json",
    "legacy_inventory": "docs/release/formula-compiler-first-party-inventory.json",
    "external_support": "docs/release/formula-compiler-external-support.json",
    "stale_guidance": "docs/release/formula-compiler-docs-check.json"
  },
  "counts": {
    "legacy_only": 0,
    "dual_declared": 0,
    "requires_only": 0,
    "background_accepted_alias": 0,
    "external_active_support": 0,
    "shadowed_external_legacy": 0
  },
  "support_rows": [],
  "items": []
}
```

Each `items[]` row includes formula name, classification, external pack class,
source path, pack binding path, requested ref, locked revision, baseline binary
result, release-history input, support-row expiration, corpus artifact path,
the alias-window-start digest, the alias-drain digest, and whether the row
blocks alias removal. Empty arrays are evidence. Missing arrays, missing
release tag/date, missing baseline binary digest/tag, missing rollback-window
status, missing input artifact digest, mismatched digest between reports,
stale `generated_at`, conflicting first-party/external classification, or
missing background accepted-alias counts are schema errors and make the command
exit `1`.

The alias-removal gate has one command/schema/exit contract:

| Evidence problem | Exit | Rule |
|---|---|---|
| missing artifact, unreadable artifact, malformed schema, stale timestamp, placeholder version, or digest mismatch | `1` | evidence is not usable; no release decision can be inferred |
| rollback window closed, rollback owner absent, active roots unrepaired, external support active, alias drain nonzero, or old-reader support still active | `2` | gate is readable but blocked |
| runtime accepted-alias count absent or process-local only | `2` | count must come from accepted artifacts, workflow-root metadata, producer diagnostic state, or release validation output |
| all evidence present, fresh, digest-linked, rollback-eligible, and zero blocking rows | `0` | alias parser support may be removed in Phase 9 only |

Runtime accepted-alias counts are durable evidence. The gate reads accepted
compile artifacts, workflow-root metadata, producer diagnostic state, and saved
release reports. Logs, stderr, in-memory warning LRUs, Event Bus warning
payloads, or a single process's counters do not satisfy the drain contract.

<!-- REVIEW: added per attempt-54-alias-removal-exit-contract -->

Alias-removal gate exit semantics are fixed:

| Condition | Exit | Report field | Required remediation |
|---|---|---|---|
| first-party `legacy_only > 0` | `2` | `legacy_only[]` | dual-declare or migrate source, then rerun inventory |
| first-party `dual_declared > 0` during alias removal | `2` | `dual_declared[]` | wait for floor/window or convert after release-captain approval |
| first-party unsupported future requirement | `2` | `unsupported_future[]` | upgrade gate binary or remove unsupported formula from supported first-party packs |
| unsupported future requirement in supported external pack | `2` | `external_unsupported_future[]` | record external-support strategy or expire support with evidence |
| missing or unreadable first-party inventory | `1` | `unreadable_inventory[]` | fix acquisition/path/symlink issue; gate cannot assume success |
| missing release artifact | `2` | `missing_artifacts[]` | seed compatibility, min-floor, external-support, stale-guidance, and first-party inventory artifacts |
| stale external-support row | `2` | `stale_external_support[]` | release captain refreshes owner, status, evidence path, and expiration decision |
| external source auth failure | `1` | `acquisition_failures[]` | rerun with non-interactive credentials or mark source unsupported through external-support process |
| unreachable remote source | `1` by default, `2` only with saved offline support policy | `acquisition_failures[]` | provide lock/cache evidence or support decision; do not silently pass |
| shadowed external formula with legacy-only alias | `2` when alias removal would make it unreadable if it later wins | `shadowed_external[]` | migrate, expire support, or record layer-order support waiver |
| SHA-pinned external formula still legacy-only | `2` | `sha_pinned_legacy[]` | publish compatible branch/LTS strategy, maintainer opt-out, or keep alias support |
| stale-guidance matcher failure | `2` | `stale_guidance[]` | update docs/help/examples or add explicit historical exception |
| malformed formula or pack metadata | `1` for parse/load failure, `2` for valid formula diagnostics | `malformed[]` or `diagnostics[]` | fix source or remove from supported inventory |

The JSON report always includes `schema_version`, `generated_at`,
`gate_command`, `release_captain`, `support_window`, `input_artifacts`,
`counts`, and arrays for every field above, even when empty. Empty arrays are
evidence; missing arrays are a report schema error. Initial release artifacts
must be seeded with conservative blocking defaults before diagnostics or
first-party source conversion ship, so a missing external-support decision
blocks alias removal instead of being interpreted as no risk.

<!-- REVIEW: added per DR41-alias-visibility -->

Accepted legacy-alias compiles are visible by policy, not by caller discretion:

| Surface | Legacy-alias visibility during alias window |
|---|---|
| `gc formula validate`, `show`, and API preview | return `formula.contract_deprecated` with source path/key/value every time the source is inspected |
| `gc sling --formula` and API sling | accept through the native compiler, attach the warning to the compile result/artifact, and suppress repeated operator display by `OnceKey` |
| order dispatch | record one grouped producer-local warning per `(order id, formula, OnceKey, config generation)`; do not emit Event Bus warning payloads |
| convergence create/retry/speculative wisp | persist the warning in the accepted artifact and expose it in convergence preview/status surfaces |
| fanout fragments and `on_complete` formulas | persist warnings for parent and fragment artifacts separately so fragment-only legacy use is not hidden |
| retry/Ralph continuation/controller validation | attach warnings to the accepted artifact or validation result and include them in release reports |
| dashboard/API read models | project the warning only from typed diagnostics or accepted artifact metadata; never infer it from raw `gc.formula_contract` |
| release validation | include legacy-only, dual-declared, requires-only, external, shadowed, and background-accepted alias counts in the saved report |

This visibility policy is required evidence for alias removal. A background
legacy alias compile that is accepted but not surfaced in one of the rows above
keeps the alias window open.

Old-reader and native-vs-`bd` parity are exercised by a pinned corpus under
`internal/formula/testdata/compat_corpus/`. The corpus contains at least these
named fixtures:

| Fixture | Required readers | Expected result |
|---|---|---|
| `legacy-only-graph-v2` | latest two published Gas City minor releases, current `main`, `bd >= 1.0.0` probe | old readers and new host-enabled readers accept; new host-disabled reader fails before writes |
| `dual-declared-graph-v2` | same | old readers accept through `contract`; new readers report source `dual` and deprecation warning |
| `requires-only-graph-v2` | current `main` only until minimum binary floor | old readers are allowed to fail; first-party distribution blocked until floor is enforced |
| `unsupported-future-requirement` | current `main` | deterministic `formula.compiler_requirement_unsupported_future` |
| `invalid-shape-non-string` | current `main` | deterministic `formula.requirement_invalid_type` |
| `diagnostic-source-attribution` | direct CLI, API-routed CLI, Huma endpoint, dashboard fixture | identical code, source path/key/value, remediation, and host capability |

The supported reader set for a release is materialized in
`docs/release/formula-compiler-compatibility.yaml`. It records exact binary
versions or SHAs for the latest two supported Gas City minor releases, current
`main`, and any optional compatibility probe. For this design baseline,
production is native-only; a `bd` or `GC_NATIVE_FORMULA=false` probe is a
release-validation tool only when a checked-in test proves the command still
exists and runs non-interactively. If that proof does not exist, the
compatibility artifact must mark the probe `status: not-needed` and no release
gate may require it.

<!-- REVIEW: added per executable-migration-gates -->

The three release artifacts are executable inputs with owners and schemas:

```yaml
# docs/release/formula-compiler-compatibility.yaml
schema_version: 1
owner: release-captain
updated_by: gc formula validate --compat-corpus internal/formula/testdata/compat_corpus --write-compatibility-artifact
supported_readers:
  - name: gas-city
    kind: binary
    version: 0.x.y
    sha: <git-sha>
    required: true
  - name: bd-probe
    kind: compatibility_probe
    status: not-needed # active only while a checked-in probe command exists
    version: 1.0.0
    required_until: native-only-runtime
fixtures:
  - id: dual-declared-graph-v2
    expected: accept
    diagnostics: [formula.contract_deprecated]
```

```json
{
  "schema_version": 1,
  "owner": "release-captain",
  "updated_by": "gc formula validate --all-packs --write-min-floor docs/release/formula-compiler-min-floor.json",
  "minimum_gc_for_requires_only": "0.x.y",
  "first_gc_with_requires": "0.x.y",
  "first_gc_with_canonical_root_metadata": "0.x.y",
  "first_gc_without_bd_runtime_fallback": "0.x.y",
  "dual_declared_compatibility_window_minor_releases": 2,
  "first_party_requires_only_allowed": false
}
```

`docs/release/formula-compiler-external-support.json` is a release gate input,
not prose-only background. It contains `schema_version`, `owner`, `status`
(`active`, `expired`, or `not-needed`), `supported_old_binaries`,
`support_strategy`, `expires_after_release`, public-notice fields, and links to
legacy inventory reports. CI blocks alias removal while the JSON file is
missing, malformed, has `status: active`, or names supported readers that still
require legacy `contract`.

External-support status transitions are checked:

| Status | Meaning | Allowed transition |
|---|---|---|
| `active` | alias support is still required for unknown or named external consumers | to `expired` only with inventory evidence and published migration notice |
| `expired` | support window elapsed and external consumers have a named migration path | back to `active` only for a documented compatibility incident |
| `not-needed` | no external support obligation exists for this source class | to `active` when a supported external pack, SHA pin, or old reader is discovered |

<!-- REVIEW: added per attempt-90-release-evidence-manifest -->

Every migration gate consumes JSON as canonical input. Markdown files are
rendered documentation only and must carry the source JSON digest. A release
gate fails closed when the JSON is missing, malformed, stale, or names a
placeholder value such as `0.x.y`, `<git-sha>`, or
`<minimum-floor-from-formula-compiler-min-floor.json>` outside an explicitly
marked template fixture.

Release evidence artifacts:

| Artifact | Owner | Generator command | Exit contract | Consumed by |
|---|---|---|---|---|
| `docs/release/formula-compiler-compatibility.yaml` | release captain | `gc formula validate --compat-corpus internal/formula/testdata/compat_corpus --write-compatibility-artifact` | `0` saved, `1` I/O/probe failure, `2` corpus diagnostic mismatch | minimum floor, alias window, alias removal |
| `docs/release/formula-compiler-min-floor.json` | release captain | `gc formula validate --all-packs --write-min-floor docs/release/formula-compiler-min-floor.json` | `0` saved only with non-placeholder versions, `2` missing provenance/floor evidence | first-party requires-only conversion |
| `docs/release/formula-compiler-first-party-inventory.json` | formula owner plus release captain | `gc formula validate --all-packs --first-party-inventory --json` | `0` full readable inventory, `1` unreadable/malformed source, `2` formula diagnostics or stale dual-declaration state | docs gate, pack-floor declaration, alias drain |
| `docs/release/formula-compiler-external-support.json` | release captain | `gc formula validate --all-packs --external-support --json` | `0` all rows expired/not-needed, `1` acquisition/auth failures, `2` active or stale support rows | alias window and alias removal |
| `docs/release/formula-compiler-alias-window-start.json` | release captain | `gc formula validate --alias-window-start --json --write ...` | `0` only when public notice, dual declarations, canonical metadata, docs bundle, stale CI, min-floor, compatibility, and external-support artifacts are all present | starts the two-minor-release/60-day clock |
| `docs/release/formula-compiler-active-roots.json` | operator/release captain | `gc formula validate --active-roots --json --write ...` | `0` all active roots accepted/repaired/drained/waived, `1` store read failure, `2` unrepaired active roots | Phase 8 conversion |
| `docs/release/formula-compiler-alias-drain.json` | release captain | `gc formula validate --alias-drain --json --write ...` | `0` two qualifying reports saved, `2` accepted alias/background alias/external active support remains | Phase 8 and Phase 9 |
| `docs/release/formula-compiler-docs-check.json` | docs owner plus release captain | `make formula-docs-check` | `0` generated docs/help/schema/OpenAPI/TS/doctest checks pass, `2` stale guidance or missing generated artifact | any user-visible diagnostic surface |
| `gc formula repair-root-artifact --dry-run --json` output | operator | command-specific invocation | `0` would-stamp/already-stamped, `1` I/O/store, `2` formula/identity/projection conflict | active-root repair and waivers |

The active-root report schema includes `schema_version`, `generated_at`,
`city`, `store_ref`, `release_captain`, `counts`, `roots[]`, and
`waivers[]`. Each `roots[]` row records root id, status, source class, formula,
artifact ref, content hash, requirement source, host capability, repair
command status, owner, and whether it blocks Phase 8. A waiver row must name
the root id, operator, timestamp, diagnostic code, reason, and cleanup or
abandonment plan. Missing roots, unreadable stores, source ambiguity, missing
content hash, downgraded host before repair, or unsupported future metadata
blocks conversion; the report cannot assume "no active roots" from a failed
query.

`formula-compiler-alias-drain.json` is a sliding evidence window, not a
single snapshot. It records the latest release candidate and the previous
completed minor release, both input artifact digests, first-party
`legacy_only`, `dual_declared`, and `background_accepted_alias` counts,
external active-support counts, public notice URL, and alias-window start
artifact digest. Empty arrays are evidence. Missing arrays, placeholder
versions, missing public notice, packman schema below the declared provenance
floor, or stale support rows make the drain command exit `2`.

First-party dual-declaration ownership belongs to the release captain until the
minimum-floor artifact allows requires-only distribution. Individual feature
PRs may add `[requires]`, but they may not remove first-party `contract`
aliases, dual root stamps, or compatibility fixtures without a release-captain
update to the minimum-floor and compatibility artifacts.

<!-- REVIEW: added per DR33-rollout-ownership -->

The initial implementation seeds these artifacts with conservative blocking
values before any first-party source conversion:

| Artifact | Initial owner | Conservative seed |
|---|---|---|
| `docs/release/formula-compiler-compatibility.yaml` | release-captain | current `main` required, older supported readers listed, optional `bd` probe `not-needed` unless a checked owner test proves it active |
| `docs/release/formula-compiler-min-floor.json` | release-captain | `first_party_requires_only_allowed: false` and no minimum floor until release evidence exists |
| `docs/release/formula-compiler-external-support.json` | release-captain | `status: active`, alias support as the default strategy, and explicit expiration unset |

"Two minor releases" means two completed Gas City minor releases after the
first release that shipped dual declarations, canonical metadata writers, and
the docs/example bundle. Pre-releases, nightly builds, failed releases, and
commits that were not published to the supported distribution channel do not
count. The release checklist records the two release tags, dates, supported
reader set, legacy inventory path, and compatibility-corpus result before a
gate depending on two minor releases may pass.

Legacy `version` handling has an explicit report owner. The same release
captain owns `gc formula validate --all-packs --legacy-version-report --json`,
which records `version` omitted, `1`, `2`, string, wrong type, coexistence
with v2-only constructs, and whether any code path still treats `version = 2`
as compiler behavior. User-visible diagnostics and first-party source
conversion are blocked while that report finds behavioral `version` coupling.

The `GC_NATIVE_FORMULA=false` and `bd` paths are validation-only probes after
this design lands, and they are optional. They are not supported runtime
fallbacks for sling, orders, convergence, fanout, controller validation, API,
or dashboard preview. Keeping either path as an active release gate requires a
checked-in owner test such as `TestLegacyFormulaProbeExists`, a documented
non-interactive command, and byte-level corpus parity: the native and probe
runs must match normalized requirement, source attribution, diagnostic code,
diagnostic order, exit code, and provenance for every fixture that first-party
distribution depends on. Without that proof, the only valid production
precondition is native Go compiler support.

```bash
gc formula validate --compat-corpus internal/formula/testdata/compat_corpus --json
# Optional only when docs/release/formula-compiler-compatibility.yaml marks the probe active:
GC_NATIVE_FORMULA=false gc formula validate --compat-corpus internal/formula/testdata/compat_corpus --json
```

The first command is authoritative for production behavior. The second command
is a release probe only when the artifact marks it active; if it is absent or
not-needed, requires-only conversion is governed by the minimum binary floor,
legacy inventory, and native corpus. If an active probe diverges on first-party
dual-declared formulas, requires-only conversion remains blocked. Pass/fail is
strict: accepted fixtures must match normalized requirement, source
attribution, diagnostics, exit code, and provenance; rejected fixtures must
match diagnostic code, count, ordering, and source attribution.

Measurable release gates:

| Gate | Artifact | Blocks |
|---|---|---|
| Legacy inventory | `gc formula validate --all-packs --legacy-contract-report --json` saved in release artifacts | first-party requires-only conversion while `legacy_only > 0`; alias removal while first-party `legacy_only > 0` or `dual_declared > 0` |
| Provenance inventory | `gc formula validate --all-packs --provenance --json` | source conversion when any first-party graph formula lacks pack binding, locked revision, or content hash |
| Minimum binary floor | `docs/release/formula-compiler-min-floor.json` plus release-note entry | publishing requires-only first-party packs |
| External pinned-pack plan | release checklist entry naming alias support, compatibility branch, or LTS binary | removing `contract` alias |
| Mixed-store compatibility | test report for old/new controllers reading dual-stamped and canonical-only roots | retiring dual root stamps |
| `bd`/native parity | golden corpus result for native and the optional active probe | source conversion only while the compatibility artifact marks a probe active |
| Stale guidance | CI report for docs/examples generated help | user-visible diagnostics |

The external support plan is a release-owned JSON artifact at
`docs/release/formula-compiler-external-support.json`. It names the release
captain, supported old binary versions, whether external SHA-pinned packs are
served by alias support, a compatibility branch, or an LTS binary, opt-in
instructions for users, removal criteria, public-notice fields, and the
artifact location for each legacy inventory report. Alias removal is blocked
while this artifact is missing, stale, malformed, or says external support is
active. A Markdown summary may be generated from the JSON, but it is not gate
input.

`formula-compiler-min-floor.json` records the lowest Gas City version allowed
to consume first-party requires-only graph formulas, the first release that
understands `[requires]`, the first release that writes canonical root metadata,
and the release that stopped needing the `bd` compatibility path. Rollback is
to restore dual declarations and dual root stamps, not to reinterpret
requires-only formulas in old binaries.

First-party graph packs may become requires-only only when their pack metadata
also declares a compatible binary floor:

```toml
[pack]
requires_gc = ">=<minimum-floor-from-formula-compiler-min-floor.json>"
```

Pack resolver and import loading must reject any pack whose `[pack]
requires_gc` exceeds the active binary before formula selection, staging,
shadow resolution, or durable writes. First-party requires-only graph packs
must set the floor from `formula-compiler-min-floor.json`; external packs set
their own floor, but the resolver still rejects them fail-closed. Tests cover
built-in packs, direct local packs, installed packs, git/registry sources,
imported pinned packs, transitive imports, local dirty packs, and shadowed
formulas where a losing source would have required a higher floor.

Dual-stamp retirement criteria:

1. All supported readers use the shared canonical workflow-root predicate.
2. Mixed-version shared-store tests pass with canonical-only roots.
3. The external pinned-pack plan has shipped.
4. The previous two minor releases wrote dual stamps and emitted no known
   compatibility incident.

Alias removal criteria are stricter than dual-stamp retirement: the parser may
stop accepting legacy `contract` only after the enforced minimum binary floor
artifact says all supported readers understand canonical `[requires]`, the
configured dual-declared compatibility window has elapsed, the external plan
expires, the old-reader artifact names exact supported binary versions or SHAs,
the legacy inventory gate has reported zero first-party `legacy_only` and zero
first-party `dual_declared` formulas for two consecutive minor releases, and no
background validation, order, convergence, fanout, retry, controller, API, or
dashboard report contains an accepted alias compile in the latest release
candidate.

## Host Capability And Diagnostics

<!-- REVIEW: added per operator-diagnostics-projection -->

`[daemon] formula_v2` remains the host capability gate. A formula whose
normalized requirement is `CompilerCapabilityV2` must fail before creating
any new runtime state when the host capability is disabled.

At config load, `[daemon] formula_v2 = true` maps through
`NewHostCapabilities(HostCapabilityInput{FormulaCompiler: CompilerCapabilityV2,
Source: ...})`; false maps through the same constructor with
`CompilerCapabilityDefault`. The boolean exists only as legacy config
vocabulary at the edge; the constructor's typed `FormulaCompiler()` value and
structured `Source()` object are the canonical capability and provenance used
by `CheckRequirements`.

Required diagnostic codes:

| Code | Severity | Meaning |
|---|---|---|
| `formula.requirement_unknown_axis` | error | `[requires]` contains an unsupported key |
| `formula.requirement_invalid_type` | error | A requirement value has the wrong TOML type |
| `formula.compiler_requirement_invalid_syntax` | error | The expression is malformed or outside the byte-exact v0 grammar |
| `formula.compiler_requirement_unsupported_future` | error | The expression is syntactically valid but requires a future compiler capability |
| `formula.compiler_requirement_unsupported` | error | A legacy compatibility field such as `contract` has an unsupported value |
| `formula.compiler_requirement_conflict` | error | `contract` and `[requires]` disagree |
| `formula.compiler_requirement_missing` | error | A v2-only construct lacks a v2 requirement |
| `formula.compiler_requirement_unsatisfied` | error | Host config cannot satisfy the normalized requirement |
| `formula.host_capability_invalid` | error | Production code passed an invalid host capability value |
| `formula.contract_deprecated` | warning | Legacy `contract` spelling was accepted |
| `formula.version_deprecated` | warning | Legacy formula `version` was present and preserved only as metadata |
| `formula.version_misuse` | warning | Legacy formula `version` appears alongside v2-only syntax or migration guidance that could imply compiler selection |

Diagnostic projection matrix:

| Code | Diagnostic class | Canonical message/remediation | CLI | API | Dashboard | Events |
|---|---|---|---|---|---|---|
| `formula.requirement_unknown_axis` | author-fixable formula source | `unsupported [requires] key <key>` / remove it or upgrade to a binary that supports the axis | fatal stderr; validate/report exit 2; runtime launch exit 1 | HTTP 400 typed diagnostic | formula preview validation error | no event except controller/order failure wrapper |
| `formula.requirement_invalid_type` | author-fixable formula source | `<key> must be a TOML string` / use `formula_compiler = ">=2"` | fatal stderr; validate/report exit 2; runtime launch exit 1 | HTTP 400 | formula preview validation error | no event except controller/order failure wrapper |
| `formula.compiler_requirement_invalid_syntax` | author-fixable formula source | `invalid formula compiler requirement <value>` / use omitted, `>=1`, or `>=2` exactly | fatal stderr; validate/report exit 2; runtime launch exit 1 | HTTP 400 | formula preview validation error | no event except controller/order failure wrapper |
| `formula.compiler_requirement_unsupported_future` | author or binary-upgrade required | `formula compiler capability <N> is not supported by this binary` / upgrade Gas City or use `>=1` or `>=2` | fatal stderr; validate/report exit 2; runtime launch exit 1 | HTTP 400 | formula preview validation error | no event except controller/order failure wrapper |
| `formula.compiler_requirement_unsupported` | author-fixable legacy source | `unsupported legacy formula contract <value>` / use `[requires] formula_compiler = ">=2"` | fatal stderr; validate/report exit 2; runtime launch exit 1 | HTTP 400 | formula preview validation error | no event except controller/order failure wrapper |
| `formula.compiler_requirement_conflict` | author-fixable formula source | `contract and [requires] disagree` / make both declare graph v2 or remove `contract` | fatal stderr; validate/report exit 2; runtime launch exit 1 | HTTP 400 | formula preview validation error | no event except controller/order failure wrapper |
| `formula.compiler_requirement_missing` | author-fixable formula source | `v2-only construct requires formula_compiler = ">=2"` / add `[requires] formula_compiler = ">=2"` | fatal stderr; validate/report exit 2; runtime launch exit 1 | HTTP 400 | formula preview validation error | no event except controller/order failure wrapper |
| `formula.compiler_requirement_unsatisfied` | operator-fixable host capability | `host formula compiler capability 1 does not satisfy >=2` / enable `[daemon] formula_v2` or choose a v1 formula | fatal stderr with host source; validate/report exit 2; runtime launch exit 1; no beads created | HTTP 400 for validation/preview or 409 for launch conflict plus diagnostic body | disabled-capability state with remediation | registered compile-failure wrapper event |
| `formula.host_capability_invalid` | internal configuration bug | `invalid host formula compiler capability <value>` / construct host capabilities at the config edge | fatal stderr, exit 1 | HTTP 500 unless caused by request-local test input | internal error state with remediation | registered producer failure event |
| `formula.contract_deprecated` | warning | `contract = "graph.v2" is deprecated` / use `[requires] formula_compiler = ">=2"` | bounded warning stderr by source/key; exit unchanged | warning diagnostic in 200/preview response | non-blocking warning | none; warnings are never Event Bus payloads |
| `formula.version_deprecated` | warning | `formula version is legacy metadata` / use pack version/ref/SHA for artifact identity | warning only on validate/show, never on launch success; exit unchanged | warning diagnostic on formula endpoints | non-blocking warning | none |
| `formula.version_misuse` | warning | `formula version does not enable compiler capability` / add `[requires] formula_compiler = ">=2"` for v2-only syntax | warning on validate/show next to the fatal missing-requirement diagnostic; exit unchanged beyond fatal status | warning diagnostic on formula endpoints | non-blocking warning grouped with author-fixable diagnostics | none |

<!-- REVIEW: added per DR53-single-diagnostic-attribution -->

Canonical diagnostic attribution is typed before any surface projection. The
formula package emits `Diagnostic{Attribution: ...}` with this minimum payload:

| Field | Required for | Meaning |
|---|---|---|
| `Primary.Kind/Path/Key/Value` | every structured diagnostic | the field an operator or author should look at first |
| `Requirement.Kind/Path/Key/Value` | every requirement-derived diagnostic | the formula field that produced the normalized requirement |
| `Host.Kind/Path/Key/Value` | every host satisfaction diagnostic | the active config or binary capability that failed satisfaction |
| `Pack.Kind/Path/Key/Value` | pack floor and external-pack diagnostics | the pack compatibility source or acquisition input |
| `Line/Column` | all valid-source TOML/JSON fields | byte-oriented source position from raw capture, never inferred from rendered errors |

`SourceKind` is not optional display metadata. It is part of the golden
fixture identity so a host-capability diagnostic cannot be mistaken for a
formula-author diagnostic when the message text is similar. The direct CLI,
API-routed CLI, Huma response, generated TypeScript fixture, dashboard state,
and Event Bus payload all read the same attribution object. If a surface needs
a flat field, it uses a mechanical projection from `Attribution.Primary`,
`Attribution.Requirement`, and `Attribution.Host`; it may not re-tokenize TOML,
parse stderr, or inspect root metadata to recover missing source information.

Projection rules:

<!-- REVIEW: added per DR33-diagnostics-policy -->

- Fatal and warning grouping keys are canonical. Warning `OnceKey` is
  `<code>|<formula>|<source_path>|<source_key>|<source_value>|<normalized_requirement>|<content_hash>`.
  Fatal `OnceKey` is
  `<code>|<formula>|<producer>|<subject_id>|<source_path>|<source_key>|<source_value>|<requirement_source_path>|<requirement_source_key>|<requirement_source_value>|<host_source_path>|<host_source_key>|<host_source_value>|<normalized_requirement>|<host_capability>|<content_hash>`.
  Background grouping adds config generation and scan series around that key;
  it never removes formula identity, producer, subject, requirement source,
  host source, or content hash. This prevents a newly failing formula from
  collapsing into an older disabled-host occurrence that happened to share the
  same config generation.
- Warning diagnostics do not publish Event Bus events. They are returned on
  synchronous CLI/API/dashboard validation surfaces and may be counted in
  producer-local state, but `formula.contract_deprecated` and
  `formula.version_deprecated` never append warning events.
- Compile-failure events are fatal-diagnostic wrappers only. Their event
  constants are limited to named background or durable producers and must carry
  the same typed diagnostic payload as the synchronous surface.
- `config_generation` is the monotonically increasing generation assigned by
  the city config loader after a successfully parsed config snapshot is
  installed. A failed reload does not advance it. Direct CLI commands use the
  snapshot generation they loaded for that invocation; order, controller, and
  convergence loops use the generation captured at the start of the scan or
  operation. Suppression and grouping reset when the generation changes.
- CLI commands that compile formulas print warnings to stderr once per
  `(code, source path, source key)` per command invocation. Fatal diagnostics
  exit non-zero and include the code, source path, offending value, normalized
  requirement, host capability, and remediation.
- `formula.compiler_requirement_unsatisfied` preserves both attributions: the
  formula requirement source (`requires.formula_compiler` or legacy `contract`)
  and the host gate source (`city.toml:[daemon].formula_v2`). The primary
  source fields point to the host gate because that is the operator action, and
  the requirement source fields remain present so API, dashboard, and release
  reports can identify which formula requirement was unsatisfied.
- API endpoints return typed diagnostic fields in Huma-registered response
  bodies. User-correctable formula input errors use HTTP 400; internal I/O
  failures remain HTTP 500.
- Dashboard state is derived from the API diagnostic projection, not by parsing
  stderr or root metadata strings.
- Remediation text is either plain prose or a syntactically valid TOML/JSON
  snippet that appears in a fixture. Copy-pasteable-looking but invalid
  snippets are not allowed in diagnostic fixtures, generated docs, CLI output,
  API examples, dashboard state, or release reports.
- Interactive preview/validate calls are synchronous by default. Direct CLI,
  API-routed CLI, and Huma preview/validate endpoints return typed diagnostics
  to the caller and do not publish Event Bus events unless they are invoked by
  a named durable/background producer listed below.
- Order dispatch emits at most one typed order failure event per
  `(order id, formula, OnceKey, host capability, config generation)` in a scan
  series and continues scanning later orders. Repeated scans increment a
  grouped occurrence counter on the producer state or next emitted event; they
  do not create wisps, mark the order fired, or append an unbounded event
  stream.
- Controller and convergence paths wrap the same `Diagnostic` code and fields
  in their existing error/event surfaces. They must not synthesize alternate
  wording that loses the remediation.
- Deprecation warnings are diagnostics attached to the compile result. CLI,
  API, dashboard, and controller projections decide whether and where to show
  them, but they all preserve the same code and source spelling.
- Event payloads for compile failures are typed and registered in
  `events.KnownEventTypes`. Payload fields include `code`, `severity`,
  `formula`, presence-aware `primary`, `requirement`, `host`, and `pack`
  source objects, `normalized_requirement`, `host_capability`, `message`,
  `remediation`, and `once_key`. No event path may hand-write JSON or project diagnostics through
  `map[string]any`.
- CLI suppression is per process invocation. Background suppression is an
  in-memory LRU per city and producer, capped at 4096 keys with oldest-entry
  eviction, reset on process restart and config reload. Warning diagnostics are
  suppressed by `OnceKey`; fatal diagnostics in background loops are grouped by
  `(producer, subject id, OnceKey, host capability, config generation)` until
  the formula source, config generation, or subject id changes. Evictions are
  observable through a producer-local counter and debug log field so operators
  can tell the difference between genuinely new failures and cache churn.

<!-- REVIEW: added per diagnostics-wire-contract -->

Typed HTTP responses use Huma structs generated from Go types. The formula
validation, preview, sling-launch conflict, order preview, and convergence
preview routes all embed the same diagnostic payload:

```go
type FormulaDiagnosticSourceWire struct {
    Present bool   `json:"present" doc:"Whether this source attribution exists"`
    Kind    string `json:"kind"`
    Path    string `json:"path"`
    Key     string `json:"key"`
    Value   string `json:"value"`
    Line    *int   `json:"line" doc:"1-based source line when available"`
    Column  *int   `json:"column" doc:"1-based source column when available"`
}

type FormulaDiagnostic struct {
    Code                  string                      `json:"code" doc:"Stable diagnostic code"`
    Severity              string                      `json:"severity" enum:"error,warning"`
    Formula               string                      `json:"formula"`
    Primary               FormulaDiagnosticSourceWire `json:"primary"`
    Requirement           FormulaDiagnosticSourceWire `json:"requirement"`
    Host                  FormulaDiagnosticSourceWire `json:"host"`
    Pack                  FormulaDiagnosticSourceWire `json:"pack"`
    NormalizedRequirement string                      `json:"normalized_requirement"`
    HostCapability        string                      `json:"host_capability"`
    Message               string                      `json:"message"`
    Remediation           string                      `json:"remediation"`
    OnceKey               string                      `json:"once_key"`
}

type FormulaDiagnosticsBody struct {
    Diagnostics []FormulaDiagnostic `json:"diagnostics"`
}
```

No formula diagnostic HTTP path may use `map[string]any`, `json.RawMessage`,
hand-written JSON, or stderr parsing. `TestOpenAPISpecInSync`,
dashboard generated-client compilation, and a golden OpenAPI excerpt test must
fail when this shape changes without regenerated schemas and TypeScript.
No diagnostic wire field that participates in source attribution may use
`omitempty`. Absence is represented by `FormulaDiagnosticSourceWire{Present:
false}` and `Line`/`Column` pointers only distinguish "known zero is
impossible" from "position unavailable." Golden Huma/OpenAPI and generated
TypeScript fixtures must assert that absent host, pack, or requirement sources
round-trip as present flags, not as dropped JSON fields.

CLI exit code mapping is fixed: `0` for accepted input with zero fatal
diagnostics, `1` for process/internal I/O failures, and `2` for formula
diagnostics on validation/report commands. Runtime launch commands such as
`gc sling --formula` and `gc order` use exit `1` for a failed operation but
still print the same diagnostic code and fields. API-routed CLI commands must
preserve the server diagnostic code, message, remediation, source attribution,
and status class instead of rewriting them locally.

Surface parity contract:

| Surface | Required projection | Parity tests |
|---|---|---|
| Direct CLI (`gc formula`, `gc sling`, `gc order`) | Prints the canonical diagnostic code, primary source, requirement source when distinct, host source when distinct, normalized requirement, host capability, and remediation | direct CLI golden stderr and exit-code tests |
| API-routed CLI | Forwards the typed Huma diagnostic without rewriting code, message, remediation, or source attribution | same formula fixture through direct and API-routed CLI produces byte-normalized diagnostics |
| Huma HTTP endpoints | Use typed response structs with `diagnostics []FormulaDiagnostic`; no `map[string]any`, `json.RawMessage`, or hand-written JSON | OpenAPI in-sync and generated-client compile tests |
| Generated TS client and dashboard | Render the generated diagnostic fields; never infer graph state or remediation from metadata strings | dashboard state tests using generated fixtures |
| Controller/order/convergence events | Wrap the same diagnostic in a registered typed payload and envelope source fields | event registry test plus repeated failure/dedup tests |

Registered event payload contract:

| Event constant | Emitted by | Payload |
|---|---|---|
| `formula.compile_failed` | named controller/background validation job only; not direct CLI/API preview | `FormulaDiagnosticPayload` plus producer id and occurrence count |
| `order.formula_compile_failed` | formula-backed order dispatch | `FormulaDiagnosticPayload` plus order id, scan generation, and occurrence count |
| `order.failed` during compatibility window | existing order failure consumers that do not yet understand `order.formula_compile_failed` | typed compatibility wrapper that embeds `FormulaDiagnosticPayload`, sets `reason=formula_compile_failed`, and never carries hand-written JSON |
| `convergence.formula_compile_failed` | convergence create/retry/speculative-wisp paths | `FormulaDiagnosticPayload` plus convergence id when one already exists |
| existing sling/API failure wrapper | sling launch and API launch conflicts | `FormulaDiagnosticPayload` embedded in the typed error body/event payload; Event Bus emission only when a durable producer owns the launch |

Every event constant above must be present in `events.KnownEventTypes` and have
`events.RegisterPayload(constant, FormulaDiagnosticPayload{})` or a typed
wrapper payload registered before it can ship. Envelope fields carry the city,
agent/session when known, and source operation; payload fields carry the formula
diagnostic. The deduplication key is the diagnostic `OnceKey` plus producer,
subject id, host capability, and config generation for automatic loops.
Interactive operator attempts are never hidden from their caller, but they do
not publish repeated Event Bus entries.

`order.failed` compatibility is explicit and temporary. While downstream
readers still subscribe to `order.failed`, order dispatch emits either only the
new `order.formula_compile_failed` event or a dual event pair declared in
`docs/release/formula-compiler-compatibility.yaml`. Dual emission must use one
shared payload constructor and one occurrence counter so event consumers cannot
see different diagnostics for the same failed scan. The compatibility row names
the removal phase, supported reader set, and fixture proving old readers still
surface the remediation.

Dashboard-less status is a first-class surface. The implementation exposes a
typed `gc formula diagnostics status --json` CLI and matching Huma read route
that project `FormulaDiagnosticRollup` rows for operators without the web
dashboard. The command exits `0` when it can read status, `1` for store/API
read failures, and never changes grouped diagnostic state. Fixtures compare the
CLI JSON, API JSON, generated TypeScript, dashboard state, and rollup event
payload for the same grouped disabled-host failure.

Non-API packages use typed constructors, not local payload literals:

```go
func NewFormulaDiagnosticPayload(d formula.Diagnostic) FormulaDiagnosticPayload
func NewOrderFormulaCompileFailedPayload(orderID string, generation uint64, state FormulaDiagnosticGroupState) OrderFormulaCompileFailedPayload
func NewConvergenceFormulaCompileFailedPayload(convergenceID string, state FormulaDiagnosticGroupState) ConvergenceFormulaCompileFailedPayload
```

These helpers live with the registered event payload types, are usable from
`cmd/gc`, order dispatch, convergence, controller, and retry/fanout packages,
and mechanically project `Diagnostic.Attribution`. A static guard fails on
formula compile-failure event payloads built with `map[string]any`,
`json.RawMessage`, hand-written JSON strings, stderr parsing, or root-metadata
reconstruction.

Required repeated-loop tests:

| Scenario | Assertion |
|---|---|
| same disabled-capability order scanned three times with unchanged config | one grouped `order.formula_compile_failed`, occurrence count `3`, no wisp roots, child beads, or fired metadata |
| same controller validation failure across config reload | first generation grouped separately from second generation |
| direct `gc formula validate` repeated three times | three synchronous stderr/API results, zero Event Bus events |
| API preview called by dashboard polling | typed response every call, zero Event Bus events |

<!-- REVIEW: added per diagnostics-parity-fixtures -->

Diagnostic parity is fixture-driven. Each fixture under
`internal/formula/testdata/diagnostics/` contains the source formula, optional
city config, expected normalized requirement, host capability, ordered
diagnostics, and projections for every surface:

```yaml
schema_version: 1
id: disabled-v2-host
source: formulas/graph.toml
host_capability: 1
expect:
      diagnostics:
        - code: formula.compiler_requirement_unsatisfied
          severity: error
          source_kind: host_capability
          source_path: city.toml
          source_key: daemon.formula_v2
          source_value: "false"
          requirement_source_kind: formula_requirement
          requirement_source_path: formulas/graph.toml
          requirement_source_key: requires.formula_compiler
          requirement_source_value: ">=2"
          host_source_kind: host_capability
          host_source_path: city.toml
          host_source_key: daemon.formula_v2
          host_source_value: "false"
          host_source_line: 12
          host_source_column: 1
          normalized_requirement: ">=2"
          host_capability: "1"
          remediation: enable [daemon] formula_v2 or choose a v1 formula
  cli_exit_validate: 2
  cli_exit_launch: 1
  api_status_preview: 400
  api_status_launch: 409
  event_constants: [order.formula_compile_failed]
  dashboard_group:
    key: formula.compiler_requirement_unsatisfied|review-loop|order_dispatch|ga-order|city.toml|daemon.formula_v2|false|formulas/graph.toml|requires.formula_compiler|>=2|city.toml|daemon.formula_v2|false|>=2|1|sha256:...
    title: Formula compiler requirement unsatisfied
    occurrence_count: 1
```

The `disabled-v2-host` fixture is a single source of truth. Direct CLI stderr,
CLI exit code, API-routed CLI output, Huma JSON, generated TypeScript fixture,
dashboard state, and `order.formula_compile_failed` payload are all generated
or asserted from this one fixture. A change that updates only one surface is a
test failure. Byte normalization is limited to path separators and timestamps;
codes, source keys, source values, remediation, host provenance, HTTP status,
CLI exit class, and event payload fields must match exactly.

The golden tests compare direct CLI, API-routed CLI, Huma response bodies,
generated TypeScript fixtures, dashboard rendering state, and event payloads.
Every projection must preserve `Code`, canonical message, remediation, primary
source path/key/value, requirement source fields, host source fields, normalized
requirement, host capability, severity, and `OnceKey`. Surfaces may add local
context such as HTTP status or producer id, but they may not rewrite or drop
formula diagnostic fields.

Dashboard-visible grouped failure state is explicit. Background producer
failures appear as grouped rows keyed by diagnostic `OnceKey`, producer,
subject id, host capability, and config generation. A config-generation change
starts a new group; a process restart may clear in-memory suppression but must
not delete persisted failure history attached to the producer or root. Preview
and polling failures are visible to the caller but do not create Event Bus
entries or durable grouped failure rows.

<!-- REVIEW: added per attempt-80-operator-rollup-surface -->

Operators get one bounded remediation surface for host/config failures. Every
background producer that encounters `formula.compiler_requirement_unsatisfied`
or `formula.host_capability_invalid` writes or updates a typed rollup row before
returning:

```go
type FormulaDiagnosticRollup struct {
    SchemaVersion    int
    RollupKey        string
    RollupClass      string // host_config, formula_source, pack_source, internal
    Title            string
    Remediation      string
    ConfigGeneration uint64
    ProducerCount    int
    SubjectCount     int
    OccurrenceCount  int
    FirstSeenAt      time.Time
    LastSeenAt       time.Time
    LastEmittedAt    time.Time
    BurstBudget      FormulaDiagnosticBurstBudget
    Children         []FormulaDiagnosticRollupChild
}

type FormulaDiagnosticRollupChild struct {
    Producer       string
    SubjectID      string
    Formula        string
    Diagnostic     FormulaDiagnosticPayload
    OccurrenceCount int
    LastSeenAt     time.Time
}
```

<!-- REVIEW: added per attempt-90-diagnostic-budget-contract -->

Burst budget and producer policy are typed configuration owned by the producer
surface, while the payload schema is owned by the formula diagnostic package.
No producer may invent local cadence fields or persist warning counters inside
accepted artifacts.

```go
type FormulaDiagnosticBurstBudget struct {
    SchemaVersion       int
    Window              time.Duration
    InitialEvents       int
    SummaryEvery        time.Duration
    MaxEventsPerWindow  int
    MaxChildrenPerRollup int
    MaxStoredExamples   int
    ResetOnConfigGeneration bool
    ResetOnFormulaContentHash bool
    ResetOnSubjectID bool
    ResetOnProducerPolicyVersion bool
}

type FormulaDiagnosticProducerPolicy struct {
    SchemaVersion   int
    Producer        string
    PolicyVersion   int
    EmitsEvents     bool
    EmitsRollups    bool
    PersistsWarnings bool
    DefaultBudget   FormulaDiagnosticBurstBudget
    PayloadOwner    string // formula, order, convergence, controller, fanout, retry
}
```

Default budgets are fixture-locked:

| Producer | Events | Budget |
|---|---|---|
| direct CLI/API preview/dashboard polling | no Event Bus events and no durable rollup | synchronous response every call |
| order dispatch | first event per `(subject, OnceKey, host, config generation)`, then one summary every 15 minutes | window 15m, `InitialEvents=1`, `MaxEventsPerWindow=2`, `MaxChildrenPerRollup=100` |
| controller validation | same as order dispatch | window 15m, summary event only after occurrence count changes |
| convergence | first event for create/retry/speculative failure, then summary every 15 minutes while blocked | window 15m, `MaxStoredExamples=20` |
| fanout/retry/on_complete/workflow-finalize | first event per failing subject, then summary every 30 minutes | window 30m, grouped by parent root or workflow root |

The policy row is included in every grouped diagnostic fixture. Reset semantics
are exact: a successful config reload always starts a new group because
`ConfigGeneration` changes; a failed reload never resets; formula content hash,
subject id, producer id, or producer policy version changes reset the budget;
process restart reloads persisted group and rollup state before applying
in-memory suppression. A config flap `false -> true -> false` creates three
observable lifecycle transitions: failing group, resolved group, new failing
group with a new config generation. The old failing group remains historical
and is not edited into the new one.

Rollup lifecycle is explicit:

| State | Transition in | Transition out |
|---|---|---|
| `active` | first fatal occurrence or warning occurrence that producer policy persists | `resolved`, `expired`, or `abandoned` |
| `resolved` | next successful scan for the same subject and formula/config source | new `active` group if the diagnostic recurs with a new key |
| `expired` | retention cleanup after no matching subject remains and release evidence no longer needs the row | none |
| `abandoned` | operator closes or waives the subject with a reason | new `active` group only if the subject is reopened or recreated |

`FormulaDiagnosticRollup` stores `LifecycleState`, `PolicyVersion`,
`ResolvedAt`, `ResolvedBy`, `ResolutionReason`, `SuppressedEventCount`,
`DroppedChildCount`, `ExampleDigest`, and `ETag`. Children beyond
`MaxChildrenPerRollup` increment `DroppedChildCount` and contribute to
`ExampleDigest`; they do not append unbounded metadata or Event Bus payloads.
Fixtures cover repeated scans, restart before the second scan, successful and
failed reload, config flaps, many disabled-host subjects, legacy-contract
warning parity, immutable accepted warning snapshots, mutable producer warning
counters, CAS loss, and grouped-state write failure.

The rollup grouping key is
`(rollup_class, code, remediation, host_source_path, host_source_key,
host_source_value, host_capability, config_generation)`. Children preserve the
producer, subject id, formula, requirement source path/key/value, content hash,
source line/column, requirement line/column, and host line/column. A disabled
host therefore appears as one dashboard/API row with bounded child details
instead of N unrelated order, convergence, retry, fanout, and controller rows.

Rollup state is producer-owned diagnostic state, not accepted-artifact state.
Accepted artifacts remain immutable and store only the diagnostics and warnings
accepted at compile time. Repeat occurrence counters, last-seen timestamps,
event cadence etags, burst budgets, and config-generation reset state live in
the producer diagnostic subject or workflow root through
`FormulaDiagnosticGroupState` and `FormulaDiagnosticRollup`. A background
warning or fatal occurrence may update those mutable records; it must not edit
an existing accepted artifact.

Event cadence is fixture-locked:

| Scenario | Event behavior | State behavior |
|---|---|---|
| first occurrence in a new config generation | append one typed producer event and one rollup event when the producer policy enables events | create group and rollup with `OccurrenceCount=1` |
| repeated occurrence inside the burst window | no Event Bus append after the configured budget is spent | increment group and rollup counters through CAS |
| process restart | reload persisted group and rollup before applying LRU suppression | counters continue from stored state |
| successful config reload, even with equivalent values | new rollup key because `ConfigGeneration` changed | old row remains historical; new row starts at one |
| failed config reload | no generation change | current row continues |
| formula/config fixed | clear or mark resolved through producer cleanup | accepted artifacts are unchanged |

The Huma response, generated TypeScript type, dashboard grouping, direct CLI
status view, API-routed CLI status view, and event payload all project the same
rollup fields. Golden fixtures for disabled host capability cover direct CLI,
API-routed CLI, Huma JSON, generated TypeScript, dashboard state, producer
event payload, rollup event payload, restart/reload behavior, burst-budget
suppression, and zero protected writes.

<!-- REVIEW: added per attempt-54-grouped-diagnostic-state -->

Background grouped diagnostic state is a typed producer-owned record:

```go
type FormulaDiagnosticGroupState struct {
    SchemaVersion     int
    Producer          string
    SubjectID         string
    ScanSeriesID      string
    OnceKey           string
    HostCapability    string
    ConfigGeneration  uint64
    Diagnostic        FormulaDiagnosticPayload
    OccurrenceCount   int
    FirstSeenAt       time.Time
    LastSeenAt        time.Time
    LastEmittedAt     time.Time
    LastResetReason   string
    LastWriteError    string
}
```

The durable owner is the producer package that would otherwise have written
runtime state: order dispatch owns order formula groups, convergence owns
convergence formula groups, controller validation owns controller formula
groups, and retry/fanout/finalize paths own their own subject groups. The
shared formula package owns the payload shape and key construction only; it
does not decide producer policy. State is persisted through the Task Store on
the producer subject bead or the workflow root bead, never in PID files, lock
files, or sidecar status files.

<!-- REVIEW: added per attempt-79-order-diagnostic-subject -->

The current `beads.Store` contract has `Create`, `Get`, `Update`,
`SetMetadataBatch`, `List`, and metadata queries, but no compare-and-swap,
unique metadata constraint, or atomic create-if-absent operation. Grouped
diagnostics therefore cannot be implemented by a read-list-create loop against
the current API; that would still duplicate groups under concurrent order
dispatchers and could lose occurrence increments on restart or write failure.
Before order/background diagnostics ship, the Task Store API must add and test
two generic store operations:

```go
type MetadataCASResult struct {
    Bead    beads.Bead
    Swapped bool
}

func EnsureSingletonByMetadata(ctx context.Context, store beads.Store, bead beads.Bead, unique map[string]string) (beads.Bead, bool, error)
func SetMetadataCompareAndSwap(ctx context.Context, store beads.Store, id string, expected map[string]string, set map[string]string) (MetadataCASResult, error)
```

`EnsureSingletonByMetadata` atomically returns the existing open or closed bead
whose metadata contains the unique key set, or creates exactly one such bead.
`SetMetadataCompareAndSwap` writes a batch only when the observed metadata
still contains the expected generation/count/hash values, and returns the
fresh bead when another writer won. `MemStore`, `FileStore`, `BdStore`, and
exec-backed stores must all pass the same conformance tests; an implementation
that cannot provide the atomicity must report an unsupported store diagnostic
and the producer must fail closed before protected writes. Multi-key updates
used for group state are encoded as one deterministic metadata blob plus an
etag so external stores do not expose partial group records.

Order dispatch uses a real diagnostic subject, not the wisp root and not the
order-run history bead. The subject is a singleton bead with metadata:

| Metadata key | Value |
|---|---|
| `gc.diagnostic_kind` | `formula-diagnostic-group` |
| `gc.producer` | `order_dispatch` |
| `gc.subject_id` | `order:<target scope>:<scoped order name>` |
| `gc.order_name` | scoped order name |
| `gc.formula_name` | selected formula |
| `gc.diagnostic_group_key` | SHA-256 of producer, subject id, formula, OnceKey, host capability, and config generation |

This bead is the durable subject for repeated disabled-host or malformed-formula
failures before any order wisp exists. It is allowed diagnostic state; it is
not labeled `order-run:<name>`, does not advance last-run/cooldown history,
does not mark the order fired, and does not block future successful dispatch
after the formula/config is fixed. The grouped state payload, occurrence
count, first/last seen timestamps, scan series id, last write error, and event
cadence etag are stored on that subject through `SetMetadataCompareAndSwap`.
Concurrent producers retry on CAS loss by reloading the subject and applying
the increment to the latest count.

<!-- REVIEW: added per attempt-73-diagnostic-state-boundary -->

"Zero protected writes" does not mean "no diagnostic-state writes." On fatal
formula diagnostics, the only allowed writes before returning are the typed
producer diagnostic group record and its corresponding typed failure event
when the producer policy emits one. Protected writes remain forbidden:
formula/wisp roots, child beads, dependencies, hooks, convoys, tracking state,
fired-order metadata, retry metadata, fanout state, convergence state,
artifact refs, workflow-root metadata, and generated control children. If the
diagnostic-state write fails, the producer records or returns that failure and
still performs zero protected writes. Fixtures must assert both sides: the
diagnostic group can be created or updated, and every protected boundary is
unchanged.

Required producer API:

```go
type FormulaDiagnosticGroupKey struct {
    Producer         string
    SubjectID        string
    OnceKey          string
    HostCapability   string
    ConfigGeneration uint64
}

func UpsertFormulaDiagnosticGroup(ctx context.Context, store beads.Store, key FormulaDiagnosticGroupKey, diagnostic FormulaDiagnosticPayload, now time.Time) (FormulaDiagnosticGroupState, error)
func ClearFormulaDiagnosticGroup(ctx context.Context, store beads.Store, key FormulaDiagnosticGroupKey, reason string, now time.Time) error
```

The write order is fixed. On a fatal background diagnostic, the producer first
computes the group key, then upserts grouped state in the Task Store, then
emits the typed failure event when policy says an event is due, then returns
without performing the protected write. If grouped-state upsert fails, the
producer returns or emits a typed producer failure with `LastWriteError` and
still performs zero formula root, child, hook, convoy, tracking, retry,
convergence, fired-order, or artifact-ref writes. On restart or controller
handoff, producers reload grouped state before applying in-memory LRU
suppression. Cleanup is explicit: a group is cleared only when the subject is
closed, the formula/config source changes, the diagnostic no longer occurs on
the next successful scan, or an operator invokes the producer's normal cleanup
path.

<!-- REVIEW: added per attempt-79-order-preflight-ordering -->

Formula-backed order dispatch has a stricter sequence than other background
producers:

1. Resolve the target store, host-capability snapshot, formula source, and scan
   series id.
2. Check trigger due-ness and open work without creating an order-run bead.
3. Compile with `CompileWithResult`, accept the result, and validate the
   artifact for `order_dispatch`.
4. On any fatal formula diagnostic, ensure the diagnostic-subject singleton,
   CAS-update grouped state, emit `order.formula_compile_failed` only when
   cadence allows, and return.
5. Only after acceptance succeeds, create or claim the order-run tracking bead
   with a singleton metadata key for this scan series.
6. Record `order.fired`, stamp fired/order-run metadata, apply route metadata,
   create the wisp root and children, hook work, convoy links, and user-visible
   history.

No `order.fired` event, fired metadata, order-run tracking bead, wisp root,
child bead, dependency, hook, convoy, route metadata, or last-run/cooldown
history write may happen before step 5. If creating or claiming the order-run
tracking bead races, the loser reloads the singleton and skips dispatch. If
the diagnostic-subject write fails, the order returns a typed producer failure
and still does not create tracking or runtime state. Fixtures must cover:
three repeated scans, two concurrent dispatchers, restart after the first
failure, CAS loss and retry, grouped-state write failure, successful recovery
after enabling the host, burst-budget event cadence, config generation change,
formula source change, and config flap `false -> true -> false`; every failure
fixture snapshots fired metadata, wisp roots, children, hooks, convoys, route
metadata, tracking beads, and last-run history as unchanged.

Ownership and update rules:

| Concern | Contract |
|---|---|
| scan series | one producer scan tick or controller validation pass; retries in the same tick update the same `ScanSeriesID` |
| grouping key | `(producer, subject id, OnceKey, host capability, config generation)` |
| occurrence count | incremented for every repeated failure before any durable write would have occurred |
| event cadence | first occurrence appends the typed event; later occurrences update grouped state and may append only when producer policy says to emit a summary |
| config reload | successful reload increments `ConfigGeneration` and starts a new group; failed reload keeps the old group |
| unrelated config reload | if generation changes but the formula source and host capability are equivalent, a new group is still created so diagnostics remain attributable to the loaded config snapshot |
| process restart/controller handoff | reload persisted producer/root state before applying LRU suppression; never infer absence from an empty in-memory cache |
| grouped-state write failure | append or return a typed producer failure with `LastWriteError`; do not proceed to formula root, child, hook, convoy, retry, convergence, or fired-order writes |
| dashboard/API projection | Huma responses and generated TypeScript expose `occurrence_count`, `first_seen_at`, `last_seen_at`, `config_generation`, `producer`, `subject_id`, and the nested typed diagnostic |
| direct CLI/API-routed CLI | synchronous validation does not create grouped state, but API-routed CLI must preserve the grouped fields when it asks for background producer status |

Required fixtures cover repeated scans, process restart before the second scan,
controller handoff, successful and failed config reload, grouped-state write
failure, direct CLI repetition, API-routed CLI parity, Huma projection,
dashboard rendering, and report-mode JSON. Every fixture asserts event count,
occurrence count, source attribution, and zero writes when the diagnostic is
fatal.

<!-- REVIEW: added per DR48-operator-surface-decisions -->

Final operator-surface decisions:

| Decision | Resolution |
|---|---|
| launch HTTP status | `409 Conflict` for unsatisfied or conflicting formula requirements during launch; `412 Precondition Failed` is reserved for future conditional requests with explicit precondition headers |
| validation HTTP status | `400 Bad Request` for formula source/config diagnostics; `500` only for internal I/O or invariant failures |
| warning LRU | per-city, per-producer in-memory LRU capped at 4096 keys; eviction is observable but not release evidence |
| CLI suppression key | `OnceKey` plus command invocation; warnings repeat across separate commands, fatal diagnostics never hide from the caller |
| CLI warning controls | `--quiet` suppresses warning display only on validation/show/report commands that still return warning diagnostics in JSON; launch commands keep fatal diagnostics visible and may use `--warnings=once` for CI logs |
| report schema versioning | every JSON report has `schema_version`, `producer`, `generated_at`, and stable diagnostic/migration-hint arrays |
| non-formula `contract` terminology | docs may use "contract" only for legacy formula aliasing or unrelated non-formula APIs that explicitly say they are not compiler requirements |
| dashboard grouping | group by operator-fixable host/config diagnostics separately from author-fixable formula/pack diagnostics; grouping is derived from typed diagnostic fields |
| accepted proof nonce | random 16-byte nonce is part of the identity hash input and is never accepted by itself as proof |

<!-- REVIEW: added per attempt-79-operation-status-warning-pin -->

HTTP status is operation-aware and fixture-locked:

| Diagnostic family | Preview/validate/report | Launch or durable producer API | Background producer |
|---|---|---|---|
| malformed formula source, wrong type, unknown axis, unsupported future formula capability | `400 Bad Request` with typed diagnostics | `400 Bad Request`; no protected writes | grouped diagnostic state plus producer failure event when configured |
| `formula.compiler_requirement_conflict` | `400 Bad Request` | `409 Conflict`; the request selected a formula whose declared compatibility state conflicts with the launch precondition | grouped diagnostic state; no fired/runtime writes |
| `formula.compiler_requirement_unsatisfied` | `400 Bad Request` because validation input is syntactically valid but cannot be satisfied by the supplied config | `409 Conflict`; current city config conflicts with the selected formula requirement | grouped diagnostic state; no fired/runtime writes |
| `formula.host_capability_invalid` from production construction | `500 Internal Server Error` | `500 Internal Server Error`; no protected writes | typed producer failure with `LastWriteError` or invalid-host diagnostic |
| warning-only legacy alias/version diagnostics | `200 OK` with warnings when JSON/preview surface supports them | operation status unchanged; warning persisted in accepted artifact when accepted | no Event Bus warning payload; optional grouped warning counter only |

Warning messages and remediation are canonical strings across CLI, API-routed
CLI, Huma JSON, generated TypeScript fixtures, dashboard state, reports, and
accepted artifacts:

| Code | Message | Remediation |
|---|---|---|
| `formula.contract_deprecated` | `contract = "graph.v2" is deprecated` | `use [requires] formula_compiler = ">=2"` |
| `formula.version_deprecated` | `formula version is legacy metadata` | `use pack version, ref, or SHA for artifact identity` |
| `formula.version_misuse` | `formula version does not enable compiler capability` | `add [requires] formula_compiler = ">=2" for v2-only syntax` |

No surface may append local advice to these strings unless it uses a separate
typed field such as docs link or producer id. Golden fixtures compare the
message, remediation, warning `OnceKey`, source path/key/value, requirement
source, content hash, and persistence decision for direct CLI, API-routed CLI,
Huma response, dashboard, order dispatch, convergence, fanout, retry/controller
validation, and release reports.

<!-- REVIEW: added per DR48-durable-alias-evidence -->

<!-- REVIEW: added per attempt-78-warning-persistence-cadence -->

Accepted warnings have one durable persistence shape. A warning that survives
acceptance is stored as `AcceptedWarningRecord` inside the accepted compile
artifact; background producers may also mirror a grouped counter on their
subject bead for dashboard/reporting cadence, but the artifact record remains
the source of truth for release evidence:

```go
type AcceptedWarningRecord struct {
    SchemaVersion    int
    Diagnostic       FormulaDiagnosticPayload
    OnceKey          string
    Formula          string
    Producer         string
    SubjectID        string
    ContentHash      string
    RequirementSource string
    HostCapability   string
    ConfigGeneration uint64
    FirstSeenAt      time.Time
    LastSeenAt       time.Time
    OccurrenceCount  int
    LastSurface      string
}
```

The grouping key for accepted warnings is
`(code, formula, source path, source key, source value, content hash,
producer, subject id, config generation)`. First occurrence writes the warning
record with `OccurrenceCount=1`. Repeat scans update `LastSeenAt` and count;
successful config reload starts a new group because the host/source
attribution changed. Host toggles update the host-capability field but do not
erase the accepted warning from the artifact. Process restarts reload producer
state from the Task Store and accepted artifacts before applying the in-memory
LRU. LRU eviction can increase display frequency, but it is never release
evidence and never deletes warning records.

Warning fixtures are required for `formula.contract_deprecated`,
`formula.version_deprecated`, and `formula.version_misuse` across direct CLI,
API-routed CLI, Huma preview, dashboard state, order dispatch, convergence,
fanout, retry/controller validation, and release reports. Each fixture asserts
whether the surface displays the warning, persists the warning, increments a
producer counter, emits no Event Bus warning payload, and contributes to alias
removal or version-misuse release counters.

Legacy-alias evidence is durable and recomputable. Process-local warning
suppression is only a UX optimization and is never used as release evidence.
The authoritative sources for accepted legacy `contract = "graph.v2"` usage
are:

| Source | Evidence retained |
|---|---|
| accepted compile artifact | warning diagnostic with source path/key/value, requirement source `contract` or `dual`, compile identity, provenance, and producer |
| workflow-root metadata | requirement source, compile artifact ref, formula name, pack binding, and content hash |
| order/convergence/fanout/retry producer state | grouped fatal or warning counters keyed by `OnceKey`, producer, subject id, host capability, and config generation when a producer accepts or rejects a formula |
| release validation reports | recomputed counts for legacy-only, dual-declared, requires-only, external, shadowed, and background-accepted alias formulas |

Alias removal gates read these durable sources and current pack validation
output, not logs, stderr, Event Bus warning payloads, or in-memory LRU state.
Background producers that accept a legacy alias but suppress display still
persist the diagnostic in the accepted artifact or producer state. After a
restart, the next validation/report run must recover the same accepted-alias
counts by scanning root metadata and accepted artifacts.

## Persisted Metadata And Provenance

<!-- REVIEW: added per metadata-provenance-contract -->

Workflow roots created from compiler-v2 formulas must be dual-stamped during
the compatibility window:

| Metadata key | Value | Status |
|---|---|---|
| `gc.formula_compiler_requirement` | `>=2` | Canonical |
| `gc.formula_requirement_source` | `requires`, `contract`, or `dual` | Canonical |
| `gc.formula_compiler_capability` | `2` | Canonical |
| `gc.formula_contract` | `graph.v2` | Deprecated compatibility stamp |

Future-readable root metadata:

| Metadata key | Value |
|---|---|
| `gc.formula_requirements_schema_version` | `1` for this v0 root metadata schema |
| `gc.formula_accepted_artifact_version` | accepted artifact serialization version |
| `gc.formula_min_reader_capability` | minimum compiler capability required before graph-specific old-reader writes |
| `gc.formula_requirement_axes` | comma-separated manifest such as `formula_compiler` |
| `gc.formula_unsupported_axes` | present only when a reader projected unknown axes for visibility |

Auditable provenance for every new workflow root:

| Metadata key | Value |
|---|---|
| `gc.formula_name` | resolved formula name |
| `gc.formula_source_path` | winning formula file path |
| `gc.formula_pack_name` | containing pack name when known |
| `gc.formula_pack_source` | local path, git URL, registry source, or `builtin` |
| `gc.formula_pack_ref` | requested tag, branch, semver, or SHA when known |
| `gc.formula_pack_revision` | locked revision or content hash used for compilation |
| `gc.formula_import_binding` | winning direct import binding name when the formula came from an import |
| `gc.formula_import_binding_path` | author-editable TOML path such as `[imports.review]` |
| `gc.formula_parent_binding` | parent binding path for transitive imports, when applicable |
| `gc.formula_lockfile_key` | lockfile entry key that resolved the pack revision, when applicable |
| `gc.formula_reproducibility` | `pinned`, `local-dirty`, `local-clean`, or `local-not-reproducibly-pinned` |
| `gc.formula_compile_artifact` | durable path or bead id for the compile result/provenance snapshot when the metadata would exceed practical size |

`CompileResult.Provenance` has a structured shape, not a formatted string:

```go
type Provenance struct {
    FormulaName     string
    SourcePath      string
    LayerName       string
    LayerPriority   int
    ImportBinding   string
    ImportBindingPath string
    Pack            PackProvenance
    Imports         []ImportProvenance
    ContentHash     string
    Reproducibility string
    Dirty           bool
}

type PackProvenance struct {
    Name                string
    Source              string
    BindingName         string
    BindingPath         string
    RequestedRef        string
    RequestedConstraint string
    LockedRevision      string
    LockfileKey         string
    RequiresGC          string
    Dirty               bool
}

type ImportProvenance struct {
    ParentPack          string
    ParentBinding       string
    BindingName         string
    BindingPath         string
    ImportSource        string
    RequestedRef        string
    RequestedConstraint string
    LockedRevision      string
    LockfileKey         string
    RequiresGC          string
    ContentHash         string
    Dirty               bool
    LayerPriority       int
    ContributedPath     string
}
```

<!-- REVIEW: added per attempt-73-source-provenance-package-home -->

Package ownership for source and provenance types is fixed:

| Type family | Package home | Rule |
|---|---|---|
| `ResolvedFormulaSource`, `PackProvenance`, `ImportProvenance` construction | `internal/packman` and resolver/import loading | owns pack acquisition, lockfile schema, binding identity, dirty state, and content hashes |
| `formula.Provenance` copy in compile results and accepted artifacts | `internal/formula` | consumes resolver-owned source data and freezes it into compiler artifacts |
| workflow-root query facts | `internal/sourceworkflow` | reads persisted formula metadata and artifact refs for visibility only |
| API/dashboard/release projections | `internal/api`, dashboard generated client, release commands | project typed provenance; never reconstruct it from paths or messages |

No package outside those homes may infer pack provenance from a staged path,
formula name, root metadata, or diagnostic text.

<!-- REVIEW: added per provenance-owner-v21 -->

Formula resolution produces a typed `ResolvedFormulaSource`; compilation does
not rediscover pack context from paths:

```go
type ResolvedFormulaSource struct {
    FormulaName       string
    OriginalPath      string
    StagedPath        string
    LayerName         string
    LayerPriority     int
    ImportBinding     string
    ImportBindingPath string
    Pack              PackProvenance
    Imports           []ImportProvenance
    RequestedRef      string
    RequestedConstraint string
    LockedRevision    string
    LockfileKey       string
    RequiresGC        string
    ContentHash       string
    Dirty             bool
    TransitiveSources []ResolvedFormulaSource
}
```

The resolver/import layer owns `ResolvedFormulaSource` construction. The
formula compiler consumes it and copies the relevant fields into
`CompileResult.Provenance`. No caller may reconstruct pack name, requested ref,
locked revision, dirty status, or transitive import chain from a staged path,
formula name, or bead metadata after compilation.

Import binding identity is part of provenance because it is the author-editable
surface. Diagnostics, validation JSON, persisted compile artifacts,
`migration_hints`, and requirement-diff reports must name the binding path
such as `[imports.review]`, the binding name, the parent binding for transitive
imports, and the lockfile key used to recover the resolved revision. A staged
path or bare pack source URL is not enough to tell an external author which
line to edit.

`ContentHash` is the lowercase hex SHA-256 of canonical source bytes after
formula resolution, before runtime variable substitution, and before any graph
transform. For a multi-file contribution such as expansion/aspect imports, the
hash input is a length-prefixed manifest of each original source path,
locked revision or local dirty marker, file mode, and raw bytes in deterministic
resolver order. The hash excludes absolute staging paths and generated
diagnostics so the same pinned pack content produces the same value across
machines.

Layer resolution must preserve pack binding before symlink staging. A staged
winner path under `.beads/formulas/` is not enough provenance by itself; the
compile result records the original layer, pack, import source/ref, locked
revision or local hash, dirty state, transitive import chain, and layer
priority that selected the winning file.

<!-- REVIEW: added per pack-provenance-compatibility -->

Pack compatibility is evaluated before formula selection, staging, or compile.
For every pack origin - built-in, local path, installed pack, git URL, registry
source, direct import, and transitive import - the resolver reads `[pack]
requires_gc` and rejects an incompatible pack before any formula from that pack
can win layer resolution. The rejection is a pack-resolution diagnostic, not a
formula diagnostic, but the provenance report must include the pack source,
requested ref or version constraint, locked revision, and parent import chain
that caused it.

Raising a formula's normalized `requires.formula_compiler` minimum is a pack
compatibility event. A released pack may raise that minimum only when the same
change raises or verifies `[pack] requires_gc` so consumers below the minimum
binary floor reject the pack at resolver time instead of selecting a formula
they cannot compile. This applies to first-party packs, examples, local packs
prepared for release, and external pack validation output.

Import upgrades must report requirement deltas before the upgraded pack is
accepted:

```bash
gc formula validate --pack-path ./packs/acme --all --requirement-diff old.lock new.lock --json
```

The JSON diff contains old and new pack source/ref/locked revision, each
formula whose normalized compiler requirement changed, the old and new
requirement source, content hash changes, transitive import changes,
`requires_gc` floor deltas, and whether the change is safe for the active
binary. CI for first-party imports fails when an import upgrade raises a
compiler requirement without the corresponding pack-floor and docs/release
artifact updates.

<!-- REVIEW: added per DR41-packv2-lockfile -->
<!-- REVIEW: added per DR48-packman-lockfile-provenance -->

PackV2 lockfile semantics are owned by `internal/packman/lockfile.go`,
`internal/packman/check.go`, `internal/packman/install.go`, and
`docs/packv2/doc-packman.md`. There is no separate `internal/packv2`
lockfile owner in the current tree. Formula validation is a reader and
reporting consumer of packman state; it does not rewrite `packs.lock`,
materialize cache entries, or introduce a competing schema.

The current lockfile format is schema `1` and is read from the root city's
`packs.lock`:

```toml
schema = 1

[packs."https://example.com/acme.git"]
version = "v1.4.0"
commit = "git-sha"
fetched = "2026-05-11T00:00:00Z"
```

`commit` is the current lockfile's locked revision. If formula requirement
validation needs durable `content_hash`, `requires_gc`, parent import, or
binding fields in the lockfile itself, that change must be made through
`internal/packman` with an explicit schema bump, backward-compatible reader
tests for schema `1`, deterministic writer tests, and documentation updates in
`docs/packv2/doc-packman.md`. Until such a schema bump lands, formula
validation derives binding identity from the resolved `[imports.<binding>]`
graph and cache content, and reports when the schema cannot prove a release
gate.

<!-- REVIEW: added per DR55-packman-schema2-prerequisite -->

Any release gate that relies on durable content hashes, parent binding
identity, transitive import identity, or lockfile-stored `requires_gc` evidence
has a hard prerequisite: `internal/packman` must first land lockfile schema `2`
with those fields, deterministic read/write tests, migration tests from schema
`1`, and `docs/packv2/doc-packman.md` updates. Before schema `2`, validation
commands may produce advisory provenance reports from resolver state and cache
content, but `--alias-removal-gate`, first-party requires-only conversion,
pack-floor enforcement for imported packs, and external pinned-pack support
expiration must fail closed when their evidence would depend on a field schema
`1` cannot prove.

<!-- REVIEW: added per attempt-73-packman-provenance-prerequisite -->

Packman schema `2`, or an equivalent packman-owned provenance contract, is a
rollout prerequisite for resolver/import enforcement. A formula PR may not
enforce imported-pack floors, expire external pinned-pack support, or run an
alias-removal gate that depends on provenance until packman owns these fields:
`schema_version`, `source`, `requested_ref`, `locked_revision`,
`content_hash`, `requires_gc`, `binding_name`, `binding_path`,
`parent_binding`, `transitive_depth`, `dirty`, `fetched_at`, and
`registry_mirror`. Schema `1` behavior is fail-closed for release gates and
read-only for compatibility reports.

<!-- REVIEW: added per attempt-78-packman-provenance-phase -->

Packman provenance readiness is its own rollout unit before pack-floor
enforcement:

| Readiness item | Contract |
|---|---|
| PR home | `internal/packman`, `cmd/gc` pack validation commands, `docs/packv2/doc-packman.md`, and release artifact schemas |
| owner | packman owner, with release-captain approval for release-gate schema fields |
| saved artifacts | schema migration report, schema-1 read report, schema-2 write/read round-trip report, requirement-diff sample, external pinned-pack inventory sample |
| schema-1 behavior | fail closed for alias removal, imported-pack floor enforcement, first-party requires-only conversion, and external support expiration; advisory reports are allowed only when marked `evidence_incomplete` |
| rollback unit | disable resolver/import enforcement and keep reports advisory; never rewrite schema-2 locks back to schema 1 without a packman-owned migration command |
| blocking command | `gc formula validate --all-packs --provenance --json` plus `go test ./internal/packman ./internal/formula` |

Pack-floor enforcement may not share a PR with the first schema-2 writer unless
the PR also includes schema-1 rollback/read compatibility tests and saved
evidence artifacts. If schema `2` is replaced by an equivalent provenance
contract, that contract must still live under packman ownership and expose the
same fields to formula validation through typed APIs, not through path parsing.

External author commands are stable JSON surfaces:

| Command | Success exit | Diagnostic exit | Required fields |
|---|---|---|---|
| `gc formula validate --pack-path <path> --all --json` | `0` when every selected formula validates | `1` acquisition/I/O, `2` formula or pack diagnostics | local path, VCS revision, dirty state, content hash, formula rows, diagnostics, migration hints |
| `gc formula validate --pack-source <url> --ref <ref> --all --json` | `0` only when acquisition is reproducible or diagnostics are absent in non-gate mode | `1` auth/network/acquisition, `2` reproducibility or formula diagnostics | source URL, requested ref, locked revision, registry mirror, credential mode, offline mode, content hash |
| `gc formula validate --all-packs --provenance --json` | `0` when inventory is readable | `1` unreadable inventory, `2` validation diagnostics | first-party/external classification, binding path, parent binding, lockfile key, shadowed formulas |
| `gc formula validate --requirement-diff old.lock new.lock --json` | `0` when deltas are compatible with the active binary | `1` unreadable locks, `2` raised requirements without matching pack floor or release artifacts | old/new lock entries, requirement deltas, `requires_gc` deltas, transitive import changes |
| `gc formula validate --all-packs --legacy-contract-report --json` | `0` when release-gate criteria pass for the selected mode | `1` load failure, `2` legacy/support blockers | legacy-only, dual-declared, requires-only, external, shadowed, background-accepted alias counts |

Direct-pack validation has a synthetic binding identity. When a user runs
`--pack-path` or `--pack-source --ref` outside an installed city/import graph,
validation creates a non-persistent binding:

```text
binding_name = "__direct_pack__"
binding_path = "<direct-pack>"
parent_binding = ""
lockfile_key = "direct:<source>@<locked_revision-or-content-hash>"
```

The synthetic identity is included in diagnostics, provenance, migration
hints, requirement-diff output, external-support rows, and content-hash
manifests. It is never written to `packs.lock`, never treated as an installed
import, and never reused as durable root provenance. Release gates may consume
direct-pack reports only when `locked_revision` or a local clean VCS revision
plus content hash is present; otherwise the report is development evidence and
not compatibility proof.

Resolver-to-compiler provenance is a named Phase 2 deliverable. The compiler
accepts a `ResolvedFormulaSource` from packman/resolver code and records its
binding identity, requested ref, locked revision, transitive imports,
`requires_gc`, dirty state, content hash, and synthetic direct-pack identity
when applicable. A formula validation command that cannot produce those fields
returns a provenance diagnostic instead of constructing partial provenance from
paths.

Worked external-pack fixtures are mandatory:

| Fixture | Required assertion |
|---|---|
| pinned SHA | reproducible when the object exists, content hash is recorded, and requirement diagnostics are clean |
| tag or branch | non-reproducible until lockfile schema `2` records locked revision and content hash |
| local dirty path | development validation may warn; release gate fails with `local-dirty` |
| transitive import | report names direct binding, parent binding, child binding, and lockfile key |
| shadowed formula | losing external formula with legacy-only or stricter requirement is reported even when not selected |
| unreachable source | exit `1` unless an offline lock/cache policy is explicitly saved; never treated as zero risk |
| registry mirror | report names mirror URL, resolved package identity, immutable revision, and content hash |

Validation input contracts are fixed:

| Input | Required behavior |
|---|---|
| `--pack-source <url> --ref <sha>` with no lockfile | reproducible only when `<sha>` is immutable and content hash is recorded in the report |
| `--pack-source <url> --ref <branch-or-tag>` with no lockfile | `local-not-reproducibly-pinned` or remote non-reproducible diagnostic; not acceptable for release gates |
| `--pack-path <local>` inside clean VCS | `local-clean` with VCS revision and content hash; release gate needs explicit approval |
| `--pack-path <local>` dirty or outside VCS | warning or failure according to gate; never treated as pinned |
| direct import with lock entry | use the lock entry's `locked_revision` and content hash before formula selection |
| transitive import without lock entry | non-reproducible diagnostic that names the parent import chain |
| conflicting transitive refs for the same source | fail validation unless the lockfile has a single resolved revision and both parents record the collision |

<!-- REVIEW: added per attempt-50-pack-acquisition-contract -->

`gc formula validate --pack-source <url> --ref <ref> --all --json` has
read-only acquisition semantics:

| Concern | Contract |
|---|---|
| Fetch target | clone or fetch into a command-owned temporary checkout under the runtime temp directory, not into the city's installed-pack cache |
| Lifetime | remove the temp checkout after the command unless `--keep-temp` is an explicit debug flag that prints the path |
| Cache mutation | never mutate `packs.lock`, installed-pack cache, city config, beads, or workflow-root metadata |
| Credentials | use existing non-interactive git credential configuration only; never prompt; auth failures return exit `1` with a typed acquisition diagnostic |
| Network/offline | `--offline` forbids network access and succeeds only when an immutable lock/cache entry is already available; otherwise it reports `pack.acquisition_offline_unavailable` |
| Mutable refs | branch and tag refs are marked non-reproducible unless a lock entry records the resolved immutable revision and content hash |
| SHA refs | accepted as reproducible only when the fetched object exists and the content hash is recorded in the report |
| Prior imports | direct and transitive imports are resolved through packman before formula selection; missing lock entries become reproducibility diagnostics with parent binding identity |
| Failure diagnostics | acquisition failures are pack diagnostics, not formula diagnostics, and include source URL, requested ref, binding path, parent binding, credential mode, offline mode, and remediation |

`[pack] requires_gc` is parsed before formula files in that pack are selected.
The v0 grammar is a TOML string of the form `">=<semver>"`, where `<semver>`
is `MAJOR.MINOR.PATCH` with optional prerelease/build suffix accepted only if
the existing Gas City version parser already accepts it. Integers, booleans,
arrays, inline tables, missing operator, bare versions, `">= 1.2.3"`, leading
or trailing whitespace, and unknown pack compatibility keys are invalid. The
active binary version is read once at resolver startup; if
`requires_gc` exceeds it, resolver/import loading fails closed before staging,
shadow resolution, formula compile, or durable writes.

Old-reader pack-load fixtures are a hard prerequisite before first-party pack
floors are written. The compatibility corpus must include full `pack.toml`
load fixtures, not only standalone formula files:

| Fixture | Required assertion |
|---|---|
| old reader loads pack with no `[pack] requires_gc` and legacy `contract` formula | unchanged legacy behavior |
| old reader loads dual-declared graph formula with `[requires]` and `contract` | old reader ignores unknown formula table if that is current behavior, or the floor prevents distribution to that reader |
| old reader loads `[pack] requires_gc = ">=<actual floor>"` | accepted only when the reader is at or above the floor; below-floor reader fails before formula selection |
| old reader sees malformed or future `[pack] requires_gc` | fails closed as a pack-load diagnostic, not a formula diagnostic |
| current reader loads actual first-party floor values from release artifacts | exact comparator result, source position, and remediation are fixture-locked |

The release gate cannot rely on placeholder floors such as `0.x.y` or
`<minimum-floor>`. Before Phase 3 writes first-party pack floors, the corpus
names the exact old-reader binaries, exact floor values, and whether each
reader accepts, ignores safely, or rejects before formula selection.

<!-- REVIEW: added per attempt-79-requires-gc-comparator -->

The active binary version source and comparator are executable release
contracts:

| Active build form | Version source | Comparator behavior |
|---|---|---|
| tagged release `vMAJOR.MINOR.PATCH` | embedded build version | normal semver comparison; build metadata ignored for ordering |
| prerelease such as `v0.18.0-rc.1` | embedded build version | satisfies floors below `0.18.0`, satisfies the exact prerelease only when the floor includes that prerelease, and does not satisfy final `>=0.18.0` |
| nightly or timestamped build derived from a tag | embedded base version plus commit SHA | satisfies floors up to the base version only when the build metadata records a non-dirty tree at or after the floor tag |
| `main` source build with known commit | `git describe`/build info plus commit SHA | accepted for development validation, but release gates require a tagged baseline binary digest in `formula-compiler-compatibility.yaml` |
| dirty source build | build info plus dirty marker | may run local validation but fails release gates and pack-floor enforcement reports as `active_binary_dirty` |
| unknown or unparsable build version | none | pack resolver fails closed with `pack.requires_gc_active_version_unknown` before formula selection |

The comparator is centralized in the packman-owned compatibility package and
has golden rows for release, prerelease, nightly, `main`, dirty, source-build,
and unknown-version inputs. Formula validation and pack acquisition call that
comparator; they do not parse Gas City versions locally.

Pack-floor remediation is required when a formula compiler requirement exceeds
what the active binary can support. The validation report must include
`formula.migration.raise_pack_requires_gc` with the pack binding path, current
floor, required floor from `formula-compiler-min-floor.json`, winning formula,
and every shadowed formula that would become invalid if it won. Fixtures must
cover built-in, local clean, local dirty, git SHA, git branch/tag, registry,
direct import, transitive import, duplicate binding, aliased transitive import,
and shadowed-winner cases.

`--requirement-diff old.lock new.lock` compares lockfile entries by pack name,
binding name, binding path, source, requested ref or version constraint,
locked revision, content hash when available, `requires_gc`, transitive parent
binding, and every formula requirement delta. Branch or tag refs are mutable
unless a lock entry pins their resolved revision. Registry sources follow the
same rule: a semver constraint is not reproducible until the lockfile records
the resolved immutable revision and content hash, or the validation report
marks the pack non-reproducible.

Packman compatibility tests required by this design:

| Test | Required assertion |
|---|---|
| schema-1 lockfile read | formula validation reads current `[packs."<source>"] commit/version/fetched` entries without rewriting the file |
| missing extended fields | report contains `reproducibility` and `migration_hints` diagnostics instead of inventing locked content hash or `requires_gc` facts |
| schema bump read/write | new packman writer preserves deterministic ordering and old schema-1 readers fail or ignore only by documented compatibility policy |
| binding identity | validation JSON, persisted artifact provenance, migration hints, and requirement diffs all include `[imports.<binding>]`, parent binding, lockfile key, and source |
| conflicting transitive refs | fail validation unless packman has resolved one immutable revision and the report names both parent bindings |

New producers must write the canonical keys. During the alias window they also
write `gc.formula_contract = "graph.v2"` for graph workflow roots so existing
readers continue to work. New consumers must use a shared predicate that reads
canonical metadata first and falls back to the legacy key only for compatibility.
If root metadata cannot carry the full provenance, the producer must write a
durable compile artifact before creating child beads and stamp
`gc.formula_compile_artifact` on the root. That artifact is immutable for the
root lifecycle and contains the full `CompileResult.Provenance`,
`NormalizedRequirements`, and diagnostics that were accepted at creation time.

The spillover boundary is deterministic: root metadata may carry at most
8 KiB of formula compiler metadata after UTF-8 encoding and key sorting, and no
single formula compiler metadata value may exceed 1024 bytes. When either
limit would be exceeded, the producer writes the accepted artifact first and
stores only `gc.formula_compile_artifact` plus the minimal future-readable root
metadata. The limit is fixture-locked so two binaries make the same artifact
versus inline-metadata decision.

Formula validation must expose a read-only provenance surface before built-in
packs are migrated:

```bash
gc formula validate <name> --provenance
gc formula validate --all-packs --provenance
gc formula validate --pack-path ./packs/acme --all --json
gc formula validate --pack-source https://example.com/acme.git --ref v1.2.3 --all --json
```

This command must not create or update beads. It reports:

- formula name and winning source file
- formula layer and pack binding that won resolution
- pack import source, ref, and locked revision when available
- local path imports as `local` with a content hash and dirty status when the
  path is under a VCS checkout
- local path imports outside a VCS checkout as `local-not-reproducibly-pinned`
- deprecated fields, invalid requirements, v2-only constructs, and normalized
  compiler requirement

External author validation is a stable user-facing surface, not only an
internal release gate. With `--json`, the response schema contains
`schema_version`, `pack`, `requested_ref`, `locked_revision`, `dirty`,
`reproducibility`, `formulas`, `imports`, `shadowed_formulas`, `diagnostics`,
and `migration_hints`. Exit code `0` means every selected formula is valid,
`2` means formula diagnostics were found, and `1` means the pack could not be
loaded. Tests must cover local clean packs, local dirty packs, imported winners,
transitive imports, lockfile-backed refs, remote SHA pins, and shadowed formulas
whose losing source still contains legacy `contract`.

Shadowed formula diagnostics default to warning, not silence. When a losing
layer has a stricter requirement than the winning formula, an unsupported
future requirement, or legacy-only `contract`, validation reports a
non-blocking shadow diagnostic with the winner path, losing path, layer
priority, and remediation. Release gates may promote that warning to failure
for first-party packs because a later layer-order change could expose the
shadowed formula.

`migration_hints` is stable JSON, not prose. Each hint contains:

```json
{
  "code": "formula.migration.add_requires",
  "severity": "info",
  "formula": "code-review-loop",
  "source_path": "packs/acme/formulas/code-review-loop.toml",
  "source_key": "contract",
  "current": "contract = \"graph.v2\"",
  "recommended": "[requires]\\nformula_compiler = \">=2\"",
  "first_party": false,
  "pack": "acme",
  "binding_name": "review",
  "binding_path": "[imports.review]",
  "parent_binding": "",
  "requested_ref": "v1.2.3",
  "locked_revision": "abc123",
  "lockfile_key": "https://example.com/acme.git",
  "requires_gc_floor": ">=<minimum-floor>",
  "safe_automatic_edit": true
}
```

Required hint codes are `formula.migration.add_requires`,
`formula.migration.keep_dual_declaration`,
`formula.migration.remove_legacy_contract_when_floor_allows`,
`formula.migration.raise_pack_requires_gc`, and
`formula.migration.pin_pack_revision`. JSON field names, codes, and severities
are versioned by `schema_version`; external pack tooling must not parse
human-readable diagnostic messages.

`safe_automatic_edit` is advisory in v0. Gas City does not apply migration edits
automatically and no validation command rewrites source files. A value of
`true` means the hint's replacement is local to one formula file, preserves
comments according to the TOML writer fixture, and can be offered by external
tooling; it is not permission for the SDK to modify third-party packs. Any
future `--apply` command needs a separate design with backups, dry-run output,
conflict detection, and author confirmation rules.

<!-- REVIEW: added per attempt-50-binding-aware-migration-output -->

Binding identity is mandatory on every external-pack remediation surface. The
stable JSON schema for validation, `migration_hints`, requirement diffs,
persisted compile artifacts, and any root metadata that points at a consumed
external pack must include `binding_name`, `binding_path`, `parent_binding`,
`lockfile_key`, `requested_ref`, `locked_revision`, and `content_hash` when
available. If a field is unavailable because the current lockfile schema cannot
prove it, the report emits a diagnostic instead of omitting the field silently.

Required binding fixtures:

| Fixture | Required assertion |
|---|---|
| duplicate direct bindings to the same source | diagnostics name both `[imports.<binding>]` paths and the lockfile key |
| aliased transitive import | migration hint names the parent binding and the child binding that must be edited |
| shadowed winner | report names winning and losing binding paths, layer priorities, and which binding would change remediation |
| pinned external SHA | hint names the immutable SHA, content hash, and whether republish or consumer lockfile update is required |
| pack-floor bump | requirement diff names the formula requirement delta and the `[pack] requires_gc` edit in the owning pack |

Operational meaning of reproducibility values:

| Value | Meaning | Policy effect |
|---|---|---|
| `pinned` | Source comes from a locked pack revision or exact SHA | acceptable for release gates |
| `local-clean` | Local source is under VCS with no changes and a content hash | acceptable for development; release gate requires explicit approval |
| `local-dirty` | Local source has uncommitted changes | validation reports warning; release gate fails |
| `local-not-reproducibly-pinned` | Local source is outside a known VCS checkout or lacks a stable ref/hash | validation reports warning; release gate fails |

Canonical workflow-root predicate semantics:

| Root metadata shape | `IsWorkflowRootMetadata` | `IsGraphWorkflowMetadata` | Query behavior |
|---|---|---|---|
| canonical keys only, compiler capability `2` | true | true | matched by all new queries |
| canonical keys only, compiler capability `1` | true | false | matched as workflow root, not graph workflow |
| canonical keys only, compiler capability `3` or higher on an old binary | true | false | matched as unknown future workflow root for observation/cleanup only; graph-specific writes fail closed |
| dual-stamped canonical plus `gc.formula_contract=graph.v2` | true | true | preferred migration shape |
| legacy-only `gc.formula_contract=graph.v2` | true during alias window | true during alias window | legacy fallback; validation reports migration warning |
| raw `contract` string copied into arbitrary metadata | false | false | not a durable workflow-root signal |
| no canonical or legacy workflow metadata | false | false | ignored by workflow-root queries |

Conflict handling is canonical-first and fail-closed. If canonical keys and
legacy `gc.formula_contract` disagree, `ClassifyWorkflowRoot` returns the
canonical facts plus a diagnostic that names every conflicting key/value.
Observation, list, cleanup, and migration reports may display the root, but
graph-specific writers must treat the facts as `needs_migration` and refuse
child, retry, fanout, convergence, or finalizer writes until operator repair
restamps a consistent accepted artifact. Legacy metadata may widen visibility
during the alias window; it cannot override canonical compiler capability,
artifact version, requirement source, or unsupported-axis facts.

Raw metadata query construction for workflow roots is owned by the shared
predicate package only. New production code outside that owner may not filter
directly on `gc.formula_contract`, `gc.formula_compiler_requirement`, or
`gc.formula_compiler_capability`; it calls the predicate/query helper so
legacy-only, dual-stamped, graph-v2-only, and requires-only roots stay visible
through the migration.

The `gc.formula_*` metadata namespace is owned by `internal/formula` and the
shared predicate package. Matching is exact after trimming surrounding
whitespace only where legacy data already requires it: canonical keys use exact
values (`">=2"`, `"1"`, `"2"`, `requires`, `contract`, `dual`), while the
legacy `gc.formula_contract` fallback accepts only the byte string `graph.v2`
after trimming historical whitespace. No caller may branch on prefixes,
substrings, case-folded variants, or arbitrary `gc.*` keys as formula signals.

`version` remains accepted as legacy input for now. It is preserved only as
legacy metadata and may produce `formula.version_deprecated` on user-facing
validation paths. It is not a compiler selector and not a formula artifact
version.

## Terminology And Documentation Contract

<!-- REVIEW: added per docs-terminology-version -->

Glossary:

| Term | Meaning |
|---|---|
| Formula compiler capability | Numeric capability level the formula requires; v0 supports `1` and `2` |
| Host capability | What the active Gas City binary and current config can satisfy |
| Compiler implementation | Internal code path chosen by the binary, never by the formula |
| `contract` | Deprecated legacy spelling for graph-v2 compiler requirement |
| `schema` | Physical config/schema description, not compiler capability |
| Pack revision | The artifact identity and reproducibility boundary for formulas |
| Formula `version` | Legacy formula metadata only; not semver and not a compiler selector |
| Formula `[requires]` | Formula-author table that declares minimum formula compiler capabilities such as `[requires].formula_compiler` |
| Pack `[[pack.requires]]` | Pack-owned dependency/capability declarations for pack ecosystem constraints; not the formula compiler requirement table |
| Pack `[pack].requires_gc` | Pack-owned minimum Gas City binary/version floor checked before pack formula selection |
| Host `[daemon] formula_v2` | City-operator capability gate that says whether this city may satisfy formula compiler capability `2` |
| Compiler capability `2` | Normalized formula compiler requirement value for graph-workflow syntax |
| Formula v2 | User-facing shorthand for formulas that require compiler capability `2`; not a second compiler implementation |
| Graph workflow | Persisted workflow-root graph produced when normalized compiler capability is `2` and accepted artifact validation authorizes graph writes |

Docs and examples must be updated in the same release phase that makes
diagnostics user-visible:

| File family | Required update |
|---|---|
| `docs/reference/formula.md` | Canonical `[requires]` syntax, grammar, diagnostics, migration notes |
| `docs/reference/config.md` and generated schema docs | Host `[daemon] formula_v2` capability wording and legacy `graph_workflows` interaction |
| `docs/reference/cli.md` and generated command help | `gc formula validate --provenance` and `--legacy-contract-report` |
| `engdocs/architecture/formulas.md` | Internal ownership of compile result, diagnostics, and host capabilities |
| `engdocs/proposals/formula-migration.md` | Compatibility gates, minimum binary floor, external SHA-pinned behavior |
| Tutorials and examples under `examples/` and first-party packs | Dual declarations during alias window, `[requires]` as canonical prose |
| Test fixtures under `internal/testfixtures/` and `test/` | Explicit old, dual, and requires-only cases |
| Dashboard generated types | Typed diagnostics and workflow requirement fields |
| PackV2 author docs | Pack-level compatibility constraints, import refs, and formula `[requires]` distinction |

<!-- REVIEW: added per attempt-78-docs-proposal-alignment -->

`engdocs/proposals/formula-migration.md` is updated or frontmatter-marked
`superseded_by: engdocs/design/formula-compiler-requirements.md` in Phase 2,
the same phase that first creates typed diagnostics and docs fixtures. The
proposal may remain as historical context, but stale rollback language,
`GC_NATIVE_FORMULA=false` as production fallback, formula `version` as identity,
or `contract` as canonical graph syntax must be either removed or explicitly
scoped as superseded before any requirement diagnostic is visible to users.

User-visible diagnostic rollout is blocked until the docs/example bundle lands
in the same PR stack. A diagnostic is user-visible when it can appear in CLI
stderr, HTTP/API responses, dashboard state, generated client types, controller
logs, order events, or release validation output.

<!-- REVIEW: added per docs-rollout-gate -->

This gate is phase-blocking, not advisory. A PR that exposes any formula
requirement diagnostic through CLI, API, dashboard, generated client types,
controller logs, order events, or release validation output must include the
docs/example bundle below and the generated-help refresh in the same branch.
There is no feature-flag exception: hidden or experimental diagnostics still
count as user-visible once an operator can trigger or observe them.

`docs/reference/formula.md` has a required rewrite skeleton, not just a list of
topics:

1. Formula identity and pack revision boundary: formula `version` is legacy
   metadata only; pack ref/version/SHA is the reproducibility boundary.
2. Canonical `[requires]` grammar: omitted, `>=1`, `>=2`, wrong-type and
   malformed-value examples, and unsupported future `>=3` behavior.
3. Host capability: `[daemon] formula_v2` as an operator capability gate, not
   a compiler selector.
4. Graph workflow examples: modern requires-only example for new binaries and
   dual-declared alias-window example for first-party packs.
5. Migration from `contract`: deprecation warning, conflict behavior,
   legacy-only report command, and alias removal criteria.
6. Pack compatibility: `[pack] requires_gc`, import refs, pinned SHA behavior,
   and requirement-diff reports.
7. Diagnostics: stable code table, CLI/API/dashboard projection, and
   remediation wording.

The rewrite is accepted only when a reader can answer, from that document
alone, which field a formula author edits, which field a pack author edits,
which field an operator edits, what exit/status to expect for a disabled host,
and why formula `version` is not the migration mechanism. Every TOML snippet in
that file must parse in a fixture or be explicitly marked invalid with the
expected diagnostic code.

The docs bundle must include these exact TOML snippets:

```toml
formula = "code-review-loop"

[requires]
formula_compiler = ">=2"
```

```toml
[pack]
name = "acme-workflows"
requires_gc = ">=<minimum-floor-from-formula-compiler-min-floor.json>"
```

<!-- REVIEW: added per attempt-54-docs-doctest-gate -->

`docs/reference/formula.md` is gated by a doctest fixture at
`docs/reference/testdata/formula-requirements-doctest.yaml`. The fixture
extracts every TOML block from the reference page and classifies it as
`valid`, `invalid_expected`, or `template_expected`. Valid snippets are parsed
through the same formula loader as `gc formula validate`; invalid snippets name
the exact diagnostic code; template snippets must contain only documented
placeholders such as `<minimum-floor-from-formula-compiler-min-floor.json>`.

The reference page must contain a copy-paste-safe "which key do I edit" table:

| User intent | Edit this key | Do not edit |
|---|---|---|
| formula author needs graph workflow syntax | `[requires].formula_compiler = ">=2"` | formula `version` or host `[daemon]` config |
| city operator wants to allow graph formulas | `[daemon].formula_v2 = true` in `city.toml` | formula source or pack lockfile |
| pack author raises required Gas City binary | `[pack].requires_gc = ">=<floor>"` | formula `version` |
| pack consumer pins a reproducible workflow set | import ref, semver, tag, branch, SHA, and `packs.lock` | formula header fields |
| legacy formula author migrates `contract` | add `[requires] formula_compiler = ">=2"` and keep `contract` only during alias window | removing `contract` before the release floor allows it |

The reference page must also include a common-confusion table:

| Confused surface | Correct distinction |
|---|---|
| Formula `[requires]` vs host `[daemon] formula_v2` | formula authors declare what the formula needs; operators decide whether this city can satisfy it |
| Formula `[requires]` vs pack `[[pack.requires]]` | formula requirements gate formula compiler capability; pack requirements describe pack ecosystem constraints and must not be parsed as formula compiler requirements |
| Formula `[requires]` vs `[pack].requires_gc` | formula requirements gate compiler capability; pack requirements gate the minimum Gas City binary before pack selection |
| `[pack].requires_gc` vs `packs.lock` revision | pack metadata declares compatibility; the lockfile records the resolved revision/content identity |
| "Formula v2" vs "compiler capability 2" | "Formula v2" is shorthand in prose; executable contracts use normalized compiler capability `2` |
| "Graph workflow" vs host `[daemon] formula_v2` | graph workflow is accepted formula output; the host flag only says whether new compiles can satisfy capability `2` |
| Formula `version` vs pack ref/SHA | formula `version` is legacy metadata only; pack ref/SHA is the reproducibility boundary |
| Legacy `contract` vs canonical `[requires]` | `contract` is an alias-window compatibility spelling and never the canonical surface |

External-pack examples must pair formula and pack compatibility in one runnable
fixture:

```toml
# formulas/review-loop.toml
formula = "review-loop"

[requires]
formula_compiler = ">=2"
```

```toml
# pack.toml
[pack]
name = "acme-review"
requires_gc = ">=<minimum-floor-from-formula-compiler-min-floor.json>"
```

The docs gate fails if these snippets appear in prose but not in the doctest
fixture, if a snippet uses stale `contract` or `version` wording outside a
migration note, or if the same branch omits any required generated artifact:
reference docs, CLI help, config schema/reference, API OpenAPI, generated
dashboard TypeScript, dashboard fixtures, architecture docs, proposal
supersession, PackV2 author docs, and release report schemas.

The formula `version` policy is fixed for rollout: preserve the field as legacy
metadata, emit `formula.version_deprecated` only on validation/display
surfaces, never treat it as compiler capability, and do not schedule removal
until a separate deprecation design names reader support, release timing, and
conversion tooling.

Requirement-surface comparison that must appear in reference docs:

| Surface | Owner | Purpose | Example |
|---|---|---|---|
| Formula `[requires].formula_compiler` | formula author | Minimum formula compiler capability needed by this formula | `formula_compiler = ">=2"` |
| Host `[daemon].formula_v2` | city operator | Whether this binary/config may satisfy compiler capability 2 | `formula_v2 = true` |
| Pack `[[pack.requires]]` | pack author | Pack ecosystem dependency/capability declaration outside formula compiler requirements | pack-specific requirement row |
| Pack `requires_gc` | pack author | Minimum Gas City binary or pack schema compatibility | `requires_gc = ">=0.x"` |
| Pack import `version`/ref | pack consumer | Which pack revision to resolve | tag, branch, semver, or SHA |
| Formula `version` | legacy formula metadata | Preserved for compatibility; not a selector | `version = 2` |
| Formula `contract` | legacy formula authoring | Deprecated alias for compiler capability 2 | `contract = "graph.v2"` |

<!-- REVIEW: added per DR53-first-party-inventory-doc-gate -->

The docs/release bundle includes a generated first-party formula inventory at
`docs/release/formula-compiler-first-party-inventory.json`. It covers bundled
packs, checked-in generated system-pack manifests, examples, runnable
tutorials, docs snippets, fixtures, formula compiler testdata, tutorial golden
sources, PackV2 author examples, architecture-doc snippets, and Ralph demo
formulas. Each row records source path, pack binding, classification,
requirement source, v2-only constructs, whether the file is dual-declared,
expected minimum pack floor, and the docs or test that exercises it. CI fails
when a first-party graph formula appears in the repository without an
inventory row or when a stale docs snippet is not connected to an executable
example or explicit migration note.

Canonical modern example:

```toml
formula = "code-review-loop"

[requires]
formula_compiler = ">=2"
```

Alias-window dual-declared example for first-party graph formulas:

```toml
formula = "code-review-loop"
contract = "graph.v2"

[requires]
formula_compiler = ">=2"
```

Generated-help and stale-guidance gate:

| Check | Fails when |
|---|---|
| `docs/reference/formula.md` examples | a graph-v2 example lacks `[requires]` or presents `contract` as canonical outside migration notes |
| canonical formula `version =` examples | `version` is presented as normal identity or compiler behavior instead of legacy metadata |
| release placeholder values | `<minimum-floor-from-formula-compiler-min-floor.json>` or `<git-sha>` survives in release artifacts outside explicitly marked templates |
| `docs/reference/cli.md` / generated command help | formula validation flags omit `--provenance` or `--legacy-contract-report` |
| `docs/reference/config.md` | `[daemon] formula_v2` is described as selecting a compiler implementation instead of declaring host capability |
| tutorials and `examples/` | new graph workflow snippets are not dual-declared during the alias window |
| first-party pack snippets | source conversion happens before the minimum binary floor gate |
| dashboard generated types | diagnostic or workflow requirement fields are stale against OpenAPI |

Generated source ownership:

| Artifact | Source of truth | Regeneration command |
|---|---|---|
| `docs/reference/config.md` and city schema | config structs and schema generator | `make generate` |
| `docs/reference/cli.md` and command help | Cobra command definitions | `make generate` or `go run ./cmd/gc gen-doc` |
| `internal/api/openapi.json`, `docs/schema/openapi.json`, dashboard generated TS | Huma route registrations and API structs | `make dashboard-check` |
| formula validation fixture table | `internal/formula/testdata/compiler_requirements_matrix.yaml` | `go generate ./internal/formula` |

Explicit exceptions are limited to tests for legacy behavior, migration notes
that label `contract` as deprecated, and historical architecture text under
`engdocs/archive/`. The stale-guidance check must print each matched file and
line so authors can fix drift without guessing.

<!-- REVIEW: added per DR48-executable-docs-rollout-gates -->

The stale-guidance check is a blocking CI job, not a reviewer convention. The
first implementation PR adds a checked config file at
`docs/release/formula-compiler-stale-guidance.yaml`:

```yaml
schema_version: 1
owner: release-captain
path_globs:
  - README.md
  - AGENTS.md
  - TESTING.md
  - docs/**/*.md
  - docs/reference/**/*.md
  - docs/guides/**/*.md
  - docs/tutorials/**/*.md
  - docs/packv2/**/*.md
  - docs/reference/formula.md
  - engdocs/**/*.md
  - engdocs/architecture/**/*.md
  - engdocs/proposals/formula-migration.md
  - examples/**/*.toml
  - internal/bootstrap/packs/**/*.toml
  - internal/formula/testdata/**/*.toml
  - internal/formula/testdata/**/*.yaml
  - internal/testfixtures/**/*.toml
  - testdata/tutorials/**/*.toml
  - "**/ralph*/**/*.toml"
  - internal/formula/testdata/generated/system-packs-manifest.json
matchers:
  - id: canonical-contract-example
    pattern: 'contract = "graph.v2"'
    allow_only_with: ['deprecated', 'legacy', 'migration', 'alias-window']
  - id: formula-version-selector
    kind: parsed_toml_key
    key: version
    scope: formula_files_and_formula_toml_blocks
    allow_only_with: ['legacy metadata', 'not semver', 'not a compiler selector', 'pack version/ref/SHA']
  - id: formula-version-prose
    kind: prose_context
    path_globs:
      - docs/reference/formula.md
      - engdocs/architecture/formulas.md
      - engdocs/proposals/formula-migration.md
      - docs/tutorials/**/*.md
    pattern: 'formula version'
    allow_only_with: ['legacy metadata', 'not semver', 'not a compiler selector', 'pack version/ref/SHA']
  - id: native-formula-production-rollback
    pattern: 'GC_NATIVE_FORMULA=false'
    allow_only_with: ['validation-only probe', 'not a production runtime fallback', 'superseded']
  - id: compiler-selector-wording
    pattern: 'selects the compiler'
    allow_only_with: ['does not select']
  - id: formula-v2-positive-content
    kind: required_positive_context
    path_globs:
      - docs/reference/config.md
      - engdocs/architecture/formulas.md
      - engdocs/proposals/formula-migration.md
    pattern: 'formula_v2'
    require_any: ['host capability', 'operator capability', 'does not select the compiler']
  - id: requires-positive-content
    kind: required_positive_context
    path_globs:
      - docs/reference/formula.md
      - docs/tutorials/**/*.md
      - examples/**/*.md
    pattern: 'formula_compiler'
    require_any: ['[requires]', 'minimum formula compiler capability']
  - id: pack-requires-confusion
    kind: prose_context
    pattern: 'requires'
    path_globs:
      - docs/packv2/**/*.md
      - docs/reference/formula.md
      - docs/reference/config.md
      - engdocs/architecture/**/*.md
    allow_only_with: ['formula [requires]', '[[pack.requires]]', '[pack].requires_gc', 'minimum formula compiler capability', 'pack compatibility']
  - id: graph-workflow-host-confusion
    kind: prose_context
    pattern: 'formula v2'
    allow_only_with: ['compiler capability 2', 'graph workflow', '[daemon] formula_v2', 'host capability']
exceptions:
  - path_glob: engdocs/archive/**
    reason: historical material
```

The CI command is owned by the implementation, but it must be non-interactive
and must fail before any user-visible diagnostic surface lands when a matcher
finds stale guidance without an explicit exception. Required generated
artifacts for each surface are pinned to the rollout phase that exposes that
surface:

The local docs loop is a named command:

```bash
make formula-docs-check
```

The first implementation PR must add this Makefile target and a CI job before
any formula requirement diagnostic can be observed in CLI, API, dashboard,
controller/order/convergence events, or release validation output. It runs the
formula-reference doctest, parsed formula-context stale-guidance scan,
generated first-party inventory check, generated CLI help/schema check,
PackV2 author-doc link check, JSON-loader policy check, proposal-supersession
check, and release-report schema validation. The command
writes a stable JSON report to
`docs/release/formula-compiler-docs-check.json` with `schema_version`,
`generated_at`, `command`, `inputs`, `counts`, `stale_guidance[]`,
`doctest_failures[]`, `inventory_failures[]`, and
`generated_artifact_failures[]`. A sample report against the pre-migration tree
is committed with expected failures so reviewers can tell scoped formula
findings from unrelated uses of words such as "version" in Go, Node, release,
runtime, or pack-import documentation.

<!-- REVIEW: added per attempt-90-docs-predecessor-gate -->

The docs/check bundle is a hard predecessor to user-visible behavior, not a
parallel workstream. The implementation branch that first exposes a diagnostic
code or `[requires]` behavior must also make `make formula-docs-check` pass
with no expected-failure baseline for that surface. A release gate may not cite
"docs follow-up" as acceptance evidence.

The check has four blocking output classes:

| Output class | Fails when |
|---|---|
| `stale_guidance[]` | `contract`, `version = 2`, `GC_NATIVE_FORMULA=false`, `formula_v2`, or `formula_compiler` appears without the required legacy/migration/host-capability context |
| `doctest_failures[]` | a TOML/JSON snippet does not parse, an invalid snippet lacks an expected diagnostic code, or a placeholder appears outside a `template_expected` fixture |
| `generated_artifact_failures[]` | CLI help, config docs, OpenAPI, dashboard TypeScript, dashboard fixtures, release schemas, or PackV2 docs are stale against source |
| `inventory_failures[]` | a first-party pack, example, tutorial, fixture, generated prompt, or docs snippet lacks a first-party inventory row and executable or explicitly invalid fixture |

The stale-guidance matcher has explicit rows for legacy `version = 2`,
canonical `contract = "graph.v2"` examples, `GC_NATIVE_FORMULA=false`,
`formula_v2` wording that implies compiler selection, and `formula_compiler`
examples that omit the top-level `[requires]` context. The report prints file,
line, matcher id, excerpt, and remediation. Exceptions are data, not comments:
they name path, line matcher, expiry phase, and why the stale wording is
historical or intentionally invalid.

| Surface | Same-branch generated/doc artifacts |
|---|---|
| CLI diagnostics or help | `docs/reference/formula.md`, `docs/reference/cli.md`, generated Cobra help, runnable examples |
| config diagnostics | `docs/reference/config.md`, config schema/reference output, deprecated `graph_workflows` wording |
| Huma/API diagnostics | `internal/api/openapi.json`, `docs/schema/openapi.json`, dashboard generated TS, API reference text |
| dashboard rendering | generated TS client, dashboard fixtures, no-metadata-inference tests |
| release validation reports | report schemas under `docs/release/`, PackV2 author docs, requirement-diff examples |

First-party dual declarations are a separate gate from docs prose. Built-in
packs, example packs, tutorial snippets, and test fixtures that exercise graph
formulas must carry both `contract = "graph.v2"` and
`[requires] formula_compiler = ">=2"` until the minimum binary floor artifact
allows requires-only first-party distribution. A PR that exposes a diagnostic
surface cannot rely on a later docs PR or a later source-conversion PR.

JSON formulas have a Phase 2 decision gate. If the JSON loader remains enabled,
Phase 2 docs include JSON examples, doctests, pointer-attribution fixtures, and
the matrix rows named above. If JSON formulas are retired first, Phase 2 removes
the loader and documents TOML as the only accepted authoring surface. There is
no state where JSON remains enabled without docs and fixtures for
`requires.formula_compiler`.

`formula.version_deprecated` is emitted only by validation/display surfaces
(`gc formula validate`, `gc formula show`, and formula API previews). Launch,
order dispatch, retry, convergence, and controller paths preserve legacy
`version` as metadata silently so operational logs are not polluted by an
artifact-identity warning after a formula has already been accepted.

## Strict Review Risk Register

<!-- REVIEW: added per attempt-54-strict-risk-register -->

These are release blockers until their executable artifacts exist. They are
not open design questions, but the workflow must keep iterating while they are
only prose:

| Risk | Blocking artifact |
|---|---|
| host downgrade accidentally halts accepted roots or permits changed-identity writes | same-identity accepted-artifact reuse fixtures and dashboard/API projection fixtures |
| a durable writer preserves bare-recipe authority | per-symbol caller manifest with `end_state`, raw-consumer allowlist, and zero-write tests |
| convergence keeps an independent parser or overlapping field authority | exported-symbol migration table and `TestNoConvergenceSubsetParserUse` allowlist |
| requirement grammar diverges across TOML, JSON, docs, and diagnostics | generated matrix rows for byte-exact grammar, raw shapes, mixed axes, and transitive constructs |
| background diagnostics either spam events or hide repeated failures | `FormulaDiagnosticGroupState` fixtures for scans, restarts, reloads, API, dashboard, and write failures |
| alias removal becomes release judgment instead of evidence | `--alias-removal-gate --json` report with seeded compatibility, min-floor, external-support, stale-guidance, and inventory artifacts |
| docs keep teaching stale `contract`, `version`, or `GC_NATIVE_FORMULA=false` paths | doctest fixture, stale-guidance CI, generated help/schema/OpenAPI/TS refresh, and same-branch reference/proposal updates |
| future capabilities are run by old binaries or lose identity proof | old-reader cross-product fixtures and normalized accepted-artifact identity checks |

<!-- REVIEW: added per attempt-73-edge-contract-closeout -->

Edge contracts that must be pinned before implementation:

| Edge | Resolution |
|---|---|
| TOML parser authority | `github.com/BurntSushi/toml` v1.6.0 owns syntax rejection; `internal/formula` raw scanner owns valid-field source attribution |
| dashboard remediation affordances | dashboard may show typed remediation text, source links, copyable snippets from fixtures, and docs links; it may not offer automatic source edits in v0 |
| Phase 5 deliverable | compatibility bridge output is `docs/release/formula-compiler-compatibility.yaml` plus compat-corpus JSON, not an informal status note |
| future grammar growth | `formula_compiler` remains monotonic integer minima only; non-monotonic requirements need a new typed axis |
| non-`>=` operators | `=`, `==`, `<`, `<=`, `~>`, caret, wildcard, and comma/range expressions are invalid syntax in v0, not reserved accepted forms |
| default-capability identity | omitted, empty `[requires]`, and explicit `>=1` share runtime identity and differ only in provenance/display fields |

## Rollout Plan

<!-- REVIEW: added per reversible-rollout -->

Rollout is split so `main` can stay green and each phase has a narrow rollback.

1. Parser and model: add `Requires`, `NormalizedRequirements`, diagnostics, and
   validation tests. Keep existing callers on current behavior and keep the
   no-raw-consumer guard report-only.
2. Compile result and acceptance: add `CompileResult`,
   `AcceptedCompileArtifact`, canonical metadata keys, dual-stamping, and the
   shared workflow-root predicate. The docs/example/generated-help bundle for
   any diagnostic that can become visible in later phases lands here, before
   that diagnostic is exposed to operators. This phase also marks
   `engdocs/proposals/formula-migration.md` superseded or updates it in place,
   either fixtures JSON formula requirements or retires the JSON loader, lands
   first-party dual declarations, and saves a green first-party inventory.
3. First-party pack-floor declaration: write or update first-party `[pack]
   requires_gc` floors for built-in graph packs, checked-in generated
   system-pack manifests, examples, tutorials, fixtures, and Ralph demo packs from
   `docs/release/formula-compiler-min-floor.json`. This phase does not enforce
   the floor or expose new diagnostics yet; it makes the upcoming
   resolver/import enforcement auditable.
4. Caller migration: move callers to the normalized result in reversible
   sub-phases. At caller sub-phase 4a the static no-raw-consumer guard becomes
   blocking for new production raw consumers; existing consumers remain only
   on the owned expiring allowlist and each later sub-phase shrinks its rows.
5. Compatibility bridge: keep first-party graph formulas dual-declared while
   any supported release-validation probe can still read only `contract`.
   Production runtime paths remain native-only and may not fall back to `bd` or
   `GC_NATIVE_FORMULA=false`.
6. Docs and examples completion: finish stale live docs, generated CLI/config
   docs, architecture docs, PackV2 author docs, tutorials, examples, testdata,
   and dashboard/API generated types that were not tied to an earlier visible
   surface. This phase is cleanup and verification only; no Phase 4 diagnostic
   surface may wait until Phase 6 for its required docs, help, schema, OpenAPI,
   dashboard, example, or stale-guidance artifact. Legacy `contract` appears
   only in migration notes.
7a. Packman provenance contract: land packman-owned schema `2` or equivalent
    typed provenance APIs, saved schema/read/write artifacts, and schema-1
    fail-closed release-gate behavior. This phase does not enforce pack floors.
7b. Pack-floor enforcement: after 7a lands, enforce `[pack] requires_gc`, seed
    or refresh the compatibility/minimum-floor/external-support artifacts, and
    prove incompatible packs are rejected before formula selection, staging,
    shadow resolution, or durable writes.
8. First-party requires-only conversion: remove first-party `contract` source
   aliases only after the minimum binary floor is enforced, the optional `bd`
   compatibility strategy is complete, and active roots have been drained,
   repaired, or explicitly waived.
9. Alias removal: remove legacy `contract` support only after the measurable
   alias-window criteria above pass.

<!-- REVIEW: added per DR53-rollout-sequencing-controls -->

Phase order is mandatory. Phase 3 pack-floor declaration must land before
Phase 7 resolver/import enforcement, and first-party dual declaration is a
cross-phase invariant from Phase 2 through the end of Phase 7. No first-party
graph formula, example, tutorial, or fixture may drop `contract = "graph.v2"`
while `docs/release/formula-compiler-min-floor.json` says
`first_party_requires_only_allowed: false`.

<!-- REVIEW: added per attempt-90-rollout-dag-closure -->

The rollout dependency graph is:

```text
1 parser/model
  -> 2 compile result + docs/check seed
  -> 3 dormant first-party floor declarations
  -> 4a shared accepted-artifact APIs + blocking new-raw-consumer guard
  -> {4b sling/CLI, 4c orders/controller, 4d API/generated clients, 4e convoy/sourceworkflow}
  -> 4f convergence/molecule/fanout
  -> 4g dashboard rendering
  -> 5 compatibility bridge evidence refresh
  -> 7a packman provenance contract
  -> 7b pack-floor enforcement
  -> 8 first-party requires-only conversion
  -> 9 parser alias removal
```

Sub-phases 4b through 4e may proceed in parallel only after 4a is merged and
only if their manifest rows and write boundaries do not overlap. Phase 4f must
follow any sub-phase whose artifact it reuses or repairs, because convergence,
molecule execution, and fanout are multi-write paths that can touch roots,
children, hooks, convoys, retry metadata, fanout state, and convergence state.
Phase 4g follows 4d because dashboard rendering must consume generated API
types rather than inventing local diagnostic or workflow-root projections.

Phase 4a is shared plumbing, not a claim that every existing caller has already
migrated. Its deliverables are `CompileResult`, `AcceptedCompileArtifact`,
projection snapshots, the accepted-artifact writer signatures, the shared
workflow-root predicate, the repository-wide caller manifest, report-mode
legacy allowlist, and a blocking guard for new raw consumers. Existing callers
may remain on legacy wrappers only while first-party graph formulas stay
dual-declared and requires-only distribution is blocked. A later producer
sub-phase is complete only when that producer's rows move to
`accepted_artifact_only` and its zero-write fixtures pass.

Phase 8 and Phase 9 are separate release gates. Phase 8 edits first-party
source to requires-only and is allowed only after packman schema 2 or an
equivalent typed provenance artifact is present, pack floors are enforced
before formula selection, minimum-floor versions are non-placeholder, the
active-root report is clear or waived, and alias-drain evidence shows the
conversion window is open. Phase 8 does not require zero first-party
`dual_declared` rows before it begins; it is the phase that removes those
dual declarations from first-party sources after the conversion gate passes.
Phase 9 removes parser support for legacy
`contract` only after Phase 8 has shipped, the alias-drain window has two
qualifying reports, external support is expired or not-needed, and the
alias-removal gate consumes JSON evidence directly. A rollback in Phase 8
restores dual declarations and resets the alias-window clock; a rollback in
Phase 9 restores parser alias support without changing accepted artifacts.

<!-- REVIEW: added per attempt-80-rollout-sequencing-lock -->

The chosen Phase 2/3 sequence is: first-party dual declarations and their green
inventory gate land in Phase 2 before any visible/runtime diagnostic surface.
Phase 3 is dormant floor declaration only; it may update first-party
`[pack] requires_gc` metadata and saved release artifacts, but it may not make a
new diagnostic visible, distribute requires-only graph formulas, or migrate a
durable producer. If Phase 2 cannot land the dual declarations, first-party
inventory, docs/check bundle, generated help, API/dashboard schema changes, and
proposal supersession together, then every visible diagnostic surface stays
disabled until that bundle is complete.

Writer lockdown is a hard predecessor for producer migration. Before Phase 4b
through 4f can merge, Phase 4a must prove all of these:

| Lockdown proof | Required evidence |
|---|---|
| accepted-artifact-only writer signatures | root, child, attachment, retry, fanout, order, convergence, and workflow-finalize writers accept `AcceptedCompileArtifact` or an operation-specific wrapper that embeds it |
| no bare durable authority | static guard fails when a durable writer accepts bare `*Recipe`, bare `*CompileResult`, caller-built metadata, raw `NormalizedRequirements`, or `bd`/prompt output as authorization |
| zero-write fixtures | disabled host and fatal diagnostic cases snapshot roots, children, dependencies, hooks, convoys, workflow metadata, retry metadata, fanout state, convergence state, fired-order metadata, and artifact refs |
| manifest-green repository scan | Go code plus first-party packs, examples, tutorials, docs snippets, and prompt commands have generated caller rows with owner, expiry phase, replacement API, and blocking test |
| deletion/compatibility tests | retired helpers and legacy `gc bd mol ...` producer commands are deleted, validation-only, or blocked for requires-only formulas |

A producer sub-phase cannot substitute a local package test for this shared
lockdown gate. If any row is absent or report-only, the migrated producer must
remain disabled before its first durable write.

<!-- REVIEW: added per DR55-rollout-operable-gates -->

Each phase has a PR home, owner, local command, and rollback unit:

| Phase | PR home | Owner | Required local command | Rollback unit |
|---|---|---|---|---|
| 1 parser/model | `internal/formula` | formula owner | `go test ./internal/formula` | revert additive parser/model changes |
| 2 compile result/artifacts/docs seed | `internal/formula`, `internal/sourceworkflow`, docs bundle | formula owner plus release captain | `go test ./internal/formula ./internal/sourceworkflow` and `make formula-docs-check` | disable diagnostic surfacing and leave callers on legacy wrappers |
| 3 first-party dual/floor declaration | bundled packs, examples, fixtures | release captain | `gc formula validate --all-packs --legacy-contract-report --json` | restore previous pack source plus dual declarations |
| 4 caller migration | owning caller package per sub-phase | caller package owner | package tests plus row-specific zero-write fixtures | disable that producer surface before its first durable write |
| 5 compatibility bridge | release artifacts | release captain | `gc formula validate --compat-corpus internal/formula/testdata/compat_corpus --json` | keep dual source and dual root metadata |
| 6 docs/examples completion | docs/reference, PackV2 docs, generated help/schema/API | docs owner plus release captain | `make formula-docs-check` and `make dashboard-check` when API/dashboard changes | revert docs surface or hide the matching diagnostic surface |
| 7a packman provenance contract | `internal/packman`, `docs/packv2`, release schemas | packman owner plus release captain | schema migration/read-write reports and `go test ./internal/packman` | keep schema-1 advisory reports and block release gates that need provenance |
| 7b pack-floor enforcement | `internal/packman`, resolver/import loading | packman owner | `go test ./internal/packman ./internal/formula` | disable resolver enforcement before formula selection |
| 8 requires-only conversion | first-party packs and release artifacts | release captain | `gc formula validate --all-packs --requires-only-conversion-gate --json` | republish dual declarations and reset alias-window clock |
| 9 alias removal | parser and compatibility shims | release captain plus formula owner | alias-removal gate plus compatibility corpus | restore parser alias support before release |

The release checklist must attach the command output artifact for each phase.
An approval comment, PR description, or release-note statement is not evidence
unless it cites the saved command output.

Phase 4 caller migration has one ordering rule: a durable producer cannot
switch to accepted artifacts until Phase 4a has landed `CompileResult`,
`AcceptedCompileArtifact`, the projection snapshot, shared workflow-root
predicate, report-mode caller manifest, and blocking no-new-raw-consumer
guard. Each later sub-phase must have a rollback switch that disables only the
new producer surface before it writes durable state; rollback may not restore a
raw TOML or `bd` runtime decision path that the allowlist no longer owns.

The alias-window clock starts on the first published Gas City release that
contains all of these together: dual-declared first-party graph formulas,
canonical root metadata writers, typed diagnostics, docs/examples bundle,
stale-guidance CI, and compatibility/min-floor/external-support artifacts.
The release captain records that release tag and date in
`formula-compiler-min-floor.json`. Phase 8 conversion cannot start until the
two completed minor releases and 60 calendar days are both measured from that
recorded start. If Phase 8 rolls back after any requires-only source has been
published, the rollback clock resets to the next release that republishes dual
declarations and dual root stamps.

<!-- REVIEW: added per attempt-79-release-clock-anchor -->

The clock cannot start from a release note alone. The release that starts the
alias window must save `docs/release/formula-compiler-alias-window-start.json`:

```json
{
  "schema_version": 1,
  "release_tag": "v0.x.y",
  "release_date": "2026-05-11",
  "release_captain": "name",
  "contains_dual_declarations": true,
  "contains_canonical_root_metadata": true,
  "contains_typed_diagnostics": true,
  "contains_docs_bundle": true,
  "contains_stale_guidance_ci": true,
  "input_artifacts": {
    "compatibility": "docs/release/formula-compiler-compatibility.yaml",
    "minimum_floor": "docs/release/formula-compiler-min-floor.json",
    "external_support": "docs/release/formula-compiler-external-support.json",
    "first_party_inventory": "docs/release/formula-compiler-first-party-inventory.json",
    "docs_check": "docs/release/formula-compiler-docs-check.json"
  },
  "public_notice": {
    "url": "",
    "sent_at": "",
    "support_window_closes_at": ""
  }
}
```

Missing files, empty public-notice fields, a release tag that is not in the
supported distribution channel, or any `false` capability field means the alias
clock has not started. Alias-drain is a sliding evidence window: before Phase 8
or Phase 9, the latest release candidate and the previous completed minor
release must both report zero first-party legacy-only formulas, zero
first-party dual-declared formulas for alias removal, zero background accepted
aliases, and zero active external support rows. A later accepted alias compile
or newly discovered supported external pack resets the relevant drain window
until two consecutive qualifying reports are saved.

Before Phase 8 begins, the release captain must attach an active-root report to
the release checklist. The report groups roots as accepted-artifact present,
legacy-only source available, legacy-only source missing, downgraded-host
blocked, unsupported future schema, and external/support-waived. Requires-only
conversion is blocked until every active first-party graph root is either
drained, repaired with `gc formula repair-root-artifact`, or covered by an
explicit waiver naming the root id, owner, diagnostic, and operator-approved
abandonment/cleanup plan.

Rollback notes:

- Phases 1 and 2 are additive and can be reverted without changing formula
  source files.
- Phase 4 rollback disables the newly migrated producer or surface before it
  writes durable state. It does not reauthorize raw consumers that were not on
  the owned allowlist at the start of the sub-phase.
- Phase 5 keeps dual source declarations, so old binaries still read built-in
  graph formulas.
- Rollback after user-visible diagnostics ship is to restore dual declarations,
  dual root stamps, or disable the new producer path before writes; it is not to
  revive the old `bd` shell-out runtime path or reinterpret requires-only
  formulas in old binaries.
- Phases 8 and 9 require an explicit release decision because they can affect
  externally pinned packs.

Caller migration sub-phases:

| Sub-phase | Scope | Required gate | Rollback |
|---|---|---|---|
| 4a shared result plumbing | `CompileResult`, `AcceptedCompileArtifact`, typed diagnostics, metadata writer, workflow-root predicate, blocking no-new-raw-consumer guard | formula unit tests, predicate parity tests, allowlist seeded with owner/expiry/test rows | callers still use legacy compile wrapper until durable writers switch; new raw consumers stay blocked |
| 4b sling and CLI launch | `gc sling`, `internal/sling`, force replacement, runtime-var validation | enabled/disabled host tests and no-partial-root tests | disable the migrated launch path before writes while existing dual-stamped roots remain readable through the shared predicate |
| 4c orders and controller scans | formula-backed order dispatch and controller validation producers | repeated scan grouping test and successful-later-order test | disable new producer event while keeping synchronous diagnostics |
| 4d API and generated clients | Huma handlers, OpenAPI, generated dashboard TS | docs/generated-client bundle already landed, `make dashboard-check`, and OpenAPI in-sync | disable the new API launch/preview surface before writes; no hand-written legacy JSON projection |
| 4e convoy/source workflow | source-workflow scans, convoy dispatch and cleanup | canonical-only, legacy-only, dual-stamped, mixed-store root tests | keep the legacy metadata branch only inside the shared predicate during the alias window |
| 4f convergence and molecule execution | convergence create/retry/speculative wisp, molecule cook/cook-on, fanout fragments | zero-write tests for every durable write boundary | reject new convergence/fanout formulas while legacy roots continue |
| 4g dashboard rendering | dashboard state and generated diagnostic rendering | generated TS compile and dashboard state tests | dashboard hides diagnostics but API remains typed |

Documentation and source conversion are separate gates:

| Gate | Scope |
|---|---|
| Docs prose | reference docs, architecture docs, PackV2 docs, tutorials, generated CLI/config docs |
| Runnable examples | examples and tutorial commands that CI executes |
| First-party dual declarations | built-in packs, examples, testdata, and fixtures carry both `contract` and `[requires]` |
| First-party requires-only conversion | only after minimum binary floor, external support plan, and `bd` probe retirement/parity gates pass |

Diagnostic visibility matrix:

| Surface becomes visible in | Required predecessor |
|---|---|
| CLI stderr or generated help | `docs/reference/formula.md`, `docs/reference/cli.md`, command help, examples, and stale-guidance checks |
| Huma API or API-routed CLI | typed response structs, OpenAPI in-sync, generated TS, dashboard fixture, and reference docs |
| dashboard state | generated API types, dashboard remediation field fixtures, and no-metadata-inference tests |
| order/controller/convergence events | registered typed event payloads, repeated-loop grouping fixtures, and operator docs |
| release validation reports | PackV2 author docs, lockfile schema, requirement-diff docs, and saved artifact schemas |

Each row is a merge precondition for the PR that exposes that surface. A
producer cannot merge typed diagnostics first and promise docs, generated
clients, dashboard fixtures, or release schemas in a later phase. Rollback
proof for a visible surface must include the control that hides or disables
that exact producer before durable writes, plus a test or command output
showing zero protected writes while the surface is disabled.

## In-Flight And Convergence Behavior

<!-- REVIEW: added per in-flight-convergence-behavior -->

Compiler requirements are evaluated when a formula is compiled for a new root,
wisp, attached molecule, expansion, or convergence instance. They are not
re-evaluated for already-created beads that are merely being observed,
dispatched, retried, closed, or finalized.

Rules:

- Current-host validation applies to new or changed compiles only. New
  `gc sling --formula`, API sling, formula-backed order dispatch, new
  convergence root creation, speculative wisp creation, retry targets, fanout
  fragments, and `on_complete` formulas compile against the active host
  capability and fail before writes when it is unsatisfied.
- Persisted-artifact reuse applies to same-identity existing roots. If
  `[daemon] formula_v2` changes from true to false, an artifact-stamped graph
  root may continue graph-specific mutations only by validating the persisted
  accepted artifact against the current write intent. The current disabled host
  does not invalidate that same-identity artifact, but it does block selecting
  or compiling any new formula.
- Legacy-only roots without an accepted artifact are observation-first. Their
  already-created step beads can be observed, dispatched, closed, and cleaned
  up when no graph-specific synthesis is required. Retry-run creation,
  scope-check repair, workflow-finalize mutation, fanout repair,
  missing-child repair, next iteration, and speculative-wisp creation are
  blocked until `gc formula repair-root-artifact` stamps a validated accepted
  artifact.
- Retrying an existing step that does not compile a new formula continues even
  if the host flag has since been disabled.
- A retry or `on_complete` action that compiles a new formula uses the current
  host capability and can fail with the shared diagnostic before creating the
  new attached molecule.
- Convergence adapters must use the same diagnostic path for create, retry,
  and speculative-wisp entry points. A failed preflight produces no active
  loop with missing children.
- Dispatch fanout compiles expansion fragments before root mutation. If any
  fragment has an unsupported or unsatisfied compiler requirement, fanout
  returns the shared diagnostic and writes no fragment beads, convoy links, or
  continuation-group metadata.

<!-- REVIEW: added per DR53-host-downgrade-semantics -->

Operator-visible host-downgrade behavior is fixed:

| Path while host disabled | Behavior |
|---|---|
| new formula compile or launch | current-host validation fails with `formula.compiler_requirement_unsatisfied`; zero durable writes |
| artifact-stamped root, same formula identity | validate persisted artifact and projection snapshot; same-identity graph-specific writes may continue |
| artifact-stamped root, changed formula/vars/options/source | compile again against current host; fail closed while disabled |
| legacy-only root with source present | observation and already-created bead completion continue; graph-specific mutation requires repair command and host enabled |
| legacy-only root with source missing | observation only plus stable remediation; no graph-specific writes |
| retry of already-created step | may continue when it does not compile a new formula or synthesize graph topology |
| retry or `on_complete` that starts a formula | current-host compile/accept before retry metadata or attached-molecule writes |
| fanout and fragment repair | parent artifact plus every fragment artifact must validate before any fragment child, convoy, or continuation write |
| scope-check and workflow-finalize | artifact projection authorizes same-identity mutation; missing or unsupported artifact blocks writes |
| convergence retry/next iteration/missing-child/speculative wisp | persisted convergence artifact is reused only when identity matches; new or changed formula compiles against current host |

This split prevents two common mistakes: treating a disabled host as a reason
to stop already-accepted roots that have a valid artifact, and treating a
legacy root's metadata as enough authority to synthesize new graph state after
the host has been downgraded.

The migration is anchored to the live production path. Today convergence wisp
creation reaches formula execution through
`cmd/gc/convergence_store.go:pourWisp -> molecule.Cook -> internal/formula`.
That call graph remains the starting point for implementation: the first
change inserts `PreflightFormula`/accepted-artifact validation before
`molecule.Cook` can write a convergence root or child, then narrows or deletes
any convergence-only subset parser left outside that path. A design or
implementation that treats the subset parser as the primary live compiler is
out of date.

Convergence implementation decision:

The convergence formula subset parser is not an independent compiler. It is
rewritten as a typed projection over generic `CompileResult` fields, followed
by convergence-only domain validation. Domain validation may check convergence
semantics such as allowed retry shape, but it may not interpret raw `contract`,
raw `[requires]`, graph metadata strings, or host capability. That keeps
formula syntax, requirement satisfaction, and diagnostic projection owned by
`internal/formula` while keeping convergence-specific API surface inside
`internal/convergence`.

<!-- REVIEW: added per attempt-73-convergence-transition-fence -->

During caller sub-phases 4b through 4f, convergence has a transition fence:
if the convergence caller has not yet moved to `AcceptedCompileArtifact`, it
must shadow-compile every selected formula through `CompileWithResult` and
fail closed when the shadow result reports a requirement, host, projection, or
provenance diagnostic. It may not continue using the legacy subset parser as a
write-authorizing path while sling, orders, or fanout enforce accepted
artifacts, because that would allow `[requires]` bypass through convergence.

Current convergence validation rules are either projected or explicitly
retired:

| Legacy rule | New owner | Required fixture |
|---|---|---|
| evaluate prompt content present | compiler projection plus `ValidateProjection` | inline prompt, prompt path, empty string, missing file, unreadable file |
| evaluate prompt content readable | `internal/convergence` domain validation after acceptance | path source attribution and zero-write unreadable failure |
| required vars supplied | compiler runtime-var projection plus `ConvergenceRuntimeInputs` | missing, defaulted, duplicate, and secret-redacted vars |
| relevant step exists | compiler `CompiledStep` projection | missing step and transformed step identity |
| retry policy supports iteration | compiler retry projection plus convergence validation | invalid retry shape and same-identity retry reuse |
| reserved step ids do not conflict | convergence validation over projected steps | conflict diagnostic before root/iteration writes |

Any legacy evaluate-prompt check not represented in this table is retired in
the same PR that migrates convergence preflight. Prompt path safety, empty
inline prompt, missing prompt, unreadable prompt, changed prompt content,
relative path normalization, and source attribution all enter
`ValidateProjection` fixtures. No convergence caller may reopen the formula
TOML or inspect root metadata to recover evaluate-prompt fields after
projection.

Convergence formulas may use `[requires] formula_compiler = ">=2"` and v2
constructs only through the same accepted-artifact path as sling, orders, and
fanout. There is no convergence-specific exemption and no convergence-only
lowering of the requirement. If the host cannot satisfy capability `2`, a
new convergence create/retry/next-iteration/speculative operation records the
typed formula diagnostic and writes zero convergence, hook, dependency, convoy,
retry, fanout, or artifact-ref state. Existing same-identity convergence roots
continue only by validating their persisted convergence artifact.

Pre-create compile failures have stable subject keys. Before a convergence root
exists, diagnostics are grouped under
`producer=convergence_create`, `subject_id=<source bead id or formula name>`,
and the diagnostic `OnceKey`. After the root exists, retries and next
iterations use the convergence root id. Fixtures cover same-identity retry,
changed-identity retry, host toggles, artifact-ref conflicts, missing source,
and pre-create failures with zero root, iteration, hook, dependency, convoy,
artifact-ref, or retry metadata writes.

<!-- REVIEW: added per attempt-80-convergence-preflight-boundary -->

Every convergence entry point has a named preflight sequence before durable
state can change:

| Entry point | Required sequence before first write | Writes forbidden on failure |
|---|---|---|
| create | resolve source -> `CompileWithResult` -> `AcceptCompileResult` -> `ValidateAcceptedArtifact(create)` -> `ProjectAcceptedFormula` -> `ValidateProjection(create)` | convergence root, first iteration, hook, dependency, convoy, artifact ref |
| retry | load root artifact -> validate same identity and retry intent, or compile/accept changed target against current host -> project -> validate retry inputs | retry metadata, retry-run bead, replacement child, hook, dependency |
| next iteration | load and validate root artifact -> project retry/iteration fields -> validate iteration intent | iteration bead, dependency, hook, attempt metadata |
| manual iterate | same as next iteration, plus validate the operator-supplied step/vars against projection before writes | manual iteration bead, retry metadata, hook, dependency |
| speculative pour | compile/accept every candidate formula and validate every projection before creating any pending wisp | pending wisp root, child beads, convoy links, adoption metadata |
| fallback pour | validate the original root artifact and the fallback formula artifact before fallback state changes | fallback root/child, fallback metadata, convoy, hook |
| missing-state repair | load root artifact, validate write intent, project expected convergence state, and compare to store snapshot before repair | state repair metadata, generated child, dependency, hook |
| missing-child repair | load root artifact, project expected child identity, and compile/accept only if the selected formula identity changes | replacement child, dependency, hook, repair marker |
| pending-wisp adoption | validate candidate artifact and projection before adopting or hooking the pending wisp | adoption metadata, hook, convoy, child mutation |
| pending-wisp burn | validate root/candidate artifact enough to classify ownership before cleanup | burn metadata, convoy cleanup, child deletion or close actions |

`PourWisp` is not a write-authorizing shortcut. Its first line after loading
inputs is a call to the same preflight helper used by create/retry. If a path
cannot supply the required accepted artifact or current-host compile input, it
returns a typed diagnostic and performs no convergence, wisp, hook, convoy, or
repair writes.

Active-loop unsatisfied requirements have a stable failure contract. If an
already active convergence loop later reaches a new or changed formula whose
requirement cannot be satisfied, the loop enters `waiting_on_formula_requirement`
instead of creating partial graph state:

| Surface | Required data |
|---|---|
| root metadata | `gc.convergence_waiting_reason=formula_requirement`, diagnostic code, `OnceKey`, host capability, config generation, and last checked time |
| status/API/dashboard | typed `ConvergenceFormulaBlockedStatus` with convergence id, operation, candidate formula, diagnostic payload, retryable flag, and remediation |
| event | one registered `convergence.formula_compile_failed` event per burst cadence with the same diagnostic payload and occurrence count |
| retry semantics | automatic retry only after config generation, formula content hash, selected formula identity, or operator retry request changes |
| pending-wisp cleanup | pending wisps created before the failure are either left untouched if they predate the failing operation, or marked `blocked_by_formula_requirement` and unhooked when they were created by the failing operation |

The waiting state is diagnostic state and may be updated through the grouped
diagnostic CAS path. It is not an accepted compile artifact and cannot
authorize future writes. Clearing it requires a successful preflight for the
same operation or explicit operator abandonment/cleanup. Fixtures cover active
loop create, retry, manual iterate, next iteration, speculative pour, fallback
pour, missing-state repair, missing-child repair, pending-wisp adoption, and
pending-wisp burn with disabled host, unsupported future requirement,
projection failure, and diagnostic-state write failure.

<!-- REVIEW: added per attempt-54-convergence-ownership-contract -->

The package-boundary flow is normative:

```text
cmd/gc or API
  -> formula.CompileWithResult(host capabilities, vars, resolved source)
  -> formula.AcceptCompileResult(identity)
  -> formula.ValidateAcceptedArtifact(write intent)
  -> convergence.ProjectAcceptedFormula(artifact)
  -> convergence.ValidateProjection(metadata)
  -> convergence durable writer
```

<!-- REVIEW: added per attempt-90-convergence-executable-flow -->

The forbidden legacy flow is:

```text
load formula/root metadata
  -> convergence subset parser or ValidateForConvergence reads raw fields
  -> convergence creates root/iteration/retry metadata
  -> formula diagnostics are projected afterward or not at all
```

The replacement flow is executable pseudocode:

```go
func createConvergence(ctx context.Context, input CreateInput) error {
    host := formulahost.FromCityConfig(input.CityConfig, input.ConfigSource)
    result := formula.CompileWithResult(ctx, input.FormulaName, input.SearchPaths, formula.CompileOptions{
        Vars: input.CompileVars,
        HostCapabilities: host,
        ValidateRuntimeVars: true,
    })
    artifact, diagnostics := formula.AcceptCompileResult(result, input.CompileIdentity())
    if fatal(diagnostics) {
        return recordConvergenceDiagnostic(ctx, input.PreCreateSubject(), diagnostics)
    }
    if diagnostics := formula.ValidateAcceptedArtifact(artifact, input.WriteIntent("convergence_create")); fatal(diagnostics) {
        return recordConvergenceDiagnostic(ctx, input.PreCreateSubject(), diagnostics)
    }
    view, diagnostics := convergence.ProjectAcceptedFormula(artifact)
    if fatal(diagnostics) {
        return recordConvergenceDiagnostic(ctx, input.PreCreateSubject(), diagnostics)
    }
    if diagnostics := convergence.ValidateProjection(view, input.RuntimeInputs(artifact)); fatal(diagnostics) {
        return recordConvergenceDiagnostic(ctx, input.PreCreateSubject(), diagnostics)
    }
    return convergence.WriteRootAndFirstIteration(ctx, input.Store, artifact, view, input.WriteSet())
}
```

Retry, next-iteration, manual iterate, missing-child repair, pending-wisp
adoption, pending-wisp burn, and fallback pour use the same helper shape with a
different `WriteIntent.Operation`. Reuse of a persisted artifact is allowed
only when `ValidateAcceptedArtifact` receives the root's current artifact ref,
expected identity hash, and same-identity write intent. If the selected
formula, vars, options, search paths, pack binding, projection snapshot
version, evaluate prompt identity, or content hash changes, the path compiles
and accepts again against the current host capability before writing.

`evaluate_prompt` is part of artifact identity because it can change
convergence behavior without changing requirement syntax. Inline prompt
content contributes `EvaluatePromptHash = sha256("inline" || source path/key ||
raw bytes)`. Prompt paths contribute `EvaluatePromptHash = sha256("path" ||
source path/key || resolved path || file content hash)`. Unreadable prompt
paths fail in `ValidateProjection` before durable writes; readable prompt
content is copied or content-addressed in the accepted projection so retry and
repair do not reopen a different file silently. Redaction policy applies to
runtime var values, not to evaluate-prompt content.

Blocked-loop metadata is diagnostic state with one owner:

| Metadata key | Meaning |
|---|---|
| `gc.convergence_waiting_reason` | `formula_requirement` |
| `gc.convergence_waiting_operation` | create, retry, next_iteration, manual_iterate, speculative_pour, fallback_pour, missing_state_repair, missing_child_repair, pending_wisp_adoption, or pending_wisp_burn |
| `gc.convergence_waiting_candidate_formula` | selected formula identity or pre-create subject |
| `gc.convergence_waiting_once_key` | formula diagnostic `OnceKey` |
| `gc.convergence_waiting_config_generation` | host config generation used for the failed preflight |
| `gc.convergence_waiting_content_hash` | candidate formula content hash when available |
| `gc.convergence_waiting_last_checked_at` | timestamp of latest preflight |

These keys cannot authorize future writes. Clearing them requires a successful
preflight for the same operation or explicit operator abandonment. They are
updated through the grouped diagnostic CAS path and are separate from
`gc.convergence_formula_compile_artifact`, the only convergence artifact-ref
authority.

Field ownership is not shared:

| Field or concern | Owner | Consumer rule |
|---|---|---|
| formula syntax, `[requires]`, legacy `contract`, formula `version` | `internal/formula` | convergence never reads raw fields |
| host capability satisfaction | `internal/formula` | convergence receives only accepted artifacts or diagnostics |
| enabled state and source attribution | compiler projection from `internal/formula` | `internal/convergence` may validate domain meaning but cannot choose a different source |
| evaluate prompt inline content and prompt path | compiler projection plus convergence validation | prompt readability is convergence-domain validation after artifact acceptance |
| runtime vars and required vars | compiler projection | convergence checks supplied values; it does not reconstruct declarations |
| retry policy | compiler projection | convergence validates whether projected retry semantics can support iteration |
| relevant step and reserved step IDs | compiler projection plus convergence validation | convergence validates conflicts using projected step identities |
| requirements, provenance, artifact ref, binding identity | accepted artifact | convergence persists refs and projections, never root-metadata-derived substitutes |
| convergence-specific diagnostic codes | `internal/convergence` | emitted only after formula acceptance and source-attributed projection |

Exported-symbol migration is checked by allowlist:

| Symbol | End state | Static guard |
|---|---|---|
| `internal/convergence/formula.Formula` | deleted or unexported preview DTO with no TOML decode methods | production imports fail |
| `ValidateForConvergence` | temporary delegator to `ProjectFormula` for preview only, then deleted | raw TOML reads and requirement inspection forbidden |
| `ProjectFormula` | preview-only projection from `*formula.CompileResult` | no durable caller may use it |
| `ProjectAcceptedFormula` | durable projection from `formula.AcceptedCompileArtifact` | required by create/retry/repair/speculative writers |
| convergence retry/create helpers | accepted-artifact-only writers | zero-write tests for formula and projection failures |

`TestNoConvergenceSubsetParserUse` owns the allowlist and fails when
production convergence code imports parser packages directly, opens formula
source files, scans raw `[requires]`, reads legacy `contract`, constructs
`convergence.Formula{}` from TOML, reconstructs required vars outside the
projection, or branches on root metadata instead of `AcceptedCompileArtifact`.

`internal/formula` exposes only generic compiled facts needed by all execution
paths. This is the same canonical `CompileResult` shape defined above,
abbreviated here to show the fields convergence consumes:

```go
type CompileResult struct {
    Recipe       *Recipe
    Requirements NormalizedRequirements
    GraphWorkflow bool
    Diagnostics  []Diagnostic
    Provenance   Provenance
    Projection   PreviewProjectionSnapshot
    Steps        []CompiledStep
    RuntimeVars  []CompiledRuntimeVar
}
```

<!-- REVIEW: added per attempt-50-convergence-projection-contract -->

`internal/formula` also owns the generic convergence projection data after raw
decode, inheritance, expansion/aspect contribution, runtime-var defaulting, and
accepted artifact validation. It does not own convergence policy, but it is the
only package that can say where these fields came from:

```go
type CompiledConvergenceProjection struct {
    Enabled            SourceValue[bool]
    RequiredVars       []CompiledRuntimeVar
    EvaluatePrompt     SourceValue[string]
    EvaluatePromptPath SourceValue[string]
    RelevantStep       SourceValue[string]
    Retry              *CompiledRetryPolicy
    SourceFormula      string
    SourcePath         string
}
```

The projection rules are fixture-locked:

| Raw authored field | Compiler-owned output | Rule |
|---|---|---|
| root, child, loop body, expansion, aspect, or import convergence enablement | `Enabled` | inherited and overridden before projection; source attribution names the winning field |
| `required_vars` and runtime var declarations | `RequiredVars` | merged after inheritance/defaulting; duplicates preserve deterministic diagnostic order |
| inline `evaluate_prompt` | `EvaluatePrompt` | projected as content with source path/key/value |
| prompt path | `EvaluatePromptPath` | projected as path only; readability is convergence-domain validation before writes |
| selected output/relevant step | `RelevantStep` | selected from compiled steps after graph transforms, never by reopening TOML |
| retry policy | `Retry` | projected from the accepted compiled step policy |

Durable convergence, reconcile, missing-child repair, retry, next-iteration,
and speculative-wisp paths pass an `AcceptedCompileArtifact` through
`ProjectAcceptedFormula`. No durable convergence path may use preview
`ProjectFormula` output. The sequence is strictly compile, accept, project,
convergence-domain validate, then write; a failure at any earlier step leaves
zero root, child, iteration, hook, dependency, convoy, artifact-ref, retry, or
missing-child writes.

<!-- REVIEW: added per convergence-boundary -->

`internal/convergence/formula_projection.go` owns the projection API and
`internal/convergence/projection_validate.go` owns convergence-domain
validation. No other convergence file may decode formula TOML or own
source-field meanings.

```go
type ConvergenceFormulaView struct {
    ArtifactRef          string
    ArtifactIdentityHash string
    Projection           formula.CompiledConvergenceProjection
    PersistedProvenance  formula.Provenance // display/reporting only
}

type ConvergenceRuntimeVarValue struct {
    Name          string
    Value         string
    Source        formula.DiagnosticSource
    SatisfiedBy   string // explicit, default, secret_ref, or missing
    Redaction     string // none, redacted, hmac
    ValueHash     string
    DuplicateOf   string
}

type ConvergenceRuntimeInputs struct {
    RootID            string
    Operation         string
    RuntimeVars       []ConvergenceRuntimeVarValue
    VarsHash          string
    ExistingArtifactRef string
    ExpectedIdentityHash string
    CurrentHost      formula.HostCapabilities
}

func ProjectFormula(result *formula.CompileResult) (ConvergenceFormulaView, []formula.Diagnostic) // preview only
func ProjectAcceptedFormula(artifact formula.AcceptedCompileArtifact) (ConvergenceFormulaView, []formula.Diagnostic)
func ValidateProjection(view ConvergenceFormulaView, vars ConvergenceRuntimeInputs) []formula.Diagnostic
```

`ConvergenceFormulaView` is intentionally a thin view over
`formula.CompiledConvergenceProjection`. It does not restate normalized
requirements, host capability, pack binding identity, retry fields, runtime
vars, or source paths as convergence-owned fields. When convergence needs to
display provenance, it uses `PersistedProvenance` as provenance only; runtime,
retry, repair, next-iteration, fanout, and speculative-wisp behavior must not
branch on provenance, requirement source, host capability source, or artifact
ref spelling. Behavior branches only on accepted-artifact validation and the
compiler-owned projection fields.

`ConvergenceRuntimeInputs` is validated against accepted projection facts.
`ExistingArtifactRef` and `ExpectedIdentityHash` must match the artifact already
validated by `ValidateAcceptedArtifact`. Runtime vars are typed evidence:
explicit caller values, compiler defaults, secret references, missing values,
duplicates, value source, redaction mode, and value hash are preserved before
validation. `VarsHash` is computed from the accepted runtime-var projection and
the redacted value evidence, not from map iteration order. `CurrentHost` is
recorded for diagnostics but cannot reauthorize a changed-identity write.
Unknown runtime-var names are reported according to convergence policy;
duplicate names preserve all sources and emit the fixture-locked diagnostic
order; missing required vars fail before root, iteration, retry, hook,
dependency, convoy, or speculative-wisp writes.

<!-- REVIEW: added per DR55-convergence-projection-collapse -->

Projection parity is generated, not reviewed by eyeballing two structs. The
implementation owns
`internal/convergence/testdata/formula_projection_equivalence.yaml` with one
row per compiler projection field:

```yaml
- compiler_field: CompiledConvergenceProjection.RequiredVars
  convergence_view_path: Projection.RequiredVars
  owner: internal/formula
  consumer: internal/convergence
  may_branch: true
  branch_reason: convergence required-var satisfaction
- compiler_field: AcceptedCompileArtifact.Requirements
  convergence_view_path: none
  owner: internal/formula
  consumer: provenance-only through accepted-artifact diagnostics
  may_branch: false
  branch_reason: requirements already decided before projection
```

CI fails when `CompiledConvergenceProjection` gains a field without an
equivalence row, when `ConvergenceFormulaView` gains a duplicate field that is
not a direct projection wrapper, or when production convergence code branches
on `Requirements`, `HostCapabilities`, `RequirementSource`, `Provenance`,
`ArtifactRef`, root metadata, or accepted-artifact identity strings. The only
allowed identity branch is the formula-owned
`ValidateAcceptedArtifact(writeIntent)` result before projection.

`ValidateProjection` emits only these convergence-domain diagnostic codes:

| Code | Meaning |
|---|---|
| `convergence.formula_disabled` | convergence metadata explicitly disables convergence for the selected formula |
| `convergence.evaluate_prompt_missing` | convergence is enabled but no evaluate prompt or prompt path was projected |
| `convergence.evaluate_prompt_unreadable` | projected prompt path cannot be read before durable writes |
| `convergence.required_var_missing` | required convergence runtime var has no value |
| `convergence.retry_policy_invalid` | projected retry policy cannot support convergence iteration |
| `convergence.reserved_step_conflict` | formula step IDs collide with convergence-reserved step IDs |
| `convergence.relevant_step_missing` | no source-attributed step can own convergence output |

These diagnostics are separate from formula requirement diagnostics. They run
only after formula acceptance and must include source path/key/value from the
projection; they never reinterpret raw requirement fields.

`ProjectFormula` may report convergence-domain diagnostics such as a missing
evaluate prompt for preview paths, but it may not reinterpret raw formula
requirement fields or host capability. Durable convergence paths use
`ProjectAcceptedFormula`. The projection is the canonical source-attributed
carrier for convergence enablement, required vars, evaluate prompt content,
prompt path, convergence source key, and convergence step identity; no caller
may read those fields from raw TOML, root metadata, or a convergence-only
subset parser. `CreateConvergenceBead`, retry root creation, next iteration,
missing-child repair, speculative wisps, and the first `PourWisp` call all run
after this order: `CompileWithResult`, `AcceptCompileResult`,
`ProjectAcceptedFormula`, `ValidateProjection`, accepted artifact persistence
when needed, then durable root, metadata, wisp, retry, iteration,
missing-child, or child writes.

Existing convergence roots use persisted accepted compile artifacts for their
current formula even if the host capability is later downgraded. Any operation
that selects or compiles a new formula after the downgrade fails closed before
durable writes. This chooses artifact reuse for existing roots and fail-closed
semantics for new or changed formulas.

Active legacy convergence roots that lack accepted artifact metadata remain
observable and can finish already-created beads through existing root metadata
during the alias window. They cannot create a new retry root, next iteration,
replacement child, fanout fragment, or speculative wisp until the operator runs
`gc formula repair-root-artifact` and the root has a validated artifact ref.
If the formula source is missing, the host is downgraded, or the projection
fails, the operation returns the typed diagnostic and writes no retry metadata,
child, iteration, wisp, convoy, or continuation state.

<!-- REVIEW: added per attempt-50-active-root-recovery -->

Unrecoverable active roots have an explicit controller/operator contract:

| State | Automatic behavior | Operator repair path |
|---|---|---|
| accepted artifact present and supported | validate artifact and continue graph-specific writes that match the original identity, even after host downgrade | no repair needed; selecting a new formula still uses current host capability |
| legacy-only root, source present, host enabled | pause graph-specific mutation until operator repair succeeds | `gc formula repair-root-artifact <root-id> --dry-run --json`, then the same command without `--dry-run` stamps only the artifact ref and migration metadata |
| legacy-only root, source missing | observation, status, and already-created bead completion only | restore the exact source/pack ref, upgrade to a binary that can read existing metadata, or abandon/close the root with an operator reason |
| host downgraded before migration artifact exists | fail closed before artifact stamping or graph mutation | re-enable `[daemon] formula_v2`, upgrade/restore the host, or abandon the root |
| future capability or unsupported artifact schema | observation and cleanup that does not synthesize graph semantics | upgrade Gas City; old binary must not re-stamp or reinterpret the root |

Artifact repair is idempotent. A dry run reports the compile identity,
diagnostics, provenance, binding identity, and exact metadata that would be
stamped. The non-dry-run command writes only when compile, accept, projection,
and write-intent validation all pass; if the root already has the same artifact
ref, the command is a no-op. If it fails, it emits a typed diagnostic and
writes no artifact ref, metadata, child, dependency, hook, convoy, retry, or
iteration state.

`gc formula repair-root-artifact` is scheduled in caller sub-phase 4f before
any convergence, fanout, scope-check, retry, workflow-finalize, or
missing-child path requires accepted artifacts for active roots. Its dry-run
JSON is a release fixture:

```json
{
  "schema_version": 1,
  "root_id": "ga-example",
  "status": "would_stamp",
  "compile_identity": "<sha256>",
  "artifact_ref": "<durable-ref>",
  "formula": "review-loop",
  "content_hash": "<sha256>",
  "host_capability": "2",
  "diagnostics": [],
  "metadata_to_write": {
    "gc.formula_compile_artifact": "<durable-ref>"
  },
  "zero_write": true
}
```

The command has fixtures for missing source, downgraded host, unsupported
artifact schema, unsupported future requirement, already-stamped same artifact,
already-stamped conflicting artifact, concurrent repair losing the compare-and-
swap, and successful idempotent no-op. Failure cases assert zero writes to
artifact refs, metadata, child beads, dependencies, hooks, convoys, retry
state, iteration state, and fanout state.

<!-- REVIEW: added per attempt-73-repair-command-contract -->

`gc formula repair-root-artifact` has an executable command contract:

| Concern | Contract |
|---|---|
| inputs | exactly one root bead id plus optional `--city`, `--host-config`, `--dry-run`, `--json`, and `--force-same-identity` flags; it never accepts a formula path that bypasses root provenance |
| source resolution | resolve the original formula from accepted root metadata, legacy root metadata, pack/import binding, lockfile key, source path, and content hash; fail closed when two sources could match |
| validation order | load root, classify root facts, resolve source/provenance, compile with current host, accept artifact, project, validate write intent, then write artifact ref |
| atomic write | write immutable artifact payload first, then compare-and-swap root metadata from the observed revision to the new artifact ref and migration metadata; orphaned payloads are safe because roots reference artifacts by immutable ref |
| idempotency | same accepted identity and artifact ref exits `0` with `status: already_stamped`; no metadata rewrite is required |
| conflict handling | a different existing artifact ref, changed source identity, changed content hash, unsupported schema, or mismatched root formula exits `2` with diagnostics and zero writes unless the future design defines a human-reviewed replacement mode |
| exit codes | `0` for stamped, would-stamp, or already-stamped success; `1` for I/O, lock acquisition, unreadable store, or acquisition failure; `2` for formula, host, projection, identity, or conflict diagnostics |
| concurrent repair | compare-and-swap permits one writer; losers reload root metadata and return already-stamped if the identity now matches, otherwise return conflict with zero writes |
| protected boundaries | command writes only immutable artifact payload plus root artifact-ref/migration metadata after validation; it never creates child beads, dependencies, hooks, convoys, retry state, fanout state, convergence state, or fired-order metadata |

Abandonment is explicit operator action, not a compiler fallback. The operator
may close or mark a root abandoned through existing bead/root lifecycle
commands with a reason that references the diagnostic. The controller must not
silently convert an unrecoverable graph root into a v1 workflow, synthesize
missing source, or drop children to make progress.

Cross-binary fixtures cover old and new binaries reading canonical-only,
dual-stamped, legacy-only, missing-source, downgraded-host, and
future-capability roots. The expected result for unsupported combinations is
visibility plus stable remediation and zero graph-specific durable writes.

<!-- REVIEW: added per DR48-convergence-projection-identity -->

Convergence projection parity is fixture-locked against the accepted artifact.
Golden fixtures cover requirement source, formula source path, pack/import
binding, content hash, compile artifact ref, host capability provenance,
runtime vars, required-var satisfaction, convergence prompt fields, retry
policy, and source-attributed relevant step identity. `ValidateProjection`
receives compiler-owned runtime-var satisfaction facts or caller vars through
typed projection fields; it never reopens the formula file or reconstructs
required vars from a convergence-only subset parser.

Legacy convergence-root identity is stable during migration:

| Legacy root state | Allowed operation |
|---|---|
| root has accepted artifact ref | validate and reuse the artifact when identity matches, even after host downgrade |
| root has canonical metadata but no artifact ref | operator repair command must stamp a validated ref before mutation; runtime mutator fails closed with zero writes |
| root has only legacy `gc.formula_contract=graph.v2` | observable and finish already-created beads; graph-specific mutation requires operator repair command first |
| root source cannot be resolved | observation only; retry, next iteration, missing-child repair, and speculative wisp creation fail closed |
| root metadata names future capability or unsupported schema | observation only with unknown-capability diagnostic |

Zero-write fixtures are required for convergence create, retry, next
iteration, missing-child repair, speculative wisps, fanout fragments, and
active legacy-root migration. Each fixture asserts no root, iteration bead,
replacement child, retry metadata, hook, dependency, convoy, or artifact ref is
written when formula acceptance or projection validation fails.

<!-- REVIEW: added per convergence-canonical-projection -->

`internal/convergence/formula.Formula` and `ValidateForConvergence` are
retired or rewritten as compatibility shims over `ProjectFormula`. They may not
parse TOML, read raw `[requires]`, inspect `contract`, or reconstruct required
vars from a formula subset. A static guard under `internal/convergence` fails
when production code imports the legacy subset parser, reads raw formula files,
or searches for convergence fields outside the compiled projection.

Convergence-specific validation runs only after formula acceptance:

1. Compile with current host capability and caller vars/options.
2. Accept the compile result, rejecting fatal diagnostics and persisting a
   compile artifact when required.
3. Project typed convergence fields from canonical compile output.
4. Validate convergence domain rules: `Enabled`, required vars, evaluate prompt
   content, reserved step IDs, retry policy, and convergence-relevant step
   identity.
5. Write convergence root, iteration, retry, missing-child, or speculative-wisp
   state.

The durable metadata contract for convergence contains
`gc.convergence_formula_compile_artifact`, `gc.convergence_formula_name`,
`gc.convergence_formula_content_hash`, `gc.convergence_formula_pack_revision`,
and `gc.convergence_formula_host_capability`. Retry and next-iteration paths
first read the existing artifact ref. If the retry reuses the same formula,
vars hash, options hash, and content hash, it reuses the persisted artifact
even after a host downgrade. If any identity changes, it must compile and accept
again against the current host capability before writing retry metadata.

`gc.convergence_formula_compile_artifact` is the only convergence artifact-ref
key. Legacy or accidental alternatives such as `gc.formula_compile_artifact`
on a convergence child, `gc.convergence_artifact`, or producer-local aliases
are display-only during migration and cannot authorize retry, next iteration,
missing-child repair, or speculative wisp writes. If more than one artifact ref
is present for the same convergence root, `convergence.formula_artifact_conflict`
is emitted with every conflicting key/value and the operation fails before
writes. The conflict fixture covers same ref under duplicate keys, different
refs, missing ref, stale ref, and unsupported artifact schema.

<!-- REVIEW: added per attempt-78-convergence-artifact-contract -->

Convergence artifact references have one precedence rule:

| Metadata shape | Runtime behavior |
|---|---|
| canonical `gc.convergence_formula_compile_artifact` only | load and validate through formula-owned artifact loader |
| canonical plus legacy alias with same value | accepted during dual-stamp migration, emits migration warning on display/report surfaces only |
| canonical plus legacy alias with different values | `convergence.formula_artifact_conflict`, zero writes |
| legacy alias only | display-only; operator repair command must restamp canonical key before graph-specific mutation |
| no artifact ref | observation only for existing roots; new writes require compile/accept first |
| unsupported artifact schema | display diagnostic and zero graph-specific writes |

Dual-stamp migration is allowed only for roots created before convergence
callers complete sub-phase 4f. New convergence roots write only the canonical
key plus future-readable formula metadata. The repair command may remove alias
keys only after it has loaded the canonical artifact, validated identity, and
written a release-report row naming the old and new keys. Conflict and repair
fixtures assert zero writes to iteration, retry, child, convoy, hook, and
artifact-ref metadata when precedence cannot be resolved.

Required migration rows:

| Path | Required signature/flow | Zero-write test |
|---|---|---|
| `internal/convergence` create | `CompileWithResult` -> `AcceptCompileResult` -> `ProjectAcceptedFormula` -> convergence validation -> write root | disabled host leaves store unchanged |
| convergence retry | reuse persisted accepted compile artifact for existing root, or compile/accept new target formula before retry write | disabled host leaves retry metadata unchanged when compiling new formula |
| missing-child repair | load and validate the persisted accepted artifact before deciding whether a child should exist; compile/accept again only when the selected formula identity changes | disabled host does not create replacement child from a new formula |
| speculative wisp creation | compile/accept all candidate formulas before first speculative write | disabled host writes no speculative root or child |
| `internal/dispatch/fanout.go` fragment expansion | parent accepted artifact plus fragment `CompileWithResult`/`AcceptCompileResult` calls before expansion writes | disabled host writes no fragment child, convoy, or continuation |
| next-iteration convergence/fanout path | compile/accept iteration formula before iteration bead creation | disabled host writes no next-iteration bead |
| active legacy root migration | operator-run `gc formula repair-root-artifact` compiles original formula, accepts artifact, validates projection, then stamps artifact ref before any graph-specific mutation | missing source, unsupported future metadata, or downgraded host writes no artifact ref, retry metadata, child, or iteration |

Required tests cover enabled and disabled host capability for CLI sling, API
sling, order-created wisps, convergence root creation, convergence retry,
scope fragments, expansion/aspect requirements, and continuation of an
already-created graph workflow after the flag is disabled.

## Forward Compatibility

<!-- REVIEW: added per forward-compatibility-axis -->

V0 is intentionally fail-closed. Unknown `[requires]` axes and unsupported
compiler expressions are hard validation errors, not ignored future hints. That
lets old binaries distinguish "upgrade required or misspelled key" from a
formula they can safely run.

<!-- REVIEW: added per DR48-future-axis-shape -->

The future-axis shape is normative now so old readers can fail closed
deterministically. Every `[requires]` axis is a flat scalar string directly
under the top-level `[requires]` table:

```toml
[requires]
formula_compiler = ">=2"
```

Nested tables, dotted tables, arrays, inline tables, integers, floats,
booleans, null-equivalent values, duplicate keys, and user-defined namespaces
are invalid for all axes unless a future design changes the schema version and
adds typed parser support. A future axis must have exactly one owner package,
one accepted byte grammar, one host-capability field, one diagnostic family,
one provenance field, docs, matrix rows, and old-reader fixtures. Adding an
axis that affects persisted behavior bumps the requirements metadata schema
version and the accepted artifact version in the same change.

<!-- REVIEW: added per attempt-79-axis-grammar-future-minima -->

Requirement axis identifiers are byte-exact lowercase ASCII:
`[a-z][a-z0-9_]{0,63}`. They may not contain dots, dashes, uppercase letters,
Unicode, whitespace, empty segments, TOML dotted-table syntax, or user
namespace prefixes. Unknown identifiers that match the grammar emit
`formula.requirement_unknown_axis`; malformed identifiers reach the raw-shape
or parser-boundary diagnostic appropriate to the TOML/JSON representation. V0
knows only `formula_compiler`.

Unsupported future compiler minima have several fixture rows, not only `>=3`:

| Value | Diagnostic |
|---|---|
| `">=3"` | `formula.compiler_requirement_unsupported_future` with requested capability `3` |
| `">=4"` | same diagnostic with requested capability `4` |
| `">=99"` | same diagnostic with requested capability `99` |
| maximum in-range ASCII integer above known capability | same diagnostic and no overflow |
| overflow-sized ASCII integer | parser-boundary or invalid-syntax diagnostic as fixed by the grammar suite; never accepted |

Combined future-axis fixtures cross unknown axis, unsupported future
`formula_compiler`, host disabled, and source contribution. Unknown axes are
reported before unsupported future compiler minima, and both are reported
before host satisfaction because no normalized requirement is usable yet.

Explicit `formula_compiler = ">=1"` remains accepted v0 provenance for the
default formula compiler capability. It is behaviorally equivalent to omitted
requirements but source-attributed as `requires`; deprecating or removing it
would need a separate compatibility design because existing reports may rely on
that explicit author intent. Future unsupported axes and unsupported schema
versions never convert a root into a known graph workflow on old binaries; old
readers classify it as `unknown_future_capability_workflow` for observation
only and refuse graph-specific writes.

Capability semantics:

| Topic | Rule |
|---|---|
| Compiler capability numbers | Monotonic minimums. A host at capability `N` satisfies formula requirement `<=N`. |
| Accepted v0 syntax | Closed grammar: omitted, `>=1`, and `>=2` only. |
| Unsupported future capability | `>=3` is `formula.compiler_requirement_unsupported_future` until the binary implements capability 3. |
| Invalid syntax | Bad spacing, exact numbers, decimals, empty strings, and wrong TOML types remain `formula.compiler_requirement_invalid_syntax` or type diagnostics, not upgrade hints. |
| Unknown axis | Always `formula.requirement_unknown_axis`; old binaries do not ignore future axes. |
| Extension namespace | No user-defined axes in v0. A future extension axis must be a first-class typed field, not `x-*` passthrough. |
| Zero value | `CompilerCapability(0)` is an internal bug and must never escape normalization. |
| Provenance | Each axis records source path/key/value and normalized value independently. |
| Host capability growth | Guarded named fields on `HostCapabilities`; no raw string map until at least two implemented axes prove a generic representation is simpler. |
| Requirement source | `RequirementSource` is attribution only; behavior uses normalized capability values, never source spelling. |
| Construct registry | Every construct registry entry has `min_compiler_capability` and source attribution; no boolean "is v2" helpers in new code. |

<!-- REVIEW: added per DR55-provenance-non-authority -->

Observable metadata is not behavioral authority. Requirement-source fields,
normalized requirement projections, future-axis manifests, host-capability
source values, provenance fields, migration hints, dashboard fields, generated
TypeScript fields, release reports, and accepted-artifact display fields are
diagnostic or provenance surfaces unless this document names a formula-owned
validation function that consumes them. Runtime decisions are limited to:
`internal/formula` requirement normalization and host satisfaction,
`ValidateAcceptedArtifact(writeIntent)`, and
`internal/sourceworkflow.ClassifyWorkflowRoot` for workflow-root visibility.
Dispatch, convergence, retry, fanout, dashboard, API, release tooling, and
external tools may display provenance, but they may not branch on whether a
requirement came from omitted/default, `[requires]`, legacy `contract`, dual
declaration, a particular host-source kind, or a pack/source spelling.

Canonical axis manifests are byte-stable. `gc.formula_requirement_axes` is a
comma-separated list of lowercase ASCII axis ids sorted by byte order with no
spaces, duplicates, empty elements, or percent-encoding. V0 writes exactly
`formula_compiler`. Unknown, malformed, duplicated, or unsorted manifests on an
old reader classify the root as `unknown_future_capability_workflow` for
observation only and fail closed for graph-specific writes.

Default-capability equivalence fixtures are required:

| Source spelling | Normalized capability identity | Allowed difference |
|---|---|---|
| omitted `[requires]` | compiler capability `1` | `RequirementSource=omitted` provenance |
| empty `[requires]` | compiler capability `1` | source position for empty table when displayed |
| `[requires] formula_compiler = ">=1"` | compiler capability `1` | `RequirementSource=requires` provenance |

The normalized identity hash used for behavior excludes the source spelling
above except for diagnostic/provenance fields. Tests fail if any runtime path,
accepted-artifact validation result, workflow-root kind, convergence behavior,
fanout behavior, or dispatch behavior differs across those three inputs.

<!-- REVIEW: added per attempt-79-default-artifact-identity -->

Default-capability identity is also fixed for durable artifacts:

| Identity surface | Omitted, empty table, and explicit `>=1` behavior |
|---|---|
| `CompileIdentity.CompileID` | identical when formula bytes, vars, options, search paths, host capability, and pack binding are otherwise identical |
| `AcceptedArtifactIdentity` hash | identical; `RequirementSource` is excluded from the behavioral identity record |
| accepted artifact ref | deduplicates to the same immutable ref when the accepted projection and provenance identity are otherwise identical |
| workflow-root kind | `default_capability_workflow` for all three spellings |
| convergence/fanout/retry validation | no behavioral difference; validation uses capability `1` only |
| diagnostics and reports | may show `omitted`, empty-table source position, or `requires` provenance as display fields |

If the accepted artifact serializer stores the display provenance for
`RequirementSource`, that field is outside the proof hash and outside artifact
deduplication. A future change that wants source spelling to affect artifact
identity must introduce a new artifact version and old-reader fixtures; it may
not change v0 hashes in place.

<!-- REVIEW: added per DR53-forward-compatibility-invariants -->

Forward-compatibility invariants are binding:

1. Compiler capability evolution is additive only. A released capability number
   is never removed, reused, or semantically redefined.
2. Released constructs keep their meaning. A future release may require a
   higher capability for a new construct or new axis, but it may not make a
   previously valid released construct mean something different under the same
   capability.
3. Released axis byte grammars are frozen. If a richer grammar is needed, it
   becomes a new typed axis or a schema-versioned extension with old-reader
   fixtures, not a reinterpretation of existing `formula_compiler` strings.
4. `RequirementSource` is diagnostic and provenance data only. Runtime,
   dispatch, convergence, and persistence behavior cannot branch on whether a
   requirement came from omitted/default, `[requires]`, legacy `contract`, or
   dual declaration.
5. Schema-version bumps are required whenever persisted root metadata,
   accepted artifacts, projection snapshots, or old-reader behavior changes.
   Parser-only diagnostics that do not affect persisted behavior may avoid a
   schema bump only with a fixture proving old-reader behavior is unchanged.

<!-- REVIEW: added per attempt-90-future-capability-release-invariants -->

Released construct capability rows are immutable release artifacts. The
registry writes `docs/release/formula-compiler-construct-registry.json` during
release validation with construct id, axis, minimum, owner, decoded paths,
source-attribution fields, first release tag, and registry-owning binary SHA.
After a row appears in a released artifact, a later change may append new rows
or add a higher-capability row for a new construct shape, but it may not lower,
remove, rename, or reinterpret the released row. If a bug requires changing a
released row, the fix must add a new schema/versioned row and old-reader
fixtures; it cannot edit history in place.

First-party graph packs are validated with the latest registry-owning binary
before publication. The publication gate records the binary SHA, registry JSON
digest, pack refs, locked revisions, content hashes, and requirement-diff
report. A pack cannot claim a minimum floor from an older registry artifact
when the source uses constructs known only to a newer registry. External packs
are allowed to set their own floor, but Gas City's resolver still evaluates
the declared floor before formula selection and records the comparator binary
in the external-support row.

Behavioral identity and provenance remain separate for explicit defaults.
Omitted `[requires]`, empty `[requires]`, and explicit
`formula_compiler = ">=1"` have the same behavioral identity, accepted artifact
deduplication, and workflow-root kind. Their different `RequirementSource`
values are append-only diagnostic/provenance enum values. Production behavior
cannot branch on them, and future enum additions cannot change existing
runtime behavior without a new design and schema-versioned fixtures.

Integer parsing is byte-bounded. The v0 grammar parser accepts only ASCII
digits after the exact prefix `>=`, rejects leading signs, leading zeroes for
multi-digit values, Unicode digits, whitespace, decimals, separators, and
empty values, and detects overflow before converting to `int`. Overflow emits
the fixture-locked invalid-syntax or parser-boundary diagnostic; it never wraps
to a supported or unsupported future capability. Mixed manifests that combine
unknown axes, malformed axes, unsupported future minima, and persisted future
metadata have old-reader fixtures for source parse, root classification,
accepted artifact load, API/dashboard projection, retry, fanout, convergence,
and cleanup.

Every future requirement axis must add all of these in the same change:

- typed normalized state, not raw maps or raw TOML pass-through
- accepted grammar and rejected-value tests
- diagnostic codes, canonical remediation, and projection behavior
- provenance fields and persisted root metadata when the axis affects runtime
- docs, examples, generated schemas/help, and stale-guidance checks

<!-- REVIEW: added per future-capability-guardrails -->

`RequirementSource` misuse is CI-guarded. Production code outside diagnostic,
provenance, migration-report, and compatibility-warning paths may not branch on
`RequirementSourceRequires`, `RequirementSourceContract`, `RequirementSourceDual`,
or `RequirementSourceOmitted`. Runtime behavior branches on
`NormalizedRequirements.FormulaCompiler()` and host capability only. The guard
prevents a future bug where dual declarations, legacy aliases, or source
spelling accidentally change execution semantics.

The construct registry is shaped for future capabilities:

```go
type AxisMinimum struct {
    Axis       string
    Minimum    string
    Capability int
}

type ConstructCapability struct {
    ID                    string
    Requirements          []AxisMinimum
    Locations             []ConstructLocation
    DiagnosticSource      SourceAttribution
}
```

V0 entries normally contain one requirement,
`{Axis: "formula_compiler", Minimum: ">=2", Capability: 2}`. Future constructs
can require several axes without encoding cross-axis policy in callers. Adding
capability `3` means adding registry entries or raising existing entries from
`2` to `3`, extending the matrix dimensions, updating docs and pack-floor
artifacts, and adding old-reader fixtures that prove older binaries fail
closed. Old readers that encounter persisted metadata for a future capability,
for example `gc.formula_compiler_capability=3`, must classify the root as a
workflow root when canonical workflow-root keys are otherwise valid but must
not classify it as a graph workflow whose semantics they understand. They emit
or surface an unknown-capability diagnostic instead of silently running new
semantics.

<!-- REVIEW: added per DR33-future-root-lifecycle -->

Future-root lifecycle rules for old binaries:

| Operation | `unknown_future_capability_workflow` behavior |
|---|---|
| Observation, list, API/dashboard read models, and convoy/source scans | include the root with an unknown-capability diagnostic and no graph-specific inference |
| Cleanup of already-closed or orphaned records | allowed only when cleanup does not inspect or synthesize future graph semantics |
| Retry, fanout, continuation, next iteration, missing-child repair, child creation, or `on_complete` launch | fail closed before writes with an unknown-capability diagnostic |
| Selecting or compiling a new formula that requires the future capability or future axis | fail closed through parser/host capability diagnostics before writes |
| Existing accepted artifact load | allowed only to display provenance and diagnostics; not accepted as authorization for graph-specific writes the old binary cannot understand |

Old-reader fixtures must cover persisted roots with
`gc.formula_compiler_capability=3`, future `[requires]` axes, missing artifact
refs, stale artifact refs, dual-stamped legacy metadata plus future canonical
metadata, and dashboard/API projections. The expected result is visibility for
operators, zero graph-specific durable writes, and stable remediation to
upgrade Gas City.

Old-reader cross-product fixtures are required, not sampled:

| Future compiler capability | Future axis | Artifact/schema state | Expected old-reader behavior |
|---|---|---|---|
| absent | unknown axis in source only | no root metadata | parser fails with `formula.requirement_unknown_axis`; zero writes |
| `3` | absent | canonical root metadata schema understood | root visible as `unknown_future_capability_workflow`; zero graph-specific writes |
| `3` | present | accepted artifact schema understood but axis unsupported | display provenance only; retry/fanout/repair fail closed |
| `2` | present | requirements schema version bumped | root visible for observation; graph-specific writes fail closed until upgraded |
| `3` | present | unsupported artifact or projection snapshot schema | display diagnostic and remediation; no artifact repair or restamp |

The fixture generator crosses source parsing, persisted root metadata,
accepted artifact load, API/dashboard projection, retry, fanout, convergence,
and cleanup. Any unsupported combination must name the diagnostic code and the
zero-write assertion rather than relying on prose.

Every new compiler capability or requirement axis has a release checklist:

| Checklist item | Required evidence |
|---|---|
| parser and matrix | byte-exact grammar, rejected values, unsupported future rows |
| host capability | typed config edge conversion and invalid-value diagnostic |
| durable metadata | canonical keys, old-reader behavior, accepted artifact fields |
| pack compatibility | `requires_gc` floor and requirement-diff report |
| diagnostics | CLI/API/dashboard/event golden projections |
| docs and generated artifacts | reference docs, examples, schema/help/OpenAPI/TS updates |
| stale guards | raw-consumer, `RequirementSource`, and construct registry guards updated |

Compiler capability numbers are stable once released and are never reused for
different semantics. If a future capability such as `3` is introduced, older
binaries reject `>=3` with `formula.compiler_requirement_unsupported_future`;
newer binaries may accept it only after adding typed support, tests,
diagnostics, and provenance. If a future requirement is not monotonic, it must
use a different axis rather than overloading `formula_compiler`.

Worked second-axis example:

```toml
[requires]
formula_compiler = ">=2"
state_store = ">=2"
```

`state_store` is illustrative, not part of v0. Adding it requires a typed
host-capability constructor and accessor for state-store capability,
`NormalizedRequirements.StateStore()`, `formula.state_store_requirement_*`
diagnostics mirroring the syntax/type/future/unsatisfied split above,
provenance fields for `requires.state_store`, persisted metadata such as
`gc.formula_state_store_requirement`, validation-matrix dimensions crossing
`state_store` with formula compiler capability, pack floor artifacts naming
the first binary that satisfies the new axis, and workflow-root predicates that
continue to classify roots by graph workflow using formula compiler metadata
only. Old binaries fail closed with `formula.requirement_unknown_axis` instead
of silently running a formula whose state-store requirement they cannot satisfy.

## Consequences

- Formula consistency remains anchored at the pack revision boundary.
- The formula header declares requirements, not artifact identity and not
  runtime compiler choice.
- Existing graph-v2 formulas keep working through a measurable compatibility
  alias.
- Operators get one stable diagnostic contract across CLI, API, dashboard,
  orders, controller, and convergence.
- Implementation work is larger than a field rename because all behavioral
  consumers must move to the normalized result before first-party formulas can
  become requires-only.
