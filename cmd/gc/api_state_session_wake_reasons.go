package main

import (
	"log"

	"github.com/gastownhall/gascity/internal/api"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// SessionListWakeReasons is api.SessionListWakeReasoner over the local `gc
// session list` REASON computation (sessionListReasonParts), so the API
// route's REASON column prints what a local read prints. Only open sessions
// are indexed, as the local read indexes its open snapshot; a closed session
// gets no reasons. A failed ready-wait read is logged and the reasons are
// computed without it, as the local read warns and continues.
func (cs *controllerState) SessionListWakeReasons(listed []sessionpkg.Info, persisted map[string]sessionpkg.Info) map[string][]string {
	infoIndex := make(map[string]sessionpkg.Info, len(persisted))
	for id, info := range persisted {
		if !info.Closed {
			infoIndex[id] = info
		}
	}
	readyWaitSet, err := readyWaitSetForList(sessionpkg.NewStore(cs.SessionsBeadStore()))
	if err != nil {
		log.Printf("api: session list wake reasons for city %s: ready wait indicators degraded: %v", cs.cityName, err)
	}
	inputs := newSessionListReasonInputs(listed, infoIndex, cs.Config(), cs.SessionProvider(), readyWaitSet)
	out := make(map[string][]string)
	for _, s := range listed {
		if _, wake := inputs.parts(s); len(wake) > 0 {
			out[s.ID] = wake
		}
	}
	return out
}

var _ api.SessionListWakeReasoner = (*controllerState)(nil)
