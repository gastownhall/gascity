package session

// LegacyOnlyMarkers returns, for each registry key that only the legacy
// session reconciler pass writes, the marker naming where the v2 reconciler
// answers it: a ledger row, a latch refusal, an accepted reason, or a pending
// ruling. A key with a pending v2 writer is marked on its Fields row instead
// and does not appear here.
func LegacyOnlyMarkers() map[string]string {
	return map[string]string{}
}
