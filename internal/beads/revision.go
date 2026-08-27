package beads

// RevisionKnown reports whether a loaded Bead.Revision names a real row version
// and can therefore carry a conditional write.
//
// Zero is the store's "unavailable" sentinel and the only value that cannot;
// every other int64 is a real revision, INCLUDING a negative one. The revision
// contract on ConditionalWriter is explicit that a revision is an opaque token
// callers may test ONLY for equality — ordering is undefined — so a `> 0` or
// `<= 0` gate is not a stricter check, it is a contract violation that
// misclassifies live rows. bd hands out signed revisions and roughly half of
// every city's rows carry a negative one, so a sign gate silently reclassifies
// half the fleet: fail-closed sites skip work that must happen (ga-f7v2ft.140's
// advisory status heal, the trigger rebind) and fail-open sites drop the CAS
// entirely, leaving the unconditional write the fence existed to replace
// (ga-f7v2ft.141's pre-wake incarnation commit).
//
// This is the only non-equality test the revision contract permits. Fence sites
// call it rather than writing the comparison inline, so the rule cannot drift
// back to the sign one site at a time.
func RevisionKnown(revision int64) bool {
	return revision != 0
}
