package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeMigrateProxiedScopeFixture lays out a city and one rig for the
// classifier: the city's own canonical config, and a rig that inherits it.
func writeMigrateProxiedScopeFixture(t *testing.T, cityConfig, rigConfig string) (string, migrateProxiedScope) {
	t.Helper()
	city := handoffGuardTestCity(t)
	rig := filepath.Join(city, "rigs", "testrig")
	for dir, body := range map[string]string{city: cityConfig, rig: rigConfig} {
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".beads", "metadata.json"),
			[]byte(`{"backend":"dolt","database":"dolt","dolt_mode":"server","dolt_database":"te"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".beads", "config.yaml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return city, migrateProxiedScope{Name: "testrig", Label: "rig:testrig", Path: rig}
}

// A city handed to bd publishes a canonical endpoint, and gc then mirrors that
// endpoint into every inherited rig's config so bd can open the rig scope. The
// "pins dolt.host" refusal is for a rig that tracks a server somewhere else;
// firing it on gc's own mirror left a handed-off city's rig with no supported
// hop to the proxied default at all.
func TestMigrateProxiedAdmitsARigMirroringItsCanonicalCity(t *testing.T) {
	city, scope := writeMigrateProxiedScopeFixture(t,
		"issue_prefix: hq\ngc.endpoint_origin: city_canonical\ndolt.host: 127.0.0.1\ndolt.port: 50301\n",
		"issue_prefix: te\ngc.endpoint_origin: inherited_city\ndolt.host: 127.0.0.1\ndolt.port: 50301\n")
	if _, err := classifyMigrateProxiedScope(city, scope); err != nil && strings.Contains(err.Error(), "pins dolt.host") {
		t.Fatalf("a rig mirroring its city's canonical endpoint was refused as external: %v", err)
	}
}

// The relaxation must not widen into "any rig endpoint is fine". A rig that
// claims a different server, or only half of one, is still an external pin.
func TestMigrateProxiedStillRefusesARigWithItsOwnEndpoint(t *testing.T) {
	for _, tt := range []struct {
		name      string
		cityBody  string
		rigBody   string
		wantRefus bool
	}{
		{
			name:      "different host",
			cityBody:  "gc.endpoint_origin: city_canonical\ndolt.host: 127.0.0.1\ndolt.port: 50301\n",
			rigBody:   "gc.endpoint_origin: inherited_city\ndolt.host: dolt.example.test\ndolt.port: 50301\n",
			wantRefus: true,
		},
		{
			name:      "different port",
			cityBody:  "gc.endpoint_origin: city_canonical\ndolt.host: 127.0.0.1\ndolt.port: 50301\n",
			rigBody:   "gc.endpoint_origin: inherited_city\ndolt.host: 127.0.0.1\ndolt.port: 50302\n",
			wantRefus: true,
		},
		{
			name:      "host without a port",
			cityBody:  "gc.endpoint_origin: city_canonical\ndolt.host: 127.0.0.1\ndolt.port: 50301\n",
			rigBody:   "gc.endpoint_origin: inherited_city\ndolt.host: 127.0.0.1\n",
			wantRefus: true,
		},
		{
			name:      "city is not canonical",
			cityBody:  "gc.endpoint_origin: managed_city\n",
			rigBody:   "gc.endpoint_origin: inherited_city\ndolt.host: 127.0.0.1\ndolt.port: 50301\n",
			wantRefus: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			city, scope := writeMigrateProxiedScopeFixture(t, tt.cityBody, tt.rigBody)
			_, err := classifyMigrateProxiedScope(city, scope)
			refused := err != nil && strings.Contains(err.Error(), "pins dolt.host")
			if refused != tt.wantRefus {
				t.Fatalf("classify refusal = %v (err %v), want %v", refused, err, tt.wantRefus)
			}
		})
	}
}
