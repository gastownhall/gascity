package main

import (
	"io"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// nudgeOutstandingStartupKickoffs is a RED-phase placeholder for the
// named/direct startup-kickoff backstop driver (ga-zogqc1.2.4). It exists
// only so startup_kickoff_nudge_test.go compiles and every assertion fails
// for a real reason instead of an undefined-symbol build error masking which
// acceptance criteria are unmet. GREEN (ga-03329i) deletes this file and adds
// the real implementation — wired through runNudgeBackstop and a new
// namedDirectStartupBackstop predicate — in a non-test file.
func nudgeOutstandingStartupKickoffs(_ runtime.Provider, _ *config.City, _ beads.Store, _, _ []beads.Bead, _ time.Time, _ events.Recorder, _ io.Writer) {
	panic("nudgeOutstandingStartupKickoffs: not implemented — RED-phase stub for ga-zogqc1.2.4")
}
