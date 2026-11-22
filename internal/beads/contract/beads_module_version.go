package contract

import (
	"runtime/debug"
	"strings"
)

// BeadsModuleVersion reports the beads module version a Go binary was built
// from, as recorded in its build info: a bd built by `go install …@pin`
// carries beads as its main module, and a binary linking the library (gc)
// carries it as a dependency. Pseudo-versions are known and compare exactly;
// ok is false when no meaningful version can be named — nil info, no beads
// module, an empty or "(devel)" version (a source build), or a module whose
// code came from a replace directive (its version numbers belong to the
// replacement, not to beads).
func BeadsModuleVersion(info *debug.BuildInfo) (version string, ok bool) {
	if info == nil {
		return "", false
	}
	if info.Main.Path == beadsModulePath {
		return buildInfoModuleVersion(&info.Main)
	}
	for _, dep := range info.Deps {
		if dep == nil || dep.Path != beadsModulePath {
			continue
		}
		return buildInfoModuleVersion(dep)
	}
	return "", false
}

// buildInfoModuleVersion applies BeadsModuleVersion's known/unknown rule to
// one module record.
func buildInfoModuleVersion(m *debug.Module) (string, bool) {
	if m == nil || m.Replace != nil {
		return "", false
	}
	version := strings.TrimSpace(m.Version)
	if version == "" || version == "(devel)" {
		return "", false
	}
	return version, true
}
