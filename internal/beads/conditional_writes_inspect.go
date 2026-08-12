package beads

import "github.com/gastownhall/gascity/internal/rollout/gate"

// Verdict vocabulary for conditional-writes inspection (§12.5). Probe reports
// the memoized capability probe; Latch reports the runtime unsupported latch.
// The two are independent on purpose: an in-place bd upgrade after a runtime
// latch reads probe=capable latch=incapable, whose fix is "restart to
// re-probe", not "upgrade bd".
const (
	ConditionalWriteProbeCapable   = "capable"
	ConditionalWriteProbeIncapable = "incapable"
	ConditionalWriteProbeUnprobed  = "unprobed"

	ConditionalWriteLatchIncapable = "incapable"
	ConditionalWriteLatchUnlatched = "unlatched"
)

// ConditionalWritesInspection is a side-effect-free snapshot of one store's
// conditional-writes state: the factory stamp plus the capability memo the
// write path consults. It never runs a probe and never mutates the memo — it
// reports the daemon's own latched state, not a re-derivation (§12.5).
type ConditionalWritesInspection struct {
	// Mode is the factory-stamped gate mode; ModeUnset when the store carries
	// no stamp (a legacy open — the write path never fences it).
	Mode gate.Mode
	// Defaulted marks a ModeUnset→Off factory mapping (unthreaded open path).
	Defaulted bool
	// StoreKind names the resolved store type in the diagnostic vocabulary
	// (BdStore, MemStore, ...; %T for build-tagged types).
	StoreKind string
	// Probe is the memoized capability-probe verdict:
	// capable | incapable | unprobed.
	Probe string
	// Latch is the runtime unsupported latch: incapable | unlatched.
	Latch string
	// Capable is what the write path would use today: false only on a
	// definitive incapable verdict (probe or latch); an unprobed store
	// reports true with Probe=unprobed so operators can tell "verified
	// capable" from "not yet exercised".
	Capable bool
	// Reason carries the incapable cause verbatim; empty when capable.
	Reason string
}

// conditionalWriteStateInspector is implemented by stores that can report
// their probe/latch memo without side effects. It is deliberately distinct
// from conditionalWriteCapabilityProber: the prober may RUN the probe (bd
// shells out four subprocesses); the inspector only reads what a prior probe
// or latch already recorded.
type conditionalWriteStateInspector interface {
	inspectConditionalWriteState() (probe, latch, reason string)
}

// inspectConditionalWriteState reads BdStore's capability memo under its
// mutex without triggering the lazy probe.
func (s *BdStore) inspectConditionalWriteState() (probe, latch, reason string) {
	s.condWriteMu.Lock()
	defer s.condWriteMu.Unlock()
	probe = ConditionalWriteProbeUnprobed
	if s.condWriteProbed {
		if s.condWriteCapable {
			probe = ConditionalWriteProbeCapable
		} else {
			probe = ConditionalWriteProbeIncapable
			if s.condWriteProbeErr != nil {
				reason = "capability probe failed: " + s.condWriteProbeErr.Error()
			} else {
				reason = "bd lacks " + conditionalWriteFlag + " (four-verb capability probe)"
			}
		}
	}
	latch = ConditionalWriteLatchUnlatched
	if s.condWriteLatched {
		latch = ConditionalWriteLatchIncapable
		reason = "conditional writes latched unsupported at runtime (bd rejected " + conditionalWriteFlag + ")"
	}
	return probe, latch, reason
}

// inspectConditionalWriteState reports MemStore's instance toggle. The check
// is instantaneous and side-effect-free, so the probe column is always
// definitive; Mem/File have no runtime latch machinery. FileStore and
// DoltliteReadStore inherit their inspectors through embedding (MemStore and
// BdStore respectively), reading the same storage the write path uses.
// latch is fixed "unlatched" by design: Mem/File have no runtime latch
// machinery; the tuple shape is the inspector interface contract.
//
//nolint:unparam
func (m *MemStore) inspectConditionalWriteState() (probe, latch, reason string) {
	if capable, why := m.probeConditionalWriteCapability(); !capable {
		return ConditionalWriteProbeIncapable, ConditionalWriteLatchUnlatched, why
	}
	return ConditionalWriteProbeCapable, ConditionalWriteLatchUnlatched, ""
}

// inspectConditionalWriteState forwards to the cache's backing (through its
// declared resolve target): cache and backing are one store instance for
// capability purposes, exactly as the write path treats them.
func (c *CachingStore) inspectConditionalWriteState() (probe, latch, reason string) {
	if inspector, ok := c.conditionalBacking().(conditionalWriteStateInspector); ok {
		return inspector.inspectConditionalWriteState()
	}
	return ConditionalWriteProbeUnprobed, ConditionalWriteLatchUnlatched, ""
}

// InspectConditionalWrites snapshots store's conditional-writes state for
// diagnostic surfaces (the status wire, doctor). It follows the same
// resolve-target walk the write path uses, then reads — never writes — the
// stamp and the capability memo. Inspecting costs no subprocesses; an
// unexercised bd store legitimately reports Probe=unprobed.
func InspectConditionalWrites(store Store) ConditionalWritesInspection {
	if store != nil {
		store = followConditionalWritesResolveTarget(store)
	}
	insp := ConditionalWritesInspection{
		StoreKind: conditionalStoreKind(store),
		Probe:     ConditionalWriteProbeUnprobed,
		Latch:     ConditionalWriteLatchUnlatched,
	}
	if store == nil {
		return insp
	}
	if carrier, ok := store.(conditionalWritesModeCarrier); ok {
		insp.Mode, insp.Defaulted = carrier.conditionalWritesMode()
	}
	if inspector, ok := store.(conditionalWriteStateInspector); ok {
		insp.Probe, insp.Latch, insp.Reason = inspector.inspectConditionalWriteState()
	}
	insp.Capable = insp.Probe != ConditionalWriteProbeIncapable &&
		insp.Latch != ConditionalWriteLatchIncapable
	return insp
}
