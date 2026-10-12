package session

// The stop request's row keys (CONTRACT v5 D1, D5, R1): new keys legacy
// ignores. The controller half: the v2 drain begin, signal, cancel and
// finalize write it. The request half: `gc runtime drain-ack` writes it by
// CAS in a v2 city only, DrainAckIncarnationKey holding the row's generation
// as the CLI read it and DrainAckAtKey the RFC 3339 time of the ack; v2's
// PreWake clears both halves, and so does Manager.ensureRunning's confirm of
// a runtime it started (restartStopRequestClears). cmd/gc's activeStop is
// the only reader, and ClearStopRequestPatch is the clear that cmd/gc's
// residue void writes. No
// production file but this one, cmd/gc/reconcile_stop_request.go and
// cmd/gc/cmd_runtime_drain.go names them (TestStopRequestKeysAreNewKeys).
const (
	DrainIntentReasonKey      = "drain_intent_reason"
	DrainIntentAtKey          = "drain_intent_at"
	DrainIntentIncarnationKey = "drain_intent_incarnation"
	DrainAckIncarnationKey    = "drain_ack_incarnation"
	DrainAckAtKey             = "drain_ack_at"
)

// stopRequestPremiseKeys are the stop request's keys a premise compares
// (FactsBaseline): the controller half and the ack's incarnation.
var stopRequestPremiseKeys = []string{DrainIntentReasonKey, DrainIntentAtKey, DrainIntentIncarnationKey, DrainAckIncarnationKey}

// restartStopRequestClears is ClearStopRequestPatch narrowed to the keys
// meta holds: what a runtime start outside PreWake (Manager.ensureRunning's,
// which keeps the generation) clears in its confirming write, as PreWake
// clears both halves, so the restarted runtime is never stopped for its
// predecessor's request (#7635 review ruling). Empty when meta holds none.
func restartStopRequestClears(meta map[string]string) MetadataPatch {
	out := MetadataPatch{}
	for k := range ClearStopRequestPatch() {
		if meta[k] != "" {
			out[k] = ""
		}
	}
	return out
}

// ClearStopRequestPatch clears both halves of a row's stop request.
func ClearStopRequestPatch() MetadataPatch {
	return MetadataPatch{
		DrainIntentReasonKey:      "",
		DrainIntentAtKey:          "",
		DrainIntentIncarnationKey: "",
		DrainAckIncarnationKey:    "",
		DrainAckAtKey:             "",
	}
}
