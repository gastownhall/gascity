//go:build !windows

package builtinpacks

import (
	"os"
	"testing"
)

// tamperMaterializedFileMode rewrites a cached file's mode to one the manifest
// never produces. 0600 is narrower than the 0644 every non-script bundled file
// carries, so it exercises the comparison without depending on an execute bit.
func tamperMaterializedFileMode(t *testing.T, path string) {
	t.Helper()
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("Chmod(%q): %v", path, err)
	}
}
