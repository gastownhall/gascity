package session

// The controller half of the stop request (CONTRACT v5 D1, R1): new row keys
// legacy ignores. The v2 drain begin, signal, cancel and finalize write them,
// and cmd/gc's activeStop is their only reader. They are defined here so a
// session-package writer (D8's resume) can clear them through
// ClearStopRequestPatch. No production file but this one and
// cmd/gc/reconcile_stop_request.go names them (TestStopRequestKeysAreNewKeys).
const (
	DrainIntentReasonKey      = "drain_intent_reason"
	DrainIntentAtKey          = "drain_intent_at"
	DrainIntentIncarnationKey = "drain_intent_incarnation"
)

// ClearStopRequestPatch clears both halves of a row's stop request: the
// controller half and E3's request half (DrainAckIncarnationKey,
// DrainAckAtKey).
func ClearStopRequestPatch() MetadataPatch {
	return MetadataPatch{
		DrainIntentReasonKey:      "",
		DrainIntentAtKey:          "",
		DrainIntentIncarnationKey: "",
		DrainAckIncarnationKey:    "",
		DrainAckAtKey:             "",
	}
}
