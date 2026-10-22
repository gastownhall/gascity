// Package beadstall classifies in_progress beads whose updated_at has stopped
// advancing and persists one bead-scoped episode per continuous stall, so an
// escalation fires once per episode and survives controller restarts.
//
// Detection is pure timestamp arithmetic over status, priority, updated_at,
// thresholds and an injected clock. The package never mutates the stalled
// bead; episode state lives on a separate tracking bead.
package beadstall

import (
	"fmt"
	"strconv"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// episodeBeadType is the bead type of an episode tracking bead. It is listed
// in beads.readyExcludeTypes so tracking beads never become actionable work.
const episodeBeadType = "stalled-bead-episode"

// fallbackPriority is the priority whose threshold applies to a bead whose
// priority is unset or has no configured threshold.
const fallbackPriority = 2

// thresholds maps a priority to the age at which an in_progress bead is
// stalled, with a fallback for priorities that have no entry.
type thresholds struct {
	byPriority map[int]time.Duration
	fallback   time.Duration
}

// defaultThresholds returns P0=2h, P1=6h, P2=24h, P3=72h with a 24h (P2)
// fallback for unset or unconfigured priorities.
func defaultThresholds() thresholds {
	return thresholds{
		byPriority: map[int]time.Duration{
			0: 2 * time.Hour,
			1: 6 * time.Hour,
			2: 24 * time.Hour,
			3: 72 * time.Hour,
		},
		fallback: 24 * time.Hour,
	}
}

// forPriority returns the stall threshold for priority; a nil or
// unconfigured priority gets the fallback.
func (t thresholds) forPriority(priority *int) time.Duration {
	if priority != nil {
		if d, ok := t.byPriority[*priority]; ok {
			return d
		}
	}
	return t.fallback
}

// detection is the pure classification of one bead at one instant.
type detection struct {
	beadID    string
	priority  int
	updatedAt time.Time
	age       time.Duration
	threshold time.Duration
	stalled   bool
}

// detectStalledBead classifies b at now. A bead is stalled when it is
// in_progress and now-updated_at has reached its priority threshold. An
// updated_at in the future yields age 0. A bead without a priority is
// reported as the fallback priority (P2).
func detectStalledBead(b beads.Bead, limits thresholds, now time.Time) detection {
	priority := fallbackPriority
	if b.Priority != nil {
		priority = *b.Priority
	}
	age := now.Sub(b.UpdatedAt)
	if age < 0 {
		age = 0
	}
	threshold := limits.forPriority(b.Priority)
	return detection{
		beadID:    b.ID,
		priority:  priority,
		updatedAt: b.UpdatedAt,
		age:       age,
		threshold: threshold,
		stalled:   b.Status == "in_progress" && age >= threshold,
	}
}

// alertDisposition records whether the escalation for an episode was sent.
type alertDisposition string

// alertDisposition values.
const (
	alertNone    alertDisposition = "none"
	alertPending alertDisposition = "pending"
	alertSent    alertDisposition = "sent"
)

// episode is one continuous stall of one bead, keyed by the stalled bead's
// ID and distinguished from an earlier stall by lastKnownUpdatedAt.
type episode struct {
	beadID             string
	lastKnownUpdatedAt time.Time
	firstStaleAt       time.Time
	priority           int
	alertDisposition   alertDisposition
}

// episodeTransition is the outcome of advancing an episode by one detection.
type episodeTransition struct {
	episode episode
	alert   bool
	clear   bool
	changed bool
}

// advanceEpisode computes the transition from prior (nil when no episode is
// recorded) given the current detection. A new stall, or one whose
// updated_at moved while still stale, opens a pending episode and asks for an
// alert. A pending episode keeps asking for the alert until it is recorded as
// sent. A bead that is no longer stalled clears its episode.
func advanceEpisode(prior *episode, current detection, now time.Time) episodeTransition {
	if !current.stalled {
		if prior == nil {
			return episodeTransition{}
		}
		return episodeTransition{episode: *prior, clear: true, changed: true}
	}
	if prior == nil || prior.beadID != current.beadID || !prior.lastKnownUpdatedAt.Equal(current.updatedAt) {
		return episodeTransition{
			episode: episode{
				beadID:             current.beadID,
				lastKnownUpdatedAt: current.updatedAt,
				firstStaleAt:       now,
				priority:           current.priority,
				alertDisposition:   alertPending,
			},
			alert:   true,
			changed: true,
		}
	}
	if prior.alertDisposition == alertSent {
		return episodeTransition{episode: *prior}
	}
	return episodeTransition{episode: *prior, alert: true}
}

// Metadata keys backing the fields of episode on its tracking bead.
const (
	stalledBeadIDKey       = "stalled_bead_id"
	lastKnownUpdatedAtKey  = "stalled_bead_last_known_updated_at"
	firstStaleAtKey        = "stalled_bead_first_stale_at"
	stalledBeadPriorityKey = "stalled_bead_priority"
	stalledBeadAlertKey    = "stalled_bead_alert_disposition"
)

// episodeStore persists episodes as metadata on tracking beads in store.
type episodeStore struct {
	store beads.Store
}

// newEpisodeStore returns an episodeStore backed by store.
func newEpisodeStore(store beads.Store) *episodeStore {
	return &episodeStore{store: store}
}

// find returns the open tracking bead for beadID, if any.
func (s *episodeStore) find(beadID string) (beads.Bead, bool, error) {
	found, err := s.store.List(beads.ListQuery{
		Type:     episodeBeadType,
		Metadata: map[string]string{stalledBeadIDKey: beadID},
		Limit:    1,
	})
	if err != nil {
		return beads.Bead{}, false, fmt.Errorf("listing stalled-bead episode for %q: %w", beadID, err)
	}
	if len(found) == 0 {
		return beads.Bead{}, false, nil
	}
	return found[0], true, nil
}

// load returns the recorded episode for beadID; ok is false when none exists.
func (s *episodeStore) load(beadID string) (episode, bool, error) {
	b, ok, err := s.find(beadID)
	if err != nil || !ok {
		return episode{}, false, err
	}
	ep, err := decodeEpisode(b)
	if err != nil {
		return episode{}, false, fmt.Errorf("loading stalled-bead episode for %q from %q: %w", beadID, b.ID, err)
	}
	return ep, true, nil
}

// save creates or updates the tracking bead for ep.beadID. Every metadata key
// is written on each save so a stale value never survives an upsert. The
// stalled bead itself is never touched.
func (s *episodeStore) save(ep episode) error {
	if ep.beadID == "" {
		return fmt.Errorf("saving stalled-bead episode: bead ID is required")
	}
	meta := encodeEpisode(ep)
	b, ok, err := s.find(ep.beadID)
	if err != nil {
		return err
	}
	if !ok {
		if _, err := s.store.Create(beads.Bead{
			Title:    "stalled-bead episode " + ep.beadID,
			Type:     episodeBeadType,
			Metadata: meta,
		}); err != nil {
			return fmt.Errorf("creating stalled-bead episode for %q: %w", ep.beadID, err)
		}
		return nil
	}
	if err := s.store.SetMetadataBatch(b.ID, meta); err != nil {
		return fmt.Errorf("updating stalled-bead episode %q for %q: %w", b.ID, ep.beadID, err)
	}
	return nil
}

// clear ends the episode for beadID by closing its tracking bead, which keeps
// the history. Clearing when no episode exists is a no-op.
func (s *episodeStore) clear(beadID string) error {
	b, ok, err := s.find(beadID)
	if err != nil || !ok {
		return err
	}
	if err := s.store.Close(b.ID); err != nil {
		return fmt.Errorf("closing stalled-bead episode %q for %q: %w", b.ID, beadID, err)
	}
	return nil
}

func encodeEpisode(ep episode) map[string]string {
	return map[string]string{
		stalledBeadIDKey:       ep.beadID,
		lastKnownUpdatedAtKey:  formatTime(ep.lastKnownUpdatedAt),
		firstStaleAtKey:        formatTime(ep.firstStaleAt),
		stalledBeadPriorityKey: strconv.Itoa(ep.priority),
		stalledBeadAlertKey:    string(ep.alertDisposition),
	}
}

func decodeEpisode(b beads.Bead) (episode, error) {
	ep := episode{
		beadID:           b.Metadata[stalledBeadIDKey],
		alertDisposition: alertDisposition(b.Metadata[stalledBeadAlertKey]),
	}
	var err error
	if ep.lastKnownUpdatedAt, err = parseTime(lastKnownUpdatedAtKey, b.Metadata[lastKnownUpdatedAtKey]); err != nil {
		return episode{}, err
	}
	if ep.firstStaleAt, err = parseTime(firstStaleAtKey, b.Metadata[firstStaleAtKey]); err != nil {
		return episode{}, err
	}
	if ep.priority, err = strconv.Atoi(b.Metadata[stalledBeadPriorityKey]); err != nil {
		return episode{}, fmt.Errorf("parsing %s %q: %w", stalledBeadPriorityKey, b.Metadata[stalledBeadPriorityKey], err)
	}
	switch ep.alertDisposition {
	case alertNone, alertPending, alertSent:
	default:
		return episode{}, fmt.Errorf("unknown %s %q", stalledBeadAlertKey, ep.alertDisposition)
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
