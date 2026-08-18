package beads

// MetadataCASCapableFor reports whether store offers a metadata CAS that can
// actually be trusted right now, not merely one that type-asserts.
//
// MetadataCASWriterFor alone is a pure static lookup: BdStore structurally
// implements MetadataCASWriter unconditionally, so a caller that stops at
// MetadataCASWriterFor gets (writer, true) even when the installed bd CLI
// cannot honor a fenced write (missing --if-revision). MetadataCASCapableFor
// closes that gap by also consulting the store's capability prober
// (conditionalWriteCapabilityProber) when it has one — the same live,
// runtime-checked answer ResolveConditionalWriter uses at the write seam.
//
// This is deliberately independent of any operator-policy mode: it does not
// consult conditionalWritesModeCarrier or require the store to already carry
// a require/auto stamp. It answers one question only — can this store
// instance, right now, actually perform a fenced metadata write — which
// callers that need a boot-time or preflight capability answer must use
// instead of MetadataCASWriterFor by itself.
//
// A store that implements MetadataCASWriter with no prober is vacuously
// capable, mirroring conditionalWriteCapabilityProber's own documented
// default. A store that does not implement MetadataCASWriter at all
// (directly or via MetadataCASWriterHandleProvider) is incapable regardless
// of any prober.
func MetadataCASCapableFor(store Store) (bool, string) {
	if store == nil {
		return false, "store is nil"
	}
	resolved := followConditionalWritesResolveTarget(store)
	if _, ok := MetadataCASWriterFor(resolved); !ok {
		return false, "store does not implement a metadata CAS (MetadataCASWriter)"
	}
	if prober, ok := resolved.(conditionalWriteCapabilityProber); ok {
		return prober.probeConditionalWriteCapability()
	}
	return true, ""
}
