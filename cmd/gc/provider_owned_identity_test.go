package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
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

// clearProviderOwnedProxyEnvironment removes the ambient proxy-root
// environment so ProviderRoot derives the root from the fixture alone.
func clearProviderOwnedProxyEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		proxyendpoint.RootPathEnv,
		proxyendpoint.DoltDataDirEnv,
		proxyendpoint.SharedServerModeEnv,
		proxyendpoint.SharedServerDirEnv,
	} {
		t.Setenv(name, "")
	}
}

// providerOwnedProxyRoot materializes a proxy root at scope/.beads/rel.
func providerOwnedProxyRoot(t *testing.T, scope, rel string) string {
	t.Helper()
	root := filepath.Join(scope, ".beads", rel)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

// makeProviderOwnedProxyRecord fabricates a record that validates against root.
func makeProviderOwnedProxyRecord(t *testing.T, root string, pid, port int) proxyendpoint.Record {
	t.Helper()
	rootID, err := proxyendpoint.RootID(root)
	if err != nil {
		t.Fatalf("identify proxy root: %v", err)
	}
	return proxyendpoint.Record{
		PID:         pid,
		Port:        port,
		UpstreamID:  "identity-test-upstream",
		Schema:      proxyendpoint.SchemaV2,
		Kind:        proxyendpoint.RecordKind,
		Birth:       proxyendpoint.BirthToken("identity-test-boot", fmt.Sprint(pid)),
		RootID:      rootID,
		ControlPort: port + 1,
	}
}

func writeProviderOwnedProxyRecord(t *testing.T, root string, record proxyendpoint.Record) {
	t.Helper()
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(proxyendpoint.PIDPath(root), data, 0o600); err != nil {
		t.Fatalf("write proxy endpoint record: %v", err)
	}
}

func providerOwnedProxyArgv(root string) []string {
	return []string{"bd", proxyendpoint.ChildVerb, proxyendpoint.RootFlag, root, proxyendpoint.IdleTimeoutFlag, "0"}
}

// stubProviderOwnedProcessTable points the resolver's proxy inspection at a
// table that vouches for record when alive and for nothing when dead.
func stubProviderOwnedProcessTable(t *testing.T, record proxyendpoint.Record, root string, alive bool) {
	t.Helper()
	prev := providerOwnedIdentityProcessTableFn
	providerOwnedIdentityProcessTableFn = func() proxyendpoint.ProcessTable {
		return proxyendpoint.ProcessTable{
			Alive: func(pid int) bool { return alive && pid == record.PID },
			Argv:  func(int) ([]string, error) { return providerOwnedProxyArgv(root), nil },
			Birth: func(int) (string, error) { return record.Birth, nil },
		}
	}
	t.Cleanup(func() { providerOwnedIdentityProcessTableFn = prev })
}

// newProviderOwnedProxiedScopeForResolution builds a bare scope whose metadata
// records an eligible proxied-server binding, with a sidecar that both roots
// the proxy under .beads/proxy-root and names an external upstream the
// resolver must never dial. It returns the scope root and the proxy root.
func newProviderOwnedProxiedScopeForResolution(t *testing.T) (scope, root string) {
	t.Helper()
	scope = t.TempDir()
	if err := os.MkdirAll(filepath.Join(scope, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeProviderOwnedScopeMetadata(t, scope, "")
	sidecar := `{"root_path":"proxy-root","idle_timeout":-1,"external":{"host":"db.example","port":4406}}`
	if err := os.WriteFile(proxyendpoint.SidecarPath(filepath.Join(scope, ".beads")), []byte(sidecar), 0o600); err != nil {
		t.Fatal(err)
	}
	root = providerOwnedProxyRoot(t, scope, "proxy-root")
	return scope, root
}

// TestProviderOwnedIdentityEndpointResolution: a proxied scope's L3 endpoint
// is bd's loopback proxy from the live record — never the sidecar's external
// upstream — and a record that is absent is the proxy's ordinary stopped
// state, while a dead record is a fault the resolver names.
func TestProviderOwnedIdentityEndpointResolution(t *testing.T) {
	t.Run("proxied resolves the live proxy record, never the sidecar upstream", func(t *testing.T) {
		clearProviderOwnedProxyEnvironment(t)
		scope, root := newProviderOwnedProxiedScopeForResolution(t)
		record := makeProviderOwnedProxyRecord(t, root, 4321, 45231)
		writeProviderOwnedProxyRecord(t, root, record)
		stubProviderOwnedProcessTable(t, record, root, true)

		endpoint, ok, err := resolveProviderOwnedIdentityEndpoint(t.TempDir(), scope, scopeMetadataJSONPath(scope))
		if err != nil || !ok {
			t.Fatalf("resolve endpoint = (%+v, %t, %v), want the live proxy", endpoint, ok, err)
		}
		if !endpoint.proxied || endpoint.port != 45231 || endpoint.database != "hq" {
			t.Fatalf("endpoint = %+v, want the proxied endpoint on the record's port 45231 for database hq", endpoint)
		}
	})

	t.Run("proxied without a record is the stopped proxy", func(t *testing.T) {
		clearProviderOwnedProxyEnvironment(t)
		scope, _ := newProviderOwnedProxiedScopeForResolution(t)

		endpoint, ok, err := resolveProviderOwnedIdentityEndpoint(t.TempDir(), scope, scopeMetadataJSONPath(scope))
		if err != nil || !ok {
			t.Fatalf("resolve endpoint = (%+v, %t, %v), want the no-record endpoint", endpoint, ok, err)
		}
		if !endpoint.proxied || endpoint.port != 0 || endpoint.database != "hq" {
			t.Fatalf("endpoint = %+v, want the proxied no-record endpoint (port 0) for database hq", endpoint)
		}
	})

	t.Run("proxied with a dead record refuses, naming the root", func(t *testing.T) {
		clearProviderOwnedProxyEnvironment(t)
		scope, root := newProviderOwnedProxiedScopeForResolution(t)
		record := makeProviderOwnedProxyRecord(t, root, 4322, 45232)
		writeProviderOwnedProxyRecord(t, root, record)
		stubProviderOwnedProcessTable(t, record, root, false)

		_, ok, err := resolveProviderOwnedIdentityEndpoint(t.TempDir(), scope, scopeMetadataJSONPath(scope))
		if err == nil || ok {
			t.Fatalf("resolve endpoint = (%t, %v), want a refusal for a dead proxy", ok, err)
		}
		for _, want := range []string{"not live", "dead", root} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("resolve error = %q, want it to name %q", err.Error(), want)
			}
		}
	})

	t.Run("direct local reads bd's own server port over legacy managed coordinates", func(t *testing.T) {
		clearProviderOwnedProxyEnvironment(t)
		scope := t.TempDir()
		beadsDir := filepath.Join(scope, ".beads")
		if err := os.MkdirAll(beadsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		// The metadata also records an external binding a stale state could
		// have left behind; bd's own live server must win over it.
		metadata := `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_server_host":"db.example","dolt_server_port":4406,"dolt_database":"prov"}`
		if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0o600); err != nil {
			t.Fatal(err)
		}
		listener := listenOnRandomPort(t)
		bdPort := fmt.Sprintf("%d", listener.Addr().(*net.TCPAddr).Port)
		if err := os.WriteFile(filepath.Join(beadsDir, "dolt-server.pid"), []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(beadsDir, "dolt-server.port"), []byte(bdPort+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte("issue_prefix: prov\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		endpoint, ok, err := resolveProviderOwnedIdentityEndpoint(t.TempDir(), scope, scopeMetadataJSONPath(scope))
		if err != nil || !ok {
			t.Fatalf("resolve endpoint = (%+v, %t, %v), want bd's own server", endpoint, ok, err)
		}
		if endpoint.proxied || endpoint.target.Host != "127.0.0.1" || endpoint.target.Port != bdPort || endpoint.target.Socket != "" || endpoint.database != "prov" {
			t.Fatalf("endpoint = %+v, want bd's own endpoint 127.0.0.1:%s for database prov, ahead of the recorded external binding", endpoint, bdPort)
		}
	})

	t.Run("direct external reads the persisted host port binding", func(t *testing.T) {
		clearProviderOwnedProxyEnvironment(t)
		scope := t.TempDir()
		beadsDir := filepath.Join(scope, ".beads")
		if err := os.MkdirAll(beadsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		metadata := `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_server_host":"db.example","dolt_server_port":4406,"dolt_database":"hosted"}`
		if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte("issue_prefix: prov\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		endpoint, ok, err := resolveProviderOwnedIdentityEndpoint(t.TempDir(), scope, scopeMetadataJSONPath(scope))
		if err != nil || !ok {
			t.Fatalf("resolve endpoint = (%+v, %t, %v), want the persisted external binding", endpoint, ok, err)
		}
		if endpoint.proxied || endpoint.target.Host != "db.example" || endpoint.target.Port != "4406" || endpoint.database != "hosted" {
			t.Fatalf("endpoint = %+v, want the external endpoint db.example:4406 for database hosted", endpoint)
		}
	})

	t.Run("direct external reads the persisted socket binding", func(t *testing.T) {
		clearProviderOwnedProxyEnvironment(t)
		scope := t.TempDir()
		beadsDir := filepath.Join(scope, ".beads")
		if err := os.MkdirAll(beadsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		metadata := `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_server_socket":"/run/dolt/provider.sock","dolt_database":"hosted"}`
		if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte("issue_prefix: prov\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		endpoint, ok, err := resolveProviderOwnedIdentityEndpoint(t.TempDir(), scope, scopeMetadataJSONPath(scope))
		if err != nil || !ok {
			t.Fatalf("resolve endpoint = (%+v, %t, %v), want the persisted socket binding", endpoint, ok, err)
		}
		if endpoint.proxied || endpoint.target.Socket != "/run/dolt/provider.sock" || endpoint.database != "hosted" {
			t.Fatalf("endpoint = %+v, want the socket endpoint /run/dolt/provider.sock for database hosted", endpoint)
		}
	})
}

// TestReconcileProviderOwnedScopeIdentityProxiedUsesOnePrivateSession: with a
// live record, the reconcile's L3 is exactly one TCP session to the proxy's
// loopback data port — the handshake failing here proves the dial reached that
// port and no redial ladder followed — and a session that cannot open leaves
// every layer untouched.
func TestReconcileProviderOwnedScopeIdentityProxiedUsesOnePrivateSession(t *testing.T) {
	clearProviderOwnedProxyEnvironment(t)
	const projectID = "11111111-2222-3333-4444-555555555555"
	scope, root := newProviderOwnedProxiedScopeForResolution(t)
	writeProviderOwnedScopeMetadata(t, scope, projectID)

	listener, err := net.Listen("tcp", net.JoinHostPort(proxyendpoint.Host, "0"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var accepted atomic.Int64
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			accepted.Add(1)
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	port := listener.Addr().(*net.TCPAddr).Port

	record := makeProviderOwnedProxyRecord(t, root, 4323, port)
	writeProviderOwnedProxyRecord(t, root, record)
	stubProviderOwnedProcessTable(t, record, root, true)

	err = reconcileProviderOwnedScopeIdentity(context.Background(), t.TempDir(), scope)
	if err == nil || !strings.Contains(err.Error(), "open proxy session") {
		t.Fatalf("reconcile against a proxy that closes at once = %v, want the session-open failure", err)
	}
	if got := accepted.Load(); got != 1 {
		t.Fatalf("the reconcile dialed the proxy %d time(s), want exactly 1: the session is private and single-shot, never database/sql's redial ladder", got)
	}
	if _, ok, readErr := contract.ReadProjectIdentity(fsys.OSFS{}, scope); readErr != nil || ok {
		t.Fatalf("identity.toml after a failed session = (%t, %v), want absent: no layer is written when L3 cannot open", ok, readErr)
	}
}
