package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

// A config-drift deferral over an ordinary seat's open work (#8345) keeps
// the seat on its old config for as long as the work stays open. Both
// reconcilers keep that protection and make the wait visible: the episode's
// start and drift key ride config_drift_deferred_{at,key}, and its first
// observation records session.config_drift_deferred; once the episode
// outlasts its horizon ([daemon] drift_deferral_escalate_after), one
// escalated event follows, config_drift_escalated_at marking it sent.

// configDriftEscalatedAtMetadata marks a held-work drift episode's one
// escalation.
const configDriftEscalatedAtMetadata = "config_drift_escalated_at"

// driftEpisodeStep is one observation of a held-work drift episode: the
// patch to write (nil: nothing), the episode's start, whether the patch
// escalates it, and when a waiting episode escalates (zero: never).
type driftEpisodeStep struct {
	Patch     session.MetadataPatch
	Since     time.Time
	Escalated bool
	Due       time.Time
}

// driftEpisode observes the held-work drift episode under key at now: a
// stamp that does not parse, lies after now or names another drift key
// starts the episode at now, clearing any escalation marker; an episode under horizon waits until it; past
// it, an episode without an escalation marker between its start and now
// escalates; one with such a marker writes nothing.
func driftEpisode(info session.Info, key string, now time.Time, horizon time.Duration) driftEpisodeStep {
	since, ok := parseRFC3339Metadata(info.ConfigDriftDeferredAt)
	if !ok || since.After(now) || info.ConfigDriftDeferredKey != key {
		return driftEpisodeStep{Since: now, Patch: driftEpisodePatch(key, now, false)}
	}
	if m, ok := parseRFC3339Metadata(info.ConfigDriftEscalatedAt); ok && !m.Before(since) && !m.After(now) {
		return driftEpisodeStep{Since: since}
	}
	if due := since.Add(horizon); now.Before(due) {
		return driftEpisodeStep{Since: since, Due: due}
	}
	return driftEpisodeStep{Since: since, Escalated: true, Patch: driftEpisodePatch(key, now, true)}
}

// driftEpisodePatch is a held-work drift episode's write at now: its one
// escalation's marker, or else the start of an episode under key, clearing
// any escalation marker an earlier episode left.
func driftEpisodePatch(key string, now time.Time, escalated bool) session.MetadataPatch {
	at := now.UTC().Format(time.RFC3339)
	if escalated {
		return session.MetadataPatch{configDriftEscalatedAtMetadata: at}
	}
	return session.MetadataPatch{
		namedSessionConfigDriftDeferredAtMetadata:  at,
		namedSessionConfigDriftDeferredKeyMetadata: key,
		configDriftEscalatedAtMetadata:             "",
	}
}

// driftDeferralHorizon is cfg's held-work escalation horizon, the default
// without a config.
func driftDeferralHorizon(cfg *config.City) time.Duration {
	if cfg == nil {
		return config.DefaultDriftDeferralEscalateAfter
	}
	return cfg.Daemon.DriftDeferralEscalateAfterDuration()
}

// configDriftDeferredEvent is the session.config_drift_deferred event for
// info's drift held by work bead workBeadID since since, observed at now,
// escalated or a first observation: the drifted field names, or "field
// details unavailable" with none.
func configDriftDeferredEvent(info session.Info, fields []string, workBeadID string, since, now time.Time, escalated bool) events.Event {
	name := strings.TrimSpace(info.SessionNameMetadata)
	detail := "field details unavailable"
	if len(fields) > 0 {
		detail = "drifted fields: " + strings.Join(fields, ", ")
	}
	pending := now.Sub(since).Round(time.Second)
	msg := fmt.Sprintf("config drift on session %q deferred %s by open work %s (%s)", name, pending, workBeadID, detail)
	if escalated {
		msg = fmt.Sprintf("config drift on session %q still deferred after %s by open work %s (%s); it runs its old config until the work closes or an operator acts", name, pending, workBeadID, detail)
	}
	return events.Event{
		Type:      events.SessionConfigDriftDeferred,
		Ts:        now,
		Actor:     "gc",
		Subject:   info.ID,
		Message:   msg,
		SessionID: info.ID,
		Payload:   api.SessionConfigDriftDeferredPayloadJSON(info.ID, name, info.Template, fields, workBeadID, since, pending, escalated),
	}
}
