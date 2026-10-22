package beadstall

import (
	"errors"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

var errStalledBeadNotImplemented = errors.New("stalled-bead alarm is not implemented")

const episodeBeadType = "stalled-bead-episode"

type thresholds struct {
	byPriority map[int]time.Duration
	fallback   time.Duration
}

func defaultThresholds() thresholds {
	return thresholds{}
}

func (thresholds) forPriority(_ *int) time.Duration {
	return 0
}

type detection struct {
	beadID    string
	priority  int
	updatedAt time.Time
	age       time.Duration
	threshold time.Duration
	stalled   bool
}

func detectStalledBead(_ beads.Bead, _ thresholds, _ time.Time) detection {
	return detection{}
}

type alertDisposition string

const (
	alertNone    alertDisposition = "none"
	alertPending alertDisposition = "pending"
	alertSent    alertDisposition = "sent"
)

type episode struct {
	beadID             string
	lastKnownUpdatedAt time.Time
	firstStaleAt       time.Time
	priority           int
	alertDisposition   alertDisposition
}

type episodeTransition struct {
	episode episode
	alert   bool
	clear   bool
	changed bool
}

func advanceEpisode(_ *episode, _ detection, _ time.Time) episodeTransition {
	return episodeTransition{}
}

type episodeStore struct {
	store beads.Store
}

func newEpisodeStore(store beads.Store) *episodeStore {
	return &episodeStore{store: store}
}

func (*episodeStore) load(_ string) (episode, bool, error) {
	return episode{}, false, errStalledBeadNotImplemented
}

func (*episodeStore) save(_ episode) error {
	return errStalledBeadNotImplemented
}

func (*episodeStore) clear(_ string) error {
	return errStalledBeadNotImplemented
}
