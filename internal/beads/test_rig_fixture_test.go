//go:build cgo && libsqlite3

package beads

import (
	"os"
	"testing"
)

const testRigFixturePath = "/data/projects/test-rig"

func TestFixtureRigDoltliteDatabase(t *testing.T) {
	if _, err := os.Stat(testRigFixturePath); err != nil {
		t.Skipf("test rig fixture unavailable: %v", err)
	}
	backing := NewBdStore(testRigFixturePath, func(string, string, ...string) ([]byte, error) {
		t.Fatal("fixture rig doltlite read test must not call bd")
		return nil, nil
	})
	store, err := NewDoltliteReadStore(testRigFixturePath, backing)
	if err != nil {
		t.Fatalf("NewDoltliteReadStore(test-rig): %v", err)
	}
	defer store.CloseStore() //nolint:errcheck

	rows, err := store.List(ListQuery{
		Label:      "gc:test",
		SkipParent: true,
	})
	if err != nil {
		t.Fatalf("List(gc:test): %v", err)
	}
	var fixture *Bead
	for i := range rows {
		if rows[i].Title == "T3Bridge nudge fixture" {
			fixture = &rows[i]
			break
		}
	}
	if fixture == nil {
		t.Fatalf("gc:test rows missing T3Bridge nudge fixture: %#v", rows)
	}
	if fixture.Type != "task" {
		t.Fatalf("fixture bead = %#v", fixture)
	}
}
