package main

import (
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// claimPoolStartAdmission is the CLI adapter for the common session start fence.
func claimPoolStartAdmission(cityPath, route, sessionID string, now time.Time, sp runtime.Provider, store beads.Store) (bool, string) {
	return session.ClaimCapacityStart(cityPath, route, sessionID, now, sp, store)
}
