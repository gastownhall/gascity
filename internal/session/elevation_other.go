//go:build !windows

package session

// checkElevatedSessions has nothing to check off Windows: no integrity level
// keeps one of this user's processes from reading another's.
func checkElevatedSessions(string) error { return nil }

// recordElevatedSessionStart has nothing to record off Windows.
func recordElevatedSessionStart(string) error { return nil }
