//go:build windows

package subprocess

import (
	"fmt"
	"os"

	"github.com/gastownhall/gascity/internal/runtime"
)

// privateFallbackNamespace names the owner-only fallback root without an
// identity in it.
//
// The euid it is handed is always -1: os.Geteuid reports that for every
// process on Windows, so putting it in a pathname would record a sentinel, not
// a user. Nothing is lost by dropping it, because the separation the euid
// buys on Unix is already there — localsocket's fallback root on Windows is
// the per-user temporary directory, not a host-shared /tmp, so two users
// cannot resolve to one path here in the first place.
//
// The name is distinct from fallbackSocketDirName on purpose. That one roots
// the legacy fallback, which is ordinary shared state; this one is created and
// verified as owner-only, and collapsing them would apply an owner-only DACL
// to the shared directory.
func privateFallbackNamespace(_ int) string {
	return fallbackSocketDirName + "-private"
}

// validatePrivateSocketDirPlatform verifies that path is a directory this user
// privately owns.
//
// The euid is deliberately unused and cannot be otherwise: os.Geteuid reports
// -1 for every process on Windows, so a comparison against it would hold for
// everyone or for no one, and either answer is worthless as an ownership
// check. Ownership is decided on the open handle instead —
// runtime.EnsurePrivateDir reads the owner SID, compares it to this process's
// token user, and fails closed on a foreign owner, a reparse point, or a DACL
// it cannot narrow to owner-only.
func validatePrivateSocketDirPlatform(path string, _ int) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("private socket directory %q is not a directory", path)
	}
	return runtime.EnsurePrivateDir(path)
}
