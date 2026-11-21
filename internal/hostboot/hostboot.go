// Package hostboot reports the host's boot time.
//
// Reap paths use it as positive proof that a runtime artifact cannot still
// exist: a tmux server never survives a reboot, so a session whose bead was
// created before the current boot has no live runtime, even when the runtime
// backend is unreachable and can therefore prove nothing by observation.
package hostboot

import "time"

// BootTime returns the wall-clock time at which the host booted.
//
// It returns an error on platforms where the boot instant is not exposed;
// callers must treat that as "cannot prove death" and fail safe.
func BootTime() (time.Time, error) { return bootTime() }
