//go:build gascity_native_beads

package beads

import (
	"testing"
	"time"
)

func TestDoltliteAuthoritativeSnapshotUsesIssuesFirstGetAuthority(t *testing.T) {
	now := time.Date(2026, 8, 11, 9, 0, 0, 0, time.UTC)
	store := newDoltliteStoreWithRows(t,
		[]testDoltliteIssue{
			{ID: "gc-collision", Status: "open", IssueType: "task", CreatedAt: now, UpdatedAt: now, Metadata: map[string]string{"source": "issue"}},
			{ID: "gc-durable", Status: "closed", IssueType: "task", CreatedAt: now.Add(time.Second), UpdatedAt: now.Add(time.Second)},
		},
		[]testDoltliteIssue{
			{ID: "gc-collision", Status: "closed", IssueType: "task", CreatedAt: now, UpdatedAt: now, Metadata: map[string]string{"source": "wisp"}, Ephemeral: true},
			{ID: "gc-wisp", Status: "closed", IssueType: "task", CreatedAt: now.Add(2 * time.Second), UpdatedAt: now.Add(2 * time.Second), Ephemeral: true},
		},
	)
	rows, err := store.AuthoritativeSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("snapshot = %#v, want three unique IDs", rows)
	}
	if rows[0].ID != "gc-collision" || rows[0].Status != "open" || rows[0].Metadata["source"] != "issue" {
		t.Fatalf("collision authority = %#v, want issues-table point Get", rows[0])
	}
}
