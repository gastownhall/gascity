//go:build windows

package contract

import "github.com/gastownhall/gascity/internal/fsys"

// ProviderOwnedServerPort returns the port of the server-mode Dolt bd started
// for scopeRoot, as bd recorded it in the scope's own .beads/dolt-server.pid
// and .beads/dolt-server.port, while that process is alive and the port
// answers. ok is false for any other scope.
func ProviderOwnedServerPort(fs fsys.FS, scopeRoot string) (string, bool) {
	return readProviderOwnedServerPort(fs, scopeRoot)
}
