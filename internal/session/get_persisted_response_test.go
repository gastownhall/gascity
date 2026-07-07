package session

import (
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestGetWithPersistedResponse asserts the manager can return the
// runtime-enriched Info and the persisted-response projection in a single
// fetch, so the API response path no longer needs a redundant raw store.Get
// beside mgr.Get. The Info must match mgr.Get and the projection must match
// PersistedResponseFromBead of the stored bead, with bead serialization
// confined inside the session package.
func TestGetWithPersistedResponse(t *testing.T) {
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
	mgr := NewManager(store, runtime.NewFake())

	info, pr, err := mgr.GetWithPersistedResponse("s-pr-1")
	if err != nil {
		t.Fatalf("GetWithPersistedResponse: %v", err)
	}

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

// TestGetWithPersistedResponseNotFound asserts a missing id surfaces the same
// error mgr.Get would return.
func TestGetWithPersistedResponseNotFound(t *testing.T) {
	store := beads.NewMemStore()
	mgr := NewManager(store, runtime.NewFake())
	if _, _, err := mgr.GetWithPersistedResponse("missing"); err == nil {
		t.Fatal("GetWithPersistedResponse(missing): want error, got nil")
	}
}
