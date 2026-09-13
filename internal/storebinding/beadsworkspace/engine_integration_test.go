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
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storebinding"
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

// openFencedEngine opens the binding for exactly the given classes and returns
// the store, closed on cleanup. The classes are a caller-chosen SUBSET of what
// the provider serves, because the fence is derived from the assignment and the
// full served set — which includes work — is deliberately unfenced.
func openFencedEngine(t *testing.T, classes ...coordclass.Class) beads.Store {
	t.Helper()
	city, provider, spec := cityWithProvider(t)
	root, err := WorkspaceRoot(city, testConfigRef)
	if err != nil {
		t.Fatalf("resolving the workspace root: %v", err)
	}
	makeWorkspace(t, root, "gcg")
	assigned, err := storebinding.NewClassSet(classes...)
	if err != nil {
		t.Fatalf("NewClassSet(%v): %v", classes, err)
	}
	store, closer, err := engineOpener(t, provider).OpenEngine(spec, assigned)
	if err != nil {
		t.Fatalf("opening the workspace binding for %v: %v", classes, err)
	}
	t.Cleanup(func() {
		if err := closer.Close(); err != nil {
			t.Errorf("closing the workspace binding: %v", err)
		}
	})
	return store
}

// TestOpenEngineFencesAnInfrastructureBindingToEveryNamespaceItHolds is the
// wiring half of invariant 16 for this provider.
//
// The rows over in storebinding pin what EngineReservedPrefixes returns, and
// the conformance suite in internal/beads pins that a fenced NativeDoltStore
// enforces it. Neither can see whether the computed set ever reaches the store:
// drop the option from OpenEngine and both stay green while this binding
// accepts any pinned id at all. Only an id pinned through an opened engine
// answers that, which is why this row is here and tagged.
func TestOpenEngineFencesAnInfrastructureBindingToEveryNamespaceItHolds(t *testing.T) {
	store := openFencedEngine(t, coordclass.ClassGraph, coordclass.ClassNudges)

	if _, err := store.Create(beads.Bead{ID: "ga-1", Title: "a work id pinned into an infrastructure binding", Type: "task"}); !errors.Is(err, beads.ErrPinnedIDOutsideNamespace) {
		t.Errorf("Create(%q) = %v, want ErrPinnedIDOutsideNamespace; the computed namespace set never reached the store, and this binding's id claim is decorative", "ga-1", err)
	}

	// The second half of the wiring, and the reason this is not one assertion:
	// a binding holds more than the class it mints under. Passing only the
	// first computed prefix — or only the graph one this workspace mints with —
	// would refuse the nudge records the binding exists to hold.
	for _, id := range []string{"gcg-pinned", "gcn-pinned", "gcnq-pinned"} {
		if _, err := store.Create(beads.Bead{ID: id, Title: "held by this binding", Type: "task"}); err != nil {
			t.Errorf("Create(%q) was refused: %v; the store was fenced to less than the binding holds", id, err)
		}
	}
}

// TestOpenEngineLeavesAWorkServingBindingUnfenced is the must-be-silent
// counterpart. A binding that serves work is unfenceable — work beads carry the
// operator's configured rig or HQ prefix — so OpenEngine must pass that through
// as an unfenced store rather than as an empty-but-present namespace set, which
// would refuse everything.
func TestOpenEngineLeavesAWorkServingBindingUnfenced(t *testing.T) {
	store := openFencedEngine(t, coordclass.ClassWork, coordclass.ClassGraph)
	if _, err := store.Create(beads.Bead{ID: "someRig-1", Title: "a work bead under an operator-configured prefix", Type: "task"}); err != nil {
		t.Errorf("Create(%q) was refused: %v; a binding serving work claims no namespace and must hold whatever the operator configured", "someRig-1", err)
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
