package events

import (
	"slices"
	"testing"
)

// TestRetiredStoreMaintenanceEventTypesAreGone pins that the legacy Dolt
// maintenance loop's event types are no longer declared, registered or
// authored. The strings are literals on purpose: the constants are deleted.
func TestRetiredStoreMaintenanceEventTypesAreGone(t *testing.T) {
	t.Parallel()
	for _, eventType := range []string{
		"gc.store.maintenance.done",
		"gc.store.maintenance.failed",
		"gc.store.disk_warn",
		"gc.store.disk_critical",
	} {
		if slices.Contains(KnownEventTypes, eventType) {
			t.Errorf("KnownEventTypes still lists retired event type %q", eventType)
		}
		if _, ok := RegisteredAuthorship(eventType); ok {
			t.Errorf("retired event type %q still has a registered authorship", eventType)
		}
	}
}
