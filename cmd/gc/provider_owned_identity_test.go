package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
)

// writeProviderOwnedInitScript writes a fake exec beads provider that stands
// in for bd: `init` writes the binding and the project_id bd stamps into the
// scope's metadata.json, and every other op succeeds.
func writeProviderOwnedInitScript(t *testing.T, path, projectID string) {
	t.Helper()
	body := "#!/bin/sh\n" +
		"if [ \"$1\" = init ]; then\n" +
		"  mkdir -p \"$2/.beads\"\n" +
		"  printf '%s' '{\"database\":\"dolt\",\"backend\":\"dolt\",\"dolt_mode\":\"proxied-server\",\"dolt_database\":\"hq\",\"project_id\":\"" + projectID + "\"}' > \"$2/.beads/metadata.json\"\n" +
		"fi\n" +
		"exit 0\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// writeProviderOwnedCity writes a city whose beads provider is the fake exec
// provider at script, and returns the city path.
func writeProviderOwnedCity(t *testing.T, script string) string {
	t.Helper()
	city := t.TempDir()
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"test\"\n[beads]\nprovider = \"exec:"+script+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return city
}

// TestInitAndHookDirProviderOwnedReconcilesProjectIdentity: the provider-owned
// init pass reconciles the scope's project identity through whatever L3
// session the opener produces — here a double that already holds the
// provider's ID — stamping L1 from the agreeing L2/L3 pair exactly once,
// before the journal commits ready.
func TestInitAndHookDirProviderOwnedReconcilesProjectIdentity(t *testing.T) {
	const projectID = "11111111-2222-3333-4444-555555555555"
	script := filepath.Join(t.TempDir(), "provider.sh")
	writeProviderOwnedInitScript(t, script, projectID)
	city := writeProviderOwnedCity(t, script)
	dir := filepath.Join(city, "rigs", "demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := persistProviderScopeOwnership(city, dir, providerScopeIntent{Transport: "proxied", Target: "local"}); err != nil {
		t.Fatal(err)
	}

	l3 := &projectIdentityL3Double{projectID: projectID}
	var l3Reads int
	var openerEndpoint providerOwnedIdentityEndpoint
	var journalStateAtOpen *providerScopeOwnershipEntry
	prevOpener := openProviderOwnedIdentityL3Fn
	prevRecorder := openProviderOwnedIdentityRecorderFn
	openProviderOwnedIdentityL3Fn = func(_ context.Context, endpoint providerOwnedIdentityEndpoint) (providerOwnedIdentityL3Session, error) {
		openerEndpoint = endpoint
		if entry, ok, err := providerScopeOwnership(city, dir); err == nil && ok {
			journalStateAtOpen = &entry
		}
		return providerOwnedIdentityL3Session{
			read: func(ctx context.Context) (string, bool, error) {
				l3Reads++
				return l3.read(ctx)
			},
			seed: l3.seed,
		}, nil
	}
	recorder := &projectIdentityApplyRecordingRecorder{}
	openProviderOwnedIdentityRecorderFn = func(string) (events.Recorder, func()) {
		return recorder, func() {}
	}
	t.Cleanup(func() {
		openProviderOwnedIdentityL3Fn = prevOpener
		openProviderOwnedIdentityRecorderFn = prevRecorder
	})

	if err := initAndHookDirResolvingDatabase(city, dir, "prov", nil); err != nil {
		t.Fatalf("initAndHookDirResolvingDatabase: %v", err)
	}

	entry, ok, err := providerScopeOwnership(city, dir)
	if err != nil || !ok || entry.State != providerScopeReady {
		t.Fatalf("scope ownership after init = (%+v, %t, %v), want ready", entry, ok, err)
	}
	if journalStateAtOpen == nil || journalStateAtOpen.State != providerScopeInitializing {
		t.Fatalf("journal state when the identity reconcile ran = %+v, want %q: the reconcile must run before the pass commits ready", journalStateAtOpen, providerScopeInitializing)
	}
	got, gotOK, err := contract.ReadProjectIdentity(fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("ReadProjectIdentity: %v", err)
	}
	if !gotOK || got != projectID {
		t.Fatalf("identity.toml after provider-owned init = (%q, present=%t), want %q migrated from the agreeing metadata and database", got, gotOK, projectID)
	}
	// The fixture writes no proxy record, so resolution hands the opener the
	// stopped-proxy endpoint; the stub supplies L3 regardless.
	if !openerEndpoint.proxied || openerEndpoint.port != 0 {
		t.Fatalf("opener endpoint = %+v, want proxied with no record (port 0)", openerEndpoint)
	}
	if l3Reads != 1 {
		t.Fatalf("L3 read %d time(s), want exactly 1", l3Reads)
	}
	assertProjectIdentityApplyStampedEvents(t, recorder.records, []projectIdentityApplyStampedEvent{
		{source: "migrated_from_metadata", layer: "L1", newID: projectID},
	})
}
