package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// The provider lifecycle ops on a READY scope, one case per supported init
// topology.
//
// TestGcBeadsBdProviderOwnedLifecycleUsesBdBoundary drives the same ops with
// GC_BEADS_TRANSPORT and GC_BEADS_TARGET set, which is the pending-init shape:
// gc projects the intent it journaled while bd is still creating the store.
// A ready scope has no intent — the journal clears it — so the adapter has to
// read the topology back out of the binding bd persisted, and getting that
// wrong is how `gc stop` ends up issuing a local Dolt lifecycle command for a
// server somebody else runs, or skipping a proxy it owns.
//
// This is the whole init matrix at the adapter boundary, with a fake bd and no
// Dolt at all, so it runs in the ordinary cmd/gc suite rather than the
// acceptance tier.

// readyTopologyScope writes the on-disk artifacts one init shape leaves behind.
type readyTopologyScope struct {
	// beadsConfig is .beads/config.yaml. bd's own template carries a prefix and
	// nothing gc wrote; gc canonicalizes only the scopes it owns.
	beadsConfig string
	// metadata is the raw .beads/metadata.json.
	metadata string
	// sidecar, when set, is .beads/proxied_server_client_info.json.
	sidecar string
}

func writeReadyTopologyScope(t *testing.T, scopeDir string, shape readyTopologyScope) {
	t.Helper()
	beadsDir := filepath.Join(scopeDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []struct {
		name    string
		content string
	}{
		{"config.yaml", shape.beadsConfig},
		{"metadata.json", shape.metadata},
		{"proxied_server_client_info.json", shape.sidecar},
	} {
		path := filepath.Join(beadsDir, f.name)
		if f.content == "" {
			if err := os.RemoveAll(path); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(path, []byte(f.content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

const (
	// bdTemplateBeadsConfig is what bd's own init writes: a prefix, and none of
	// the gc.* keys gc adds when it canonicalizes a scope it owns.
	bdTemplateBeadsConfig = "issue_prefix: prov\nissue-prefix: prov\n"

	proxiedLocalMetadata    = `{"database":"dolt","backend":"dolt","dolt_mode":"proxied-server","dolt_database":"prov"}`
	directLocalMetadata     = `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"prov"}`
	directExternalMetadata  = `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_server_host":"db.example","dolt_server_port":4406,"dolt_database":"hosted"}`
	residentProxySidecar    = `{"idle_timeout":-1}`
	externalProxySidecar    = `{"idle_timeout":-1,"external":{"host":"db.example","port":4406}}`
	canonicalExternalConfig = "issue_prefix: prov\ngc.endpoint_origin: city_canonical\ngc.endpoint_status: verified\ndolt.host: db.example\ndolt.port: 4406\ndolt.mode: server\ndolt.auto-start: false\n"
	legacyManagedConfig     = "issue_prefix: prov\ngc.endpoint_origin: managed_city\ngc.endpoint_status: verified\ndolt.mode: server\ndolt.auto-start: false\n"
)

func TestGcBeadsBdReadyScopeLifecycleReadsItsPersistedTopology(t *testing.T) {
	scriptPath := filepath.Join(repoRootForLint(t), "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")

	for _, tt := range []struct {
		name  string
		doc   string
		shape readyTopologyScope
		// wantByOp is the exact sequence of bd sub-commands each op must issue.
		wantByOp map[string][]string
	}{
		{
			name: "M1 proxied-local retires the proxy it owns",
			doc:  "the default: bd's proxy and its Dolt child live under this scope",
			shape: readyTopologyScope{
				beadsConfig: bdTemplateBeadsConfig,
				metadata:    proxiedLocalMetadata,
				sidecar:     residentProxySidecar,
			},
			wantByOp: map[string][]string{
				"start": {"ping"},
				"stop":  {"dolt stop"},
			},
		},
		{
			name: "M2 direct-local retires the server bd started here",
			doc:  "the escape hatch: bd owns a server-mode Dolt in this scope, no proxy",
			shape: readyTopologyScope{
				beadsConfig: bdTemplateBeadsConfig,
				metadata:    directLocalMetadata,
			},
			wantByOp: map[string][]string{
				"start": {"ping"},
				"stop":  {"dolt stop"},
			},
		},
		{
			name: "M3a direct-external alias never controls the endpoint",
			doc:  "the legacy --dolt-host alias: a canonical city endpoint somebody else runs",
			shape: readyTopologyScope{
				beadsConfig: canonicalExternalConfig,
				metadata:    directLocalMetadata,
			},
			wantByOp: map[string][]string{
				"start": {"ping"},
				"stop":  nil,
			},
		},
		{
			// bd records the upstream it was initialized against in the scope's
			// own metadata, and a provider-owned scope has no gc.* config keys
			// to read instead. Without that record this scope looked local and
			// `gc stop` issued a Dolt lifecycle command for someone else's
			// server.
			name: "M3b direct-external selector never controls the endpoint",
			doc:  "the transport/target selector: bd persisted the upstream in metadata",
			shape: readyTopologyScope{
				beadsConfig: bdTemplateBeadsConfig,
				metadata:    directExternalMetadata,
			},
			wantByOp: map[string][]string{
				"start": {"ping"},
				"stop":  nil,
			},
		},
		{
			// The proxy is ours even when the data is not: stop retires the
			// local child and leaves the upstream alone, which is a lifecycle
			// command bd scopes to its own proxy root.
			name: "M4 proxied-external retires its local proxy only",
			doc:  "a local bd proxy fronting an external server",
			shape: readyTopologyScope{
				beadsConfig: bdTemplateBeadsConfig,
				metadata:    proxiedLocalMetadata,
				sidecar:     externalProxySidecar,
			},
			wantByOp: map[string][]string{
				"start": {"ping"},
				"stop":  {"dolt stop"},
			},
		},
		{
			// A GC-managed direct scope disables bd auto-start to keep a second
			// server from appearing, but its Dolt is still local and still ours
			// to retire. Ordinary managed cities never reach this op — they are
			// not provider-owned — so this case pins the transferred-scope arm.
			name: "M5 legacy GC-managed direct is still a local lifecycle",
			doc:  "the pre-journal managed-city shape, transferred to the provider lifecycle",
			shape: readyTopologyScope{
				beadsConfig: legacyManagedConfig,
				metadata:    directLocalMetadata,
			},
			wantByOp: map[string][]string{
				"start": {"ping"},
				"stop":  {"dolt stop"},
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Logf("%s", tt.doc)
			for _, op := range []string{"start", "stop"} {
				t.Run(op, func(t *testing.T) {
					cityDir := t.TempDir()
					scopeDir := filepath.Join(cityDir, "rigs", "provider")
					writeReadyTopologyScope(t, scopeDir, tt.shape)

					logPath := filepath.Join(t.TempDir(), "bd.log")
					bdPath := filepath.Join(t.TempDir(), "bd")
					if err := os.WriteFile(bdPath, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+strconv.Quote(logPath)+"\n"), 0o755); err != nil {
						t.Fatal(err)
					}

					// No GC_BEADS_TRANSPORT and no GC_BEADS_TARGET: a ready
					// scope carries no intent, so the adapter has only the
					// binding on disk to go on.
					cmd := exec.Command(scriptPath, op) //nolint:gosec // fixed repo script path
					cmd.Env = sanitizedBaseEnv(
						"GC_CITY_PATH="+cityDir,
						"BEADS_DIR="+filepath.Join(scopeDir, ".beads"),
						"BD_BIN="+bdPath,
						"GC_BEADS_PROVIDER_OWNED=1",
					)
					if out, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("gc-beads-bd %s: %v\n%s", op, err, out)
					}

					want := tt.wantByOp[op]
					data, err := os.ReadFile(logPath)
					if os.IsNotExist(err) && len(want) == 0 {
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					got := strings.FieldsFunc(strings.TrimSpace(string(data)), func(r rune) bool { return r == '\n' })
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("bd commands for %s = %#v, want %#v", op, got, want)
					}
				})
			}
		})
	}
}
