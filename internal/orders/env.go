package orders

import "fmt"

// ValidateExecEnvOverrides rejects [order.env] keys owned by the controller.
func ValidateExecEnvOverrides(a Order) error {
	for key := range a.Env {
		if IsReservedExecEnvKey(key) {
			return fmt.Errorf("order %q: [order.env] cannot override controller-owned env key %q", a.ScopedName(), key)
		}
	}
	return nil
}

// IsReservedExecEnvKey reports whether key is controlled by order execution.
func IsReservedExecEnvKey(key string) bool {
	switch key {
	case
		"BD_EXPORT_AUTO",
		"BEADS_ACTOR",
		"BEADS_CREDENTIALS_FILE",
		"BEADS_DIR",
		"BEADS_DOLT_AUTO_START",
		"BEADS_DOLT_PASSWORD",
		"BEADS_DOLT_SERVER_HOST",
		"BEADS_DOLT_SERVER_PORT",
		"BEADS_DOLT_SERVER_USER",
		"BEADS_DOLT_SYNC_CLI_REMOTES",
		"BEADS_POSTGRES_DATABASE",
		"BEADS_POSTGRES_HOST",
		"BEADS_POSTGRES_PASSWORD",
		"BEADS_POSTGRES_PORT",
		"BEADS_POSTGRES_USER",
		"BD_DOLT_SYNC_CLI_REMOTES",
		"GC_BEADS",
		"GC_BEADS_PREFIX",
		"GC_BEADS_SCOPE_ROOT",
		"GC_CITY",
		"GC_CITY_PATH",
		"GC_CITY_ROOT",
		"GC_CITY_RUNTIME_DIR",
		"GC_CONTROL_DISPATCHER_TRACE_DEFAULT",
		"GC_DOLT",
		"GC_DOLT_CONFIG_FILE",
		"GC_DOLT_DATA_DIR",
		"GC_DOLT_HOST",
		"GC_DOLT_LOCK_FILE",
		"GC_DOLT_LOG_FILE",
		"GC_DOLT_MANAGED_LOCAL",
		"GC_DOLT_PASSWORD",
		"GC_DOLT_PID_FILE",
		"GC_DOLT_PORT",
		"GC_DOLT_STATE_FILE",
		"GC_DOLT_USER",
		"GC_PACK_DIR",
		"GC_PACK_NAME",
		"GC_PACK_STATE_DIR",
		"GC_POSTGRES_PASSWORD",
		"GC_RIG",
		"GC_RIG_ROOT",
		"GC_STORE_ROOT",
		"GC_STORE_SCOPE",
		"ORDER_DIR",
		"PACK_DIR":
		return true
	default:
		return false
	}
}
