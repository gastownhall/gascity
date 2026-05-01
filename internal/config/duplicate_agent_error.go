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
//
// cityRoot is accepted but currently unused; the hook is reserved for
// future relativization without breaking the call sites already passing
// it through.
func (a *Agent) describeSource(cityRoot, cityFile string) string {
	_ = cityRoot
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
// migration error); that bead specializes (V1Inline, V2Convention) layout
// pairs onto a migration-guidance variant. This bead's contract: every
// rendered descriptor is non-empty, so the error never carries an empty
// quoted "" path.
//
// cityRoot and cityFile are passed through to describeSource. Both may
// be empty when the helper is called from validation paths that do not
// know the city's filesystem context (e.g., test fixtures that build
// []Agent directly).
func formatDuplicateAgentError(a, b Agent, cityRoot, cityFile string) error {
	return fmt.Errorf("agent %q: duplicate name (from %q and %q)",
		a.QualifiedName(),
		a.describeSource(cityRoot, cityFile),
		b.describeSource(cityRoot, cityFile))
}
