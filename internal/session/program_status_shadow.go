package session

import "github.com/gastownhall/gascity/internal/runtime"

// RecordObservationShadow records, under the program-status shadow switch,
// the observation point (observation) for info's session: phase is the
// worker's transcript-derived reading (idle, busy or pending), set beside
// the record's idle verdict on a reading taken fresh, off the caller's path
// (runtime.ShadowObserveIdle). It records only when the pair changes (the
// sink filters) and never feeds the observation it compares.
func (m *Manager) RecordObservationShadow(info Info, phase runtime.ProgramVerdict) {
	if info.SessionName == "" {
		return
	}
	runtime.ShadowObserveIdle(m.sp, info.SessionName, info.ID, ProviderFamilyFromInfo(info, ""), phase)
}
