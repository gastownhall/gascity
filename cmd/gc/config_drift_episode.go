package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

// A config-drift deferral over an ordinary seat's open work (#8345) keeps
// the seat on its old config for as long as the work stays open. Both
// reconcilers keep that protection and make the wait visible: the episode's
// start and drift key ride config_drift_deferred_{at,key}, and its first
// observation records session.config_drift_deferred.

// driftEpisodeStep is one observation of a held-work drift episode: the
// patch to write (nil: nothing) and the episode's start.
type driftEpisodeStep struct {
	Patch session.MetadataPatch
	Since time.Time
}

// driftEpisode observes the held-work drift episode under key at now: a
// stamp that does not parse, lies after now or names another drift key
// starts the episode at now; this key's stamp writes nothing.
func driftEpisode(info session.Info, key string, now time.Time) driftEpisodeStep {
	since, ok := parseRFC3339Metadata(info.ConfigDriftDeferredAt)
	if !ok || since.After(now) || info.ConfigDriftDeferredKey != key {
		return driftEpisodeStep{Since: now, Patch: session.MetadataPatch{
			namedSessionConfigDriftDeferredAtMetadata:  now.UTC().Format(time.RFC3339),
			namedSessionConfigDriftDeferredKeyMetadata: key,
		}}
	}
	return driftEpisodeStep{Since: since}
}

// configDriftDeferredEvent is the session.config_drift_deferred event for
// info's drift held by work bead workBeadID since since, observed at now:
// the drifted field names, or "field details unavailable" with none.
func configDriftDeferredEvent(info session.Info, fields []string, workBeadID string, since, now time.Time) events.Event {
	name := strings.TrimSpace(info.SessionNameMetadata)
	detail := "field details unavailable"
	if len(fields) > 0 {
		detail = "drifted fields: " + strings.Join(fields, ", ")
	}
	pending := now.Sub(since).Round(time.Second)
	return events.Event{
		Type:      events.SessionConfigDriftDeferred,
		Ts:        now,
		Actor:     "gc",
		Subject:   info.ID,
		Message:   fmt.Sprintf("config drift on session %q deferred %s by open work %s (%s)", name, pending, workBeadID, detail),
		SessionID: info.ID,
		Payload:   api.SessionConfigDriftDeferredPayloadJSON(info.ID, name, info.Template, fields, workBeadID, since, pending, false),
	}
}
