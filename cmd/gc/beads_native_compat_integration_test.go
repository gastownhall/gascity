//go:build integration

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestManagedBeadsNativeCLICompatibility exercises the released CLI against
// the linked library. A successful init alone can hide a schema mismatch by
// silently selecting the subprocess fallback instead of native storage.
//
// The city is the gc-managed direct-server shape on purpose: it is the shape
// gc opens through the linked library. A fresh provider-owned city — the
// proxied-local default and the `--beads-transport direct` escape hatch
// alike — is bd's, and gc reads it through the bd front door by design (gate
// proxied_provider, or a preflight that cannot confirm identity on a scope gc
// never canonicalised), so no library open happens there and nothing about
// the linked schema would be exercised.
func TestManagedBeadsNativeCLICompatibility(t *testing.T) {
	// Select the installed test CLI from PATH, not a developer home override.
	for _, entry := range gcBeadsBdTestHomeEnv(t) {
		key, value, _ := strings.Cut(entry, "=")
		t.Setenv(key, value)
	}
	cityPath, _ := setupManagedBdWaitTestCity(t)
	bdPath := waitTestRealBDPath(t)

	witness, err := os.ReadFile(filepath.Join(cityPath, ".beads", ".local_version"))
	if err != nil || strings.TrimSpace(string(witness)) == "" {
		t.Fatalf("fresh managed workspace version witness = %q, err = %v", witness, err)
	}
	result, err := openStoreResultAtForCity(cityPath, cityPath)
	if err != nil {
		t.Fatalf("open managed store: %v", err)
	}
	if result.Diagnostic.Store != beads.BeadsStoreNameNativeDoltStore || !result.Diagnostic.NativeStoreEligible {
		t.Fatalf("fresh workspace must use native storage without fallback: %+v", result.Diagnostic)
	}
	store := result.Store
	t.Cleanup(func() {
		if err := closeBeadStoreHandle(store); err != nil {
			t.Errorf("close managed store: %v", err)
		}
	})
	id := parseCreatedBeadID(t, runRawBDFromDir(t, bdPath, cityPath,
		"create", "--json", "CLI to native compatibility", "-t", "task"))
	if _, err := store.Get(id); err != nil {
		t.Fatalf("native read of CLI-created bead: %v", err)
	}
	cliStore := beads.NewBdStore(cityPath, beads.ExecCommandRunner())
	snapshot, err := cliStore.Get(id)
	if err != nil || snapshot.Revision == 0 {
		t.Fatalf("CLI fallback must decode the revision token: bead=%+v err=%v", snapshot, err)
	}
	title := "CLI fallback update"
	if err := cliStore.Update(id, beads.UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("CLI fallback write: %v", err)
	}
	if got, err := store.Get(id); err != nil || got.Title != title {
		t.Fatalf("native read after CLI fallback write: bead=%+v err=%v", got, err)
	}
	if err := store.SetMetadata(id, "compatibility", "native-write"); err != nil {
		t.Fatalf("native metadata write: %v", err)
	}
	if out := runRawBDFromDir(t, bdPath, cityPath, "show", "--json", id); !strings.Contains(out, "native-write") {
		t.Fatalf("CLI did not observe native write: %s", out)
	}
	runRawBDFromDir(t, bdPath, cityPath, "update", "--json", id, "--set-metadata", "compatibility=cli-write")
	got, err := store.Get(id)
	if err != nil || got.Metadata["compatibility"] != "cli-write" {
		t.Fatalf("native read after CLI update: bead=%+v err=%v", got, err)
	}

	// Mail uses the ephemeral tables, whose schema has previously drifted
	// independently of ordinary issues.
	mail, err := store.Create(beads.Bead{Title: "native mail compatibility", Type: "message", Ephemeral: true})
	if err != nil {
		t.Fatalf("native ephemeral message create: %v", err)
	}
	if out := runRawBDFromDir(t, bdPath, cityPath, "show", "--json", mail.ID); !strings.Contains(out, mail.ID) {
		t.Fatalf("CLI did not observe native ephemeral message: %s", out)
	}
	runRawBDFromDir(t, bdPath, cityPath, "close", "--force", "--json", id)
	got, err = store.Get(id)
	if err != nil || got.Status != "closed" {
		t.Fatalf("native read after CLI close: bead=%+v err=%v", got, err)
	}
}
