package session

// The stop request's row keys (CONTRACT v5 D1, D5, R1): new keys legacy
// ignores. The controller half: the v2 drain begin, signal, cancel and
// finalize write it. The request half: `gc runtime drain-ack` writes it by
// CAS in a v2 city only, DrainAckIncarnationKey holding the row's generation
// as the CLI read it and DrainAckAtKey the RFC 3339 time of the ack; v2's
// PreWake clears both halves. cmd/gc's activeStop is the only reader, and
// ClearStopRequestPatch is the clear that cmd/gc's residue void writes. No
// production file but this one, cmd/gc/reconcile_stop_request.go and
// cmd/gc/cmd_runtime_drain.go names them (TestStopRequestKeysAreNewKeys).
const (
	DrainIntentReasonKey      = "drain_intent_reason"
	DrainIntentAtKey          = "drain_intent_at"
	DrainIntentIncarnationKey = "drain_intent_incarnation"
	DrainAckIncarnationKey    = "drain_ack_incarnation"
	DrainAckAtKey             = "drain_ack_at"
)

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
