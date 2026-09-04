package contract

import "strings"

// IsDoltBackend reports whether backend uses Gas City's Dolt contract.
func IsDoltBackend(backend string) bool {
	backend = strings.ToLower(strings.TrimSpace(backend))
	return backend == "" || backend == "dolt"
}

// IsProxiedDoltMode reports whether mode selects Beads' proxied-server path
// for a backend that Gas City treats as Dolt.
func IsProxiedDoltMode(backend, mode string) bool {
	if !IsDoltBackend(backend) {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(mode), "proxied-server")
}
