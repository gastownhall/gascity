package hooks

import (
	"path"
	"path/filepath"
)

// managedOverlayHookPaths maps the flattened per-provider overlay path of every
// hook file that carries a version marker to its provider name. The paths are
// unique across providers, so a relative path alone identifies the file.
var managedOverlayHookPaths = map[string]string{
	path.Join(".pi", "extensions", "gc-hooks.js"):   "pi",
	path.Join(".opencode", "plugins", "gascity.js"): "opencode",
	path.Join(".mimocode", "plugin", "gascity.js"):  "mimocode",
	path.Join(".omp", "hooks", "gc-hook.ts"):        "omp",
}

// PreserveManagedFile reports whether an existing file at relPath is a managed
// hook file that is already current, and so must not be replaced.
//
// Overlay staging copies the bundled per-provider tree over a work directory on
// every reconcile tick, with no version check and no backup, which silently
// reverted hook files that were current or newer (#5554). Passing this to
// runtime.WithPreserve lets staging defer to the same versioning policy
// internal/hooks already applies, without package runtime depending on this one.
//
// It is deliberately conservative: anything it does not recognize as a current
// managed hook file returns false and is staged exactly as before.
func PreserveManagedFile(relPath string, existing []byte) bool {
	provider, ok := managedOverlayHookPaths[filepath.Clean(relPath)]
	if !ok {
		return false
	}
	needsUpgrade := overlayManagedNeedsUpgrade(provider, filepath.Clean(relPath))
	if needsUpgrade == nil {
		return false
	}
	return !needsUpgrade(existing)
}
