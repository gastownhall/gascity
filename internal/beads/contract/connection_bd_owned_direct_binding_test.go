package contract

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// ScopeCarriesBdOwnedDirectBinding is what keeps gc's boot-time canonicalizer
// off a scope bd owns. The stamp that canonicalizer would otherwise write —
// `gc.endpoint_origin` — is the very discriminator ResolveDoltConnectionTarget's
// bd-owned fallbacks rest on, so a scope carrying bd's binding and none of gc's
// endpoint keys has to be recognized before anything writes to it.
func TestScopeCarriesBdOwnedDirectBinding(t *testing.T) {
	fs := fsys.OSFS{}
	for name, tc := range map[string]struct {
		metadata string
		config   func(t *testing.T, scopeRoot string)
		want     bool
	}{
		"bd template config and an external binding": {
			metadata: bdExternalTCPMetadata,
			config:   func(t *testing.T, scopeRoot string) { writeBdTemplateConfig(t, fs, scopeRoot, "gc") },
			want:     true,
		},
		"no config at all": {
			metadata: bdExternalTCPMetadata,
			want:     true,
		},
		// The gc-managed legacy non-regression: bd's `init --server` records
		// whichever server it was pointed at, gc's own managed one included, so
		// a gc-managed legacy scope carries a binding too. A loopback host does
		// not tell the two apart; the port gc recorded for the city's managed
		// Dolt does, and that scope must still be canonicalised.
		"loopback binding on gc's recorded managed port is gc's own server": {
			metadata: `{"database":"dolt","backend":"dolt","dolt_mode":"server",` +
				`"dolt_server_host":"127.0.0.1","dolt_server_port":3307,"dolt_database":"hq"}`,
			config: func(t *testing.T, scopeRoot string) {
				writeBdTemplateConfig(t, fs, scopeRoot, "gc")
				dir := filepath.Join(scopeRoot, ".gc", "runtime", "packs", "dolt")
				if err := fs.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := fs.WriteFile(filepath.Join(dir, "dolt-state.json"), []byte(`{"running":false,"pid":0,"port":3307}`), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: false,
		},
		// And the shape the old host rule refused: a dolt sql-server the
		// operator runs on this machine, in a city gc runs no Dolt for.
		"loopback binding with no gc-managed record is bd's": {
			metadata: `{"database":"dolt","backend":"dolt","dolt_mode":"server",` +
				`"dolt_server_host":"127.0.0.1","dolt_server_port":3307,"dolt_database":"hq"}`,
			config: func(t *testing.T, scopeRoot string) { writeBdTemplateConfig(t, fs, scopeRoot, "gc") },
			want:   true,
		},
		// The record settles a loopback binding only. A hosted binding is bd's
		// even on a port both of gc's records name: 3307 is gc's legacy default
		// and a common Dolt port, so a record must never claim a hosted store.
		"hosted binding on gc's recorded managed port is bd's": {
			metadata: `{"database":"dolt","backend":"dolt","dolt_mode":"server",` +
				`"dolt_server_host":"db.example","dolt_server_port":3307,"dolt_database":"hosted"}`,
			config: func(t *testing.T, scopeRoot string) {
				writeBdTemplateConfig(t, fs, scopeRoot, "gc")
				dir := filepath.Join(scopeRoot, ".gc", "runtime", "packs", "dolt")
				if err := fs.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{"dolt-state.json", "dolt-provider-state.json"} {
					if err := fs.WriteFile(filepath.Join(dir, name), []byte(`{"running":true,"pid":1,"port":3307}`), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			},
			want: true,
		},
		"socket binding names this host": {
			metadata: `{"database":"dolt","backend":"dolt","dolt_mode":"server",` +
				`"dolt_server_socket":"/var/run/dolt.sock","dolt_database":"hosted"}`,
			config: func(t *testing.T, scopeRoot string) { writeBdTemplateConfig(t, fs, scopeRoot, "gc") },
			want:   false,
		},
		"no binding at all": {
			metadata: `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`,
			config:   func(t *testing.T, scopeRoot string) { writeBdTemplateConfig(t, fs, scopeRoot, "gc") },
			want:     false,
		},
		"gc-authoritative config outranks the binding": {
			metadata: bdExternalTCPMetadata,
			config: func(t *testing.T, scopeRoot string) {
				if _, err := EnsureCanonicalConfig(fs, filepath.Join(scopeRoot, ".beads", "config.yaml"), ConfigState{
					IssuePrefix:    "gc",
					EndpointOrigin: EndpointOriginManagedCity,
					EndpointStatus: EndpointStatusVerified,
					DoltMode:       "server",
				}); err != nil {
					t.Fatal(err)
				}
			},
			want: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			city := t.TempDir()
			writeRawMetadata(t, fs, city, tc.metadata)
			if tc.config != nil {
				tc.config(t, city)
			}
			got, err := ScopeCarriesBdOwnedDirectBinding(fs, city, city, "gc")
			if err != nil {
				t.Fatalf("ScopeCarriesBdOwnedDirectBinding(): %v", err)
			}
			if got != tc.want {
				t.Fatalf("ScopeCarriesBdOwnedDirectBinding() = %v, want %v", got, tc.want)
			}
		})
	}
}

// GC_DOLT_HOST redirects gc's own managed target away from loopback, so a
// binding naming that host is still a record of gc's server, not bd's upstream.
func TestScopeCarriesBdOwnedDirectBindingIgnoresTheManagedHostOverride(t *testing.T) {
	fs := fsys.OSFS{}
	t.Setenv(ManagedCityHostEnv, "dolt.internal")
	city := t.TempDir()
	writeBdTemplateConfig(t, fs, city, "gc")
	writeRawMetadata(t, fs, city, `{"database":"dolt","backend":"dolt","dolt_mode":"server",`+
		`"dolt_server_host":"dolt.internal","dolt_server_port":3307,"dolt_database":"hq"}`)

	got, err := ScopeCarriesBdOwnedDirectBinding(fs, city, city, "gc")
	if err != nil {
		t.Fatalf("ScopeCarriesBdOwnedDirectBinding(): %v", err)
	}
	if got {
		t.Fatalf("a binding naming gc's own managed host classified as bd-owned")
	}
}

// A loopback binding names a server on this host, and gc's own managed Dolt is
// only one of the servers that can be spelled that way. Only gc's own runtime
// record for the city can claim a loopback port.
func TestLoopbackBindingClassifiedByManagedRuntimeRecord(t *testing.T) {
	fs := fsys.OSFS{}
	const loopbackBinding = `{"database":"dolt","backend":"dolt","dolt_mode":"server",` +
		`"dolt_server_host":"127.0.0.1","dolt_server_port":17333,"dolt_database":"hv"}`
	for name, tc := range map[string]struct {
		published string
		provider  string
		want      bool
	}{
		"no gc-managed runtime record: the loopback server is bd's upstream": {want: true},
		"gc's published record names another port": {
			published: `{"running":true,"pid":1,"port":3307,"data_dir":"x"}`, want: true,
		},
		"gc's published record names this port, server stopped": {
			published: `{"running":false,"pid":0,"port":17333,"data_dir":"x"}`, want: false,
		},
		"gc's provider state names this port": {
			provider: `{"running":false,"pid":0,"port":17333,"data_dir":"x"}`, want: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			city := t.TempDir()
			rig := filepath.Join(city, "rigs", "hv")
			writeRawMetadata(t, fs, rig, loopbackBinding)
			writeBdTemplateConfig(t, fs, rig, "hv")
			runtimeDir := filepath.Join(city, ".gc", "runtime", "packs", "dolt")
			if err := fs.MkdirAll(runtimeDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if tc.published != "" {
				if err := fs.WriteFile(filepath.Join(runtimeDir, "dolt-state.json"), []byte(tc.published), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.provider != "" {
				if err := fs.WriteFile(filepath.Join(runtimeDir, "dolt-provider-state.json"), []byte(tc.provider), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := ScopeCarriesBdOwnedDirectBinding(fs, city, rig, "hv")
			if err != nil {
				t.Fatalf("ScopeCarriesBdOwnedDirectBinding(): %v", err)
			}
			if got != tc.want {
				t.Fatalf("ScopeCarriesBdOwnedDirectBinding() = %v, want %v", got, tc.want)
			}
		})
	}
}

// A record gc cannot read is not a guess either way; the classification must
// fail and name the file.
func TestMalformedManagedRuntimeRecordIsAnError(t *testing.T) {
	fs := fsys.OSFS{}
	for _, name := range []string{"dolt-state.json", "dolt-provider-state.json"} {
		t.Run(name, func(t *testing.T) {
			city := t.TempDir()
			rig := filepath.Join(city, "rigs", "hv")
			writeRawMetadata(t, fs, rig, `{"database":"dolt","backend":"dolt","dolt_mode":"server",`+
				`"dolt_server_host":"127.0.0.1","dolt_server_port":17333,"dolt_database":"hv"}`)
			writeBdTemplateConfig(t, fs, rig, "hv")
			dir := filepath.Join(city, ".gc", "runtime", "packs", "dolt")
			if err := fs.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := fs.WriteFile(filepath.Join(dir, name), []byte("{not json"), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := ScopeCarriesBdOwnedDirectBinding(fs, city, rig, "hv")
			if err == nil {
				t.Fatalf("ScopeCarriesBdOwnedDirectBinding() = %v, nil; want an error naming %s", got, name)
			}
			if !strings.Contains(err.Error(), name) {
				t.Fatalf("error %q does not name %s", err, name)
			}
		})
	}
}

// Every loopback spelling DoltHostIsLocal accepts is classified by the record,
// not by its spelling.
func TestLoopbackSpellingsWithNoRecordAreBds(t *testing.T) {
	fs := fsys.OSFS{}
	for _, host := range []string{"127.0.0.1", "localhost", "::1", "127.0.0.2"} {
		t.Run(host, func(t *testing.T) {
			city := t.TempDir()
			rig := filepath.Join(city, "rigs", "hv")
			writeRawMetadata(t, fs, rig, `{"database":"dolt","backend":"dolt","dolt_mode":"server",`+
				`"dolt_server_host":"`+host+`","dolt_server_port":17333,"dolt_database":"hv"}`)
			writeBdTemplateConfig(t, fs, rig, "hv")
			got, err := ScopeCarriesBdOwnedDirectBinding(fs, city, rig, "hv")
			if err != nil {
				t.Fatalf("ScopeCarriesBdOwnedDirectBinding(): %v", err)
			}
			if !got {
				t.Fatalf("loopback %q binding with no gc record classified as gc's", host)
			}
		})
	}
}

// The record is read only where it decides the answer, so a malformed record
// never fails a scope whose gc-authoritative config already says it is gc's.
func TestMalformedRecordIsNotReadWhenConfigDecides(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	writeRawMetadata(t, fs, city, `{"database":"dolt","backend":"dolt","dolt_mode":"server",`+
		`"dolt_server_host":"127.0.0.1","dolt_server_port":3307,"dolt_database":"hq"}`)
	if _, err := EnsureCanonicalConfig(fs, filepath.Join(city, ".beads", "config.yaml"), ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: EndpointOriginManagedCity,
		EndpointStatus: EndpointStatusVerified,
		DoltMode:       "server",
	}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(city, ".gc", "runtime", "packs", "dolt")
	if err := fs.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(filepath.Join(dir, "dolt-state.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ScopeCarriesBdOwnedDirectBinding(fs, city, city, "gc")
	if err != nil || got {
		t.Fatalf("ScopeCarriesBdOwnedDirectBinding() = %v, %v; want false, nil", got, err)
	}
}
