package main

// The controller half of the stop request (CONTRACT v5 D1, R1): new row keys
// legacy ignores. Drain begin, signal, cancel and finalize write them, and
// PreWake clears them with E3's request half (session.DrainAckIncarnationKey,
// session.DrainAckAtKey). Only activeStop (C6a) reads them.
const (
	drainIntentReasonKey      = "drain_intent_reason"
	drainIntentAtKey          = "drain_intent_at"
	drainIntentIncarnationKey = "drain_intent_incarnation"
)
