//go:build integration

package main

// The serving arm of the beads workspace provider, end to end from a city's
// configuration to a bead on disk.
//
// It is tagged because the only honest fixture is a workspace the linked beads
// library opened itself: the library reads the workspace's own configuration
// to decide which backend serves it, so a substitute would prove the routing
// and nothing about where the bead went. The untagged proofs next door cover
// every arm that stops before the workspace is opened.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storebinding/beadsworkspace"
)

// provisionWorkspace creates the workspace a city's binding names and gives it
// the configuration a serving workspace needs: the reserved id prefix this
// build's bindings mint under, and the coordination bead types the library's
// own closed set does not carry.
func provisionWorkspace(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".beads"), 0o755); err != nil {
		t.Fatalf("creating %s: %v", root, err)
	}
	// The workspace's own configuration file is what makes the directory a
	// provisioned workspace; without it the library would build one from
	// defaults and this provider refuses before it can.
	if err := os.WriteFile(filepath.Join(root, ".beads", "metadata.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("writing the workspace configuration: %v", err)
	}
	ctx := context.Background()
	storage, err := beads.OpenNativeStorage(ctx, root, nil)
	if err != nil {
		t.Skipf("the linked beads library cannot open a workspace here: %v", err)
	}
	if err := storage.SetConfig(ctx, "issue_prefix", "gcg"); err != nil {
		t.Fatalf("configuring the workspace id prefix: %v", err)
	}
	if err := storage.SetConfig(ctx, "types.custom", `["session","convoy","wisp","wait"]`); err != nil {
		t.Fatalf("configuring the workspace bead types: %v", err)
	}
	if err := storage.Close(); err != nil {
		t.Fatalf("closing the workspace fixture: %v", err)
	}
}

// TestStorageGateServesACityFromItsBeadsWorkspace is the whole claim this
// provider exists for: a city that names a workspace boots, its infrastructure
// classes route to that workspace, and a bead written through a routed class
// store is in the workspace afterwards.
func TestStorageGateServesACityFromItsBeadsWorkspace(t *testing.T) {
	// Deliberately not standing in the city: the gate must resolve the binding
	// from the city it was handed, which is what a supervisor hosting many
	// cities from one process depends on.
	t.Chdir(t.TempDir())
	cityPath := t.TempDir()
	stubInfraMigrationSource(t)
	root, err := beadsworkspace.WorkspaceRoot(cityPath, "infra")
	if err != nil {
		t.Fatalf("resolving the workspace root: %v", err)
	}
	provisionWorkspace(t, root)

	var stderr bytes.Buffer
	routes, err := storageBootGate(cityPath, workspaceSplitConfig("infra"), "gc start", nil, &stderr)
	if err != nil {
		t.Fatalf("a clean city with a provisioned workspace refused to serve: %v\nstderr: %s", err, stderr.String())
	}
	if routes == nil {
		t.Fatal("the gate served but returned no routes")
	}
	store, ok := routes.stores[coordclass.ClassSessions]
	if !ok {
		_ = routes.close()
		t.Fatal("the routes carry no store for the session class")
	}
	created, err := store.Create(beads.Bead{Title: "born on the workspace", Type: "session", Labels: []string{"gc:session"}})
	if err != nil {
		_ = routes.close()
		t.Fatalf("writing through the workspace routes: %v", err)
	}
	if err := routes.close(); err != nil {
		t.Fatalf("closing the served routes: %v", err)
	}

	// Read back from the workspace directly. A read through the routes would
	// pass even if the write had gone somewhere else entirely.
	reopened, err := beads.OpenNativeDoltStoreAt(context.Background(), root, nil)
	if err != nil {
		t.Fatalf("reopening the workspace at %s: %v", root, err)
	}
	t.Cleanup(func() {
		if err := reopened.CloseStore(); err != nil {
			t.Errorf("closing the reopened workspace: %v", err)
		}
	})
	got, err := reopened.Get(created.ID)
	if err != nil {
		t.Fatalf("reading %s back from the workspace: %v", created.ID, err)
	}
	if got.Title != "born on the workspace" {
		t.Errorf("bead %s title = %q, want the one written through the routed class store", got.ID, got.Title)
	}
}
