package runproj

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// TestBuildRunDetailResolvesScopeFromRootStoreRef proves the detail path applies
// the same gc.root_store_ref scope fallback that summary uses. A run root that
// carries only gc.root_store_ref (no explicit gc.scope_kind/gc.scope_ref pair)
// lists in /runs/summary via fromRootMetadataScope, and must also open in
// /runs/{id}/detail instead of failing with invalid_snapshot. Regression for the
// summary/detail scope-fallback divergence.
func TestBuildRunDetailResolvesScopeFromRootStoreRef(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "beads_fixture.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var beadList []beads.Bead
	if err := json.Unmarshal(raw, &beadList); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}

	// Strip the explicit scope pair from the run root, leaving only
	// gc.root_store_ref=rig:gascity-packs — the scopeless shape that summary
	// accepts but detail previously rejected with 422 invalid_snapshot.
	found := false
	for i := range beadList {
		if beadList[i].ID == "dt-adopt1" {
			delete(beadList[i].Metadata, beadmeta.ScopeKindMetadataKey)
			delete(beadList[i].Metadata, beadmeta.ScopeRefMetadataKey)
			found = true
		}
	}
	if !found {
		t.Fatal("fixture missing dt-adopt1 root; test needs updating")
	}

	detail, err := BuildRunDetail(beadList, "dt-adopt1", 1, 100)
	if err != nil {
		t.Fatalf("BuildRunDetail on root_store_ref-only root: %v", err)
	}
	if detail.ScopeKind != "rig" || detail.ScopeRef != "gascity-packs" {
		t.Errorf("scope not recovered from root_store_ref: got kind=%q ref=%q, want rig/gascity-packs",
			detail.ScopeKind, detail.ScopeRef)
	}
}

// TestFromSnapshotScopeFallback unit-tests the scope resolver directly: the
// explicit pair wins (root_store_ref ignored), the store ref recovers scope when
// the pair is absent, and neither an empty nor a malformed store ref resolves.
func TestFromSnapshotScopeFallback(t *testing.T) {
	if k, r, ok := fromSnapshotScope(runSnapshot{scopeKind: "city", scopeRef: "main", rootStoreRef: "rig:other"}); !ok || k != "city" || r != "main" {
		t.Errorf("explicit pair: got (%q,%q,%v), want (city,main,true)", k, r, ok)
	}
	if k, r, ok := fromSnapshotScope(runSnapshot{rootStoreRef: "rig:gascity-packs"}); !ok || k != "rig" || r != "gascity-packs" {
		t.Errorf("store-ref fallback: got (%q,%q,%v), want (rig,gascity-packs,true)", k, r, ok)
	}
	if _, _, ok := fromSnapshotScope(runSnapshot{}); ok {
		t.Error("empty snapshot: got ok=true, want false")
	}
	if _, _, ok := fromSnapshotScope(runSnapshot{rootStoreRef: "not-a-store-ref"}); ok {
		t.Error("malformed store ref: got ok=true, want false")
	}
}
