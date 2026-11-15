//go:build !windows

package main

// managedDoltNativeIdentitySupported reports whether this platform has the
// process-identity mechanisms the managed Dolt lifecycle depends on.
func managedDoltNativeIdentitySupported() error {
	return nil
}
