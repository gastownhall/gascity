package contract

import (
	"runtime/debug"
	"strings"
)

// BeadsModuleVersion returns the github.com/steveyegge/beads module version
// recorded in info, or "" when info does not carry beads. It reads both
// shapes a binary can take: beads as the main module (a `go install`ed bd)
// and beads as a dependency (gc, or a bd built from a scratch module). A
// replaced dependency reports the replacement's own version, as
// linkedBeadsLibraryFrom does. A bd built from an untagged local checkout
// records "(devel)", which names no version, so it reports "" too.
func BeadsModuleVersion(info *debug.BuildInfo) string {
	if info == nil {
		return ""
	}
	if info.Main.Path == beadsModulePath {
		if v := strings.TrimSpace(info.Main.Version); v != "(devel)" {
			return v
		}
		return ""
	}
	return strings.TrimSpace(linkedBeadsLibraryFrom(info).Version)
}
