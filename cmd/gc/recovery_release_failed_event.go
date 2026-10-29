package main

import (
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// publishRecoveryReleaseFailures records one bead.recovery_release_failed
// event for each failed release the default on_death/on_boot hook reported in
// out. The hook exits 0 even when a bd write fails, so its gc-recovery line is
// the only signal; this turns it into a typed event naming the bead, the pool
// agent, the hook and bd's error text. Output without a parseable failure (a
// clean run, a user override) publishes nothing. A nil recorder is a no-op.
func publishRecoveryReleaseFailures(rec events.Recorder, out, agent string) {
	if rec == nil {
		return
	}
	for _, f := range config.ParseRecoveryFailures(out) {
		rec.Record(events.Event{
			Type:    events.BeadRecoveryReleaseFailed,
			Ts:      time.Now().UTC(),
			Actor:   "gc",
			Subject: f.BeadID,
			Message: f.Hook + " recovery could not release " + f.BeadID + " for " + agent + ": " + f.Error,
			Payload: api.BeadRecoveryReleaseFailedPayloadJSON(f.BeadID, agent, f.Hook, f.Error),
		})
	}
}
