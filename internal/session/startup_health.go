package session

import (
	"fmt"
	"strconv"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

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
	StartupHealthKindMetadataKey             = "startup_health_kind"
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

// FailureKind classifies why a startup attempt failed, so a caller (such as
// the visible session row mirror) can distinguish a hung/slow provider from
// any other failure without parsing LastDetail.
type FailureKind string

// FailureKind values.
const (
	FailureKindUnspecified FailureKind = ""
	FailureKindTimeout     FailureKind = "timeout"
	FailureKindOther       FailureKind = "other"
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
	Kind             FailureKind
	AlertDisposition AlertDisposition
	QuarantinedUntil time.Time
}

// StartupHealthEpisodeFromMetadata projects a StartupHealthEpisode from a
// bead's metadata map. Malformed or absent numeric/time fields fall back to
// the zero value; string fields (LastDetail, AlertDisposition) pass through
// verbatim, including unrecognized AlertDisposition values, so a caller can
// observe raw persisted state without this projection silently discarding it.
func StartupHealthEpisodeFromMetadata(meta map[string]string) StartupHealthEpisode {
	ep := StartupHealthEpisode{
		SessionName:      meta[StartupHealthSessionNameMetadataKey],
		LastDetail:       meta[StartupHealthLastDetailMetadataKey],
		Kind:             FailureKind(meta[StartupHealthKindMetadataKey]),
		AlertDisposition: AlertDisposition(meta[StartupHealthAlertMetadataKey]),
	}
	if n, err := strconv.Atoi(meta[StartupHealthConsecutiveMetadataKey]); err == nil {
		ep.ConsecutiveCount = n
	}
	if t, err := time.Parse(time.RFC3339, meta[StartupHealthFirstFailureMetadataKey]); err == nil {
		ep.FirstFailureAt = t
	}
	if t, err := time.Parse(time.RFC3339, meta[StartupHealthLastFailureMetadataKey]); err == nil {
		ep.LastFailureAt = t
	}
	if t, err := time.Parse(time.RFC3339, meta[StartupHealthQuarantinedUntilMetadataKey]); err == nil {
		ep.QuarantinedUntil = t
	}
	return ep
}

// startupHealthEpisodeToMetadata is the exact inverse of
// StartupHealthEpisodeFromMetadata. Every key is written unconditionally,
// including an empty string for a zero-valued time field: SaveStartupHealthEpisode
// upserts in place (never deletes a key), so a field that regresses to zero
// (ClearStartupHealthEpisode after a successful start) must overwrite the
// stale prior value rather than leave it in place. Empty-string metadata
// values are a pinned cross-backend clear contract (TestMetadataEmptyStringClearContract).
func startupHealthEpisodeToMetadata(ep StartupHealthEpisode) map[string]string {
	return map[string]string{
		StartupHealthSessionNameMetadataKey:      ep.SessionName,
		StartupHealthConsecutiveMetadataKey:      strconv.Itoa(ep.ConsecutiveCount),
		StartupHealthFirstFailureMetadataKey:     formatStartupHealthTime(ep.FirstFailureAt),
		StartupHealthLastFailureMetadataKey:      formatStartupHealthTime(ep.LastFailureAt),
		StartupHealthLastDetailMetadataKey:       ep.LastDetail,
		StartupHealthKindMetadataKey:             string(ep.Kind),
		StartupHealthAlertMetadataKey:            string(ep.AlertDisposition),
		StartupHealthQuarantinedUntilMetadataKey: formatStartupHealthTime(ep.QuarantinedUntil),
	}
}

func formatStartupHealthTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// RecordStartupFailure returns the episode transition for one more
// consecutive startup failure. ConsecutiveCount always increments;
// FirstFailureAt is stamped only once (preserved across accrual); LastDetail
// and Kind are overwritten on every call (no accrual, no stickiness);
// QuarantinedUntil is set once ConsecutiveCount reaches threshold; and
// AlertDisposition escalates to pending on entering quarantine but never
// regresses away from AlertDispositionSent (an already-sent escalation stays
// sent through further accrual).
func RecordStartupFailure(prior StartupHealthEpisode, kind FailureKind, detail string, now time.Time, threshold int, quarantineDuration time.Duration) StartupHealthEpisode {
	firstFailureAt := prior.FirstFailureAt
	if firstFailureAt.IsZero() {
		firstFailureAt = now
	}
	detailRunes := []rune(detail)
	if len(detailRunes) > startupHealthLastDetailMaxRunes {
		detailRunes = detailRunes[:startupHealthLastDetailMaxRunes]
	}
	next := StartupHealthEpisode{
		SessionName:      prior.SessionName,
		ConsecutiveCount: prior.ConsecutiveCount + 1,
		FirstFailureAt:   firstFailureAt,
		LastFailureAt:    now,
		LastDetail:       string(detailRunes),
		Kind:             kind,
		AlertDisposition: prior.AlertDisposition,
		QuarantinedUntil: prior.QuarantinedUntil,
	}
	if next.ConsecutiveCount >= threshold {
		next.QuarantinedUntil = now.Add(quarantineDuration)
		if next.AlertDisposition != AlertDispositionSent {
			next.AlertDisposition = AlertDispositionPending
		}
	}
	return next
}

// ClearStartupHealthEpisode returns the zero-value episode for sessionName,
// recorded on a successful start.
func ClearStartupHealthEpisode(sessionName string) StartupHealthEpisode {
	return StartupHealthEpisode{SessionName: sessionName}
}

// LoadStartupHealthEpisode loads the startup-health episode for sessionName,
// returning the zero value if none is recorded. Read-only: it issues a single
// metadata query and no mutating store call.
func (s *Store) LoadStartupHealthEpisode(sessionName string) (StartupHealthEpisode, error) {
	matches, err := s.store.ListByMetadata(map[string]string{StartupHealthSessionNameMetadataKey: sessionName}, 1)
	if err != nil {
		return StartupHealthEpisode{}, fmt.Errorf("loading startup-health episode for %q: %w", sessionName, err)
	}
	if len(matches) == 0 {
		return StartupHealthEpisode{}, nil
	}
	return StartupHealthEpisodeFromMetadata(matches[0].Metadata), nil
}

// SaveStartupHealthEpisode upserts the startup-health episode for its
// SessionName: creates a new startup-health-episode bead if none exists yet,
// otherwise updates the existing one in place. The bead carries no session
// label, so it is never returned by Store.List (which queries by
// LabelSession) and is excluded from bd ready via internal/beads'
// readyExcludeTypes.
func (s *Store) SaveStartupHealthEpisode(ep StartupHealthEpisode) error {
	meta := startupHealthEpisodeToMetadata(ep)
	matches, err := s.store.ListByMetadata(map[string]string{StartupHealthSessionNameMetadataKey: ep.SessionName}, 1)
	if err != nil {
		return fmt.Errorf("looking up startup-health episode for %q: %w", ep.SessionName, err)
	}
	if len(matches) == 0 {
		if _, err := s.store.Create(beads.Bead{
			Title:    "Startup health: " + ep.SessionName,
			Type:     StartupHealthEpisodeType,
			Metadata: meta,
		}); err != nil {
			return fmt.Errorf("creating startup-health episode for %q: %w", ep.SessionName, err)
		}
		return nil
	}
	if err := s.store.SetMetadataBatch(matches[0].ID, meta); err != nil {
		return fmt.Errorf("updating startup-health episode for %q: %w", ep.SessionName, err)
	}
	return nil
}

// ListStartupHealthEpisodes returns every persisted startup-health episode,
// including ones behind a closed bead: a doctor check surfacing active
// episodes must not silently miss one because some other path already closed
// its bead.
func (s *Store) ListStartupHealthEpisodes() ([]StartupHealthEpisode, error) {
	matches, err := s.store.List(beads.ListQuery{Type: StartupHealthEpisodeType, IncludeClosed: true})
	if err != nil {
		return nil, fmt.Errorf("listing startup-health episodes: %w", err)
	}
	episodes := make([]StartupHealthEpisode, 0, len(matches))
	for _, b := range matches {
		episodes = append(episodes, StartupHealthEpisodeFromMetadata(b.Metadata))
	}
	return episodes, nil
}
