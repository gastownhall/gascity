package main

import (
	"strconv"
	"strings"
	"time"
)

// wakeBackoffInvalidationGrace bounds how long after backoff_last_set a work
// bead's UpdatedAt is still attributed to the write that recorded the backoff
// itself, rather than to a genuine external change (mail reply, PR update,
// label change). Without this grace window, the very metadata write that
// sets backoff_until/backoff_last_set would bump UpdatedAt past
// backoff_last_set and immediately self-invalidate the backoff it just
// recorded. See ga-7fldxz.2 (NFR2a).
const wakeBackoffInvalidationGrace = 5 * time.Minute

// applyWakeBackoffMetadata projects a work bead's externally-written wake-backoff
// metadata (backoff_until / backoff_count / backoff_last_set, set out-of-band via
// `gc ... --set-metadata` when a bead is externally blocked) onto its
// AwakeWorkBead. It is the raw-metadata half of buildAwakeInputFromReconciler's
// work-bead projection, kept in this file so the reconciler front-door bridge
// (compute_awake_bridge.go) stays free of raw bead-metadata cracks per the
// metadataInfoOnlyFiles guard. The projection semantics are unchanged from the
// former inline block; updatedAt is the work bead's UpdatedAt.
//
// NFR2a invalidation net: a genuine external change since the backoff was
// recorded forces an immediate reset independent of agent judgment. The grace
// window keeps the write that records backoff_last_set from immediately defeating
// its own suppression (that write's UpdatedAt lands at ~backoff_last_set, not
// strictly before it). A backoff with no valid backoff_last_set anchor has no
// invalidation net at all, so it fails open: without a trustworthy anchor the
// bridge cannot tell a stale window from a fresh one, and suppressing would risk
// hiding genuinely-ready work until backoff_until elapsed. Clearing
// WakeBackoffUntil keeps the worst case at extra wake activity, never hidden work.
func applyWakeBackoffMetadata(awakeWB *AwakeWorkBead, metadata map[string]string, updatedAt time.Time) {
	if until, err := time.Parse(time.RFC3339, strings.TrimSpace(metadata["backoff_until"])); err == nil && !until.IsZero() {
		awakeWB.WakeBackoffUntil = until
	}
	if n, err := strconv.Atoi(strings.TrimSpace(metadata["backoff_count"])); err == nil {
		awakeWB.WakeBackoffCount = n
	}
	if lastSet, err := time.Parse(time.RFC3339, strings.TrimSpace(metadata["backoff_last_set"])); err == nil && !lastSet.IsZero() {
		if updatedAt.After(lastSet.Add(wakeBackoffInvalidationGrace)) {
			awakeWB.WakeBackoffUntil = time.Time{}
		}
	} else {
		awakeWB.WakeBackoffUntil = time.Time{}
	}
}
