package events

// Typed payloads for the store-health, breaker, controller-heartbeat and
// doctor event types declared in events.go. The store-health payloads are
// what the store health patrol (internal/storehealth) renders through its
// injected Emit hooks; the breaker payload mirrors a resilience breaker
// transition; the heartbeat is recorded by the controller's reconcile loop;
// the doctor alert by a supervisor-cadence doctor.

// Store-degradation classes carried in StoreDegradedPayload.Class. The
// patrol distinguishes a wedged transport (the routed connection path) from
// a backend fault (the sql-server itself) and from a write rejection (the
// store answers reads but rejects custom-type writes, which a transport-only
// breaker cannot see).
const (
	// StoreDegradedClassTransport marks degradation where a fresh routed
	// read fails while the direct backend probe succeeds.
	StoreDegradedClassTransport = "transport"
	// StoreDegradedClassBackend marks degradation where the sql-server
	// itself is unreachable or read-only. The patrol never auto-kills the
	// sql-server in this class.
	StoreDegradedClassBackend = "backend"
	// StoreDegradedClassWriteRejection marks degradation where the store
	// is reachable but persistently rejects a RequiredCustomType write.
	StoreDegradedClassWriteRejection = "write-rejection"
)

// StoreDegradedPayload is the typed payload for store.degraded events.
type StoreDegradedPayload struct {
	Scope            string `json:"scope" doc:"Canonical scope root path whose store degraded."`
	Class            string `json:"class" doc:"Degradation class: transport, backend, or write-rejection."`
	Reason           string `json:"reason,omitempty" doc:"Human-readable cause from the failing probe."`
	ConsecutiveFails int    `json:"consecutive_fails,omitempty" doc:"Consecutive failed probe cycles at the trip."`
}

// IsEventPayload marks StoreDegradedPayload as an events.Payload variant.
func (StoreDegradedPayload) IsEventPayload() {}

// StoreRecoveredPayload is the typed payload for store.recovered events.
type StoreRecoveredPayload struct {
	Scope string `json:"scope" doc:"Canonical scope root path whose store recovered."`
	Class string `json:"class,omitempty" doc:"Degradation class that recovered, if known."`
}

// IsEventPayload marks StoreRecoveredPayload as an events.Payload variant.
func (StoreRecoveredPayload) IsEventPayload() {}

// StoreProbeFailedPayload is the typed payload for store.probe_failed events.
type StoreProbeFailedPayload struct {
	Scope  string `json:"scope" doc:"Canonical scope root path of the failing probe."`
	Probe  string `json:"probe" doc:"Which probe failed: routed (probe A) or backend (probe B)."`
	Reason string `json:"reason,omitempty" doc:"Human-readable cause from the failing probe."`
}

// IsEventPayload marks StoreProbeFailedPayload as an events.Payload variant.
func (StoreProbeFailedPayload) IsEventPayload() {}

// BreakerStateChangedPayload is the typed payload for breaker.state_changed
// events. It mirrors resilience.Transition without importing the resilience
// package: internal/events keeps no upward dependency on the breaker
// registry, so the emitter does the conversion.
type BreakerStateChangedPayload struct {
	Scope     string `json:"scope" doc:"Breaker scope: a store scope root path, or an endpoint key."`
	OpClass   string `json:"op_class" doc:"Operation class, e.g. bd or session-start."`
	From      string `json:"from" doc:"Breaker state before the transition."`
	To        string `json:"to" doc:"Breaker state after the transition."`
	Failures  int    `json:"failures,omitempty" doc:"Consecutive transport-failure count at the change."`
	BackoffMs int64  `json:"backoff_ms,omitempty" doc:"Open-state backoff chosen for this episode, in milliseconds."`
}

// IsEventPayload marks BreakerStateChangedPayload as an events.Payload variant.
func (BreakerStateChangedPayload) IsEventPayload() {}

// ControllerTickCompletedPayload is the typed payload for
// controller.tick_completed events, the controller heartbeat. Duration and
// Phase identify what work the tick did; ThresholdBreach flags a tick whose
// duration reached the slow-tick threshold (a multiple of the configured
// patrol interval).
type ControllerTickCompletedPayload struct {
	DurationMs      int64  `json:"duration_ms" doc:"Wall-clock duration of the completed tick, in milliseconds."`
	Phase           string `json:"phase" doc:"Tick trigger phase: patrol, poke, control-dispatcher, etc."`
	ThresholdBreach bool   `json:"threshold_breach,omitempty" doc:"True when the tick's duration reached the slow-tick threshold (a multiple of the configured patrol interval)."`
}

// IsEventPayload marks ControllerTickCompletedPayload as an events.Payload variant.
func (ControllerTickCompletedPayload) IsEventPayload() {}

// DoctorAlertPayload is the typed payload for doctor.alert events, recorded
// when a supervisor-cadence doctor check goes red.
type DoctorAlertPayload struct {
	Check   string `json:"check" doc:"Name of the doctor check that went red."`
	City    string `json:"city,omitempty" doc:"City name the check evaluated, when scoped to a city."`
	Detail  string `json:"detail" doc:"Human-readable description of the red condition."`
	Subject string `json:"subject,omitempty" doc:"Optional subject identifier (scope, path) the alert concerns."`
}

// IsEventPayload marks DoctorAlertPayload as an events.Payload variant.
func (DoctorAlertPayload) IsEventPayload() {}

func init() {
	RegisterPayload(StoreDegraded, StoreDegradedPayload{})
	RegisterPayload(StoreRecovered, StoreRecoveredPayload{})
	RegisterPayload(StoreProbeFailed, StoreProbeFailedPayload{})
	RegisterPayload(BreakerStateChanged, BreakerStateChangedPayload{})
	RegisterPayload(ControllerTickCompleted, ControllerTickCompletedPayload{})
	RegisterPayload(DoctorAlert, DoctorAlertPayload{})
}
