package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

// TestProviderOwnedInitReconcilesProjectIdentity: once a provider-owned
// `bd init` succeeds, the scope's L1 identity (.beads/identity.toml) must
// agree with the project_id bd just persisted in metadata.json, without
// waiting for some later pass to reconcile them.
func TestProviderOwnedInitReconcilesProjectIdentity(t *testing.T) {
	const projectID = "11111111-2222-3333-4444-555555555555"
	city := t.TempDir()
	script := filepath.Join(city, "provider.sh")
	writeProviderOwnedInitScript(t, script, projectID)
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"test\"\n[beads]\nprovider = \"exec:"+script+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := persistProviderScopeOwnership(city, city, providerScopeIntent{Transport: "proxied", Target: "local"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadCityConfig(city, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := startBeadsLifecycle(city, "", cfg, io.Discard); err != nil {
		t.Fatalf("startBeadsLifecycle: %v", err)
	}
	entry, ok, err := providerScopeOwnership(city, city)
	if err != nil || !ok || entry.State != providerScopeReady {
		t.Fatalf("scope ownership after init = (%+v, %t, %v), want ready", entry, ok, err)
	}
	got, ok, err := contract.ReadProjectIdentity(fsys.OSFS{}, city)
	if err != nil {
		t.Fatalf("ReadProjectIdentity: %v", err)
	}
	if !ok || got != projectID {
		t.Fatalf("identity.toml after provider-owned init = (%q, present=%t), want %q matching metadata project_id", got, ok, projectID)
	}
}
