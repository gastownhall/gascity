//go:build windows

package main

import "time"

// statusProviderCallTimeoutDefault bounds one runtime-provider call made for a
// status read. A provider call on Windows starts processes: one herdr CLI call
// is a process start of 40-80 ms measured, and a liveness observation makes up
// to three. The bound leaves a call that is merely slow to start its processes
// a real answer instead of an unknown.
const statusProviderCallTimeoutDefault = 500 * time.Millisecond

// statusObservationTimeoutDefault bounds the observation of one session for a
// status read: a session-store read plus at most two bounded provider calls
// (liveness, then last activity).
const statusObservationTimeoutDefault = 2 * time.Second
