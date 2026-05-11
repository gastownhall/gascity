package config

import (
	"fmt"
	"path/filepath"
)

// describeSource renders a non-empty descriptor for this agent's
// configuration origin. ValidateAgents uses it to format duplicate-name
// errors so the operator can distinguish auto-imported system packs from
// inline city.toml [[agent]] blocks from user packs. The returned string
// is never empty — that is the visible bug ga-tpfc.1 fixes.
//
// Resolution order:
//
//  1. SourceDir != "" → return SourceDir as-is. Pinned by existing
//     tests (TestValidateAgentsDupNameWithProvenance, etc.).
//  2. source == sourceAutoImport → "<auto-import: <BindingName>>"
//     when binding is known, else "<auto-import>".
//  3. source == sourceInline → "<inline: <basename(cityFile)>>" when
//     a city file path is supplied, else bare "<inline>".
//  4. source == sourcePack → "<pack: <BindingName>>" when binding is
//     known, else "<pack>".
//  5. fall through (sourceUnknown or any unstamped agent) → render an
//     identity hint: "<unknown: binding=…>" or "<unknown: name=…>"
//     or "<unknown>".
func (a *Agent) describeSource(cityFile string) string {
	if a.SourceDir != "" {
		return a.SourceDir
	}
	switch a.source {
	case sourceAutoImport:
		if a.BindingName != "" {
			return fmt.Sprintf("<auto-import: %s>", a.BindingName)
		}
		return "<auto-import>"
	case sourceInline:
		if cityFile != "" {
			return fmt.Sprintf("<inline: %s>", filepath.Base(cityFile))
		}
		return "<inline>"
	case sourcePack:
		if a.BindingName != "" {
			return fmt.Sprintf("<pack: %s>", a.BindingName)
		}
		return "<pack>"
	}
	if a.BindingName != "" {
		return fmt.Sprintf("<unknown: binding=%s>", a.BindingName)
	}
	if a.Name != "" {
		return fmt.Sprintf("<unknown: name=%s>", a.Name)
	}
	return "<unknown>"
}

// formatDuplicateAgentError renders the duplicate-agent-name error for a
// pair of conflicting agents. Co-owned with ga-9ogb (layout-version
// migration error); when the pair is exactly (layoutV1Inline,
// layoutV2Convention) — in either order — the helper emits a
// migration-guidance variant including both source descriptors and the
// migration-guide doc path. Otherwise it falls through to the generic
// "duplicate name (from … and …)" format.
//
// Every rendered descriptor is non-empty (the contract ga-tpfc.1 fixed),
// so the error never carries an empty quoted "" path.
//
// The validation paths that call this helper do not know the city's filesystem
// context, so source descriptors render without a city.toml filename.
func formatDuplicateAgentError(a, b Agent) error {
	if v1, v2, ok := orderV1V2(a, b); ok {
		return formatV1V2MigrationError(v1, v2)
	}
	return fmt.Errorf("agent %q: duplicate name (from %q and %q)",
		a.QualifiedName(),
		a.describeSource(""),
		b.describeSource(""))
}

// orderV1V2 reports whether (a, b) is exactly the (V1Inline,
// V2Convention) pair the migration variant cares about, and returns
// the agents in canonical (v1, v2) order.
func orderV1V2(a, b Agent) (v1, v2 Agent, ok bool) {
	switch {
	case a.layout == layoutV1Inline && b.layout == layoutV2Convention:
		return a, b, true
	case a.layout == layoutV2Convention && b.layout == layoutV1Inline:
		return b, a, true
	}
	return Agent{}, Agent{}, false
}

// migrationGuideDocPath is the documentation slug for the v1→v2 pack
// migration. It tracks ga-6wrr's deliverable; the path is repository-
// relative so the operator can copy-paste it without fighting an FQDN.
const migrationGuideDocPath = "docs/packv2/migration.mdx"

// formatV1V2MigrationError renders the migration-guidance variant of
// the duplicate-agent-name error. The headline is byte-stable; the
// body prose may evolve.
func formatV1V2MigrationError(v1, v2 Agent) error {
	v1Source := v1.describeSource("") + "/pack.toml ([[agent]] " + v1.Name + ")"
	v2Source := v2.describeSource("") + "/agents/" + v2.Name + "/agent.toml"
	return fmt.Errorf(
		"agent %q: pack v1/v2 layout collision\n"+
			"  v1 source: %s\n"+
			"  v2 source: %s\n"+
			"A v1 [[agent]] block coexists with a v2 agents/<name>/agent.toml of the same name.\n"+
			"To migrate, see: %s",
		v1.QualifiedName(),
		v1Source,
		v2Source,
		migrationGuideDocPath,
	)
}
