package main

import (
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// seedSessionBeads populates a Store with the given number of open and
// closed session beads. Open beads carry a fresh session_name and template
// so newSessionBeadSnapshot's identity indexes get exercised the same way
// as in production.
func seedSessionBeads(tb testing.TB, store beads.Store, openCount, closedCount int) {
	tb.Helper()
	for i := 0; i < openCount; i++ {
		bead, err := store.Create(beads.Bead{
			Title:  fmt.Sprintf("open session %d", i),
			Type:   session.BeadType,
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"session_name": fmt.Sprintf("agent-open-%d", i),
				"template":     fmt.Sprintf("template-open-%d", i),
			},
		})
		if err != nil {
			tb.Fatalf("seed open session bead %d: %v", i, err)
		}
		_ = bead
	}
	for i := 0; i < closedCount; i++ {
		bead, err := store.Create(beads.Bead{
			Title:  fmt.Sprintf("closed session %d", i),
			Type:   session.BeadType,
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"session_name": fmt.Sprintf("agent-closed-%d", i),
				"template":     fmt.Sprintf("template-closed-%d", i),
			},
		})
		if err != nil {
			tb.Fatalf("seed closed session bead %d: %v", i, err)
		}
		if err := store.Close(bead.ID); err != nil {
			tb.Fatalf("close session bead %d: %v", i, err)
		}
	}
}

// BenchmarkLoadSessionBeadSnapshot_LargeStore exercises the hot-path
// snapshot loader against a store dominated by closed session beads. After
// the IncludeClosed drop in loadSessionBeadSnapshot, runtime should scale
// with the open count, not the open+closed total.
func BenchmarkLoadSessionBeadSnapshot_LargeStore(b *testing.B) {
	store := beads.NewMemStore()
	seedSessionBeads(b, store, 50, 5000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		snap, err := loadSessionBeadSnapshot(store)
		if err != nil {
			b.Fatal(err)
		}
		if got := len(snap.Open()); got != 50 {
			b.Fatalf("Open()=%d, want 50", got)
		}
	}
}

// BenchmarkLoadSessionBeadSnapshot_OpenOnlyBaseline establishes a control
// for BenchmarkLoadSessionBeadSnapshot_LargeStore: same open count, no
// closed history. The two benchmarks should report comparable ns/op.
func BenchmarkLoadSessionBeadSnapshot_OpenOnlyBaseline(b *testing.B) {
	store := beads.NewMemStore()
	seedSessionBeads(b, store, 50, 0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		snap, err := loadSessionBeadSnapshot(store)
		if err != nil {
			b.Fatal(err)
		}
		if got := len(snap.Open()); got != 50 {
			b.Fatalf("Open()=%d, want 50", got)
		}
	}
}

// TestLoadSessionBeadSnapshot_IncludesTypedBeadsWithoutLabel guards against
// the regression where canonical configured_named_session beads that have
// lost their gc:session label (observed in production after crashes /
// schema migrations) become invisible to the reconciler. Such beads still
// carry issue_type=session and IsSessionBeadOrRepairable accepts them; the
// snapshot loader must surface them so the reconciler can heal their
// state=awake → state=asleep transition once the runtime is gone. Without
// this, the bead lives forever holding its alias reservation and the pool
// cannot materialize a fresh session for the same template ("alias …
// already belongs to gm-XXXX").
func TestLoadSessionBeadSnapshot_IncludesTypedBeadsWithoutLabel(t *testing.T) {
	store := beads.NewMemStore()
	// Bead with proper Type but NO labels — the production failure mode for
	// canonical configured_named_session beads after a crash.
	if _, err := store.Create(beads.Bead{
		Title:  "beads/reviewer",
		Type:   session.BeadType,
		Labels: nil,
		Metadata: map[string]string{
			"session_name":              "beads--reviewer",
			"template":                  "beads/reviewer",
			"configured_named_session":  "true",
			"configured_named_identity": "beads/reviewer",
			"state":                     "awake",
		},
	}); err != nil {
		t.Fatalf("seed labelless typed session bead: %v", err)
	}
	// Bead with the label set normally — control case to verify the loader
	// still surfaces label-only beads.
	if _, err := store.Create(beads.Bead{
		Title:  "beads/builder",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "s-pool-builder",
			"template":     "beads/builder",
		},
	}); err != nil {
		t.Fatalf("seed labeled typed session bead: %v", err)
	}

	snap, err := loadSessionBeadSnapshot(store)
	if err != nil {
		t.Fatalf("loadSessionBeadSnapshot: %v", err)
	}
	if got := len(snap.Open()); got != 2 {
		t.Fatalf("Open()=%d, want 2 (labelless + labeled session beads)", got)
	}
	if got := snap.FindSessionNameByTemplate("beads/reviewer"); got != "beads--reviewer" {
		t.Errorf("FindSessionNameByTemplate(beads/reviewer)=%q, want beads--reviewer — labelless typed bead must be visible", got)
	}
	if got := snap.FindSessionNameByTemplate("beads/builder"); got != "s-pool-builder" {
		t.Errorf("FindSessionNameByTemplate(beads/builder)=%q, want s-pool-builder", got)
	}
}

// TestLoadSessionBeadSnapshot_DeduplicatesAcrossQueries verifies a bead that
// matches BOTH the Type and Label queries is included exactly once.
func TestLoadSessionBeadSnapshot_DeduplicatesAcrossQueries(t *testing.T) {
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "dual-match",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "s-dual",
			"template":     "dual-match",
		},
	}); err != nil {
		t.Fatalf("seed dual-match bead: %v", err)
	}
	snap, err := loadSessionBeadSnapshot(store)
	if err != nil {
		t.Fatalf("loadSessionBeadSnapshot: %v", err)
	}
	if got := len(snap.Open()); got != 1 {
		t.Fatalf("Open()=%d, want 1 — bead matching both queries must dedup", got)
	}
}

func TestSessionBeadSnapshotIndexesCanonicalSingletonPoolManagedBead(t *testing.T) {
	snapshot := newSessionBeadSnapshot([]beads.Bead{{
		ID:     "refinery-session",
		Title:  "cashmaster/refinery",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "agent:cashmaster/refinery"},
		Metadata: map[string]string{
			"template":             "cashmaster/refinery",
			"agent_name":           "cashmaster/refinery",
			"session_name":         "s-canonical-refinery",
			poolManagedMetadataKey: boolMetadata(true),
		},
	}})

	if got := snapshot.FindSessionNameByTemplate("cashmaster/refinery"); got != "s-canonical-refinery" {
		t.Fatalf("FindSessionNameByTemplate(canonical singleton pool bead) = %q, want s-canonical-refinery", got)
	}
	bead, ok := snapshot.FindSessionBeadByTemplate("cashmaster/refinery")
	if !ok {
		t.Fatal("FindSessionBeadByTemplate(canonical singleton pool bead) = false")
	}
	if bead.ID != "refinery-session" {
		t.Fatalf("FindSessionBeadByTemplate ID = %q, want refinery-session", bead.ID)
	}
}
