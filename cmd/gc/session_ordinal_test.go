// ABOUTME: Tests for ordinal session resolution via snapshot file written
// ABOUTME: by `gc session list` and consulted by resolver (issue #2031).
package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

func TestSessionListSnapshot_RoundTrip(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	ids := []string{"fa-aaa1", "fa-aaa2", "fa-aaa3"}
	if err := writeSessionListSnapshot(cityPath, ids); err != nil {
		t.Fatalf("writeSessionListSnapshot: %v", err)
	}
	got, err := readSessionListSnapshot(cityPath)
	if err != nil {
		t.Fatalf("readSessionListSnapshot: %v", err)
	}
	if len(got) != len(ids) {
		t.Fatalf("got %d ids, want %d", len(got), len(ids))
	}
	for i, id := range ids {
		if got[i] != id {
			t.Errorf("snapshot[%d] = %q, want %q", i, got[i], id)
		}
	}
}

func TestSessionListSnapshot_OverwritesLastWriteWins(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := writeSessionListSnapshot(cityPath, []string{"old1", "old2"}); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := writeSessionListSnapshot(cityPath, []string{"new1"}); err != nil {
		t.Fatalf("second write: %v", err)
	}
	got, err := readSessionListSnapshot(cityPath)
	if err != nil {
		t.Fatalf("readSessionListSnapshot: %v", err)
	}
	if len(got) != 1 || got[0] != "new1" {
		t.Fatalf("got %v, want [new1]", got)
	}
}

func TestSessionListSnapshot_MissingReturnsNotFound(t *testing.T) {
	cityPath := t.TempDir()
	_, err := readSessionListSnapshot(cityPath)
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("readSessionListSnapshot on missing snapshot = %v, want ErrSessionNotFound", err)
	}
}

func TestResolveOrdinalFromSnapshot_Resolves(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	ids := []string{"fa-aaa1", "fa-aaa2", "fa-aaa3"}
	if err := writeSessionListSnapshot(cityPath, ids); err != nil {
		t.Fatalf("writeSessionListSnapshot: %v", err)
	}
	for i, want := range ids {
		got, err := resolveOrdinalFromSnapshot(cityPath, itoa(i))
		if err != nil {
			t.Fatalf("resolveOrdinalFromSnapshot(%d): %v", i, err)
		}
		if got != want {
			t.Errorf("ordinal %d = %q, want %q", i, got, want)
		}
	}
}

func TestResolveOrdinalFromSnapshot_OutOfRange(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := writeSessionListSnapshot(cityPath, []string{"fa-aaa1"}); err != nil {
		t.Fatalf("writeSessionListSnapshot: %v", err)
	}
	_, err := resolveOrdinalFromSnapshot(cityPath, "5")
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("out-of-range ordinal = %v, want ErrSessionNotFound", err)
	}
}

func TestResolveOrdinalFromSnapshot_RejectsNonCanonical(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := writeSessionListSnapshot(cityPath, []string{"fa-aaa1", "fa-aaa2"}); err != nil {
		t.Fatalf("writeSessionListSnapshot: %v", err)
	}
	bad := []string{"01", "+0", "-1", " 0", "0 ", "0.0", "", "fa-aaa1"}
	for _, ident := range bad {
		_, err := resolveOrdinalFromSnapshot(cityPath, ident)
		if !errors.Is(err, session.ErrSessionNotFound) {
			t.Errorf("resolveOrdinalFromSnapshot(%q) = %v, want ErrSessionNotFound", ident, err)
		}
	}
}

func TestResolveOrdinalFromSnapshot_MissingSnapshot(t *testing.T) {
	cityPath := t.TempDir()
	_, err := resolveOrdinalFromSnapshot(cityPath, "0")
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("missing snapshot = %v, want ErrSessionNotFound", err)
	}
}

// TestResolveSessionIDWithConfig_OrdinalViaSnapshot exercises the full
// resolver path: snapshot present, pure-digit identifier resolves to the
// snapshot's bead at that index.
func TestResolveSessionIDWithConfig_OrdinalViaSnapshot(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	store := beads.NewMemStore()
	first, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
	})
	second, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
	})
	third, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
	})
	if err := writeSessionListSnapshot(cityPath, []string{first.ID, second.ID, third.ID}); err != nil {
		t.Fatalf("writeSessionListSnapshot: %v", err)
	}

	cases := []struct {
		ordinal string
		want    string
	}{
		{"0", first.ID},
		{"1", second.ID},
		{"2", third.ID},
	}
	for _, tc := range cases {
		got, err := resolveSessionIDWithConfig(cityPath, nil, store, tc.ordinal)
		if err != nil {
			t.Errorf("resolveSessionIDWithConfig(%q): %v", tc.ordinal, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ordinal %q = %q, want %q", tc.ordinal, got, tc.want)
		}
	}
}

// TestResolveSessionIDWithConfig_AliasBeatsOrdinal verifies that when a
// session is aliased to a digit (e.g. "1"), the alias wins. Alias
// resolution runs before the ordinal-snapshot branch in the resolver.
func TestResolveSessionIDWithConfig_AliasBeatsOrdinal(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	store := beads.NewMemStore()
	first, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
	})
	aliased, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias": "1",
		},
	})
	other, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
	})
	// Snapshot puts `other` at ordinal 1 — if ordinal won, "1" would
	// resolve to `other`. The aliased bead must win instead.
	if err := writeSessionListSnapshot(cityPath, []string{first.ID, other.ID, aliased.ID}); err != nil {
		t.Fatalf("writeSessionListSnapshot: %v", err)
	}

	got, err := resolveSessionIDWithConfig(cityPath, nil, store, "1")
	if err != nil {
		t.Fatalf("resolveSessionIDWithConfig(\"1\"): %v", err)
	}
	if got != aliased.ID {
		t.Fatalf("got %q, want aliased %q (ordinal would have returned %q)", got, aliased.ID, other.ID)
	}
}

// TestResolveSessionIDWithConfig_OrdinalForClosedBeadResolves verifies a
// closed bead in the snapshot still resolves via its ordinal. The
// resolver doesn't filter by status — closed-session semantics are the
// caller's concern, mirroring how `gc session attach <closed-id>` behaves.
func TestResolveSessionIDWithConfig_OrdinalForClosedBeadResolves(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	store := beads.NewMemStore()
	closed, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
	})
	if err := store.Close(closed.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	open1, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
	})
	if err := writeSessionListSnapshot(cityPath, []string{closed.ID, open1.ID}); err != nil {
		t.Fatalf("writeSessionListSnapshot: %v", err)
	}

	got, err := resolveSessionIDWithConfig(cityPath, nil, store, "0")
	if err != nil {
		t.Fatalf("resolveSessionIDWithConfig(\"0\"): %v", err)
	}
	if got != closed.ID {
		t.Fatalf("got %q, want closed bead %q (snapshot drives resolution)", got, closed.ID)
	}
}

// TestSessionListSnapshot_EmptyOverwritesPrior verifies that a fresh
// `gc session list` that finds no sessions invalidates an earlier
// snapshot. Otherwise a stale ID would survive after all sessions
// closed and user-typed ordinals would resurrect ghosts.
func TestSessionListSnapshot_EmptyOverwritesPrior(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := writeSessionListSnapshot(cityPath, []string{"fa-aaa1", "fa-aaa2"}); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := writeSessionListSnapshot(cityPath, nil); err != nil {
		t.Fatalf("nil write: %v", err)
	}
	got, err := readSessionListSnapshot(cityPath)
	if err != nil {
		t.Fatalf("readSessionListSnapshot: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want empty after nil overwrite", got)
	}
}

// TestResolveSessionIDWithConfig_OrdinalForMissingBeadFalls verifies that
// if a snapshot references a bead the store no longer has, the ordinal
// branch falls through (returns ErrSessionNotFound) rather than returning
// the stale ID. Stale snapshots after `bd close` of a deleted bead must
// not produce phantom resolutions.
func TestResolveSessionIDWithConfig_OrdinalForMissingBeadFalls(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	store := beads.NewMemStore()
	if err := writeSessionListSnapshot(cityPath, []string{"fa-deadbe"}); err != nil {
		t.Fatalf("writeSessionListSnapshot: %v", err)
	}

	_, err := resolveSessionIDWithConfig(cityPath, nil, store, "0")
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("got %v, want ErrSessionNotFound", err)
	}
}
