package session

import (
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestLifecycleInputConstructorsProjectIdentically is the byte-identical oracle
// guarding the Step-4B typed LifecycleInput: for every representative session
// bead shape, feeding ProjectLifecycle from the raw metadata map
// (LifecycleInputFromMetadata) must yield the exact same LifecycleView as
// feeding it from the projected session.Info (LifecycleInputFromInfo ∘
// InfoFromPersistedBead). Both paths cover the thirteen metadata keys
// ProjectLifecycle reads, including missing-key-vs-empty-string and the closed
// status that LifecycleInputFromInfo reconstructs from Info.Closed.
func TestLifecycleInputConstructorsProjectIdentically(t *testing.T) {
	now := time.Date(2026, 5, 3, 9, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour).Format(time.RFC3339)
	past := now.Add(-time.Hour).Format(time.RFC3339)

	tests := []struct {
		name               string
		status             string
		meta               map[string]string
		runtime            RuntimeFacts
		createdAt          time.Time
		staleCreatingAfter time.Duration
	}{
		{
			name:   "empty metadata open",
			status: "open",
			meta:   map[string]string{},
		},
		{
			name:   "nil metadata open",
			status: "open",
			meta:   nil,
		},
		{
			name:   "empty metadata closed",
			status: "closed",
			meta:   map[string]string{},
		},
		{
			name:   "closed status wins over stale active state",
			status: "closed",
			meta:   map[string]string{"state": "active"},
		},
		{
			name:   "all thirteen keys populated",
			status: "open",
			meta: map[string]string{
				"state":                     "asleep",
				"sleep_reason":              "user-hold",
				"continuity_eligible":       "true",
				"configured_named_identity": "worker",
				"held_until":                future,
				"quarantined_until":         future,
				"pending_create_claim":      "true",
				"last_woke_at":              past,
				"session_key":               "conv-1",
				"started_config_hash":       "hash-1",
				"pending_create_started_at": past,
				"pin_awake":                 "true",
				"wake_request":              "explicit",
			},
			runtime: RuntimeFacts{Observed: true, Alive: false},
		},
		{
			name:   "creating stale with pending create claim and last woke",
			status: "open",
			meta: map[string]string{
				"state":                "creating",
				"session_key":          "old-conv",
				"pending_create_claim": "true",
				"last_woke_at":         now.Add(-2 * time.Minute).Format(time.RFC3339),
			},
			runtime:            RuntimeFacts{Observed: true, Alive: false},
			createdAt:          now.Add(-2 * time.Minute),
			staleCreatingAfter: time.Minute,
		},
		{
			name:   "creating fresh via pending create started at",
			status: "open",
			meta: map[string]string{
				"state":                     "creating",
				"pending_create_started_at": now.Add(-10 * time.Second).Format(time.RFC3339),
			},
			runtime:            RuntimeFacts{Observed: true, Alive: false},
			createdAt:          now.Add(-2 * time.Minute),
			staleCreatingAfter: time.Minute,
		},
		{
			name:   "future holds block",
			status: "open",
			meta: map[string]string{
				"state":             "asleep",
				"held_until":        future,
				"quarantined_until": future,
				"pin_awake":         "true",
			},
		},
		{
			name:   "expired holds do not block",
			status: "open",
			meta: map[string]string{
				"state":             "asleep",
				"held_until":        past,
				"quarantined_until": past,
				"pin_awake":         "true",
			},
		},
		{
			name:   "archived continuity eligible",
			status: "open",
			meta: map[string]string{
				"state":                     "archived",
				"continuity_eligible":       "true",
				"configured_named_identity": "worker",
			},
		},
		{
			name:   "archived continuity ineligible",
			status: "open",
			meta: map[string]string{
				"state":               "archived",
				"continuity_eligible": "false",
			},
		},
		{
			name:   "non explicit wake request is not a cause",
			status: "open",
			meta: map[string]string{
				"state":        "asleep",
				"wake_request": "work",
			},
		},
		{
			name:   "dead active runtime resets stale resume identity",
			status: "open",
			meta: map[string]string{
				"state":               "active",
				"session_key":         "old-conv",
				"started_config_hash": "old-hash",
			},
			runtime: RuntimeFacts{Observed: true, Alive: false},
		},
		{
			name:   "keys present but empty strings",
			status: "open",
			meta: map[string]string{
				"state":                     "",
				"sleep_reason":              "",
				"continuity_eligible":       "",
				"configured_named_identity": "",
				"held_until":                "",
				"quarantined_until":         "",
				"pending_create_claim":      "",
				"last_woke_at":              "",
				"session_key":               "",
				"started_config_hash":       "",
				"pending_create_started_at": "",
				"pin_awake":                 "",
				"wake_request":              "",
			},
		},
		{
			name:   "whitespace padded values trim identically",
			status: "open",
			meta: map[string]string{
				"state":                " creating ",
				"pending_create_claim": " true ",
				"pin_awake":            " true ",
				"wake_request":         " explicit ",
			},
			runtime: RuntimeFacts{Observed: true, Alive: false},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := beads.Bead{
				ID:        "s-oracle",
				Type:      "session",
				Status:    tt.status,
				Metadata:  tt.meta,
				CreatedAt: tt.createdAt,
			}

			fromMeta := LifecycleInputFromMetadata(b.Status, b.Metadata)
			fromInfo := LifecycleInputFromInfo(infoFromPersistedBead(b))

			// The caller supplies external facts identically to both inputs;
			// only the thirteen metadata-derived fields and the Status may
			// differ between the two construction paths.
			for _, in := range []*LifecycleInput{&fromMeta, &fromInfo} {
				in.Runtime = tt.runtime
				in.CreatedAt = tt.createdAt
				in.StaleCreatingAfter = tt.staleCreatingAfter
				in.Now = now
			}

			gotMeta := ProjectLifecycle(fromMeta)
			gotInfo := ProjectLifecycle(fromInfo)
			if !reflect.DeepEqual(gotMeta, gotInfo) {
				t.Fatalf("projection diverged between FromMetadata and FromInfo:\n from meta = %#v\n from info = %#v", gotMeta, gotInfo)
			}
		})
	}
}
