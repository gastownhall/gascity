package contract

import (
	"path/filepath"
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
		// a gc-managed legacy scope carries a binding too. The host is what
		// tells the two apart, and that scope must still be canonicalised.
		"loopback binding is gc's own managed server": {
			metadata: `{"database":"dolt","backend":"dolt","dolt_mode":"server",` +
				`"dolt_server_host":"127.0.0.1","dolt_server_port":3307,"dolt_database":"hq"}`,
			config: func(t *testing.T, scopeRoot string) { writeBdTemplateConfig(t, fs, scopeRoot, "gc") },
			want:   false,
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
