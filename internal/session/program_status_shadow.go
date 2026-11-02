package session

import (
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// shadowWaitIdlePreread records, under the program-status shadow switch, the
// pre-read a wait-idle delivery to b's session sessName would take
// (wait_idle_preread) beside the program's record. It never feeds the
// delivery: the wait that follows decides exactly as it does with the switch
// off.
func (m *Manager) shadowWaitIdlePreread(b beads.Bead, sessName string) {
	runtime.ShadowWaitIdlePreread(m.sp, sessName, b.ID, providerKind(b), time.Now())
}

// RecordObservationShadow records, under the program-status shadow switch,
// the observation point (observation) for info's session: phase is the
// worker's transcript-derived reading (idle, busy or pending), set beside
// the record's idle verdict. It records only when the pair changes (the sink
// filters) and never feeds the observation it compares.
func (m *Manager) RecordObservationShadow(info Info, phase runtime.ProgramVerdict) {
	if info.SessionName == "" {
		return
	}
	now := time.Now()
	runtime.ShadowCompare(m.sp, info.SessionName, info.ID, ProviderFamilyFromInfo(info, ""),
		runtime.ProgramPointObservation, phase, func(s runtime.ProgramStatus) runtime.ProgramVerdict {
			return runtime.ProgramIdleVerdict(s, time.Time{}, now)
		})
}
