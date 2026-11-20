package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	gcapi "github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
)

const (
	providerOwnedIdentityP = "11111111-2222-3333-4444-555555555555"
	providerOwnedIdentityQ = "99999999-8888-7777-6666-555555555555"
	// providerOwnedProxyPort is the live proxy record's port in fixtures. No
	// listener is behind it: the L3 opener is stubbed wherever it is reached.
	providerOwnedProxyPort = 45123
)

// providerOwnedL3Fake stands in for the L3 opener: it records every endpoint
// it was opened for and every close, and serves L3 from a
// projectIdentityL3Double.
type providerOwnedL3Fake struct {
	l3     *projectIdentityL3Double
	opened []providerOwnedIdentityEndpoint
	closed int
	seeds  int
	onOpen func()
}

func (f *providerOwnedL3Fake) ReadProjectID(ctx context.Context) (string, bool, error) {
	return f.l3.read(ctx)
}

func (f *providerOwnedL3Fake) SeedProjectID(ctx context.Context, projectID string) (bool, error) {
	f.seeds++
	return f.l3.seed(ctx, projectID)
}

// stubProviderOwnedL3 routes the L3 opener to fake for the rest of the test.
func stubProviderOwnedL3(t *testing.T, fake *providerOwnedL3Fake) {
	t.Helper()
	prev := openProviderOwnedScopeL3Fn
	openProviderOwnedScopeL3Fn = func(_ context.Context, endpoint providerOwnedIdentityEndpoint) (projectIdentityL3Store, func() error, error) {
		fake.opened = append(fake.opened, endpoint)
		if fake.onOpen != nil {
			fake.onOpen()
		}
		return fake, func() error { fake.closed++; return nil }, nil
	}
	t.Cleanup(func() { openProviderOwnedScopeL3Fn = prev })
}

// clearProviderOwnedProxyEnv removes the inherited environment that would
// move ProviderRoot away from the scope's own default root.
func clearProviderOwnedProxyEnv(t *testing.T) {
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

// stubLiveProviderOwnedProxy publishes a live proxy record on
// providerOwnedProxyPort under a proxy root dedicated to the test, and a
// process table that vouches for it.
func stubLiveProviderOwnedProxy(t *testing.T) {
	t.Helper()
	const port = providerOwnedProxyPort
	root := filepath.Join(t.TempDir(), "proxy-root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(proxyendpoint.RootPathEnv, root)
	rootID, err := proxyendpoint.RootID(root)
	if err != nil {
		t.Fatalf("identify proxy root: %v", err)
	}
	const pid = 4242
	record := proxyendpoint.Record{
		PID:         pid,
		Port:        port,
		UpstreamID:  "provider-owned-identity-test",
		Schema:      proxyendpoint.SchemaV2,
		Kind:        proxyendpoint.RecordKind,
		Birth:       proxyendpoint.BirthToken("test-boot", strconv.Itoa(pid)),
		RootID:      rootID,
		ControlPort: port + 1,
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(proxyendpoint.PIDPath(root), data, 0o600); err != nil {
		t.Fatal(err)
	}
	argv := []string{"bd", proxyendpoint.ChildVerb, proxyendpoint.RootFlag, root, proxyendpoint.IdleTimeoutFlag, "0"}
	prev := providerOwnedIdentityProcessTable
	providerOwnedIdentityProcessTable = func() proxyendpoint.ProcessTable {
		return proxyendpoint.ProcessTable{
			Alive: func(p int) bool { return p == pid },
			Argv:  func(int) ([]string, error) { return argv, nil },
			Birth: func(int) (string, error) { return record.Birth, nil },
		}
	}
	t.Cleanup(func() { providerOwnedIdentityProcessTable = prev })
}

// skipProviderOwnedIdentityReconcile makes the provider-owned identity step a
// no-op, for tests whose exec provider stands in for bd with no store behind
// the binding it writes.
func skipProviderOwnedIdentityReconcile(t *testing.T) {
	t.Helper()
	prev := reconcileProviderOwnedScopeIdentityFn
	reconcileProviderOwnedScopeIdentityFn = func(context.Context, string, string) error { return nil }
	t.Cleanup(func() { reconcileProviderOwnedScopeIdentityFn = prev })
}

// providerOwnedIdentityCity is a city whose own scope is provider-owned
// through a fake exec provider: init writes metadata (when set), every op
// succeeds, and each op is logged.
type providerOwnedIdentityCity struct {
	dir   string
	opLog string
}

func newProviderOwnedIdentityCity(t *testing.T, metadata string) providerOwnedIdentityCity {
	t.Helper()
	clearInheritedBeadsEnv(t)
	clearProviderOwnedProxyEnv(t)
	city := t.TempDir()
	tools := t.TempDir()
	opLog := filepath.Join(tools, "ops.log")
	writeMetadata := ""
	if metadata != "" {
		writeMetadata = "  mkdir -p \"$2/.beads\"\n  printf '%s' " + shellQuoteForTest(metadata) + " > \"$2/.beads/metadata.json\"\n"
	}
	provider := filepath.Join(tools, "provider.sh")
	body := "#!/bin/sh\n" +
		"printf '%s\\n' \"$1\" >> " + strconv.Quote(opLog) + "\n" +
		"if [ \"$1\" = init ]; then\n" + writeMetadata + "  exit 0\nfi\n" +
		"exit 0\n"
	if err := os.WriteFile(provider, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	bd := filepath.Join(tools, "bd")
	if err := os.WriteFile(bd, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"identity\"\n\n[workspace.env]\nBD_BIN = \"" + bd + "\"\n" +
		"[beads]\nprovider = \"exec:" + provider + "\"\n"
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := persistProviderScopeOwnership(city, city, providerScopeIntent{Transport: "proxied", Target: "local"}); err != nil {
		t.Fatal(err)
	}
	return providerOwnedIdentityCity{dir: city, opLog: opLog}
}

func shellQuoteForTest(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ops returns the provider ops the city ran, in order, and resets the log.
func (c providerOwnedIdentityCity) ops(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(c.opLog)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(c.opLog); err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(data))
}

func (c providerOwnedIdentityCity) entry(t *testing.T) providerScopeOwnershipEntry {
	t.Helper()
	entry, ok, err := providerScopeOwnership(c.dir, c.dir)
	if err != nil || !ok {
		t.Fatalf("scope ownership = (%+v, %t, %v), want a journaled entry", entry, ok, err)
	}
	return entry
}

// stampedLayers returns the layer of every ProjectIdentityStamped event in
// the city's event log, in order.
func (c providerOwnedIdentityCity) stampedLayers(t *testing.T) []string {
	t.Helper()
	path := filepath.Join(c.dir, citylayout.RuntimeRoot, "events.jsonl")
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	all, err := events.ReadAll(path)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	var layers []string
	for _, event := range all {
		if event.Type != events.ProjectIdentityStamped {
			continue
		}
		var payload gcapi.ProjectIdentityStampedPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("decode stamped payload: %v", err)
		}
		if payload.NewID != providerOwnedIdentityP {
			t.Fatalf("stamped event new_id = %q, want %q", payload.NewID, providerOwnedIdentityP)
		}
		layers = append(layers, payload.Layer)
	}
	return layers
}

func (c providerOwnedIdentityCity) metadataProjectID(t *testing.T) string {
	t.Helper()
	id, err := readManagedMetadataProjectID(scopeMetadataJSONPath(c.dir))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (c providerOwnedIdentityCity) identity(t *testing.T) (string, bool) {
	t.Helper()
	id, ok, err := contract.ReadProjectIdentity(fsys.OSFS{}, c.dir)
	if err != nil {
		t.Fatalf("ReadProjectIdentity: %v", err)
	}
	return id, ok
}

func proxiedIdentityMetadata(projectID string) string {
	m := `{"database":"dolt","backend":"dolt","dolt_mode":"proxied-server","dolt_database":"hq"`
	if projectID != "" {
		m += `,"project_id":"` + projectID + `"`
	}
	return m + "}"
}

func writeIdentityFile(t *testing.T, scope, id string) {
	t.Helper()
	if _, err := writeProjectIdentityIfNeeded(fsys.OSFS{}, scope, id); err != nil {
		t.Fatal(err)
	}
}

// TestInitAndHookDirProviderOwnedReconcilesProjectIdentity: a pending
// proxied-local scope whose bd init left metadata project_id = Dolt
// _project_id = P and no identity.toml gets identity.toml = P, one L1
// stamped event, and turns ready only after L1 was written.
func TestInitAndHookDirProviderOwnedReconcilesProjectIdentity(t *testing.T) {
	city := newProviderOwnedIdentityCity(t, proxiedIdentityMetadata(providerOwnedIdentityP))
	stubLiveProviderOwnedProxy(t)
	var stateAtOpen string
	fake := &providerOwnedL3Fake{l3: &projectIdentityL3Double{projectID: providerOwnedIdentityP}}
	fake.onOpen = func() { stateAtOpen = city.entry(t).State }
	stubProviderOwnedL3(t, fake)

	if err := initAndHookDirResolvingDatabase(city.dir, city.dir, "gc", nil); err != nil {
		t.Fatalf("initAndHookDirResolvingDatabase: %v", err)
	}

	if got, ok := city.identity(t); !ok || got != providerOwnedIdentityP {
		t.Fatalf("identity.toml = (%q, present=%t), want %q", got, ok, providerOwnedIdentityP)
	}
	if got := city.stampedLayers(t); len(got) != 1 || got[0] != "L1" {
		t.Fatalf("stamped layers = %v, want exactly [L1]", got)
	}
	if got := city.ops(t); strings.Join(got, " ") != "init health" {
		t.Fatalf("provider ops = %v, want [init health]", got)
	}
	if stateAtOpen != providerScopeInitializing {
		t.Fatalf("journal state when the reconcile ran = %q, want %q", stateAtOpen, providerScopeInitializing)
	}
	if got := city.entry(t).State; got != providerScopeReady {
		t.Fatalf("journal state after the pass = %q, want %q", got, providerScopeReady)
	}
	if fake.closed != len(fake.opened) {
		t.Fatalf("L3 opened %d time(s) and closed %d, want every open closed", len(fake.opened), fake.closed)
	}
}

// TestInitAndHookDirProviderOwnedReconcileLayers: with L1 = P, exactly the
// layers decideReconcile selects are written to P, one stamped event each.
func TestInitAndHookDirProviderOwnedReconcileLayers(t *testing.T) {
	for _, tc := range []struct {
		name       string
		l1, l2, l3 string
		wantLayers []string
		wantSeeds  int
	}{
		{name: "seed L3", l1: providerOwnedIdentityP, l2: providerOwnedIdentityP, wantLayers: []string{"L3"}, wantSeeds: 1},
		{name: "repair stale L2", l1: providerOwnedIdentityP, l2: providerOwnedIdentityQ, l3: providerOwnedIdentityP, wantLayers: []string{"L2"}},
		{name: "repair stale L2 and seed L3", l1: providerOwnedIdentityP, l2: providerOwnedIdentityQ, wantLayers: []string{"L2", "L3"}, wantSeeds: 1},
		{name: "seed missing L2", l1: providerOwnedIdentityP, l3: providerOwnedIdentityP, wantLayers: []string{"L2"}},
		{name: "seed missing L2 and L3", l1: providerOwnedIdentityP, wantLayers: []string{"L2", "L3"}, wantSeeds: 1},
		{name: "adopt L1 and L2 from L3", l3: providerOwnedIdentityP, wantLayers: []string{"L1", "L2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city := newProviderOwnedIdentityCity(t, proxiedIdentityMetadata(tc.l2))
			if tc.l1 != "" {
				writeIdentityFile(t, city.dir, tc.l1)
			}
			stubLiveProviderOwnedProxy(t)
			fake := &providerOwnedL3Fake{l3: &projectIdentityL3Double{projectID: tc.l3}}
			stubProviderOwnedL3(t, fake)

			if err := initAndHookDirResolvingDatabase(city.dir, city.dir, "gc", nil); err != nil {
				t.Fatalf("initAndHookDirResolvingDatabase: %v", err)
			}

			if got := city.stampedLayers(t); strings.Join(got, ",") != strings.Join(tc.wantLayers, ",") {
				t.Fatalf("stamped layers = %v, want %v", got, tc.wantLayers)
			}
			if fake.seeds != tc.wantSeeds {
				t.Fatalf("L3 seeds = %d, want %d", fake.seeds, tc.wantSeeds)
			}
			if got, ok := city.identity(t); !ok || got != providerOwnedIdentityP {
				t.Fatalf("L1 = (%q, %t), want %q", got, ok, providerOwnedIdentityP)
			}
			if got := city.metadataProjectID(t); got != providerOwnedIdentityP {
				t.Fatalf("L2 = %q, want %q", got, providerOwnedIdentityP)
			}
			if fake.l3.projectID != providerOwnedIdentityP {
				t.Fatalf("L3 = %q, want %q", fake.l3.projectID, providerOwnedIdentityP)
			}
			if got := city.entry(t).State; got != providerScopeReady {
				t.Fatalf("journal state = %q, want %q", got, providerScopeReady)
			}
		})
	}
}

// TestProviderOwnedIdentityProxiedEndpoint: a proxied scope, local or
// external upstream, reaches L3 through bd's live local proxy on the record's
// port for the metadata dolt_database; the sidecar's upstream is never the
// endpoint, and the one session is closed before the reconcile returns.
func TestProviderOwnedIdentityProxiedEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name    string
		sidecar string
	}{
		{name: "local upstream", sidecar: residentProxySidecar},
		{name: "external upstream", sidecar: externalProxySidecar},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearInheritedBeadsEnv(t)
			clearProviderOwnedProxyEnv(t)
			city := t.TempDir()
			writeReadyTopologyScope(t, city, readyTopologyScope{
				beadsConfig: bdTemplateBeadsConfig,
				metadata:    proxiedLocalMetadata,
				sidecar:     tc.sidecar,
			})
			stubLiveProviderOwnedProxy(t)
			fake := &providerOwnedL3Fake{l3: &projectIdentityL3Double{}}
			stubProviderOwnedL3(t, fake)
			writeIdentityFile(t, city, providerOwnedIdentityP)

			if err := reconcileProviderOwnedScopeIdentity(context.Background(), city, city); err != nil {
				t.Fatalf("reconcileProviderOwnedScopeIdentity: %v", err)
			}

			want := providerOwnedIdentityEndpoint{ProxyPort: providerOwnedProxyPort, Database: "prov"}
			if len(fake.opened) != 1 || fake.opened[0] != want {
				t.Fatalf("L3 opened for %+v, want exactly one open for %+v", fake.opened, want)
			}
			if fake.closed != 1 {
				t.Fatalf("L3 session closed %d time(s), want 1 before the reconcile returned", fake.closed)
			}
			if fake.l3.projectID != providerOwnedIdentityP {
				t.Fatalf("L3 = %q, want seeded %q", fake.l3.projectID, providerOwnedIdentityP)
			}
		})
	}
}

// TestProviderOwnedIdentityDirectEndpoint: a direct scope connects to the
// target the connection contract resolves: bd's own live server port, even
// when legacy managed coordinates are in the environment, and the socket or
// host:port bd persisted for an external binding.
func TestProviderOwnedIdentityDirectEndpoint(t *testing.T) {
	directScope := func(t *testing.T, metadata string) (string, *providerOwnedL3Fake) {
		t.Helper()
		clearInheritedBeadsEnv(t)
		clearProviderOwnedProxyEnv(t)
		city := t.TempDir()
		writeReadyTopologyScope(t, city, readyTopologyScope{beadsConfig: bdTemplateBeadsConfig, metadata: metadata})
		writeIdentityFile(t, city, providerOwnedIdentityP)
		fake := &providerOwnedL3Fake{l3: &projectIdentityL3Double{projectID: providerOwnedIdentityP}}
		stubProviderOwnedL3(t, fake)
		return city, fake
	}
	reconcileTarget := func(t *testing.T, city string, fake *providerOwnedL3Fake) contract.DoltConnectionTarget {
		t.Helper()
		if err := reconcileProviderOwnedScopeIdentity(context.Background(), city, city); err != nil {
			t.Fatalf("reconcileProviderOwnedScopeIdentity: %v", err)
		}
		if len(fake.opened) != 1 {
			t.Fatalf("L3 opened %d time(s), want 1", len(fake.opened))
		}
		opened := fake.opened[0]
		if opened.ProxyPort != 0 {
			t.Fatalf("direct scope opened through a proxy port %d", opened.ProxyPort)
		}
		if opened.Database != opened.Target.Database {
			t.Fatalf("opened database %q, target database %q: want the same", opened.Database, opened.Target.Database)
		}
		return opened.Target
	}

	t.Run("bd-owned local server wins over legacy managed coordinates", func(t *testing.T) {
		city, fake := directScope(t, `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"prov","project_id":"`+providerOwnedIdentityP+`"}`)
		listener := listenOnRandomPort(t)
		t.Cleanup(func() { _ = listener.Close() })
		port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
		beadsDir := filepath.Join(city, ".beads")
		if err := os.WriteFile(filepath.Join(beadsDir, "dolt-server.pid"), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(beadsDir, "dolt-server.port"), []byte(port+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GC_DOLT_HOST", "127.0.0.1")
		t.Setenv("GC_DOLT_PORT", "1")

		target := reconcileTarget(t, city, fake)
		if target.Host != "127.0.0.1" || target.Port != port || target.Socket != "" || target.Database != "prov" {
			t.Fatalf("target = %+v, want bd's own server 127.0.0.1:%s/prov", target, port)
		}
	})

	t.Run("external host:port binding", func(t *testing.T) {
		city, fake := directScope(t, `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_server_host":"db.example","dolt_server_port":4406,"dolt_database":"hosted","project_id":"`+providerOwnedIdentityP+`"}`)
		target := reconcileTarget(t, city, fake)
		if target.Host != "db.example" || target.Port != "4406" || target.Socket != "" || target.Database != "hosted" {
			t.Fatalf("target = %+v, want bd's persisted external db.example:4406/hosted", target)
		}
	})

	t.Run("external socket binding", func(t *testing.T) {
		city, fake := directScope(t, `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_server_socket":"/run/dolt/provider.sock","dolt_database":"hosted","project_id":"`+providerOwnedIdentityP+`"}`)
		target := reconcileTarget(t, city, fake)
		if target.Socket != "/run/dolt/provider.sock" || target.Database != "hosted" {
			t.Fatalf("target = %+v, want bd's persisted socket /run/dolt/provider.sock for hosted", target)
		}
	})
}

// TestInitAndHookDirProviderOwnedIdentityFailureKeepsInitializing: when
// bd init succeeded but the reconcile fails, the pass returns an error naming
// the scope and the cause, the journal stays provider_initializing with its
// intent, nothing under .beads/ is removed or rewritten, and a later pass with
// the cause removed reconciles and marks the scope ready.
func TestInitAndHookDirProviderOwnedIdentityFailureKeepsInitializing(t *testing.T) {
	for _, tc := range []struct {
		name string
		// arrange sets up the failure and returns the fix.
		arrange   func(t *testing.T, city providerOwnedIdentityCity) (fake *providerOwnedL3Fake, fix func())
		wantCause string
	}{
		{
			name: "L1/L3 mismatch refusal",
			arrange: func(t *testing.T, city providerOwnedIdentityCity) (*providerOwnedL3Fake, func()) {
				writeIdentityFile(t, city.dir, providerOwnedIdentityQ)
				stubLiveProviderOwnedProxy(t)
				fake := &providerOwnedL3Fake{l3: &projectIdentityL3Double{projectID: providerOwnedIdentityP}}
				stubProviderOwnedL3(t, fake)
				return fake, func() {
					if err := os.Remove(contract.ProjectIdentityPath(city.dir)); err != nil {
						t.Fatal(err)
					}
					writeIdentityFile(t, city.dir, providerOwnedIdentityP)
				}
			},
			wantCause: providerOwnedIdentityQ,
		},
		{
			name: "L3 write error",
			arrange: func(t *testing.T, city providerOwnedIdentityCity) (*providerOwnedL3Fake, func()) {
				writeIdentityFile(t, city.dir, providerOwnedIdentityP)
				stubLiveProviderOwnedProxy(t)
				fake := &providerOwnedL3Fake{l3: &projectIdentityL3Double{seedErr: errors.New("metadata table is read-only")}}
				stubProviderOwnedL3(t, fake)
				return fake, func() { fake.l3.seedErr = nil }
			},
			wantCause: "metadata table is read-only",
		},
		{
			name: "non-live proxy",
			arrange: func(t *testing.T, _ providerOwnedIdentityCity) (*providerOwnedL3Fake, func()) {
				fake := &providerOwnedL3Fake{l3: &projectIdentityL3Double{projectID: providerOwnedIdentityP}}
				stubProviderOwnedL3(t, fake)
				return fake, func() { stubLiveProviderOwnedProxy(t) }
			},
			wantCause: "is not live",
		},
		{
			name: "unresolvable endpoint",
			arrange: func(t *testing.T, city providerOwnedIdentityCity) (*providerOwnedL3Fake, func()) {
				stubLiveProviderOwnedProxy(t)
				fake := &providerOwnedL3Fake{l3: &projectIdentityL3Double{projectID: providerOwnedIdentityP}}
				stubProviderOwnedL3(t, fake)
				// A proxy root nothing can resolve: the sidecar is not JSON.
				if err := os.MkdirAll(filepath.Join(city.dir, ".beads"), 0o700); err != nil {
					t.Fatal(err)
				}
				t.Setenv(proxyendpoint.RootPathEnv, "")
				sidecar := proxyendpoint.SidecarPath(filepath.Join(city.dir, ".beads"))
				if err := os.WriteFile(sidecar, []byte("{not json"), 0o600); err != nil {
					t.Fatal(err)
				}
				return fake, func() {
					if err := os.Remove(sidecar); err != nil {
						t.Fatal(err)
					}
					stubLiveProviderOwnedProxy(t)
				}
			},
			wantCause: "resolve bd proxy root",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metadata := proxiedIdentityMetadata(providerOwnedIdentityP)
			city := newProviderOwnedIdentityCity(t, metadata)
			_, fix := tc.arrange(t, city)
			before := city.entry(t)
			identityBefore, identityErr := os.ReadFile(contract.ProjectIdentityPath(city.dir))

			err := initAndHookDirResolvingDatabase(city.dir, city.dir, "gc", nil)
			if err == nil {
				t.Fatal("initAndHookDirResolvingDatabase succeeded, want the reconcile failure")
			}
			for _, want := range []string{city.dir, tc.wantCause} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error = %q, want it to name %q", err, want)
				}
			}
			after := city.entry(t)
			if after.State != providerScopeInitializing || after.Intent != before.Intent || after.Intent == (providerScopeIntent{}) {
				t.Fatalf("journal entry after failure = %+v, want %q with intent %+v", after, providerScopeInitializing, before.Intent)
			}
			if got := string(mustReadFile(t, scopeMetadataJSONPath(city.dir))); got != metadata {
				t.Fatalf("metadata.json after failure = %q, want bd's %q untouched", got, metadata)
			}
			identityAfter, identityAfterErr := os.ReadFile(contract.ProjectIdentityPath(city.dir))
			if string(identityAfter) != string(identityBefore) || (identityErr == nil) != (identityAfterErr == nil) {
				t.Fatalf("identity.toml after failure = (%q, %v), want unchanged (%q, %v)", identityAfter, identityAfterErr, identityBefore, identityErr)
			}

			fix()
			if err := initAndHookDirResolvingDatabase(city.dir, city.dir, "gc", nil); err != nil {
				t.Fatalf("retry after removing the cause: %v", err)
			}
			if got := city.entry(t).State; got != providerScopeReady {
				t.Fatalf("journal state after retry = %q, want %q", got, providerScopeReady)
			}
			if got, ok := city.identity(t); !ok || got != providerOwnedIdentityP {
				t.Fatalf("identity.toml after retry = (%q, %t), want %q", got, ok, providerOwnedIdentityP)
			}
		})
	}
}

// TestInitAndHookDirProviderOwnedReadyScopeReconcilesEachPass: a ready scope
// is reconciled on every pass, or refused with an error, and its journal
// entry never changes.
func TestInitAndHookDirProviderOwnedReadyScopeReconcilesEachPass(t *testing.T) {
	readyCity := func(t *testing.T) (providerOwnedIdentityCity, providerScopeOwnershipEntry) {
		t.Helper()
		city := newProviderOwnedIdentityCity(t, proxiedIdentityMetadata(providerOwnedIdentityP))
		stubLiveProviderOwnedProxy(t)
		stubProviderOwnedL3(t, &providerOwnedL3Fake{l3: &projectIdentityL3Double{projectID: providerOwnedIdentityP}})
		if err := initAndHookDirResolvingDatabase(city.dir, city.dir, "gc", nil); err != nil {
			t.Fatalf("first pass: %v", err)
		}
		entry := city.entry(t)
		if entry.State != providerScopeReady {
			t.Fatalf("journal state after the first pass = %q, want %q", entry.State, providerScopeReady)
		}
		city.ops(t)
		if err := os.Remove(contract.ProjectIdentityPath(city.dir)); err != nil {
			t.Fatal(err)
		}
		return city, entry
	}

	t.Run("missing L1 is written", func(t *testing.T) {
		city, before := readyCity(t)
		if err := initAndHookDirResolvingDatabase(city.dir, city.dir, "gc", nil); err != nil {
			t.Fatalf("ready pass: %v", err)
		}
		if got, ok := city.identity(t); !ok || got != providerOwnedIdentityP {
			t.Fatalf("identity.toml = (%q, %t), want %q", got, ok, providerOwnedIdentityP)
		}
		if got := city.ops(t); strings.Join(got, " ") != "start health" {
			t.Fatalf("provider ops = %v, want [start health]", got)
		}
		if after := city.entry(t); after != before {
			t.Fatalf("journal entry = %+v, want unchanged %+v", after, before)
		}
	})

	t.Run("drifted L1 is refused", func(t *testing.T) {
		city, before := readyCity(t)
		writeIdentityFile(t, city.dir, providerOwnedIdentityQ)
		err := initAndHookDirResolvingDatabase(city.dir, city.dir, "gc", nil)
		if err == nil || !strings.Contains(err.Error(), city.dir) || !strings.Contains(err.Error(), providerOwnedIdentityQ) {
			t.Fatalf("ready pass with drifted L1 = %v, want a refusal naming the scope and %s", err, providerOwnedIdentityQ)
		}
		if after := city.entry(t); after != before {
			t.Fatalf("journal entry = %+v, want unchanged %+v", after, before)
		}
	})
}
