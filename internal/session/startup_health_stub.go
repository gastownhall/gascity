package session

import (
	"time"
)

// This file is a RED-phase placeholder for the startup-health episode API
// (ga-o04bfr.1.1). It exists only so startup_health_test.go and
// cmd/gc/startup_health_reconcile_test.go compile and every assertion fails
// for a real reason instead of an undefined-symbol build error masking which
// acceptance criteria are unmet. GREEN (ga-jugpkj) deletes this file and adds
// the real implementation in internal/session/startup_health.go.

// StartupHealthEpisodeType is the bead type for a persisted startup-health
// episode record.
const StartupHealthEpisodeType = "startup-health-episode"

// Metadata keys backing the fields of StartupHealthEpisode on its bead.
const (
	StartupHealthSessionNameMetadataKey      = "startup_health_session_name"
	StartupHealthConsecutiveMetadataKey      = "startup_health_consecutive"
	StartupHealthFirstFailureMetadataKey     = "startup_health_first_failure_at"
	StartupHealthLastFailureMetadataKey      = "startup_health_last_failure_at"
	StartupHealthLastDetailMetadataKey       = "startup_health_last_detail"
	StartupHealthAlertMetadataKey            = "startup_health_alert_disposition"
	StartupHealthQuarantinedUntilMetadataKey = "startup_health_quarantined_until"
)

// startupHealthLastDetailMaxRunes bounds StartupHealthEpisode.LastDetail.
const startupHealthLastDetailMaxRunes = 500

// AlertDisposition tracks whether a startup-health escalation has been sent
// for the current run of consecutive failures.
type AlertDisposition string

// AlertDisposition values.
const (
	AlertDispositionNone    AlertDisposition = ""
	AlertDispositionPending AlertDisposition = "pending"
	AlertDispositionSent    AlertDisposition = "sent"
)

// StartupHealthEpisode is a bookkeeping record of consecutive startup
// failures for one session name, used to escalate and quarantine a session
// whose provider start keeps failing.
type StartupHealthEpisode struct {
	SessionName      string
	ConsecutiveCount int
	FirstFailureAt   time.Time
	LastFailureAt    time.Time
	LastDetail       string
	AlertDisposition AlertDisposition
	QuarantinedUntil time.Time
}

// StartupHealthEpisodeFromMetadata projects a StartupHealthEpisode from a
// bead's metadata map.
func StartupHealthEpisodeFromMetadata(_ map[string]string) StartupHealthEpisode {
	panic("StartupHealthEpisodeFromMetadata: not implemented — RED-phase stub for ga-o04bfr.1.1")
}

// RecordStartupFailure returns the episode transition for one more
// consecutive startup failure.
func RecordStartupFailure(_ StartupHealthEpisode, _ string, _ time.Time, _ int, _ time.Duration) StartupHealthEpisode {
	panic("RecordStartupFailure: not implemented — RED-phase stub for ga-o04bfr.1.1")
}

// ClearStartupHealthEpisode returns the zero-value episode for sessionName,
// recorded on a successful start.
func ClearStartupHealthEpisode(_ string) StartupHealthEpisode {
	panic("ClearStartupHealthEpisode: not implemented — RED-phase stub for ga-o04bfr.1.1")
}

// LoadStartupHealthEpisode loads the startup-health episode for sessionName,
// returning the zero value if none is recorded.
func (*Store) LoadStartupHealthEpisode(_ string) (StartupHealthEpisode, error) {
	panic("LoadStartupHealthEpisode: not implemented — RED-phase stub for ga-o04bfr.1.1")
}

// SaveStartupHealthEpisode upserts the startup-health episode for its
// SessionName.
func (*Store) SaveStartupHealthEpisode(_ StartupHealthEpisode) error {
	panic("SaveStartupHealthEpisode: not implemented — RED-phase stub for ga-o04bfr.1.1")
}
