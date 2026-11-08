package session

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// createEpisodeBeadForTest creates a startup-health-episode bead for ep with
// the given create-time shape and returns it, bypassing SaveStartupHealthEpisode
// (which upserts) so a fixture can hold several beads for one session name.
func createEpisodeBeadForTest(t *testing.T, mem *beads.MemStore, ep StartupHealthEpisode, ephemeral bool) beads.Bead {
	t.Helper()
	b, err := mem.Create(beads.Bead{
		Title:     "Startup health: " + ep.SessionName,
		Type:      StartupHealthEpisodeType,
		Metadata:  startupHealthEpisodeToMetadata(ep),
		Ephemeral: ephemeral,
	})
	if err != nil {
		t.Fatalf("create episode bead for %q: %v", ep.SessionName, err)
	}
	return b
}

// TestOpenStartupHealthEpisodesMatchesPerSessionLoad pins that the batch read
// answers, for every session name, exactly what LoadStartupHealthEpisode
// answers: an open episode, a closed one, two beads for one name, a quarantined
// one, an ephemeral (wisp-tier) one, a name with no bead, and a non-episode
// bead that merely carries the episode's keys.
func TestOpenStartupHealthEpisodesMatchesPerSessionLoad(t *testing.T) {
	mem := beads.NewMemStore()
	s := NewStore(beads.SessionStore{Store: mem})
	quarantineEnd := time.Date(2026, 8, 20, 13, 0, 0, 0, time.UTC)

	createEpisodeBeadForTest(t, mem, StartupHealthEpisode{SessionName: "open", ConsecutiveCount: 1, Kind: FailureKindOther}, false)

	closed := createEpisodeBeadForTest(t, mem, StartupHealthEpisode{SessionName: "closed", ConsecutiveCount: 2, Kind: FailureKindTimeout}, false)
	if err := mem.Close(closed.ID); err != nil {
		t.Fatalf("close episode bead: %v", err)
	}

	// Two beads for one name: an older closed one and a newer open one, then
	// two open ones, so the newest-wins rule is exercised both ways.
	older := createEpisodeBeadForTest(t, mem, StartupHealthEpisode{SessionName: "dup-closed", ConsecutiveCount: 1}, false)
	createEpisodeBeadForTest(t, mem, StartupHealthEpisode{SessionName: "dup-closed", ConsecutiveCount: 5}, false)
	if err := mem.Close(older.ID); err != nil {
		t.Fatalf("close older episode bead: %v", err)
	}
	createEpisodeBeadForTest(t, mem, StartupHealthEpisode{SessionName: "dup-open", ConsecutiveCount: 1}, false)
	createEpisodeBeadForTest(t, mem, StartupHealthEpisode{SessionName: "dup-open", ConsecutiveCount: 3}, false)

	createEpisodeBeadForTest(t, mem, StartupHealthEpisode{SessionName: "quarantined", ConsecutiveCount: 3, QuarantinedUntil: quarantineEnd, AlertDisposition: AlertDispositionPending}, false)
	createEpisodeBeadForTest(t, mem, StartupHealthEpisode{SessionName: "ephemeral", ConsecutiveCount: 4, QuarantinedUntil: quarantineEnd}, true)

	// A bead of another type carrying the episode's session-name key holds no
	// session down (mc-lp7zb): neither read may see it.
	if _, err := mem.Create(beads.Bead{
		Title:    "not an episode",
		Type:     "task",
		Metadata: startupHealthEpisodeToMetadata(StartupHealthEpisode{SessionName: "impostor", ConsecutiveCount: 9, QuarantinedUntil: quarantineEnd}),
	}); err != nil {
		t.Fatalf("create impostor bead: %v", err)
	}

	batch, err := s.OpenStartupHealthEpisodes()
	if err != nil {
		t.Fatalf("OpenStartupHealthEpisodes: %v", err)
	}

	for _, name := range []string{"open", "closed", "dup-closed", "dup-open", "quarantined", "ephemeral", "impostor", "never-seen"} {
		want, err := s.LoadStartupHealthEpisode(name)
		if err != nil {
			t.Fatalf("LoadStartupHealthEpisode(%q): %v", name, err)
		}
		// A name the batch does not hold means "no episode": the zero value,
		// exactly what the per-session load returns for it.
		got := batch[name]
		if !reflect.DeepEqual(got, want) {
			t.Errorf("batch[%q] = %+v, want %+v (the per-session load's answer)", name, got, want)
		}
	}
}

// TestOpenStartupHealthEpisodesReadsOnce pins the point of the batch: one
// store read, however many sessions have an episode.
func TestOpenStartupHealthEpisodesReadsOnce(t *testing.T) {
	mem := beads.NewMemStore()
	for _, name := range []string{"a", "b", "c", "d"} {
		createEpisodeBeadForTest(t, mem, StartupHealthEpisode{SessionName: name, ConsecutiveCount: 1}, false)
	}
	counting := &episodeListCountingStore{Store: mem}
	s := NewStore(beads.SessionStore{Store: counting})

	got, err := s.OpenStartupHealthEpisodes()
	if err != nil {
		t.Fatalf("OpenStartupHealthEpisodes: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("batch holds %d episodes, want 4", len(got))
	}
	if counting.lists != 1 {
		t.Fatalf("store List calls = %d, want 1", counting.lists)
	}
}

// TestOpenStartupHealthEpisodesPropagatesListError pins that a failed read is
// an error, never an empty map a caller would read as "no episodes".
func TestOpenStartupHealthEpisodesPropagatesListError(t *testing.T) {
	boom := errors.New("store down")
	s := NewStore(beads.SessionStore{Store: &erroringStore{Store: beads.NewMemStore(), listQueryErr: boom}})

	got, err := s.OpenStartupHealthEpisodes()
	if !errors.Is(err, boom) {
		t.Fatalf("OpenStartupHealthEpisodes error = %v, want wrapped %v", err, boom)
	}
	if got != nil {
		t.Fatalf("OpenStartupHealthEpisodes = %v on error, want nil", got)
	}
}

// episodeListCountingStore counts List calls made through it.
type episodeListCountingStore struct {
	beads.Store
	lists int
}

func (c *episodeListCountingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	c.lists++
	return c.Store.List(q)
}
