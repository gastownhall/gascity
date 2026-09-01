package contract

import "strings"

// IsDoltBackend reports whether backend uses Gas City's Dolt contract. An
// empty backend is the legacy managed-Dolt shape and is intentionally treated
// as Dolt; every named backend owns its own configuration vocabulary.
func IsDoltBackend(backend string) bool {
	backend = strings.ToLower(strings.TrimSpace(backend))
	return backend == "" || backend == "dolt"
}

// IsProxiedDoltMode reports whether mode selects Beads' proxied-server path
// for a backend that Gas City treats as the Dolt backend. An empty backend is
// the legacy managed-Dolt shape and is intentionally accepted; any other
// backend owns its mode vocabulary and must not be routed through the Dolt
// proxy merely because stale metadata carries a dolt_mode field.
func IsProxiedDoltMode(backend, mode string) bool {
	if !IsDoltBackend(backend) {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(mode), "proxied-server")
}
