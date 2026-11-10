// Package beadstall classifies in_progress beads whose updated_at has stopped
// advancing and persists one bead-scoped episode per continuous stall, so an
// escalation fires once per episode and survives controller restarts.
//
// Detection is pure timestamp arithmetic over status, priority, updated_at,
// thresholds and an injected clock. The package never mutates the stalled
// bead; episode state lives on a separate tracking bead.
//
// A caller resolves thresholds once with ResolveThresholds, classifies each
// in_progress bead with Detect, steps its episode with Advance, and persists
// the result through an EpisodeStore. The package sends no alert and
// publishes no event: Transition.Alert only asks the caller to.
package beadstall

import (
	"fmt"
	"strconv"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// EpisodeBeadType is the bead type of an episode tracking bead. It is listed
// in beads.RequiredCustomTypes so every bd store accepts it, and in
// beads.readyExcludeTypes so tracking beads never become actionable work.
const EpisodeBeadType = "stalled-bead-episode"

// fallbackPriority is the priority a bead with no priority is reported as.
const fallbackPriority = 2

// Thresholds maps a priority to the age at which an in_progress bead is
// stalled, with a fallback for beads whose priority is unset or has no entry.
// The zero value has no threshold and classifies nothing as stalled, so the
// value returned beside a ResolveThresholds error is safe to hold.
type Thresholds struct {
	byPriority map[int]time.Duration
	fallback   time.Duration
}

// DefaultThresholds returns P0=2h, P1=6h, P2=24h, P3=72h with a 24h fallback
// for unset or unconfigured priorities.
func DefaultThresholds() Thresholds {
	return Thresholds{
		byPriority: map[int]time.Duration{
			0: 2 * time.Hour,
			1: 6 * time.Hour,
			2: 24 * time.Hour,
			3: 72 * time.Hour,
		},
		fallback: 24 * time.Hour,
	}
}

// ResolveThresholds layers the [stalled_beads] section of cfg over the
// defaults: each configured priority replaces that priority's default, a
// configured fallback_threshold replaces the 24h fallback, and everything
// else keeps its default. A bead with no priority is reported as P2 but is
// judged by the fallback, so a P2 override moves only beads labeled P2.
//
// An invalid section returns its config error, which names the offending
// key, together with the zero Thresholds; callers must treat the error as
// fatal for alarming rather than fall back to the defaults.
func ResolveThresholds(cfg config.StalledBeadsConfig) (Thresholds, error) {
	overrides, fallback, err := cfg.Overrides()
	if err != nil {
		return Thresholds{}, fmt.Errorf("resolving stalled-bead thresholds: %w", err)
	}
	limits := DefaultThresholds()
	for priority, d := range overrides {
		limits.byPriority[priority] = d
	}
	if fallback > 0 {
		limits.fallback = fallback
	}
	return limits, nil
}

// ForPriority returns the stall threshold for priority; a nil or
// unconfigured priority gets the fallback.
func (t Thresholds) ForPriority(priority *int) time.Duration {
	if priority != nil {
		if d, ok := t.byPriority[*priority]; ok {
			return d
		}
	}
	return t.fallback
}

// Detection is the pure classification of one bead at one instant.
type Detection struct {
	// BeadID is the classified bead.
	BeadID string
	// Priority is the bead's priority, or P2 when it has none.
	Priority int
	// UpdatedAt is the bead's updated_at at classification.
	UpdatedAt time.Time
	// Age is now minus UpdatedAt, never negative.
	Age time.Duration
	// Threshold is the age at which this bead counts as stalled.
	Threshold time.Duration
	// Stalled reports an in_progress bead whose Age has reached Threshold.
	Stalled bool
}

// Detect classifies b at now. A bead is stalled when it is in_progress, its
// threshold is positive, and now-updated_at has reached that threshold. An
// updated_at in the future yields age 0. A bead without a priority is
// reported as the fallback priority (P2) and judged by the fallback
// threshold. Detect reads nothing but its arguments.
func Detect(b beads.Bead, limits Thresholds, now time.Time) Detection {
	priority := fallbackPriority
	if b.Priority != nil {
		priority = *b.Priority
	}
	age := now.Sub(b.UpdatedAt)
	if age < 0 {
		age = 0
	}
	threshold := limits.ForPriority(b.Priority)
	return Detection{
		BeadID:    b.ID,
		Priority:  priority,
		UpdatedAt: b.UpdatedAt,
		Age:       age,
		Threshold: threshold,
		Stalled:   b.Status == "in_progress" && threshold > 0 && age >= threshold,
	}
}

// AlertDisposition records whether the escalation for an episode was sent.
type AlertDisposition string

// AlertDisposition values.
const (
	AlertNone    AlertDisposition = "none"
	AlertPending AlertDisposition = "pending"
	AlertSent    AlertDisposition = "sent"
)

// Episode is one continuous stall of one bead, keyed by the stalled bead's
// ID and distinguished from an earlier stall by LastKnownUpdatedAt.
type Episode struct {
	BeadID             string
	LastKnownUpdatedAt time.Time
	FirstStaleAt       time.Time
	Priority           int
	AlertDisposition   AlertDisposition
}

// Transition is the outcome of advancing an episode by one detection.
type Transition struct {
	// Episode is the episode to persist (or, with Clear, to end).
	Episode Episode
	// Alert asks the caller to send the escalation for Episode.
	Alert bool
	// Clear asks the caller to end the recorded episode.
	Clear bool
	// Changed reports that Episode differs from the prior one.
	Changed bool
}

// Advance computes the transition from prior (nil when no episode is
// recorded) given the current detection. A new stall, or one whose
// updated_at moved while still stale, opens a pending episode and asks for an
// alert. A pending episode keeps asking for the alert until it is recorded as
// sent. A bead that is no longer stalled clears its episode. Advance is pure.
func Advance(prior *Episode, current Detection, now time.Time) Transition {
	if !current.Stalled {
		if prior == nil {
			return Transition{}
		}
		return Transition{Episode: *prior, Clear: true, Changed: true}
	}
	if prior == nil || prior.BeadID != current.BeadID || !prior.LastKnownUpdatedAt.Equal(current.UpdatedAt) {
		return Transition{
			Episode: Episode{
				BeadID:             current.BeadID,
				LastKnownUpdatedAt: current.UpdatedAt,
				FirstStaleAt:       now,
				Priority:           current.Priority,
				AlertDisposition:   AlertPending,
			},
			Alert:   true,
			Changed: true,
		}
	}
	if prior.AlertDisposition == AlertSent {
		return Transition{Episode: *prior}
	}
	return Transition{Episode: *prior, Alert: true}
}

// Metadata keys backing the fields of episode on its tracking bead.
const (
	stalledBeadIDKey       = "stalled_bead_id"
	lastKnownUpdatedAtKey  = "stalled_bead_last_known_updated_at"
	firstStaleAtKey        = "stalled_bead_first_stale_at"
	stalledBeadPriorityKey = "stalled_bead_priority"
	stalledBeadAlertKey    = "stalled_bead_alert_disposition"
)

// EpisodeStore persists episodes as metadata on tracking beads in a work
// store. It never touches the stalled bead itself.
type EpisodeStore struct {
	store beads.WorkStore
}

// NewEpisodeStore returns an EpisodeStore backed by store.
func NewEpisodeStore(store beads.WorkStore) *EpisodeStore {
	return &EpisodeStore{store: store}
}

// find returns the newest open tracking bead for beadID, if any. A bead the
// store returns for another stalled bead is an error, not a match: the
// episode of one bead is never read, rewritten or closed as another's.
func (s *EpisodeStore) find(beadID string) (beads.Bead, bool, error) {
	found, err := s.store.List(beads.ListQuery{
		Type:     EpisodeBeadType,
		Metadata: map[string]string{stalledBeadIDKey: beadID},
		Limit:    1,
		Sort:     beads.SortCreatedDesc,
	})
	if err != nil {
		return beads.Bead{}, false, fmt.Errorf("listing stalled-bead episode for %q: %w", beadID, err)
	}
	if len(found) == 0 {
		return beads.Bead{}, false, nil
	}
	if got := found[0].Metadata[stalledBeadIDKey]; got != beadID {
		return beads.Bead{}, false, fmt.Errorf("stalled-bead episode %q tracks %s %q, not the requested %q", found[0].ID, stalledBeadIDKey, got, beadID)
	}
	return found[0], true, nil
}

// Load returns the recorded episode for beadID; ok is false when none exists.
// When more than one open tracking bead exists for beadID the newest wins.
func (s *EpisodeStore) Load(beadID string) (Episode, bool, error) {
	b, ok, err := s.find(beadID)
	if err != nil || !ok {
		return Episode{}, false, err
	}
	ep, err := decodeEpisode(b, beadID)
	if err != nil {
		return Episode{}, false, fmt.Errorf("loading stalled-bead episode for %q from %q: %w", beadID, b.ID, err)
	}
	return ep, true, nil
}

// Save creates or updates the tracking bead for ep.BeadID. Every metadata key
// is written on each save so a stale value never survives an upsert. The
// stalled bead itself is never touched.
func (s *EpisodeStore) Save(ep Episode) error {
	if ep.BeadID == "" {
		return fmt.Errorf("saving stalled-bead episode: bead ID is required")
	}
	meta := encodeEpisode(ep)
	b, ok, err := s.find(ep.BeadID)
	if err != nil {
		return err
	}
	if !ok {
		if _, err := s.store.Create(beads.Bead{
			Title:    "stalled-bead episode " + ep.BeadID,
			Type:     EpisodeBeadType,
			Metadata: meta,
		}); err != nil {
			return fmt.Errorf("creating stalled-bead episode for %q: %w", ep.BeadID, err)
		}
		return nil
	}
	if err := s.store.SetMetadataBatch(b.ID, meta); err != nil {
		return fmt.Errorf("updating stalled-bead episode %q for %q: %w", b.ID, ep.BeadID, err)
	}
	return nil
}

// Clear ends the episode for beadID by closing its tracking bead, which keeps
// the history. Clearing when no episode exists is a no-op.
func (s *EpisodeStore) Clear(beadID string) error {
	b, ok, err := s.find(beadID)
	if err != nil || !ok {
		return err
	}
	if err := s.store.Close(b.ID); err != nil {
		return fmt.Errorf("closing stalled-bead episode %q for %q: %w", b.ID, beadID, err)
	}
	return nil
}

func encodeEpisode(ep Episode) map[string]string {
	return map[string]string{
		stalledBeadIDKey:       ep.BeadID,
		lastKnownUpdatedAtKey:  formatTime(ep.LastKnownUpdatedAt),
		firstStaleAtKey:        formatTime(ep.FirstStaleAt),
		stalledBeadPriorityKey: strconv.Itoa(ep.Priority),
		stalledBeadAlertKey:    string(ep.AlertDisposition),
	}
}

// decodeEpisode reads the episode stored on b, which must track beadID.
func decodeEpisode(b beads.Bead, beadID string) (Episode, error) {
	if got := b.Metadata[stalledBeadIDKey]; got != beadID {
		return Episode{}, fmt.Errorf("%s %q is not the requested %q", stalledBeadIDKey, got, beadID)
	}
	ep := Episode{
		BeadID:           beadID,
		AlertDisposition: AlertDisposition(b.Metadata[stalledBeadAlertKey]),
	}
	var err error
	if ep.LastKnownUpdatedAt, err = parseTime(lastKnownUpdatedAtKey, b.Metadata[lastKnownUpdatedAtKey]); err != nil {
		return Episode{}, err
	}
	if ep.FirstStaleAt, err = parseTime(firstStaleAtKey, b.Metadata[firstStaleAtKey]); err != nil {
		return Episode{}, err
	}
	if ep.Priority, err = strconv.Atoi(b.Metadata[stalledBeadPriorityKey]); err != nil {
		return Episode{}, fmt.Errorf("parsing %s %q: %w", stalledBeadPriorityKey, b.Metadata[stalledBeadPriorityKey], err)
	}
	switch ep.AlertDisposition {
	case AlertNone, AlertPending, AlertSent:
	default:
		return Episode{}, fmt.Errorf("unknown %s %q", stalledBeadAlertKey, ep.AlertDisposition)
	}
	return ep, nil
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(key, raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing %s %q: %w", key, raw, err)
	}
	return t, nil
}
