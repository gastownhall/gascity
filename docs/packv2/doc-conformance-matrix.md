# Pack/City v2 Conformance Matrix

This document turns the reconciled pack/city v.next docs into an
executable conformance plan for the current Pack/City v2 rollout.

The goal is not to restate the full design. The goal is to answer three
practical questions:

1. what behavior should block CI now
2. what behavior should enter the suite as soon as warning plumbing lands
3. what behavior is documented intent, but must not be treated as a
   release blocker yet

## Authority order

Use the sources in this order when deciding what the suite should assert:

1. [skew-analysis.md](skew-analysis.md) — release gating ledger for the
   current desired state
2. [migrating-to-pack-vnext.md](../guides/migrating-to-pack-vnext.md) —
   migration-target behavior, but only where `skew-analysis.md` does not
   mark the surface missing, deferred, or non-gating
3. [doc-agent-v2.md](doc-agent-v2.md) — prompt, template, fragment, and
   prompt-related patch behavior
4. [doc-pack-v2.md](doc-pack-v2.md) and
   [doc-directory-conventions.md](doc-directory-conventions.md) —
   supporting design and directory guidance; useful, but not allowed to
   overrule `skew-analysis.md`
5. [TESTING.md](../../TESTING.md) — which test tier to use

If a design doc describes an ideal v.next surface but
`skew-analysis.md` marks it missing or deferred, keep it in the matrix as
tracked work, not as a CI gate.

## Test tier mapping

| Tier | Use it for |
|---|---|
| Unit / package tests | discovery, merge order, path resolution, template gating, warning classification |
| Testscript (`cmd/gc/testdata/*.txtar`) | user-visible migration, command success/failure, warning text, rewritten layout |
| Docsync | keeping tutorial-facing command examples aligned with testscript coverage |
| Integration | only when real external infra is required; not the default tier for pack/city schema conformance |

## Gate In CI Now

These are settled enough, and implemented enough, to block CI now.

| Area | Required behavior | Suggested tier | Current implementation seam |
|---|---|---|---|
| Root composition | `pack.toml` and `city.toml` are composed together rather than treated as separate products | Unit + testscript | `internal/config/compose.go` |
| Pack imports | `[imports.<binding>]` in `pack.toml` resolves and composes imported content | Unit + testscript | `internal/config/pack.go` |
| Import target taxonomy | `source` stays the only public locator field; `gc import add` classifies the resolved target as plain directory, tagged git, untagged git, or invalid pack target, then synthesizes `version` accordingly (`none`, semver default, or `sha:`) | Unit + testscript | `cmd/gc/cmd_import.go`, `internal/packman/resolve.go` |
| Rig imports | `[rigs.imports.<binding>]` in `city.toml` resolves for the targeted rig | Unit + testscript | `internal/config/pack.go`, `internal/config/compose.go` |
| Agent discovery | `agents/<name>/` creates an agent without requiring `[[agent]]` | Unit | `internal/config/agent_discovery.go` |
| Current runtime provider resolution | Gate only the implemented runtime chain we are willing to freeze in this release wave: `agent.start_command` escape hatch, then `agent.provider`, then `workspace.provider`, then auto-detect; `workspace.start_command` is only the no-provider escape hatch. Do not treat the replacement/deprecation direction from `skew-analysis.md` as part of this row. | Unit | `internal/config/resolve.go` |
| Provider preset merge and lookup | Imported pack providers merge into the city provider map additively, city/local providers shadow imported ones on name collision, and provider lookup layers city overrides onto builtins when supported | Unit | `internal/config/pack.go`, `internal/config/resolve.go` |
| Prompt naming | `prompt.md` is inert markdown and `prompt.template.md` enables template processing | Unit + testscript | `internal/config/agent_discovery.go`, `cmd/gc/prompt.go` |
| Overlay discovery | pack-wide `overlay/` and agent-local `agents/<name>/overlay/` are discovered by convention | Unit | `internal/config/agent_discovery.go`, `internal/overlay/overlay.go` |
| Provider overlay filtering | only `per-provider/<provider>/` content for the effective provider is materialized | Unit | `internal/overlay/overlay.go` |
| Namepool convention | `agents/<name>/namepool.txt` is discovered by convention | Unit | `internal/config/agent_discovery.go` |
| Template fragments | `template-fragments/` and `agents/<name>/template-fragments/` are discovered and rendered into template prompts | Unit + testscript | `cmd/gc/prompt.go` |
| Agent-local auto-append bridge | `append_fragments` declared on an agent applies only to `.template.md` prompts and does nothing to plain `.md` prompts | Unit + testscript | `cmd/gc/prompt.go` |
| `[agent_defaults]` auto-append bridge | `[agent_defaults].append_fragments` composes and auto-appends only for `.template.md` prompts | Unit + testscript | `internal/config/compose.go`, `cmd/gc/prompt.go` |
| Agent defaults layering | `[agent_defaults]` is legal in both `pack.toml` and `city.toml`, with city winning on merge; runtime inheritance is gated only for fields the implementation actually applies today | Unit | `internal/config/compose.go`, `internal/config/config.go` |
| Qualified patch targeting | imported agents can be targeted by qualified name in `[[patches.agent]]` | Unit | `internal/config/patch.go` |
| Patch prompt template gating | An explicitly patched `prompt_template` path follows the same `.template.` rule as agent prompt files: `.template.md` renders, plain `.md` stays inert | Unit | `internal/config/patch.go`, `cmd/gc/prompt.go` |
| Formulas filename truth | PR2 formula files use flat `formulas/<name>.toml` filenames as the current truth surface | Unit + testscript | `cmd/gc/system_formulas.go`, `internal/citylayout/layout.go` |
| Orders discovery | top-level `orders/` discovery works by convention | Unit | `internal/orders/discovery.go` |
| Commands discovery | The default `commands/<name>/run.sh` discovery path works; final manifest shape remains non-gating | Unit + testscript | `internal/config/command_discovery.go` |
| Doctor discovery | The default `doctor/<name>/run.sh` discovery path works | Unit + testscript | `internal/config/doctor_discovery.go` |
| Legacy migration rewrite | `gc doctor` inventories legacy Pack/City v1 usage and `gc doctor --fix` performs the safe mechanical rewrites for agent directories, prompt/overlay/namepool moves, and import-oriented composition. Legacy remote `workspace.includes` is a hard-break migration issue, not a runtime compatibility target. | Testscript | `cmd/gc/doctor_v2_checks.go`, migration fix path TBD |
| Registration naming | `gc register --name` stores the chosen machine-local alias in the supervisor registry without mutating `city.toml`; plain `gc register` uses the effective city identity (site binding / legacy config / basename) and stores that value in the registry without backfilling `city.toml` | Unit | `cmd/gc/cmd_register.go`, `cmd/gc/cmd_supervisor_city.go`, `internal/supervisor/registry.go` |

## Add To CI When Warning Plumbing Lands

These behaviors are part of the current Pack/City v2 desired state, but they depend on
deprecation or warning infrastructure that is not yet fully trustworthy.
Write the tests now if helpful, but do not let them fail CI until the
warning surface is implemented end to end.

| Area | Expected behavior | Suggested tier |
|---|---|---|
| Legacy `[[agent]]` | accepted for schema 2 migration compatibility, but emits a loud warning | Testscript |
| Legacy composition | `workspace.includes`, `workspace.default_rig_includes`, and `rig.includes` emit loud warnings directing users to imports | Testscript |
| Legacy prompt injection | `global_fragments`, `inject_fragments`, and `inject_fragments_append` emit deprecation warnings toward `append_fragments` or explicit `{{ template }}` | Testscript + unit |
| Legacy fallback model | `fallback` emits a loud warning and is not part of the v.next authoring surface | Testscript |
| Legacy path wiring | `prompt_template`, `overlay_dir`, and `namepool` on legacy agent definitions warn during migration-facing flows | Testscript |
| Workspace soft deprecations | `workspace.provider`, `workspace.start_command`, and `workspace.install_agent_hooks` warn with the documented replacement path; `workspace.name` and `workspace.prefix` now have an active site-binding migration path via `gc doctor --fix` | Testscript |
| Formula directory path | `[formulas].dir = "formulas"` soft-warns; any other value is rejected | Unit + testscript |
| Rig override naming | `rig.overrides` is accepted with a soft warning in favor of `rig.patches` | Unit + testscript |
| Fragment-only include | top-level `include` stays fragment-only and rejects pack-composition content such as `[imports]`, include-based composition, or `pack.toml` references | Unit + testscript |

## Track, But Do Not Gate Yet

These are either explicitly missing in the implementation or still too
unsettled to be reliable release gates.

| Area | Current status | Why it is non-gating for now |
|---|---|---|
| `[defaults.rig.imports.<binding>]` loader support | documented intent, not implemented | Migration tooling may write it, but the loader does not yet honor it |
| `[agent_defaults] provider` driving runtime provider selection | migration target is documented, but runtime behavior is not aligned enough to gate | Current implementation still resolves runtime defaults through `workspace.provider` / `ResolveProvider`; locking in the future rule now would create false failures |
| `patches/` directory convention for imported prompt replacements | documented in v.next docs, not implemented | Current implementation still relies on explicit patch fields rather than full loader-discovered patch files |
| Pack `skills/` discovery | documented, not implemented | First slice is current-city-pack only with list-only visibility; imported-pack catalogs are later |
| `mcp/` TOML abstraction | documented, not implemented | Same first-slice scope as skills: current-city-pack only, list-only visibility first, provider projection later |
| `.gc/site.toml` site-binding split | implemented for workspace identity + `rig.path` | Loader overlays `.gc/site.toml`, commands write site bindings, and `gc doctor --fix` migrates legacy `workspace.name`, `workspace.prefix`, and `rig.path`; `rig.prefix` / `rig.suspended` remain in `city.toml` in this phase |
| Final doctor manifest symmetry/shape | still under-specified | Discovery is testable now, but the final manifest shape should not be frozen by the first-pass suite |
| Command collision rules and final command/doctor manifest shape | still under-specified | The docs still use "current preferred direction" language rather than frozen contract language |
| Legacy cleanup surfaces | e.g. stale `.order.` / `.formula.` references in old docs/examples or dismantling `[workspace]` | Keep as handoff cleanup, but do not treat it as a current-wave ship gate |

## Import Source Coverage

These cases should be covered by the import-focused unit/testscript bundle so the
POR in `doc-packman.md` stays executable:

- plain path to plain directory pack => import is written with no `version`
- plain path to local git repo with semver tags => import is canonicalized to a
  git-backed source and gets the default semver constraint
- plain path to local git repo without semver tags => import is canonicalized to
  a git-backed source and gets `sha:<commit>`
- `file://` local git repo with semver tags => default semver constraint
- `file://` local git repo without semver tags => default `sha:<commit>`
- bare `github.com/org/repo` => treated as git-backed import syntax
- invalid pack target / schema mismatch => hard error, no import written

## First Fixture Set

If we start implementing the suite immediately, this is the smallest set
that would materially raise confidence without exploding scope.

### Testscript

- keep extending `cmd/gc/testdata/migrate-v2.txtar` as the canonical
  migration regression
- add `pack-v2-imports.txtar` for `pack.toml` imports and rig-scoped
  imports
- add `pack-v2-warnings.txtar` for legacy field warnings once warning
  plumbing is stable
- add `pack-v2-errors.txtar` for hard errors such as illegal
  `[formulas].dir` values and non-fragment top-level includes
- extend import-focused fixtures for:
  - plain path directory imports
  - plain path git-backed imports
  - bare `github.com/...` imports
  - invalid pack targets

### Unit tests

- `internal/config/compose.go`: pack + city merge order, field placement,
  city-wins semantics
- `cmd/gc/cmd_import.go`: import target classification and default version
  synthesis
- `internal/packman/resolve.go`: semver vs `sha:` defaulting and git source
  resolution
- `internal/config/resolve.go`: runtime provider resolution and provider
  preset lookup/merge behavior
- `internal/config/agent_discovery.go`: `agents/<name>/`, prompt naming,
  overlay and namepool conventions
- `cmd/gc/prompt.go`: `.template.` gating, fragment lookup, and
  `append_fragments` behavior for both agent-local and
  `[agent_defaults]` sources
- `internal/overlay/overlay.go`: provider filtering and overlay layering
- `internal/config/patch.go`: qualified-name patch targeting and patched
  prompt-template path handling
- `internal/config/doctor_discovery.go`: default doctor discovery
- `cmd/gc/system_formulas.go`: PR2 formula/order filename truth
- `internal/orders/discovery.go`: top-level orders discovery

## Exit Criteria

The suite is strong enough to drive product quality when all of the
following are true:

1. every row in **Gate In CI Now** has at least one automated assertion
2. every row in **Add To CI When Warning Plumbing Lands** has a named test
   owner, even if the assertions are temporarily skipped
3. every row in **Track, But Do Not Gate Yet** is either moved upward or
   explicitly re-affirmed before release decisions are made
4. migration docs and testscript fixtures stay in sync as examples change

That is the line between "we have design notes" and "we have a real
conformance suite roadmap."
