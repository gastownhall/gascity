//go:build !windows

package main

// posixShellAutomationEnabled reports whether the POSIX shell automation Gas
// City ships is in play on this platform. It is, everywhere the port has not
// had to disable it.
func posixShellAutomationEnabled() bool { return true }

// backendUnavailableOnThisPlatform reports a backend this platform cannot run
// at all, as distinct from one that is merely not installed.
func backendUnavailableOnThisPlatform(_ string) (string, bool) { return "", false }

// managedDoltLifecycleDepApplies reports whether the managed Dolt lifecycle is
// a requirement for this city beyond needing a local Dolt identity. This
// platform can run a managed server, so the requirement is always met and
// needs no narrowing here.
func managedDoltLifecycleDepApplies(string) bool { return true }
