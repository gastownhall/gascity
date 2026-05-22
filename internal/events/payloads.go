package events

// Domain payload types shared across packages. Payloads specific to one
// package live with their emitter (see internal/api/event_payloads.go and
// internal/extmsg/events.go); this file holds payload shapes that are
// used by multiple callers — today, the supervisor's Dolt maintenance
// loop and its CLI/API projections (beads ga-e3s, ga-zn8, ga-p5n).

// StoreMaintenanceDonePayload is the typed payload for
// gc.store.maintenance.done events. Emitted after a successful
// maintenance cycle (backup snapshot + CALL DOLT_GC + smoke test).
type StoreMaintenanceDonePayload struct {
	DurationSeconds float64 `json:"duration_s"`
	BeforeBytes     int64   `json:"before_bytes"`
	AfterBytes      int64   `json:"after_bytes"`
	SnapshotPath    string  `json:"snapshot_path"`
}

// IsEventPayload marks StoreMaintenanceDonePayload as an events.Payload variant.
func (StoreMaintenanceDonePayload) IsEventPayload() {}

// StoreMaintenanceFailedPayload is the typed payload for
// gc.store.maintenance.failed events. Emitted when a maintenance stage
// returns an error. Stage names the failing phase ("backup" | "gc" |
// "smoke-test" | "prune"); ErrorMsg carries the human-readable cause;
// SnapshotPath is populated when the backup stage completed before a
// later stage failed (so operators can recover from the snapshot).
type StoreMaintenanceFailedPayload struct {
	Stage           string  `json:"stage"`
	ErrorMsg        string  `json:"error_msg"`
	SnapshotPath    string  `json:"snapshot_path,omitempty"`
	DurationSeconds float64 `json:"duration_s"`
}

// IsEventPayload marks StoreMaintenanceFailedPayload as an events.Payload variant.
func (StoreMaintenanceFailedPayload) IsEventPayload() {}
