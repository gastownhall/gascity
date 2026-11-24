//go:build windows

package session

// checkRuntimeOwner has nothing to check on Windows: the elevation fence
// (elevation_windows.go) covers a session another gc started there.
func (m *Manager) checkRuntimeOwner(string) error { return nil }

// recordRuntimeOwner has nothing to record on Windows, where nothing checks
// the record.
func (m *Manager) recordRuntimeOwner(string) {}
