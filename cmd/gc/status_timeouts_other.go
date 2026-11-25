//go:build !windows

package main

import "time"

// statusProviderCallTimeoutDefault bounds one runtime-provider call made for a
// status read.
const statusProviderCallTimeoutDefault = 50 * time.Millisecond

// statusObservationTimeoutDefault bounds the observation of one session for a
// status read.
const statusObservationTimeoutDefault = 750 * time.Millisecond
