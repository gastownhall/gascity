# internal/config — change guide

`internal/config` loads and composes `pack.toml` / `city.toml`: pack
imports, patches, rig overrides, provider resolution, and validation. It is
the machinery beneath the **Pack** primitive and the universal activation
mechanism (capabilities activate from section presence — no capability
flags). Architecture: `engdocs/architecture/config.md`.

## Workflow topology is not fresh worker demand

Pool demand counts, controller demand, and hook claims must all exclude
expanded workflow roots, scopes, and specs even when they retain a route.
Keep never-expanded root-only workflows executable for compatibility, and
keep already-owned workflow anchors available for continuation. Update the
shell count and Go predicates together; otherwise an unclaimable latch can
continually wake replacement workers.

## Route labels (`route_label`, `route_label_any`)

An agent's `route_label` (every label, AND) and `route_label_any` (at least one,
OR) narrow only the routed pool-demand tier: the generated routed-work readers,
the count form, and the controller's demand predicate. They ride on
`PoolDemandServeRules` (`PoolDemandServeRulesFor(agent)`), the same value the
shell flags are rendered from and `demandServableForTemplateRules` (`cmd/gc`)
answers with, so a consumer of the routed tier takes the rules, never a
re-derived label list. Work already assigned to the agent (the in_progress and
assigned-ready tiers, the graph anchor) and the boot/death hooks are owned work
and stay unlabeled. Label values reach `bd ready` / `gc ready` as shell-quoted
`--label=` / `--label-any=` data and reach the legacy ephemeral jq path as JSON
string literals. `validateRouteLabels` rejects what bd would silently rewrite
(commas, blanks, surrounding whitespace, line breaks) and any combination with a
raw `work_query` or `scale_check`; `LoadWithIncludes` runs it on the effective
agents after patches. The builder and investigator pack queries have pipeline
stages and stay raw `work_query` overrides, so they cannot take route labels.

## Adding agent config fields

When adding a field to `config.Agent`, also add it to `AgentPatch` and
`AgentOverride`, wire it into the shared merge body `applyAgentMutation` (in
`internal/config/patch.go`) — and, for the rig-override path, copy it in
`AgentOverride.toAgentPatch` — and, if the field is a slice/map/pointer,
deep-copy it in `Agent.Clone` (`internal/config/config.go`). All four are
test-guarded, so a missed field fails the build: `TestAgentFieldSync` (struct
field sets), `TestApplyAgentPatchCoversAllFields` /
`TestApplyAgentOverrideCoversAllFields` (merge + `toAgentPatch`
completeness), and `TestAgentCloneIsDeep` (clone deepness). Both patch and
rig override share `applyAgentMutation`, and the pack-load cache
(`deepCopyAgents`) shares `Agent.Clone`.

Pool expansion does **not** share `Agent.Clone`: `deepCopyAgent` in
`cmd/gc/pool.go` hand-copies each field into a fresh `config.Agent`. Add the
new field there too; `TestDeepCopyAgentCoversAllFields` (`cmd/gc/pool_test.go`)
fails if you miss it.

## Adding rig config fields

When adding a field to `config.Rig`, also add the corresponding optional
field to `RigPatch` and wire the merge into `applyRigPatch` so layered
configs (fragments, patches) can override it. No field-sync test exists for
Rig today; the patch path must be checked manually.

Exception: `federation` and `namespace` (`internal/config/rig_federation.go`)
are deliberately absent from `RigPatch`. They decide which rows of a shared
bead store the city may act on, so only the rig's own `[[rigs]]` entry may set
them; a pack-level patch must not be able to drop the guard. Their checks run
in `ValidateRigFederation` on every load and in `ValidateRigFederationLayout`
beside each `ValidateRigs` call. Unlike `ValidateRigs`, a layout failure on
reload is a refusal (`tryReloadConfig`), not a warning: a running city must
not adopt a config that start would refuse.

## Adding `.gc/site.toml` fields

`.gc/site.toml` holds machine-local bindings and may carry tables this binary
does not model, so writers never re-encode `SiteBinding` wholesale. Each
writer passes `persistSiteBinding` a patch that touches only the keys it owns
(`setSiteWorkspaceIdentity`, `setSiteRigBindings`); everything else
round-trips. When adding a field to `SiteBinding` or `RigSiteBinding`, register
its TOML key in `siteBindingKeyOwners` / `rigSiteBindingKeyOwners` and patch it
only in that owning writer. `TestSiteBindingFieldsHaveWriterOwner` fails on an
unregistered field.
