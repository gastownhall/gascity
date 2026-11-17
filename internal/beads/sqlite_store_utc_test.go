package beads_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// stampedTimes returns the created_at and updated_at text a reader of the
// bead's JSON wire form sees.
func stampedTimes(t *testing.T, b beads.Bead) (createdAt, updatedAt string) {
	t.Helper()
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("Marshal(%s): %v", b.ID, err)
	}
	var wire struct {
		CreatedAt string `json:"created_at"`
		UpdatedAt string `json:"updated_at"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("Unmarshal(%s): %v", b.ID, err)
	}
	return wire.CreatedAt, wire.UpdatedAt
}

// requireUTCStamp fails unless stamp is RFC3339 in UTC (a "Z" suffix, never a
// numeric offset) and names an instant inside [lo, hi].
func requireUTCStamp(t *testing.T, what, stamp string, lo, hi time.Time) {
	t.Helper()
	if !strings.HasSuffix(stamp, "Z") {
		t.Errorf("%s = %q, want a UTC stamp ending in Z", what, stamp)
		return
	}
	got, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		t.Errorf("%s = %q: %v", what, stamp, err)
		return
	}
	if got.Before(lo) || got.After(hi) {
		t.Errorf("%s = %s, want an instant in [%s, %s]", what, stamp, lo.UTC().Format(time.RFC3339Nano), hi.UTC().Format(time.RFC3339Nano))
	}
}

// TestSQLiteStoreStampsUTCUnderEveryHostZone pins that created_at and
// updated_at are written and printed in UTC ("...Z"), the shape the Dolt side
// writes, whatever zone the host runs in. A half-hour zone is included so a
// whole-hour-only rendering cannot pass.
func TestSQLiteStoreStampsUTCUnderEveryHostZone(t *testing.T) {
	zones := []struct {
		name string
		loc  *time.Location
	}{
		{"America/Los_Angeles", time.FixedZone("PST", -8*60*60)},
		{"Asia/Kolkata", time.FixedZone("IST", 5*60*60+30*60)},
	}
	strPtr := func(s string) *string { return &s }
	writes := []struct {
		name  string
		write func(t *testing.T, s beads.Store, id string)
	}{
		{"Update", func(t *testing.T, s beads.Store, id string) {
			if err := s.Update(id, beads.UpdateOpts{Title: strPtr("renamed")}); err != nil {
				t.Fatalf("Update: %v", err)
			}
		}},
		{"SetMetadata", func(t *testing.T, s beads.Store, id string) {
			if err := s.SetMetadata(id, "k", "v"); err != nil {
				t.Fatalf("SetMetadata: %v", err)
			}
		}},
		{"Close", func(t *testing.T, s beads.Store, id string) {
			if err := s.Close(id); err != nil {
				t.Fatalf("Close: %v", err)
			}
		}},
		{"Claim", func(t *testing.T, s beads.Store, id string) {
			claimer, ok := s.(interface {
				Claim(id, assignee string) (beads.Bead, bool, error)
			})
			if !ok {
				t.Fatalf("%T does not implement Claim", s)
			}
			if _, claimed, err := claimer.Claim(id, "alice"); err != nil || !claimed {
				t.Fatalf("Claim = claimed %v, err %v", claimed, err)
			}
		}},
	}
	origLocal := time.Local
	t.Cleanup(func() { time.Local = origLocal })
	for _, zone := range zones {
		t.Run(zone.name, func(t *testing.T) {
			time.Local = zone.loc
			store, err := beads.OpenSQLiteStore(t.TempDir())
			if err != nil {
				t.Fatalf("OpenSQLiteStore: %v", err)
			}
			beforeCreate := time.Now().Add(-time.Second)
			created, err := store.Create(beads.Bead{Title: "stamped", Type: "task"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			afterCreate := time.Now().Add(time.Second)
			createdAt, updatedAt := stampedTimes(t, created)
			requireUTCStamp(t, "Create returned created_at", createdAt, beforeCreate, afterCreate)
			requireUTCStamp(t, "Create returned updated_at", updatedAt, beforeCreate, afterCreate)
			got, err := store.Get(created.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			createdAt, updatedAt = stampedTimes(t, got)
			requireUTCStamp(t, "Get created_at", createdAt, beforeCreate, afterCreate)
			requireUTCStamp(t, "Get updated_at", updatedAt, beforeCreate, afterCreate)

			for _, w := range writes {
				t.Run(w.name, func(t *testing.T) {
					row, err := store.Create(beads.Bead{Title: w.name, Type: "task"})
					if err != nil {
						t.Fatalf("Create: %v", err)
					}
					before := time.Now().Add(-time.Second)
					w.write(t, store, row.ID)
					after := time.Now().Add(time.Second)
					got, err := store.Get(row.ID)
					if err != nil {
						t.Fatalf("Get: %v", err)
					}
					_, updatedAt := stampedTimes(t, got)
					requireUTCStamp(t, w.name+" updated_at", updatedAt, before, after)
				})
			}
		})
	}
}

// TestSQLiteStoreReadsStoredOffsetStampsAsUTC pins that a row already stored
// with an offset (what the store wrote before it stamped UTC) reads back as
// the same instant in UTC, without the row being rewritten: through Get and
// through a list read, which share one decoder.
func TestSQLiteStoreReadsStoredOffsetStampsAsUTC(t *testing.T) {
	store, err := beads.OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	pst := time.FixedZone("PST", -8*60*60)
	stamp := time.Date(2026, 11, 3, 11, 30, 41, 868281726, pst)
	created, err := store.Create(beads.Bead{Title: "legacy", Type: "task", CreatedAt: stamp, UpdatedAt: stamp})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	const want = "2026-11-03T19:30:41.868281726Z"

	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	listed, err := store.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("List returned %d beads, want 1", len(listed))
	}
	for name, b := range map[string]beads.Bead{"Get": got, "List": listed[0]} {
		createdAt, updatedAt := stampedTimes(t, b)
		if createdAt != want || updatedAt != want {
			t.Errorf("%s created_at/updated_at = %q / %q, want %q for both", name, createdAt, updatedAt, want)
		}
	}
}
