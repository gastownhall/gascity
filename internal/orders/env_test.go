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

// TestReservedExecEnvKeysIncludeBdContributorRouting guards the
// contributor-routing opt-out: gc forces bd's fork/contributor auto-routing
// off via BD_ROUTING_MODE, so an order's [order.env] must not be able to
// re-enable the split-brain routing that diverts create/list/update to an
// out-of-band store.
func TestReservedExecEnvKeysIncludeBdContributorRouting(t *testing.T) {
	for _, key := range []string{"BD_ROUTING_MODE", "BEADS_ROUTING_MODE"} {
		if !IsReservedExecEnvKey(key) {
			t.Errorf("IsReservedExecEnvKey(%q) = false, want true", key)
		}
	}
}

// TestValidateExecEnvOverridesRejectsBdContributorRouting confirms the
// reservation is enforced end-to-end through the order validation path.
func TestValidateExecEnvOverridesRejectsBdContributorRouting(t *testing.T) {
	order := Order{Name: "o", Env: map[string]string{"BD_ROUTING_MODE": "auto"}}
	if err := ValidateExecEnvOverrides(order); err == nil {
		t.Fatal("ValidateExecEnvOverrides() = nil, want error for reserved BD_ROUTING_MODE override")
	}
}
