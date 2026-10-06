package beads

import (
	"context"
	"strings"
	"testing"
)

func TestSQLiteStoreApplyGraphPlanRejectsInvalidPlanBeforeWriting(t *testing.T) {
	tests := []struct {
		name string
		plan *GraphApplyPlan
	}{
		{
			name: "nil plan",
			plan: nil,
		},
		{
			name: "empty plan",
			plan: &GraphApplyPlan{},
		},
		{
			name: "empty node title",
			plan: &GraphApplyPlan{Nodes: []GraphApplyNode{{Key: "root"}}},
		},
		{
			name: "unknown parent key",
			plan: &GraphApplyPlan{Nodes: []GraphApplyNode{{Key: "root", Title: "root", ParentKey: "missing"}}},
		},
		{
			name: "unknown metadata ref",
			plan: &GraphApplyPlan{Nodes: []GraphApplyNode{{
				Key:          "root",
				Title:        "root",
				MetadataRefs: map[string]string{"gc.root_bead_id": "missing"},
			}}},
		},
		{
			name: "unknown edge endpoint",
			plan: &GraphApplyPlan{
				Nodes: []GraphApplyNode{{Key: "root", Title: "root"}},
				Edges: []GraphApplyEdge{{FromKey: "root", ToKey: "missing"}},
			},
		},
		{
			name: "missing edge endpoint",
			plan: &GraphApplyPlan{
				Nodes: []GraphApplyNode{{Key: "root", Title: "root"}},
				Edges: []GraphApplyEdge{{FromKey: "root"}},
			},
		},
		{
			name: "invalid edge type",
			plan: &GraphApplyPlan{
				Nodes: []GraphApplyNode{{Key: "root", Title: "root"}},
				Edges: []GraphApplyEdge{{FromKey: "root", ToID: "external-1", Type: strings.Repeat("x", 51)}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newSQLiteGraphApplyStore(t, t.TempDir())
			if _, err := store.ApplyGraphPlan(context.Background(), tt.plan); err == nil {
				t.Fatal("ApplyGraphPlan error = nil, want preflight rejection")
			}
			assertSQLiteGraphApplyRowCounts(t, store, 0, 0)
		})
	}
}

func TestSQLiteStoreApplyGraphPlanPreservesOpaqueEdgeMetadataAtomically(t *testing.T) {
	store := newSQLiteGraphApplyStore(t, t.TempDir())
	result, err := store.ApplyGraphPlan(context.Background(), &GraphApplyPlan{
		Nodes: []GraphApplyNode{
			{Key: "root", Title: "root"},
			{Key: "child", Title: "child"},
		},
		Edges: []GraphApplyEdge{{
			FromKey:  "child",
			ToKey:    "root",
			Type:     "tracks",
			Metadata: `{"recipe":"edge payload"}`,
		}},
	})
	if err != nil {
		t.Fatalf("ApplyGraphPlan: %v", err)
	}
	metadata, ok, err := store.DepMetadata(result.IDs["child"], result.IDs["root"])
	if err != nil {
		t.Fatalf("DepMetadata: %v", err)
	}
	if !ok || metadata != `{"recipe":"edge payload"}` {
		t.Fatalf("DepMetadata = (%q, %v), want opaque payload", metadata, ok)
	}

	_, err = store.ApplyGraphPlan(context.Background(), &GraphApplyPlan{
		Nodes: []GraphApplyNode{
			{Key: "parent", Title: "parent"},
			{Key: "child", Title: "child", ParentKey: "parent"},
		},
		Edges: []GraphApplyEdge{
			{FromKey: "child", ToKey: "parent", Type: "tracks", Metadata: "must roll back"},
			{FromKey: "child", ToKey: "parent", Type: "blocks"},
		},
	})
	if err == nil {
		t.Fatal("ApplyGraphPlan duplicate parent edge error = nil, want rollback")
	}
	var metadataRows int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM kv WHERE key LIKE 'gascity.graph-edge-metadata.v1/%'`).Scan(&metadataRows); err != nil {
		t.Fatalf("count edge metadata rows: %v", err)
	}
	if metadataRows != 1 {
		t.Fatalf("edge metadata rows after rollback = %d, want only committed edge", metadataRows)
	}
}

func TestSQLiteStoreDeletingBeadRemovesGraphEdgeMetadataSidecars(t *testing.T) {
	tests := []struct {
		name   string
		delete func(*SQLiteStore, Bead) error
	}{
		{
			name: "Delete",
			delete: func(store *SQLiteStore, child Bead) error {
				return store.Delete(child.ID)
			},
		},
		{
			name: "DeleteIfMatch",
			delete: func(store *SQLiteStore, child Bead) error {
				return store.DeleteIfMatch(child.ID, child.Revision)
			},
		},
		{
			name: "DeleteBatch",
			delete: func(store *SQLiteStore, child Bead) error {
				return store.DeleteBatch([]string{child.ID})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newSQLiteGraphApplyStore(t, t.TempDir())
			result, err := store.ApplyGraphPlan(context.Background(), &GraphApplyPlan{
				Nodes: []GraphApplyNode{
					{Key: "root", Title: "root"},
					{Key: "child", Title: "child"},
					{Key: "peer", Title: "peer"},
				},
				Edges: []GraphApplyEdge{
					{FromKey: "child", ToKey: "root", Type: "tracks", Metadata: `{"direction":"outgoing"}`},
					{FromKey: "peer", ToKey: "child", Type: "tracks", Metadata: `{"direction":"incoming"}`},
				},
			})
			if err != nil {
				t.Fatalf("ApplyGraphPlan: %v", err)
			}
			child, err := store.Get(result.IDs["child"])
			if err != nil {
				t.Fatalf("Get child: %v", err)
			}
			if got := sqliteGraphEdgeMetadataRowCount(t, store); got != 2 {
				t.Fatalf("edge metadata rows before delete = %d, want 2", got)
			}

			if err := tt.delete(store, child); err != nil {
				t.Fatalf("delete child: %v", err)
			}
			if got := sqliteGraphEdgeMetadataRowCount(t, store); got != 0 {
				t.Fatalf("edge metadata rows after delete = %d, want 0", got)
			}
		})
	}
}

func TestSQLiteGraphEdgeMetadataDeletionMatchesEncodedIDsExactly(t *testing.T) {
	store := newSQLiteGraphApplyStore(t, t.TempDir())
	// The target's URL-safe base64 encoding ends in "_". SQL LIKE would treat
	// that character as a wildcard and accidentally match the distinct ID
	// below, whose encoding differs only in that position.
	targetID := "  ?"
	survivorID := "  !"
	if got, want := sqliteGraphEdgeMetadataPart(targetID), "ICA_"; got != want {
		t.Fatalf("target encoding = %q, want %q", got, want)
	}
	if got, want := sqliteGraphEdgeMetadataPart(survivorID), "ICAh"; got != want {
		t.Fatalf("survivor encoding = %q, want %q", got, want)
	}

	tx, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck
	for _, id := range []string{targetID, survivorID} {
		if _, err := tx.Exec(`INSERT INTO kv(key,value) VALUES(?,?)`, sqliteGraphEdgeMetadataKey(id, "dependency", "tracks"), id); err != nil {
			t.Fatalf("insert sidecar for %q: %v", id, err)
		}
	}
	if err := store.clearGraphEdgeMetadataForBeadsTx(context.Background(), tx, []string{targetID}); err != nil {
		t.Fatalf("clear target metadata: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var survivor string
	if err := store.db.QueryRow(`SELECT value FROM kv WHERE key=?`, sqliteGraphEdgeMetadataKey(survivorID, "dependency", "tracks")).Scan(&survivor); err != nil {
		t.Fatalf("read unrelated sidecar after target delete: %v", err)
	}
	if survivor != survivorID {
		t.Fatalf("unrelated sidecar = %q, want %q", survivor, survivorID)
	}
}

func sqliteGraphEdgeMetadataRowCount(t *testing.T, store *SQLiteStore) int {
	t.Helper()
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM kv WHERE key LIKE 'gascity.graph-edge-metadata.v1/%'`).Scan(&count); err != nil {
		t.Fatalf("count Graph edge metadata rows: %v", err)
	}
	return count
}

func TestSQLiteStoreApplyGraphPlanMatchesGraphSemantics(t *testing.T) {
	store := newSQLiteGraphApplyStore(t, t.TempDir())

	result, err := store.ApplyGraphPlan(context.Background(), &GraphApplyPlan{
		Nodes: []GraphApplyNode{
			{Key: "root", Title: "root"},
			{Key: "blocker", Title: "blocker"},
			{
				Key:               "child",
				Title:             "child",
				ParentKey:         "root",
				Assignee:          "worker",
				AssignAfterCreate: true,
				MetadataRefs:      map[string]string{"gc.root_bead_id": "root"},
			},
		},
		Edges: []GraphApplyEdge{{FromKey: "child", ToKey: "blocker", Type: "blocks"}},
	})
	if err != nil {
		t.Fatalf("ApplyGraphPlan: %v", err)
	}

	root, err := store.Get(result.IDs["root"])
	if err != nil {
		t.Fatalf("Get root: %v", err)
	}
	if root.Priority == nil || *root.Priority != 2 {
		t.Fatalf("root Priority = %v, want default 2", root.Priority)
	}
	child, err := store.Get(result.IDs["child"])
	if err != nil {
		t.Fatalf("Get child: %v", err)
	}
	if child.Assignee != "worker" {
		t.Fatalf("child Assignee = %q, want worker", child.Assignee)
	}
	if child.ParentID != root.ID {
		t.Fatalf("child ParentID = %q, want %q", child.ParentID, root.ID)
	}
	if child.Metadata["gc.root_bead_id"] != root.ID {
		t.Fatalf("child metadata refs = %#v, want root id %q", child.Metadata, root.ID)
	}
	deps, err := store.DepList(child.ID, "down")
	if err != nil {
		t.Fatalf("DepList child: %v", err)
	}
	assertSQLiteGraphApplyDep(t, deps, child.ID, result.IDs["blocker"], "blocks")
	assertSQLiteGraphApplyDep(t, deps, child.ID, root.ID, "parent-child")
}

func TestSQLiteStoreApplyGraphPlanRemapsOnlySymbolicReferences(t *testing.T) {
	dir := t.TempDir()
	first := newSQLiteGraphApplyStore(t, dir, WithSQLiteStoreIDPrefix(sqliteGraphPrefix))
	second := newSQLiteGraphApplyStore(t, dir, WithSQLiteStoreIDPrefix(sqliteGraphPrefix))
	if _, err := second.Create(Bead{Title: "existing"}); err != nil {
		t.Fatalf("Create existing: %v", err)
	}

	result, err := first.ApplyGraphPlan(context.Background(), &GraphApplyPlan{
		Nodes: []GraphApplyNode{
			{Key: "root", Title: "root"},
			{
				Key:      "literal",
				Title:    "literal",
				ParentID: "gcg-1",
				Metadata: map[string]string{"literal": "gcg-1"},
			},
			{
				Key:          "symbolic",
				Title:        "symbolic",
				ParentKey:    "root",
				MetadataRefs: map[string]string{"symbolic": "root"},
			},
		},
	})
	if err != nil {
		t.Fatalf("ApplyGraphPlan: %v", err)
	}
	root, err := first.Get(result.IDs["root"])
	if err != nil {
		t.Fatalf("Get root: %v", err)
	}
	literal, err := first.Get(result.IDs["literal"])
	if err != nil {
		t.Fatalf("Get literal: %v", err)
	}
	if literal.ParentID != "gcg-1" || literal.Metadata["literal"] != "gcg-1" {
		t.Fatalf("literal references remapped: parent=%q metadata=%#v", literal.ParentID, literal.Metadata)
	}
	symbolic, err := first.Get(result.IDs["symbolic"])
	if err != nil {
		t.Fatalf("Get symbolic: %v", err)
	}
	if symbolic.ParentID != root.ID || symbolic.Metadata["symbolic"] != root.ID {
		t.Fatalf("symbolic references = parent %q metadata %#v, want root %q", symbolic.ParentID, symbolic.Metadata, root.ID)
	}
}

func newSQLiteGraphApplyStore(t *testing.T, dir string, opts ...SQLiteStoreOption) *SQLiteStore {
	t.Helper()
	opened, err := OpenSQLiteStore(dir, opts...)
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	store := opened.(*SQLiteStore)
	t.Cleanup(func() { _ = store.CloseStore() })
	return store
}

func assertSQLiteGraphApplyRowCounts(t *testing.T, store *SQLiteStore, wantBeads, wantDeps int) {
	t.Helper()
	var beadsCount, depsCount int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM beads`).Scan(&beadsCount); err != nil {
		t.Fatalf("count beads: %v", err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM deps`).Scan(&depsCount); err != nil {
		t.Fatalf("count deps: %v", err)
	}
	if beadsCount != wantBeads || depsCount != wantDeps {
		t.Fatalf("row counts beads=%d deps=%d, want beads=%d deps=%d", beadsCount, depsCount, wantBeads, wantDeps)
	}
}

func assertSQLiteGraphApplyDep(t *testing.T, deps []Dep, issueID, dependsOnID, depType string) {
	t.Helper()
	for _, dep := range deps {
		if dep.IssueID == issueID && dep.DependsOnID == dependsOnID && dep.Type == depType {
			return
		}
	}
	t.Fatalf("dependencies = %#v, missing %s -> %s (%s)", deps, issueID, dependsOnID, depType)
}
