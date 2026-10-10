//go:build !unix

package bazeltest

import "os"

// lockFile is a no-op on platforms without flock; callers fall through
// unlocked.
func lockFile(*os.File) error { return nil }

// unlockFile is a no-op on platforms without flock.
func unlockFile(*os.File) {}
