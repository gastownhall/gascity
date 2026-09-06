//go:build integration

package beadsworkspace

// The serving arm against a real workspace.
//
// It is tagged and skips the same way every other proof in this tree that
// needs the linked beads library to actually open something: the library
// chooses the backend from the workspace's own configuration, and the only
// honest fixture is a workspace it opened itself. A substitute would prove
// the code compiles against an interface, which the untagged tests next door
// already prove, and would say nothing about whether a bead written through
// this binding lands in the workspace the binding names.

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// makeWorkspace creates the workspace a binding names and configures the id
// prefix it mints under, through the same library the provider opens it with.
// A library that cannot open a workspace here skips the test rather than
// failing it, exactly as the other real-workspace proofs in this tree do.
func makeWorkspace(t *testing.T, root, prefix string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("creating %s: %v", root, err)
	}
	// The configuration file first, because that is what provisioning means
	// here: the library reads it to learn which backend serves the workspace,
	// and a provider that finds no such file refuses rather than letting the
	// library build one from defaults. An empty object keeps every default,
	// which is the embedded backend this fixture wants.
	provisionWorkspaceConfig(t, root)
	ctx := context.Background()
	storage, err := beads.OpenNativeStorage(ctx, root, nil)
	if err != nil {
		t.Skipf("the linked beads library cannot open a workspace here: %v", err)
	}
	// issue_prefix is the workspace's own configuration key: the prefix the
	// library mints ids under, and the one this provider requires.
	if err := storage.SetConfig(ctx, "issue_prefix", prefix); err != nil {
		t.Fatalf("configuring the workspace id prefix: %v", err)
	}
	// The library validates a bead's type against its own closed set plus the
	// workspace's declared custom types, and a city's coordination beads carry
	// types that set does not name. Declaring them is part of provisioning a
	// workspace a city serves from, which is why the fixture does it here
	// rather than the provider doing it at open: the workspace's configuration
	// belongs to whoever created it.
	if err := storage.SetConfig(ctx, "types.custom", `["session","convoy","wisp","wait"]`); err != nil {
		t.Fatalf("configuring the workspace bead types: %v", err)
	}
	if err := storage.Close(); err != nil {
		t.Fatalf("closing the workspace fixture: %v", err)
	}
}

// TestOpenEngineServesTheWorkspaceTheBindingNames is the whole serving claim:
// the store handed back writes into the workspace the configuration reference
// names, and the bead is still there after the handle is closed.
func TestOpenEngineServesTheWorkspaceTheBindingNames(t *testing.T) {
	city, provider, spec := cityWithProvider(t)
	root, err := WorkspaceRoot(city, testConfigRef)
	if err != nil {
		t.Fatalf("resolving the workspace root: %v", err)
	}
	makeWorkspace(t, root, "gcg")
	classes, err := workspaceClasses()
	if err != nil {
		t.Fatalf("building the served class set: %v", err)
	}

	store, closer, err := engineOpener(t, provider).OpenEngine(spec, classes)
	if err != nil {
		t.Fatalf("opening the workspace binding: %v", err)
	}
	created, err := store.Create(beads.Bead{Title: "served from the workspace", Type: "session", Labels: []string{"gc:session"}})
	if err != nil {
		_ = closer.Close()
		t.Fatalf("writing through the workspace binding: %v", err)
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("closing the workspace binding: %v", err)
	}

	// Reopened from the directory itself rather than from the handle under
	// test: a bead that survives only in the closed handle's memory would pass
	// a read-back through the same store.
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
	if got.Title != "served from the workspace" {
		t.Errorf("bead %s title = %q, want the one written through the binding", got.ID, got.Title)
	}
}

// TestOpenEngineRefusesAWorkspaceOnAnotherIDPrefix proves the prefix
// requirement against a real workspace, not just against the comparison: a
// workspace that mints under the work store's namespace is refused before its
// store escapes.
func TestOpenEngineRefusesAWorkspaceOnAnotherIDPrefix(t *testing.T) {
	city, provider, spec := cityWithProvider(t)
	root, err := WorkspaceRoot(city, testConfigRef)
	if err != nil {
		t.Fatalf("resolving the workspace root: %v", err)
	}
	makeWorkspace(t, root, "gc")
	classes, err := workspaceClasses()
	if err != nil {
		t.Fatalf("building the served class set: %v", err)
	}

	store, closer, err := engineOpener(t, provider).OpenEngine(spec, classes)
	if !errors.Is(err, ErrInvalidWorkspaceBinding) {
		if closer != nil {
			_ = closer.Close()
		}
		t.Fatalf("OpenEngine on a foreign-prefix workspace = %v, want %v", err, ErrInvalidWorkspaceBinding)
	}
	if store != nil || closer != nil {
		t.Fatal("a refused open returned a store or a closer")
	}
}
