package session

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestGetPersistedResponseWithEnrich asserts the production Get read-model
// composition — Store.GetPersistedResponse for the persisted (Info,
// PersistedResponse) pair plus Manager.EnrichInfo for the runtime overlay —
// reproduces mgr.Get's enriched Info and PersistedResponseFromBead's projection,
// with bead serialization confined inside the session package. This is the
// composition that replaced the retired Manager.GetWithPersistedResponse.
func TestGetPersistedResponseWithEnrich(t *testing.T) {
	b := sessionBeadFixture("s-pr-1", "open", map[string]string{
		"__title":                   "Persisted",
		"template":                  "polecat",
		"state":                     "asleep",
		"alias":                     "pc-1",
		"agent_name":                "polecat-7",
		"provider":                  "claude",
		"work_dir":                  "/tmp/wd",
		"session_name":              "s-pr-1",
		"real_world_app_project_id": "proj-9",
	})
	store := beads.NewMemStoreFrom(1, []beads.Bead{b}, nil)
	mgr := NewManagerWithOptions(store, runtime.NewFake())

	persistedInfo, pr, err := mgr.PersistedStore().GetPersistedResponse("s-pr-1")
	if err != nil {
		t.Fatalf("GetPersistedResponse: %v", err)
	}
	info := mgr.EnrichInfo(persistedInfo)

	wantInfo, err := mgr.Get("s-pr-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !reflect.DeepEqual(info, wantInfo) {
		t.Fatalf("Info mismatch:\n got = %+v\nwant = %+v", info, wantInfo)
	}

	want := PersistedResponseFromBead(b)
	if pr.Status != want.Status {
		t.Fatalf("PersistedResponse.Status = %q, want %q", pr.Status, want.Status)
	}
	if len(pr.Metadata) != len(want.Metadata) {
		t.Fatalf("PersistedResponse.Metadata len = %d, want %d", len(pr.Metadata), len(want.Metadata))
	}
	for k, v := range want.Metadata {
		if pr.Metadata[k] != v {
			t.Fatalf("PersistedResponse.Metadata[%q] = %q, want %q", k, pr.Metadata[k], v)
		}
	}
}

// TestGetPersistedResponseNotFound asserts a missing id surfaces an error
// through the Store front door (the persisted-read half of the Get read model).
func TestGetPersistedResponseNotFound(t *testing.T) {
	store := beads.NewMemStore()
	mgr := NewManagerWithOptions(store, runtime.NewFake())
	if _, _, err := mgr.PersistedStore().GetPersistedResponse("missing"); err == nil {
		t.Fatal("GetPersistedResponse(missing): want error, got nil")
	}
}

func TestPersistedResponseFromBeadCarriesRevision(t *testing.T) {
	bead := sessionBeadFixture("s-revision", "open", map[string]string{
		"state": "asleep",
	})
	bead.Revision = 47

	got := PersistedResponseFromBead(bead)
	if got.Revision != bead.Revision {
		t.Fatalf("Revision = %d, want %d", got.Revision, bead.Revision)
	}
}

func TestPersistedResponseRevisionIsJSONInvisible(t *testing.T) {
	response := PersistedResponse{Status: "open", Revision: 47}
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(strings.ToLower(string(data)), "revision") {
		t.Fatalf("PersistedResponse JSON leaked revision: %s", data)
	}

	if err := json.Unmarshal([]byte(`{"Status":"closed","Revision":99}`), &response); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if response.Status != "closed" || response.Revision != 47 {
		t.Fatalf("unmarshaled response = %#v, want updated status with revision retained at 47", response)
	}
}
