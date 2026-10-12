package proxyendpoint

// ServedProbeForTest is the ONE way code outside readCursorsOver gets a served
// ProbeResult whose cursor reality counts as evaluated (council pr2 D-F11).
//
// It exists for probe stubs in tests across internal/beads, internal/doctor and
// cmd/gc. A stub that builds ProbeResult{Outcome: ProbeServed, Cursors: …} by
// hand gets an UNCHECKED reality, and the admission gate refuses it with a
// non-terminal verdict instead of reading its zero value as "nothing was
// missing" — so a stub that means "served, and the reality is this" has to say
// so by name, here. Production must never call it: the probe session is the
// only thing entitled to say a reality was checked, and
// TestServedProbeForTestIsNeverCalledInProduction holds that line.
func ServedProbeForTest(cursors Cursors, reality CursorReality) ProbeResult {
	reality.checked = true
	return ProbeResult{Outcome: ProbeServed, Cursors: cursors, Reality: reality}
}
