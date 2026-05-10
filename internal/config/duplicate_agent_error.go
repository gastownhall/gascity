package config

import "fmt"

// describeSource renders a non-empty descriptor for this agent's
// configuration origin. ValidateAgents uses it to format duplicate-name
// errors so the operator can distinguish auto-imported system packs from
// inline city.toml [[agent]] blocks from user packs. The returned string
// is never empty — that is the visible bug ga-tpfc.1 fixes.
//
// Pack and auto-import sources include the binding descriptor before the
// source directory so real pack-loaded agents do not hide the user-visible
// binding behind SourceDir.
func (a *Agent) describeSource() string {
	switch a.source {
	case sourceAutoImport:
		return describeBoundSource("auto-import", a.BindingName, a.SourceDir)
	case sourceInline:
		if a.SourceDir != "" {
			return a.SourceDir
		}
		return "<inline>"
	case sourcePack:
		if a.BindingName != "" {
			return describeBoundSource("pack", a.BindingName, a.SourceDir)
		}
		if a.SourceDir != "" {
			return a.SourceDir
		}
		return "<pack>"
	}
	if a.SourceDir != "" {
		return a.SourceDir
	}
	if a.BindingName != "" {
		return fmt.Sprintf("<unknown: binding=%s>", a.BindingName)
	}
	if a.Name != "" {
		return fmt.Sprintf("<unknown: name=%s>", a.Name)
	}
	return "<unknown>"
}

func describeBoundSource(kind, bindingName, sourceDir string) string {
	descriptor := fmt.Sprintf("<%s>", kind)
	if bindingName != "" {
		descriptor = fmt.Sprintf("<%s: %s>", kind, bindingName)
	}
	if sourceDir != "" {
		return fmt.Sprintf("%s %s", descriptor, sourceDir)
	}
	return descriptor
}

// formatDuplicateAgentError renders the duplicate-agent-name error for a
// pair of conflicting agents. Co-owned with ga-9ogb (layout-version
// migration error); that bead specializes (V1Inline, V2Convention) layout
// pairs onto a migration-guidance variant. This bead's contract: every
// rendered descriptor is non-empty, so the error never carries an empty
// quoted "" path.
func formatDuplicateAgentError(a, b Agent) error {
	return fmt.Errorf("agent %q: duplicate name (from %q and %q)",
		a.QualifiedName(),
		a.describeSource(),
		b.describeSource())
}
