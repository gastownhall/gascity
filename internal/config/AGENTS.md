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

## Adding `.gc/site.toml` fields

`.gc/site.toml` holds machine-local bindings and may carry tables this binary
does not model, so writers never re-encode `SiteBinding` wholesale. Each
writer passes `persistSiteBinding` a patch that touches only the keys it owns
(`setSiteWorkspaceIdentity`, `setSiteRigBindings`); everything else
round-trips. When adding a field to `SiteBinding` or `RigSiteBinding`, register
its TOML key in `siteBindingKeyOwners` / `rigSiteBindingKeyOwners` and patch it
only in that owning writer. `TestSiteBindingFieldsHaveWriterOwner` fails on an
unregistered field.
