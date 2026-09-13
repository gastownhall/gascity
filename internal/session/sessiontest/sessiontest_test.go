package sessiontest_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// richBead is a broadly-populated session bead: it exercises the closed-status
// blanking, a pinned CreatedAt, custom labels, and a spread of metadata clusters
// so the byte-identity pins below cover more than the trivial fields.
func richBead(id, status string) beads.Bead {
	return beads.Bead{
		ID:        id,
		Type:      session.BeadType,
		Status:    status,
		Title:     "My Session",
		Labels:    []string{session.LabelSession, "agent:polecat-7", "custom:keep-me"},
		CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Metadata: map[string]string{
			"session_name":   id,
			"template":       "polecat",
			"state":          "asleep",
			"alias":          "pc-1",
			"agent_name":     "polecat-7",
			"provider":       "claude",
			"command":        "claude --foo",
			"work_dir":       "/tmp/wd",
			"session_key":    "uuid-abc",
			"sleep_reason":   "idle",
			"wake_attempts":  "3",
			"session_health": "degraded",
			"pool_managed":   "true",
		},
	}
}

// TestSeedBeadMatchesCodec pins the load-bearing claim of the whole wave: reading
// a verbatim-seeded bead back through the front door yields the session.Info the
// store's projection produces — so a test that swaps a raw-bead crack for
// SeedBead(t, b) is behavior-identical. It asserts the fields SeedBead must
// preserve verbatim (id / labels / pinned CreatedAt, which a store.Create would
// rewrite) plus a representative spread of metadata-projected fields and the
// closed-status blanking. (The exact byte-identity of the front-door read to the
// codec is pinned inside internal/session, where the now-unexported codec lives;
// this package cannot see it, and doesn't need to.)
func TestSeedBeadMatchesCodec(t *testing.T) {
	for _, status := range []string{"open", "closed"} {
		t.Run(status, func(t *testing.T) {
			b := richBead("s-seed-"+status, status)
			got := sessiontest.SeedBead(t, b)

			// Verbatim preservation (the fields store.Create would rewrite).
			if got.ID != b.ID {
				t.Errorf("SeedBead dropped id: got %q, want %q", got.ID, b.ID)
			}
			if !reflect.DeepEqual(got.Labels, b.Labels) {
				t.Errorf("SeedBead dropped custom labels: got %v, want %v", got.Labels, b.Labels)
			}
			if !got.CreatedAt.Equal(b.CreatedAt) {
				t.Errorf("SeedBead dropped pinned CreatedAt: got %v, want %v", got.CreatedAt, b.CreatedAt)
			}

			// Metadata projected through the store front door (a representative spread).
			if got.Title != "My Session" || got.Template != "polecat" || got.Alias != "pc-1" ||
				got.AgentName != "polecat-7" || got.Provider != "claude" || got.Command != "claude --foo" ||
				got.WorkDir != "/tmp/wd" || got.SessionKey != "uuid-abc" || got.SleepReason != "idle" ||
				got.WakeAttempts != 3 || got.WakeAttemptsMetadata != "3" || got.HealthState != "degraded" ||
				!got.PoolManaged || got.SessionName != "s-seed-"+status {
				t.Errorf("SeedBead metadata projection wrong: %+v", got)
			}

			// closed blanks the runtime State; open keeps the stored state verbatim.
			wantClosed := status == "closed"
			if got.Closed != wantClosed {
				t.Errorf("SeedBead Closed = %v, want %v", got.Closed, wantClosed)
			}
			wantState := "asleep"
			if wantClosed {
				wantState = ""
			}
			if string(got.State) != wantState {
				t.Errorf("SeedBead State = %q, want %q", got.State, wantState)
			}
		})
	}
}

// TestStoreVerbatimSeedReadsBack pins that Store(seed…) inserts each bead verbatim
// and a front-door Get reads it back — the multi-bead store-read path — preserving
// the id and pinned CreatedAt, honoring the closed status, and projecting metadata.
func TestStoreVerbatimSeedReadsBack(t *testing.T) {
	a := richBead("s-a", "open")
	c := richBead("s-c", "closed")
	s, _ := sessiontest.Store(t, a, c)

	for _, b := range []beads.Bead{a, c} {
		got, err := s.Get(b.ID)
		if err != nil {
			t.Fatalf("Get(%q): %v", b.ID, err)
		}
		if got.ID != b.ID {
			t.Errorf("Get(%q).ID = %q", b.ID, got.ID)
		}
		if wantClosed := b.Status == "closed"; got.Closed != wantClosed {
			t.Errorf("Get(%q).Closed = %v, want %v", b.ID, got.Closed, wantClosed)
		}
		if got.Template != "polecat" || !got.CreatedAt.Equal(b.CreatedAt) {
			t.Errorf("Get(%q) verbatim seed lost fields: %+v", b.ID, got)
		}
	}
}

// TestInfoStoreCreateRoundTrips pins that Info (store-create) returns the same
// projection a subsequent Get would — and that the id is the store-assigned one
// (the documented reason Info is only for tests that read the returned id back).
func TestInfoStoreCreateRoundTrips(t *testing.T) {
	s, _ := sessiontest.Store(t)
	// NOTE: CreateSpec.AgentName drives the "agent:<name>" selection LABEL; the
	// projected Info.AgentName comes from metadata["agent_name"], so a fixture
	// that asserts Info.AgentName must set it in Metadata too.
	info := sessiontest.Info(t, s, session.CreateSpec{
		Title:     "worker",
		AgentName: "worker",
		Metadata:  map[string]string{"template": "worker", "agent_name": "worker", "state": "asleep"},
	})
	if info.ID == "" {
		t.Fatal("Info returned an empty id")
	}
	got, err := s.Get(info.ID)
	if err != nil {
		t.Fatalf("Get(%q): %v", info.ID, err)
	}
	if !reflect.DeepEqual(got, info) {
		t.Fatalf("Info != subsequent Get\n got: %+v\ninfo: %+v", got, info)
	}
	if info.Template != "worker" || info.AgentName != "worker" {
		t.Errorf("Info dropped fields: template=%q agent=%q", info.Template, info.AgentName)
	}
}

// TestInfoFromMetaProjectsMetadata pins the metadata-only one-liner: the projected
// fields match the codec, and the synthetic id follows session_name.
func TestInfoFromMetaProjectsMetadata(t *testing.T) {
	meta := map[string]string{"session_name": "s-meta", "state": "active", "sleep_reason": "idle"}
	got := sessiontest.InfoFromMeta(t, meta)
	if got.ID != "s-meta" {
		t.Errorf("InfoFromMeta id = %q, want s-meta (from session_name)", got.ID)
	}
	if got.SleepReason != "idle" || string(got.State) != "active" {
		t.Errorf("InfoFromMeta dropped fields: state=%q sleep=%q", got.State, got.SleepReason)
	}
}
