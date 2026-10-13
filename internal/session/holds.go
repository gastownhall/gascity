package session

import (
	"strings"
	"time"
)

// HoldVerdict is CONTRACT v5.9 D8 7(a)'s one hold predicate, for resume,
// wake requests, controller-routed sends and queued delivery: a hold that
// blocks a consume, unless the row is not suspended and its runtime is
// running (a working session; a heartbeat held_until keeps it up, not queued).
// It is the disposition's HoldsConsume over meta.
func HoldVerdict(meta map[string]string, runtimeRunning bool, now time.Time) bool {
	return DecodeDisposition(meta).HoldsConsume(runtimeRunning, State(strings.TrimSpace(meta["state"])), now)
}

// HoldVerdictInfo is HoldVerdict over a typed row.
func HoldVerdictInfo(info Info, runtimeRunning bool, now time.Time) bool {
	return HoldVerdict(dispositionMeta(info), runtimeRunning, now)
}
