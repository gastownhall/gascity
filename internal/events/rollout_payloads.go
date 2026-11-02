package events

// Rollout-gate event payloads. These are defined and registered here rather
// than beside an emitter because no emitter exists yet: stage 2 of the
// beads.conditional_writes rollout registers the type so the SSE union and
// generated clients carry the schema before stage 3 wires emission (the
// beads factory injects an emission callback at store-open — internal/beads
// is Layer 0 and never imports this package).

// ConditionalWritesDegradedPayload is the typed payload for
// beads.conditional_writes.degraded events (DESIGN §12.2): a store resolved
// at mode=auto was vetoed by runtime capability and loud-degraded to the
// legacy write path. Emission is latched once per store instance, so one
// event per store per process is the ceiling.
type ConditionalWritesDegradedPayload struct {
	// StoreID names the degraded store's scope, e.g. "rig/gastown" or "graph".
	StoreID string `json:"store_id"`
	// StoreKind is the store implementation in the DESIGN §12.2 wire
	// vocabulary: bd | native | sqlite-graph | caching | mem | file. The
	// stage-3 emitter maps internal store-kind names (BeadsDiagnostic's
	// "BdStore"-style constants) onto this enum; doctor and the §12.5 status
	// wire assume it.
	StoreKind string `json:"store_kind"`
	// Mode is the resolved gate mode. In practice always "auto": off never
	// consults capability and require refuses instead of degrading. The field
	// exists so a future mode can degrade without a wire change.
	Mode string `json:"mode"`
	// Origin says where the resolved mode came from: builtin | config | env.
	Origin string `json:"origin"`
	// Reason carries the capability veto verbatim, e.g. "bd 1.1.0 lacks
	// --if-revision (four-verb capability probe)".
	Reason string `json:"reason"`
	// BDVersion is the probed bd version when the store is bd-backed.
	BDVersion string `json:"bd_version,omitempty"`
}

// IsEventPayload marks ConditionalWritesDegradedPayload as an events.Payload variant.
func (ConditionalWritesDegradedPayload) IsEventPayload() {}

// OpsRecordingDegradedPayload is the typed payload for ops.recording_degraded
// events (DES-O0API §10.4): an op store failure the API or the controller's
// op sweeper degraded past under one ops gate. In auto the request ran
// unrecorded (or its op stayed open for the sweeper); in require it was
// refused before any effect.
type OpsRecordingDegradedPayload struct {
	Gate    string `json:"gate" enum:"ops.content,ops.control,ops.lifecycle" doc:"The ops rollout gate whose recording degraded."`
	Mode    string `json:"mode" enum:"auto,require" doc:"The gate's resolved mode."`
	Surface string `json:"surface" enum:"api,controller" doc:"Which producer degraded: the API server or the controller's op sweeper."`
	Stage   string `json:"stage" enum:"open,propose,close,adopt,sweep" doc:"The recording step that failed."`
	Reason  string `json:"reason" doc:"Reason class: conditional_writes_unsupported, type_unregistered, no_namespace, store_error or contention."`
	Detail  string `json:"detail,omitempty" doc:"The store error's text, at most 512 bytes."`
}

// IsEventPayload marks OpsRecordingDegradedPayload as an events.Payload variant.
func (OpsRecordingDegradedPayload) IsEventPayload() {}

func init() {
	RegisterPayload(BeadsConditionalWritesDegraded, ConditionalWritesDegradedPayload{})
	RegisterPayload(OpsRecordingDegraded, OpsRecordingDegradedPayload{})
}
