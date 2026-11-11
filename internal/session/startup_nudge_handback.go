package session

import (
	"log"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
)

// queueStartupNudgeHandbackLocked takes the startup nudge the runtime handed
// back after a start of sessName that succeeded
// (runtime.TakeStartupNudgeHandback): under program_status = "on", one its
// program held back behind a dialog through the whole startup ladder, never
// typed (design §10.2 row A20). It is queued as a user's submit for b's
// session (nudgequeue.SourceSubmit), delivered verbatim once the dialog is
// answered. A start that handed nothing back queues nothing; a queueing
// failure is logged, since the start itself succeeded.
func (m *Manager) queueStartupNudgeHandbackLocked(b beads.Bead, sessName string) {
	nudge, ok := runtime.TakeStartupNudgeHandback(m.sp, sessName)
	if !ok {
		return
	}
	if err := m.enqueueDeferredSubmitLocked(b, sessName, nudge, nudgequeue.SourceSubmit); err != nil {
		log.Printf("session: %q: started, but queueing the startup nudge its program's dialog held back failed: %v", sessName, err)
	}
}
