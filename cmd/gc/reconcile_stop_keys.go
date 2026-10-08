package main

import "github.com/gastownhall/gascity/internal/session"

// The controller half of the stop request (CONTRACT v5 D1, R1): new row keys
// legacy ignores. Drain begin, signal, cancel and finalize write them, and
// PreWake clears them with E3's request half (session.DrainAckIncarnationKey,
// session.DrainAckAtKey). Only activeStop (C6a) reads them. They are defined
// in internal/session, whose resume CAS (D8) clears both halves.
const (
	drainIntentReasonKey      = session.DrainIntentReasonKey
	drainIntentAtKey          = session.DrainIntentAtKey
	drainIntentIncarnationKey = session.DrainIntentIncarnationKey
)
