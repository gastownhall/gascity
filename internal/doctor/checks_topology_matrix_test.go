package doctor

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// The init topology matrix: every supported way a Gas City beads scope can be
// bound, as it appears on disk. The acceptance tier exercises these against a
// real bd and dolt and skips without them, so these tests carry the same
// shapes through the doctor checks from fixtures alone.
//
//	M1  proxied-local      bd owns proxy + child dolt; nothing for gc to dial
//	M2  direct-local       bd started a server and recorded it in the scope
//	M3a direct-external    gc canonicalized the endpoint into config.yaml
//	M3b direct-external    bd persisted the endpoint into metadata.json
//	M4  proxied-external   bd's proxy fronts an external upstream
//	M5  legacy GC-managed  gc started the server and owns the runtime state
//
// Every shape resolves to a live loopback listener rather than the documented
// db.example:4406 because doctor's verdict is a dial: a shape can only be
// asserted healthy against an endpoint that answers.

// writeBdTemplateConfig writes the .beads/config.yaml bd's own init leaves
// behind: a prefix and nothing gc wrote. gc does not canonicalize a
// provider-owned scope, so this is the real on-disk config for every shape
// whose endpoint lives outside config.yaml.
func writeBdTemplateConfig(t *testing.T, dir, prefix string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".beads", "config.yaml"), []byte("issue_prefix: "+prefix+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeDoctorRawMetadata writes metadata.json verbatim. M3b's endpoint keys
// (dolt_server_host / dolt_server_port) are bd's, not gc's, so the canonical
// metadata writer cannot produce them.
func writeDoctorRawMetadata(t *testing.T, dir, raw string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".beads", "metadata.json"), []byte(raw+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeDoctorBdOwnedServerRecord writes the pid/port pair bd leaves for a
// server it started. The pid must name a live process and the port must
// answer, or the record reads as a crashed server rather than a binding.
func writeDoctorBdOwnedServerRecord(t *testing.T, dir string) string {
	t.Helper()
	port := listenLoopbackPort(t)
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "dolt-server.pid"), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "dolt-server.port"), []byte(port+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return port
}

// --- city shapes: each returns the endpoint doctor must name, "" when the
// shape has no endpoint gc dials. ---

func buildProxiedLocalCity(t *testing.T, dir string) string {
	t.Helper()
	writeBdTemplateConfig(t, dir, "gc")
	writeDoctorProxiedMetadata(t, dir, "hq")
	writeDoctorSidecar(t, dir, `{"idle_timeout":-1}`)
	return ""
}

func buildDirectLocalCity(t *testing.T, dir string) string {
	t.Helper()
	writeBdTemplateConfig(t, dir, "gc")
	writeDoctorCanonicalMetadata(t, fsys.OSFS{}, dir, "hq")
	return "127.0.0.1:" + writeDoctorBdOwnedServerRecord(t, dir)
}

func buildDirectExternalAliasCity(t *testing.T, dir string) string {
	t.Helper()
	port := listenLoopbackPort(t)
	writeDoctorCanonicalConfig(t, fsys.OSFS{}, dir, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginCityCanonical,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "127.0.0.1",
		DoltPort:       port,
		DoltMode:       "server",
	})
	writeDoctorCanonicalMetadata(t, fsys.OSFS{}, dir, "hq")
	return "127.0.0.1:" + port
}

func buildDirectExternalSelectorCity(t *testing.T, dir string) string {
	t.Helper()
	port := listenLoopbackPort(t)
	writeBdTemplateConfig(t, dir, "gc")
	writeDoctorRawMetadata(t, dir, fmt.Sprintf(
		`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_server_host":"127.0.0.1","dolt_server_port":%s,"dolt_database":"hq"}`, port))
	return "127.0.0.1:" + port
}

func buildProxiedExternalCity(t *testing.T, dir string) string {
	t.Helper()
	port := listenLoopbackPort(t)
	writeBdTemplateConfig(t, dir, "gc")
	writeDoctorProxiedMetadata(t, dir, "hq")
	writeDoctorSidecar(t, dir, fmt.Sprintf(`{"idle_timeout":-1,"external":{"host":"127.0.0.1","port":%s}}`, port))
	return "127.0.0.1:" + port
}

func buildLegacyManagedCity(t *testing.T, dir string) string {
	t.Helper()
	port := listenLoopbackPort(t)
	writeDoctorCanonicalConfig(t, fsys.OSFS{}, dir, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginManagedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	writeDoctorCanonicalMetadata(t, fsys.OSFS{}, dir, "hq")
	writeDoctorRuntimeState(t, fsys.OSFS{}, dir, port)
	return "127.0.0.1:" + port
}

// --- rig shapes: build the city of the same shape, then the rig. M1/M2/M3b/M4
// rigs carry their own binding of the same kind; M3a/M5 rigs inherit the city
// endpoint and own none. Each returns the endpoint the rig resolves to. ---

func buildProxiedLocalRig(t *testing.T, cityDir, rigDir string) string {
	t.Helper()
	buildProxiedLocalCity(t, cityDir)
	writeBdTemplateConfig(t, rigDir, "de")
	writeDoctorProxiedMetadata(t, rigDir, "de")
	writeDoctorSidecar(t, rigDir, `{"idle_timeout":-1}`)
	return ""
}

func buildDirectLocalRig(t *testing.T, cityDir, rigDir string) string {
	t.Helper()
	cityEndpoint := buildDirectLocalCity(t, cityDir)
	writeBdTemplateConfig(t, rigDir, "de")
	writeDoctorCanonicalMetadata(t, fsys.OSFS{}, rigDir, "de")
	endpoint := "127.0.0.1:" + writeDoctorBdOwnedServerRecord(t, rigDir)
	if endpoint == cityEndpoint {
		t.Fatalf("fixture gave the rig the city's endpoint %s", cityEndpoint)
	}
	return endpoint
}

func buildDirectExternalAliasRig(t *testing.T, cityDir, rigDir string) string {
	t.Helper()
	cityEndpoint := buildDirectExternalAliasCity(t, cityDir)
	host, port, err := net.SplitHostPort(cityEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	// An inherited rig under a canonical city must mirror the city endpoint
	// exactly; a mismatch is the drift the rig check reports as an error.
	writeDoctorCanonicalConfig(t, fsys.OSFS{}, rigDir, contract.ConfigState{
		IssuePrefix:    "de",
		EndpointOrigin: contract.EndpointOriginInheritedCity,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       host,
		DoltPort:       port,
		DoltMode:       "server",
	})
	writeDoctorCanonicalMetadata(t, fsys.OSFS{}, rigDir, "de")
	return cityEndpoint
}

func buildDirectExternalSelectorRig(t *testing.T, cityDir, rigDir string) string {
	t.Helper()
	buildDirectExternalSelectorCity(t, cityDir)
	port := listenLoopbackPort(t)
	writeBdTemplateConfig(t, rigDir, "de")
	writeDoctorRawMetadata(t, rigDir, fmt.Sprintf(
		`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_server_host":"127.0.0.1","dolt_server_port":%s,"dolt_database":"de"}`, port))
	return "127.0.0.1:" + port
}

func buildProxiedExternalRig(t *testing.T, cityDir, rigDir string) string {
	t.Helper()
	buildProxiedExternalCity(t, cityDir)
	port := listenLoopbackPort(t)
	writeBdTemplateConfig(t, rigDir, "de")
	writeDoctorProxiedMetadata(t, rigDir, "de")
	writeDoctorSidecar(t, rigDir, fmt.Sprintf(`{"idle_timeout":-1,"external":{"host":"127.0.0.1","port":%s}}`, port))
	return "127.0.0.1:" + port
}

func buildLegacyManagedRig(t *testing.T, cityDir, rigDir string) string {
	t.Helper()
	cityEndpoint := buildLegacyManagedCity(t, cityDir)
	// An inherited rig under a managed city must not track an endpoint of its
	// own: the city's runtime state is the only record of where the server is.
	writeDoctorCanonicalConfig(t, fsys.OSFS{}, rigDir, contract.ConfigState{
		IssuePrefix:    "de",
		EndpointOrigin: contract.EndpointOriginInheritedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	writeDoctorCanonicalMetadata(t, fsys.OSFS{}, rigDir, "de")
	return cityEndpoint
}

func TestDoltServerCheckAcrossInitTopologies(t *testing.T) {
	tests := []struct {
		name string
		// build lays the city scope down and returns the endpoint the check
		// must name, or "" when the shape has no endpoint gc dials.
		build func(t *testing.T, dir string) string
		// wantMessage is asserted as a substring; wantNamesEndpoint asserts
		// the resolved endpoint appears in the message.
		wantMessage       string
		wantNamesEndpoint bool
	}{
		{
			// bd owns the proxy and the child Dolt process; there is no
			// address for gc to dial and none is a healthy answer.
			name:        "M1 proxied-local",
			build:       buildProxiedLocalCity,
			wantMessage: "managed by beads proxied-server provider",
		},
		{
			// The server bd started is recorded only in the scope's own
			// pid/port pair, which is the record readProviderOwnedServerPort
			// reads; without it this shape resolves as a managed city with no
			// runtime state.
			name:              "M2 direct-local",
			build:             buildDirectLocalCity,
			wantNamesEndpoint: true,
		},
		{
			name:              "M3a direct-external alias",
			build:             buildDirectExternalAliasCity,
			wantNamesEndpoint: true,
		},
		{
			// The endpoint lives only in bd's metadata keys, which is what
			// bdExternalBindingTarget reads.
			name:              "M3b direct-external selector",
			build:             buildDirectExternalSelectorCity,
			wantNamesEndpoint: true,
		},
		{
			// A proxied scope with an external upstream is dialable: the
			// sidecar names the endpoint bd's proxy fronts.
			name:              "M4 proxied-external",
			build:             buildProxiedExternalCity,
			wantNamesEndpoint: true,
		},
		{
			name:              "M5 legacy GC-managed",
			build:             buildLegacyManagedCity,
			wantNamesEndpoint: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearInheritedBeadsEnv(t)
			dir := t.TempDir()
			endpoint := tt.build(t, dir)

			r := NewDoltServerCheck(dir, false).Run(&CheckContext{})
			if r.Status != StatusOK {
				t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
			}
			if tt.wantMessage != "" && !strings.Contains(r.Message, tt.wantMessage) {
				t.Errorf("message = %q, want it to contain %q", r.Message, tt.wantMessage)
			}
			if tt.wantNamesEndpoint && !strings.Contains(r.Message, endpoint) {
				t.Errorf("message = %q, want it to name the endpoint %s", r.Message, endpoint)
			}
		})
	}
}

func TestBeadsStoreCheckAcrossInitTopologies(t *testing.T) {
	tests := []struct {
		name        string
		build       func(t *testing.T, dir string) string
		wantMessage string
	}{
		{
			name:        "M1 proxied-local",
			build:       buildProxiedLocalCity,
			wantMessage: "store accessible",
		},
		{
			name:        "M2 direct-local",
			build:       buildDirectLocalCity,
			wantMessage: "store accessible",
		},
		{
			name:        "M3a direct-external alias",
			build:       buildDirectExternalAliasCity,
			wantMessage: "store accessible",
		},
		{
			name:        "M3b direct-external selector",
			build:       buildDirectExternalSelectorCity,
			wantMessage: "store accessible",
		},
		{
			name:        "M4 proxied-external",
			build:       buildProxiedExternalCity,
			wantMessage: "store accessible",
		},
		{
			name:        "M5 legacy GC-managed",
			build:       buildLegacyManagedCity,
			wantMessage: "store accessible",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearInheritedBeadsEnv(t)
			dir := t.TempDir()
			tt.build(t, dir)

			pinged := false
			spy := &spyPingStore{pingFunc: func() error {
				pinged = true
				return nil
			}}
			c := NewBeadsStoreCheck(dir, func(string) (beads.StoreOpenResult, error) {
				return beads.StoreOpenResult{Store: spy}, nil
			})
			r := c.Run(&CheckContext{})
			if r.Status != StatusOK {
				t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
			}
			if r.Message != tt.wantMessage {
				t.Errorf("message = %q, want %q", r.Message, tt.wantMessage)
			}
			// Reaching Ping is the point: the shape's endpoint resolved and
			// its preflight dial succeeded, so the store was really opened.
			if !pinged {
				t.Error("Ping was not called")
			}
		})
	}
}

func TestRigDoltServerCheckAcrossInitTopologies(t *testing.T) {
	tests := []struct {
		name              string
		build             func(t *testing.T, cityDir, rigDir string) string
		wantMessage       string
		wantNamesEndpoint bool
	}{
		{
			name:        "M1 proxied-local",
			build:       buildProxiedLocalRig,
			wantMessage: "not required (bd backend=dolt proxied-server)",
		},
		{
			// Current behavior, not the ideal one: the rig runs its own
			// bd-started server, but the check classifies a rig by whether its
			// config.yaml carries an explicit endpoint, and a bd-owned scope's
			// never does. So it reports the inherited-city skip and never
			// dials the server the rig actually resolves to. Healthy, but the
			// message names the wrong topology.
			name:        "M2 direct-local",
			build:       buildDirectLocalRig,
			wantMessage: "inherits city dolt endpoint",
		},
		{
			name:        "M3a direct-external alias",
			build:       buildDirectExternalAliasRig,
			wantMessage: "inherits city dolt endpoint",
		},
		{
			// Same gap as M2: the rig's own upstream lives in bd's metadata
			// keys, which the explicit-endpoint classification does not see.
			name:        "M3b direct-external selector",
			build:       buildDirectExternalSelectorRig,
			wantMessage: "inherits city dolt endpoint",
		},
		{
			// The external proxied branch runs before the explicit-endpoint
			// classification, so this shape is dialed and named.
			name:              "M4 proxied-external",
			build:             buildProxiedExternalRig,
			wantMessage:       "(proxied-server external)",
			wantNamesEndpoint: true,
		},
		{
			name:        "M5 legacy GC-managed",
			build:       buildLegacyManagedRig,
			wantMessage: "inherits city dolt endpoint",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearInheritedBeadsEnv(t)
			cityDir := t.TempDir()
			rigDir := filepath.Join(cityDir, "demo")
			endpoint := tt.build(t, cityDir, rigDir)

			r := NewRigDoltServerCheck(cityDir, config.Rig{Name: "demo", Path: rigDir}, false).Run(&CheckContext{})
			if r.Status != StatusOK {
				t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
			}
			if !strings.Contains(r.Message, tt.wantMessage) {
				t.Errorf("message = %q, want it to contain %q", r.Message, tt.wantMessage)
			}
			if tt.wantNamesEndpoint && !strings.Contains(r.Message, endpoint) {
				t.Errorf("message = %q, want it to name the endpoint %s", r.Message, endpoint)
			}
		})
	}
}

func TestRigBeadsCheckAcrossInitTopologies(t *testing.T) {
	tests := []struct {
		name        string
		build       func(t *testing.T, cityDir, rigDir string) string
		wantMessage string
	}{
		{
			// The rig's store is the bd CLI front door by design here, so the
			// message says so instead of reading as a degraded fallback.
			name:        "M1 proxied-local",
			build:       buildProxiedLocalRig,
			wantMessage: proxiedProviderStoreMessage,
		},
		{
			name:        "M2 direct-local",
			build:       buildDirectLocalRig,
			wantMessage: "store accessible",
		},
		{
			name:        "M3a direct-external alias",
			build:       buildDirectExternalAliasRig,
			wantMessage: "store accessible",
		},
		{
			name:        "M3b direct-external selector",
			build:       buildDirectExternalSelectorRig,
			wantMessage: "store accessible",
		},
		{
			// Proxied but with an external upstream: gc dials it, so this is
			// the ordinary reachable-store lens rather than the bd front door.
			name:        "M4 proxied-external",
			build:       buildProxiedExternalRig,
			wantMessage: "store accessible",
		},
		{
			name:        "M5 legacy GC-managed",
			build:       buildLegacyManagedRig,
			wantMessage: "store accessible",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearInheritedBeadsEnv(t)
			cityDir := t.TempDir()
			rigDir := filepath.Join(cityDir, "demo")
			tt.build(t, cityDir, rigDir)

			pinged := false
			spy := &spyPingStore{pingFunc: func() error {
				pinged = true
				return nil
			}}
			c := NewRigBeadsCheck(cityDir, config.Rig{Name: "demo", Path: rigDir}, func(string) (beads.Store, error) {
				return spy, nil
			})
			r := c.Run(&CheckContext{})
			if r.Status != StatusOK {
				t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
			}
			if r.Message != tt.wantMessage {
				t.Errorf("message = %q, want %q", r.Message, tt.wantMessage)
			}
			if !pinged {
				t.Error("Ping was not called")
			}
		})
	}
}
