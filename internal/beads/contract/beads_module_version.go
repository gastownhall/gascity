package contract

import (
	"runtime/debug"
	"strings"
)

// BeadsModuleVersion reports the beads module version a Go binary was built
// from, read from its build info. A bd built with `go install
// github.com/steveyegge/beads/cmd/bd@<version>` carries beads as its main
// module; gc carries it as a dependency. Pseudo-versions are returned as they
// are, to be compared exactly.
//
// ok is false when the version is unknown: nil build info, no beads module, a
// "(devel)" or empty version (a source build), or a replace directive on the
// module, whose code is the replacement's and not the version's.
func BeadsModuleVersion(info *debug.BuildInfo) (version string, ok bool) {
	if info == nil {
		return "", false
	}
	if info.Main.Path == beadsModulePath {
		return knownBeadsModuleVersion(&info.Main)
	}
	for _, dep := range info.Deps {
		if dep != nil && dep.Path == beadsModulePath {
			return knownBeadsModuleVersion(dep)
		}
	}
	return "", false
}

func knownBeadsModuleVersion(mod *debug.Module) (string, bool) {
	version := strings.TrimSpace(mod.Version)
	if mod.Replace != nil || version == "" || version == "(devel)" {
		return "", false
	}
	return version, true
}
