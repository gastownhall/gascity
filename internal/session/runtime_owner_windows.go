//go:build windows

package session

import "github.com/gastownhall/gascity/internal/runtime"

// checkRuntimeOwner has nothing to check on Windows: the elevation fence
// (elevation_windows.go) covers a session another gc started there.
func (m *Manager) checkRuntimeOwner(string) error { return nil }

// recordRuntimeOwner has nothing to record on Windows, where nothing checks
// the record.
func (m *Manager) recordRuntimeOwner(string) {}

// RecordRuntimeOwner has nothing to record on Windows, where nothing checks
// the record.
func (s *Store) RecordRuntimeOwner(runtime.Provider, string, string) {}

// RecordAdoptedRuntimeOwners has nothing to record on Windows, where nothing
// checks the record.
func (s *Store) RecordAdoptedRuntimeOwners(runtime.Provider, string) (int, error) { return 0, nil }
