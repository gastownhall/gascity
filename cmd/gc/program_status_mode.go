package main

import (
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// programStatusModeSetter is a session leaf that arms and reads program
// status under the city switch it is given (the tmux provider).
type programStatusModeSetter interface {
	SetProgramStatusMode(mode runtime.ProgramStatusMode)
}

// publishProgramStatusMode publishes an accepted config's [session]
// program_status switch to every leaf of sp that arms or reads under it. A
// reload keeps a provider whose selection is unchanged (rebuilding it would
// stop its sessions), so the switch it was built with goes stale unless each
// published config reaches it here: readings and arming then follow the
// switch off->shadow/on, on->off and on->shadow alike. A nil config reads as
// off.
func publishProgramStatusMode(cfg *config.City, sp runtime.Provider) {
	mode := runtime.ProgramStatusOff
	if cfg != nil {
		mode = cfg.Session.ProgramStatusMode()
	}
	publishProgramStatusModeTo(sp, mode)
}

// publishProgramStatusModeTo publishes mode to sp's leaves, walking
// composites through their backends.
func publishProgramStatusModeTo(sp runtime.Provider, mode runtime.ProgramStatusMode) {
	switch p := sp.(type) {
	case nil:
	case runtime.BackendsProvider:
		for _, b := range p.Backends() {
			publishProgramStatusModeTo(b.Provider, mode)
		}
	case programStatusModeSetter:
		p.SetProgramStatusMode(mode)
	}
}
