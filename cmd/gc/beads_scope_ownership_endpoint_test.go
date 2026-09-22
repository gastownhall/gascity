package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// A `gc init --beads-target external` that dies after journaling its pending
// record used to strand the city: the endpoint lived only in the failed
// process, so every retry died inside the provider adapter for want of it. The
// pending record now carries the endpoint, and a retry in a fresh process
// recovers it.
func TestPendingExternalProviderScopeRecoversItsEndpointFromTheJournal(t *testing.T) {
	for _, transport := range []string{"direct", "proxied"} {
		t.Run(transport, func(t *testing.T) {
			city := t.TempDir()
			logPath := filepath.Join(city, "provider-ops")
			script := filepath.Join(city, "gc-beads-bd.sh")
			recorder := "#!/bin/sh\nprintf '%s|%s|%s|%s\\n' \"$*\" \"${GC_DOLT_HOST:-}${GC_BEADS_PROXY_EXTERNAL_HOST:-}\" \"${GC_DOLT_PORT:-}${GC_BEADS_PROXY_EXTERNAL_PORT:-}\" \"${GC_BEADS_TARGET:-}\" >> \"$GC_TEST_PROVIDER_LOG\"\n"
			if err := os.WriteFile(script, []byte(recorder), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GC_TEST_PROVIDER_LOG", logPath)
			if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"test\"\n[beads]\nprovider = \"exec:"+script+"\"\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			// First attempt: journal the pending record with its endpoint, then
			// lose the process-local registration the way a crash would.
			opts := hostedDoltInitOptions{Transport: transport, Target: "external", Host: "db.example.test", Port: "4406", Database: "bd_external", ProjectID: "project"}
			if err := persistFreshProviderOwnership(city, opts); err != nil {
				t.Fatalf("persistFreshProviderOwnership: %v", err)
			}
			selectorExternalInitOptions.Delete(normalizePathForCompare(city))
			clearCityDoltConfig(city)
			t.Cleanup(func() {
				selectorExternalInitOptions.Delete(normalizePathForCompare(city))
				clearCityDoltConfig(city)
			})

			entry, owned, err := providerScopeOwnership(city, city)
			if err != nil || !owned {
				t.Fatalf("providerScopeOwnership = (%+v, %v, %v)", entry, owned, err)
			}
			want := providerScopeEndpoint{Host: "db.example.test", Port: "4406", Database: "bd_external"}
			if entry.Endpoint != want {
				t.Fatalf("journaled endpoint = %+v, want %+v", entry.Endpoint, want)
			}

			// Retry in a process with no selector registration and no explicit
			// environment: the journal is the only surviving authority.
			cfg, err := loadCityConfig(city, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if err := startBeadsLifecycle(city, "", cfg, io.Discard); err != nil {
				t.Fatalf("retried pending external init: %v", err)
			}
			data, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			wantInit := "init " + city + " " + config.EffectiveHQPrefix(cfg) + " bd_external|db.example.test|4406|external"
			if len(lines) == 0 || lines[0] != wantInit {
				t.Fatalf("retried init = %#v, want first line %q", lines, wantInit)
			}
			readyEntry, owned, err := providerScopeOwnership(city, city)
			if err != nil || !owned || readyEntry.State != providerScopeReady {
				t.Fatalf("retried scope ownership = (%+v, %v, %v), want ready", readyEntry, owned, err)
			}
			if readyEntry.Endpoint != (providerScopeEndpoint{}) {
				t.Fatalf("ready entry retained its initialization endpoint: %+v", readyEntry.Endpoint)
			}
		})
	}
}

// A local scope owns its own listener, so an endpoint in its journal record is
// a contradiction both the writer and the loader must refuse.
func TestProviderScopeOwnershipRejectsEndpointOnLocalScope(t *testing.T) {
	city := t.TempDir()
	if err := persistProviderScopeOwnershipWithEndpoint(city, city, providerScopeIntent{Transport: "proxied", Target: "local"}, providerScopeEndpoint{Host: "db.example.test", Port: "4406"}); err == nil {
		t.Fatal("persisted an external endpoint for a local scope")
	}
	if err := os.MkdirAll(filepath.Join(city, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	journal := `{"version":1,"scopes":{"city":{"scope_path":"` + normalizePathForCompare(city) +
		`","lifecycle_owner":"provider","state":"provider_initializing",` +
		`"intent":{"transport":"proxied","target":"local"},"endpoint":{"host":"db.example.test","port":"4406"}}}}`
	if err := os.WriteFile(filepath.Join(city, ".gc", "scope-ownership.json"), []byte(journal), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := providerScopeOwnership(city, city); err == nil {
		t.Fatal("loaded a local scope record carrying an external endpoint")
	}
}

// Marking a scope ready clears the endpoint with the intent: bd's own binding
// is authoritative from that point, and a stale copy would outrank it on a
// later repair.
func TestMarkProviderScopeOwnershipReadyClearsJournaledEndpoint(t *testing.T) {
	city := t.TempDir()
	intent := providerScopeIntent{Transport: "direct", Target: "external"}
	endpoint := providerScopeEndpoint{Host: "db.example.test", Port: "4406", Database: "bd_external"}
	if err := persistProviderScopeOwnershipWithEndpoint(city, city, intent, endpoint); err != nil {
		t.Fatal(err)
	}
	if err := markProviderScopeOwnershipReady(city, city); err != nil {
		t.Fatal(err)
	}
	entry, owned, err := providerScopeOwnership(city, city)
	if err != nil || !owned {
		t.Fatalf("providerScopeOwnership = (%+v, %v, %v)", entry, owned, err)
	}
	if entry.Endpoint != (providerScopeEndpoint{}) {
		t.Fatalf("ready entry = %+v, want no retained endpoint", entry)
	}
	data, err := os.ReadFile(filepath.Join(city, ".gc", "scope-ownership.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "db.example.test") {
		t.Fatalf("ready ownership journal persisted an endpoint: %s", data)
	}
}

// A partial endpoint would pass the pending-endpoint check and then fail
// inside bd, so it is dropped rather than journaled.
func TestNormalizeProviderScopeEndpointDropsPartialEndpoints(t *testing.T) {
	for _, tt := range []struct {
		name string
		in   providerScopeEndpoint
		want providerScopeEndpoint
	}{
		{name: "host without port", in: providerScopeEndpoint{Host: "db.example.test"}, want: providerScopeEndpoint{}},
		{name: "port without host", in: providerScopeEndpoint{Port: "4406"}, want: providerScopeEndpoint{}},
		{name: "database survives alone", in: providerScopeEndpoint{Database: "bd_external"}, want: providerScopeEndpoint{Database: "bd_external"}},
		{name: "whitespace is trimmed", in: providerScopeEndpoint{Host: " db ", Port: " 4406 ", Database: " bd "}, want: providerScopeEndpoint{Host: "db", Port: "4406", Database: "bd"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeProviderScopeEndpoint(tt.in); got != tt.want {
				t.Fatalf("normalizeProviderScopeEndpoint(%+v) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}
