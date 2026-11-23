//go:build !windows

package main

import "time"

// statusRuntimeProbeBudget is how long one status observation may take. It
// matches one tmux StateCache fetch (internal/runtime/tmux fetchTimeout): a
// refresh still inside that contract is an observation, not an unobserved
// fleet. The former 50ms per-call and 750ms per-observation timers were both
// shorter than a fetch, so under host load gc status and gc rig status
// reported live agents unknown.
const statusRuntimeProbeBudget = 3 * time.Second

// statusProviderCallTimeoutDefault bounds one runtime-provider call made for a
// status read when positive. It is zero so the call runs on the observation
// goroutine: that goroutine is already scheduled, and
// statusObservationTimeoutDefault is the single budget of one snapshot fetch.
// A second timer here raced the fetch and substituted not-running.
const statusProviderCallTimeoutDefault = time.Duration(0)

// statusObservationTimeoutDefault bounds the observation of one session for a
// status read: one runtime snapshot fetch.
const statusObservationTimeoutDefault = statusRuntimeProbeBudget
