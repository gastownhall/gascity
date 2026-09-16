package beads

import (
	"encoding/json"
	"testing"
)

// The cache's three field-by-field comparators each enumerate Bead's fields by
// hand, so a field added to the struct is invisible to all of them until it is
// named. That is not cosmetic: beadChanged gates the bead.updated cache refresh
// (caching_store_events.go), and the reconcile path suppresses the downstream
// notification when it reports no change (caching_store_reconcile.go). A gate
// whose await_type/await_id transitions, or a `bd update --notes`, would wake
// nothing downstream — no dashboard refresh, no SSE, no event-gated order —
// until an unrelated field happened to change.
//
// These call the pure comparators directly rather than driving a store, so a
// missing field fails on the comparator itself instead of on whatever the
// surrounding machinery happens to do with its answer.

func TestBeadChangedDetectsPlainColumnTransitions(t *testing.T) {
	base := Bead{ID: "gc-1", Title: "t", Status: "open", Type: "gate"}
	for _, tc := range []struct {
		field  string
		mutate func(*Bead)
	}{
		{"AwaitType", func(b *Bead) { b.AwaitType = "gh:pr" }},
		{"AwaitID", func(b *Bead) { b.AwaitID = "4912" }},
		{"CreatedBy", func(b *Bead) { b.CreatedBy = "seeder" }},
		{"Owner", func(b *Bead) { b.Owner = "owner@example.com" }},
		{"Notes", func(b *Bead) { b.Notes = "a note" }},
	} {
		t.Run(tc.field, func(t *testing.T) {
			fresh := base
			tc.mutate(&fresh)
			for _, skipLabels := range []bool{false, true} {
				if !beadChanged(base, fresh, skipLabels) {
					t.Errorf("beadChanged(skipLabels=%v) = false for a %s transition; the cache would not refresh and the reconcile path would suppress the notification", skipLabels, tc.field)
				}
			}
		})
	}
}

func TestMergeCacheEventPatchAppliesPlainColumns(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want string
		get  func(Bead) string
		set  func(*Bead, string)
	}{
		{"await_type", "gh:pr", func(b Bead) string { return b.AwaitType }, func(b *Bead, v string) { b.AwaitType = v }},
		{"await_id", "4912", func(b Bead) string { return b.AwaitID }, func(b *Bead, v string) { b.AwaitID = v }},
		{"created_by", "seeder", func(b Bead) string { return b.CreatedBy }, func(b *Bead, v string) { b.CreatedBy = v }},
		{"owner", "owner@example.com", func(b Bead) string { return b.Owner }, func(b *Bead, v string) { b.Owner = v }},
		{"notes", "a note", func(b Bead) string { return b.Notes }, func(b *Bead, v string) { b.Notes = v }},
	} {
		t.Run(tc.key, func(t *testing.T) {
			base := Bead{ID: "gc-1", Title: "t", Status: "open", Type: "gate"}
			patch := base
			tc.set(&patch, tc.want)
			fields := map[string]json.RawMessage{tc.key: json.RawMessage(`"` + tc.want + `"`)}
			merged := mergeCacheEventPatch(base, patch, fields)
			if got := tc.get(merged); got != tc.want {
				t.Errorf("mergeCacheEventPatch dropped %q: got %q, want %q", tc.key, got, tc.want)
			}
		})
	}
}

func TestCacheEventConflictsCurrentSeesPlainColumns(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want string
		set  func(*Bead, string)
	}{
		{"await_type", "gh:pr", func(b *Bead, v string) { b.AwaitType = v }},
		{"await_id", "4912", func(b *Bead, v string) { b.AwaitID = v }},
		{"created_by", "seeder", func(b *Bead, v string) { b.CreatedBy = v }},
		{"owner", "owner@example.com", func(b *Bead, v string) { b.Owner = v }},
		{"notes", "a note", func(b *Bead, v string) { b.Notes = v }},
	} {
		t.Run(tc.key, func(t *testing.T) {
			current := Bead{ID: "gc-1", Title: "t", Status: "open", Type: "gate"}
			patch := current
			tc.set(&patch, tc.want)
			fields := map[string]json.RawMessage{tc.key: json.RawMessage(`"` + tc.want + `"`)}
			if !cacheEventConflictsCurrent(current, patch, fields) {
				t.Errorf("cacheEventConflictsCurrent = false for a %q divergence; a stale cached value would be treated as already matching the event", tc.key)
			}
		})
	}
}
