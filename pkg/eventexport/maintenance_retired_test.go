package eventexport

import "testing"

// TestRetiredMaintenanceEventLeavesAllowlistAndBumpsSchema pins the export
// contract change that retiring the Dolt maintenance loop makes: the
// allowlist loses gc.store.maintenance.done, which changes what an exporter
// may emit, so SchemaVersion moves 6 -> 7 per the policy above SchemaVersion.
func TestRetiredMaintenanceEventLeavesAllowlistAndBumpsSchema(t *testing.T) {
	t.Parallel()
	if allowedTypes["gc.store.maintenance.done"] {
		t.Error("allowedTypes still allows retired gc.store.maintenance.done")
	}
	if SchemaVersion != 7 {
		t.Errorf("SchemaVersion = %d, want 7 (allowlist change per the SchemaVersion policy)", SchemaVersion)
	}
}
