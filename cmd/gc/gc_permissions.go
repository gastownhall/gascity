package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// gcDirPerm is the enforced permission for the .gc/ runtime directory.
const gcDirPerm = 0o700

// enforceGCPermissions ensures the .gc/ directory and its sensitive
// subdirectories have restrictive permissions. Called at controller
// startup to tighten any directories created with looser defaults.
//
// Enforced permissions:
//   - .gc/          → 0700
//   - .gc/secrets/  → 0700
func enforceGCPermissions(cityPath string, stderr io.Writer) {
	gcDir := filepath.Join(cityPath, ".gc")
	chmodIfExists(gcDir, gcDirPerm, stderr)
	chmodIfExists(filepath.Join(gcDir, secretsDir), secretsDirPerm, stderr)
}

// chmodIfExists sets the permission on path if it exists. Logs errors
// to stderr but does not fail — permission enforcement is best-effort.
func chmodIfExists(path string, perm os.FileMode, stderr io.Writer) {
	info, err := os.Stat(path)
	if err != nil {
		return // doesn't exist yet — will be created with correct perms
	}
	if !info.IsDir() {
		return
	}
	if info.Mode().Perm() == perm {
		return // already correct
	}
	if err := os.Chmod(path, perm); err != nil {
		fmt.Fprintf(stderr, prog()+": chmod %s to %o: %v\n", path, perm, err) //nolint:errcheck
	}
}
