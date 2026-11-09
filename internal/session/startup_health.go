package session

import (
	"errors"
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
	StartupHealthRuntimeNameMetadataKey      = "startup_health_runtime_session_name"
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

// AlertDisposition tracks the startup-health escalation for the current run of
// consecutive failures. A run escalates at most once: pending is the only
// state a sender may claim, and every other non-empty state is terminal until
// a successful start clears the episode.
type AlertDisposition string

// AlertDisposition values.
const (
	AlertDispositionNone    AlertDisposition = ""
	AlertDispositionPending AlertDisposition = "pending"
	// AlertDispositionSending marks a claimed escalation whose outcome is not
	// yet recorded. It is written before the send, so an episode left here by a
	// crash is never sent again.
	AlertDispositionSending AlertDisposition = "sending"
	AlertDispositionSent    AlertDisposition = "sent"
	// AlertDispositionSkipped records a threshold crossing with no
	// [session].escalation_to configured.
	AlertDispositionSkipped AlertDisposition = "skipped"
	// AlertDispositionFailed records an escalation that was attempted and did
	// not go out: an invalid recipient, an unavailable provider, or a send error.
	AlertDispositionFailed AlertDisposition = "failed"
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
	// FailureKindStartupDeath marks a start whose process died during startup,
	// leaving start-stderr.log in the session diagnostics directory.
	FailureKindStartupDeath FailureKind = "startup_death"
)

// StartupHealthEpisode is a bookkeeping record of consecutive startup
// failures for one session name, used to escalate and quarantine a session
// whose provider start keeps failing.
type StartupHealthEpisode struct {
	SessionName string
	// RuntimeSessionName is the runtime session name of the last failed
	// attempt, the name the runtime wrote start-stderr.log under. It differs
	// from SessionName for a pool row, whose episode is keyed by identity.
	RuntimeSessionName string
	ConsecutiveCount   int
	FirstFailureAt     time.Time
	LastFailureAt      time.Time
	LastDetail         string
	Kind               FailureKind
	AlertDisposition   AlertDisposition
	QuarantinedUntil   time.Time
}

// StartupHealthEpisodeFromMetadata projects a StartupHealthEpisode from a
// bead's metadata map. Malformed or absent numeric/time fields fall back to
// the zero value; string fields (LastDetail, AlertDisposition) pass through
// verbatim, including unrecognized AlertDisposition values, so a caller can
// observe raw persisted state without this projection silently discarding it.
func StartupHealthEpisodeFromMetadata(meta map[string]string) StartupHealthEpisode {
	ep := StartupHealthEpisode{
		SessionName:        meta[StartupHealthSessionNameMetadataKey],
		RuntimeSessionName: meta[StartupHealthRuntimeNameMetadataKey],
		LastDetail:         meta[StartupHealthLastDetailMetadataKey],
		Kind:               FailureKind(meta[StartupHealthKindMetadataKey]),
		AlertDisposition:   AlertDisposition(meta[StartupHealthAlertMetadataKey]),
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
		StartupHealthRuntimeNameMetadataKey:      ep.RuntimeSessionName,
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
// AlertDisposition escalates to pending only from AlertDispositionNone, so
// every later state (pending, sending, sent, skipped, failed) is kept through
// further accrual and one run of failures escalates at most once.
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
		SessionName:        prior.SessionName,
		RuntimeSessionName: prior.RuntimeSessionName,
		ConsecutiveCount:   prior.ConsecutiveCount + 1,
		FirstFailureAt:     firstFailureAt,
		LastFailureAt:      now,
		LastDetail:         string(detailRunes),
		Kind:               kind,
		AlertDisposition:   prior.AlertDisposition,
		QuarantinedUntil:   prior.QuarantinedUntil,
	}
	if next.ConsecutiveCount >= threshold {
		next.QuarantinedUntil = now.Add(quarantineDuration)
		if next.AlertDisposition == AlertDispositionNone {
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

// startupHealthEpisodeBeads lists sessionName's startup-health episode bead,
// if any. Only a bead of StartupHealthEpisodeType is one: another bead
// carrying the episode's keys (a task's) holds no session down (mc-lp7zb).
func (s *Store) startupHealthEpisodeBeads(sessionName string) ([]beads.Bead, error) {
	return s.store.List(beads.ListQuery{
		Type:     StartupHealthEpisodeType,
		Metadata: map[string]string{StartupHealthSessionNameMetadataKey: sessionName},
		Limit:    1,
		Sort:     beads.SortCreatedDesc,
	})
}

// LoadStartupHealthEpisode loads the startup-health episode for sessionName,
// returning the zero value if none is recorded. Read-only: it issues a single
// typed metadata query and no mutating store call.
func (s *Store) LoadStartupHealthEpisode(sessionName string) (StartupHealthEpisode, error) {
	matches, err := s.startupHealthEpisodeBeads(sessionName)
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
	matches, err := s.startupHealthEpisodeBeads(ep.SessionName)
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

// ErrStartupHealthAlertConflict reports that a startup-health episode's alert
// disposition was not the one the caller expected, or that the episode no
// longer exists. The caller lost the claim and must not act on it.
var ErrStartupHealthAlertConflict = errors.New("startup-health alert disposition changed")

// SetStartupHealthAlert moves sessionName's episode from the expected alert
// disposition to next, writing only the disposition key so a concurrent
// failure or clear of the other fields is not overwritten. It returns
// ErrStartupHealthAlertConflict when the episode is absent or holds a
// different disposition. The read-then-write is not atomic across processes;
// the controller's single escalation sweep is the only writer of these
// transitions.
func (s *Store) SetStartupHealthAlert(sessionName string, expected, next AlertDisposition) error {
	matches, err := s.startupHealthEpisodeBeads(sessionName)
	if err != nil {
		return fmt.Errorf("looking up startup-health episode for %q: %w", sessionName, err)
	}
	if len(matches) == 0 {
		return fmt.Errorf("startup-health episode for %q: %w", sessionName, ErrStartupHealthAlertConflict)
	}
	current := AlertDisposition(matches[0].Metadata[StartupHealthAlertMetadataKey])
	if current != expected {
		return fmt.Errorf("startup-health episode for %q is %q, want %q: %w", sessionName, current, expected, ErrStartupHealthAlertConflict)
	}
	if err := s.store.SetMetadataBatch(matches[0].ID, map[string]string{StartupHealthAlertMetadataKey: string(next)}); err != nil {
		return fmt.Errorf("setting startup-health alert for %q: %w", sessionName, err)
	}
	return nil
}

// ListStartupHealthEpisodesByAlert returns the open startup-health episodes
// whose alert disposition is exactly d. Unlike ListStartupHealthEpisodes it
// skips closed beads: only an open bead can be claimed through
// SetStartupHealthAlert.
func (s *Store) ListStartupHealthEpisodesByAlert(d AlertDisposition) ([]StartupHealthEpisode, error) {
	matches, err := s.store.List(beads.ListQuery{
		Type:     StartupHealthEpisodeType,
		Metadata: map[string]string{StartupHealthAlertMetadataKey: string(d)},
	})
	if err != nil {
		return nil, fmt.Errorf("listing startup-health episodes with alert %q: %w", d, err)
	}
	episodes := make([]StartupHealthEpisode, 0, len(matches))
	for _, b := range matches {
		episodes = append(episodes, StartupHealthEpisodeFromMetadata(b.Metadata))
	}
	return episodes, nil
}
