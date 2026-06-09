package orders

import "testing"

// TestReservedExecEnvKeysIncludeBdAutoBackup guards ga-0eq: the controller
// forces bd's PersistentPostRun auto-backup off via BD_BACKUP_ENABLED, so an
// order's [order.env] must not be able to re-enable the destructive
// backup_export sync that wedged the town on 2026-06-08.
func TestReservedExecEnvKeysIncludeBdAutoBackup(t *testing.T) {
	for _, key := range []string{"BD_BACKUP_ENABLED", "BEADS_BACKUP_ENABLED"} {
		if !IsReservedExecEnvKey(key) {
			t.Errorf("IsReservedExecEnvKey(%q) = false, want true", key)
		}
	}
}

// TestValidateExecEnvOverridesRejectsBdAutoBackup confirms the reservation is
// enforced end-to-end through the order validation path.
func TestValidateExecEnvOverridesRejectsBdAutoBackup(t *testing.T) {
	order := Order{Name: "o", Env: map[string]string{"BD_BACKUP_ENABLED": "true"}}
	if err := ValidateExecEnvOverrides(order); err == nil {
		t.Fatal("ValidateExecEnvOverrides() = nil, want error for reserved BD_BACKUP_ENABLED override")
	}
}
