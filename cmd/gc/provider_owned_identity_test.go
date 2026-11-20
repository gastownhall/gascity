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

// writeProviderOwnedLoggingInitScript writes a fake exec beads provider that
// logs every op it is asked to run (lifecycle ops are invoked with the verb
// alone, so the log path is baked in) and, when failInit is set, fails the
// init op. It leaves the scope's files alone, so a test pre-seeds whatever
// layer state its case needs.
func writeProviderOwnedLoggingInitScript(t *testing.T, path, oplog string, failInit bool) {
	t.Helper()
	fail := "  :\n"
	if failInit {
		fail = "  exit 1\n"
	}
	body := "#!/bin/sh\n" +
		"printf '%s\\n' \"$1\" >> '" + oplog + "'\n" +
		"if [ \"$1\" = init ]; then\n" +
		fail +
		"fi\n" +
		"exit 0\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// newProviderOwnedIdentityScope builds a city with the provider script and a
// pending provider-owned proxied-local scope at rigs/demo (the scope root the
// stamped-event assertions expect), with .beads/ materialized.
func newProviderOwnedIdentityScope(t *testing.T, script string) (city, dir string) {
	t.Helper()
	city = t.TempDir()
	dir = filepath.Join(city, "rigs", "demo")
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"test\"\n[beads]\nprovider = \"exec:"+script+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := persistProviderScopeOwnership(city, dir, providerScopeIntent{Transport: "proxied", Target: "local"}); err != nil {
		t.Fatal(err)
	}
	return city, dir
}

// writeProviderOwnedScopeMetadata writes the scope's metadata.json as an
// eligible proxied-server Dolt binding; projectID is omitted when empty, which
// is the L2-absent shape.
func writeProviderOwnedScopeMetadata(t *testing.T, dir, projectID string) {
	t.Helper()
	payload := "{\"database\":\"dolt\",\"backend\":\"dolt\",\"dolt_mode\":\"proxied-server\",\"dolt_database\":\"hq\""
	if projectID != "" {
		payload += ",\"project_id\":\"" + projectID + "\""
	}
	payload += "}"
	if err := os.WriteFile(filepath.Join(dir, ".beads", "metadata.json"), []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
}

// stubProviderOwnedIdentitySeams installs the L3-opener and recorder seams for
// one test: the opener serves the double as L3 whatever endpoint resolution
// produced (captured for the caller, with the count of opener invocations) and
// the recorder records stamped events.
func stubProviderOwnedIdentitySeams(t *testing.T, l3 *projectIdentityL3Double) (recorder *projectIdentityApplyRecordingRecorder, endpoint *providerOwnedIdentityEndpoint, openerCalls *int) {
	t.Helper()
	recorder = &projectIdentityApplyRecordingRecorder{}
	endpoint = &providerOwnedIdentityEndpoint{}
	calls := 0
	openerCalls = &calls
	prevOpener := openProviderOwnedIdentityL3Fn
	prevRecorder := openProviderOwnedIdentityRecorderFn
	openProviderOwnedIdentityL3Fn = func(_ context.Context, ep providerOwnedIdentityEndpoint) (providerOwnedIdentityL3Session, error) {
		calls++
		*endpoint = ep
		return providerOwnedIdentityL3Session{read: l3.read, seed: l3.seed}, nil
	}
	openProviderOwnedIdentityRecorderFn = func(string) (events.Recorder, func()) {
		return recorder, func() {}
	}
	t.Cleanup(func() {
		openProviderOwnedIdentityL3Fn = prevOpener
		openProviderOwnedIdentityRecorderFn = prevRecorder
	})
	return recorder, endpoint, openerCalls
}

// TestInitAndHookDirProviderOwnedReconcileLayers: whatever drift the three
// layers hold when the init pass reaches the reconcile, exactly the layers
// decideReconcile selects are written to the provider's ID, one stamped event
// per written layer.
func TestInitAndHookDirProviderOwnedReconcileLayers(t *testing.T) {
	const projectID = "11111111-2222-3333-4444-555555555555"
	const staleID = "99999999-8888-7777-6666-555555555555"
	tests := []struct {
		name       string
		l1         string
		l2         string
		l3         string
		wantEvents []projectIdentityApplyStampedEvent
	}{
		{
			name:       "seed L3 when L1 and L2 agree",
			l1:         projectID,
			l2:         projectID,
			wantEvents: []projectIdentityApplyStampedEvent{{source: "cache_repair", layer: "L3", newID: projectID}},
		},
		{
			name:       "repair a stale L2 against the L1/L3 agreement",
			l1:         projectID,
			l2:         staleID,
			l3:         projectID,
			wantEvents: []projectIdentityApplyStampedEvent{{source: "cache_repair", layer: "L2", oldID: staleID, newID: projectID}},
		},
		{
			name: "seed both L2 and L3 when only L1 holds the ID",
			l1:   projectID,
			wantEvents: []projectIdentityApplyStampedEvent{
				{source: "cache_repair", layer: "L2", newID: projectID},
				{source: "cache_repair", layer: "L3", newID: projectID},
			},
		},
		{
			name:       "adopt a missing L2 from the L1/L3 agreement",
			l1:         projectID,
			l3:         projectID,
			wantEvents: []projectIdentityApplyStampedEvent{{source: "cache_repair", layer: "L2", newID: projectID}},
		},
		{
			name: "adopt L1 and L2 from the database",
			l3:   projectID,
			wantEvents: []projectIdentityApplyStampedEvent{
				{source: "migrated_from_database", layer: "L1", newID: projectID},
				{source: "migrated_from_database", layer: "L2", newID: projectID},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			script := filepath.Join(t.TempDir(), "provider.sh")
			writeProviderOwnedLoggingInitScript(t, script, filepath.Join(t.TempDir(), "oplog"), false)
			city, dir := newProviderOwnedIdentityScope(t, script)
			writeProviderOwnedScopeMetadata(t, dir, tc.l2)
			if tc.l1 != "" {
				if err := contract.WriteProjectIdentity(fsys.OSFS{}, dir, tc.l1); err != nil {
					t.Fatalf("pre-write L1: %v", err)
				}
			}
			l3 := &projectIdentityL3Double{projectID: tc.l3}
			recorder, _, _ := stubProviderOwnedIdentitySeams(t, l3)

			if err := initAndHookDirResolvingDatabase(city, dir, "prov", nil); err != nil {
				t.Fatalf("initAndHookDirResolvingDatabase: %v", err)
			}

			entry, ok, err := providerScopeOwnership(city, dir)
			if err != nil || !ok || entry.State != providerScopeReady {
				t.Fatalf("scope ownership after init = (%+v, %t, %v), want ready", entry, ok, err)
			}
			gotL1, l1OK, err := contract.ReadProjectIdentity(fsys.OSFS{}, dir)
			if err != nil {
				t.Fatalf("ReadProjectIdentity: %v", err)
			}
			if !l1OK || gotL1 != projectID {
				t.Fatalf("L1 after reconcile = (%q, %t), want %q", gotL1, l1OK, projectID)
			}
			gotL2, err := readManagedMetadataProjectID(scopeMetadataJSONPath(dir))
			if err != nil {
				t.Fatalf("read L2: %v", err)
			}
			if gotL2 != projectID {
				t.Fatalf("L2 after reconcile = %q, want %q", gotL2, projectID)
			}
			if l3.projectID != projectID {
				t.Fatalf("L3 after reconcile = %q, want %q", l3.projectID, projectID)
			}
			assertProjectIdentityApplyStampedEvents(t, recorder.records, tc.wantEvents)
		})
	}
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
