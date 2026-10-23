package dolt_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fake-Dolt regressions for a city on managed Dolt whose rig is pinned to a
// different Dolt endpoint (`gc rig set-endpoint <rig> --external` or --self,
// a legacy config.yaml host or socket, or bd's binding to a remote host). The
// compactor talks only to the city's compaction target, and the pinned rig's
// database does not live there. It must be skipped with a log line: never
// queried on the managed server, never compacted as a same-named copy there,
// and never a reason to fail the run. Rigs that follow the city endpoint are
// still compacted. The real-Dolt counterpart is
// compact_rig_endpoint_real_dolt_test.go.

// compactCloudDoltHost is a Dolt host that is not on this machine, and
// compactCloudDoltEndpoint the endpoint a cloud-pinned rig names.
// compactCityDoltHost is a non-loopback name for the city's managed server,
// as a containerized deployment spells it.
const (
	compactCloudDoltHost     = "beads.dolt-cloud.example"
	compactCloudDoltEndpoint = compactCloudDoltHost + ":3306"
	compactCityDoltHost      = "dolt.city.example"
)

// compactUnknownEndpointSkip is how a scope's skip line names the reason when
// its config.yaml cannot be read.
const compactUnknownEndpointSkip = "has an unknown Dolt endpoint"

// compactCloudPinConfig is the canonical .beads/config.yaml that
// `gc rig set-endpoint <rig> --external --host beads.dolt-cloud.example
// --port 3306` writes.
func compactCloudPinConfig(prefix string) string {
	return "issue_prefix: " + prefix + "\n" +
		"gc.endpoint_origin: explicit\n" +
		"gc.endpoint_status: verified\n" +
		"dolt.host: " + compactCloudDoltHost + "\n" +
		"dolt.port: 3306\n"
}

// compactInheritedConfig is the canonical .beads/config.yaml of a rig that
// inherits a managed city's endpoint: no host or port of its own.
func compactInheritedConfig(prefix string) string {
	return "issue_prefix: " + prefix + "\n" +
		"gc.endpoint_origin: inherited_city\n" +
		"gc.endpoint_status: verified\n"
}

// writeCompactRigScope creates a rig scope at dir: a server-mode
// .beads/metadata.json naming db and, when configYAML is non-empty, the
// scope's .beads/config.yaml. It returns dir.
func writeCompactRigScope(t *testing.T, dir, db, configYAML string) string {
	t.Helper()
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir rig beads dir: %v", err)
	}
	meta := fmt.Sprintf(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":%q}`, db)
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(meta+"\n"), 0o644); err != nil {
		t.Fatalf("write rig metadata: %v", err)
	}
	if configYAML != "" {
		if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte(configYAML), 0o644); err != nil {
			t.Fatalf("write rig config: %v", err)
		}
	}
	return dir
}

// installCompactRigListGC replaces the fixture's fake gc so `gc rig list
// --json` reports the HQ row plus rigPaths. Rows go one per line so the
// script's jq and grep/sed parsers both read every path. Every other call is
// logged and succeeds, as in writeCompactFakeGC.
func installCompactRigListGC(t *testing.T, fixture compactScriptFixture, rigPaths ...string) {
	t.Helper()
	rows := []string{fmt.Sprintf(`{"name":"hq","path":%q,"hq":true}`, fixture.cityPath)}
	for i, path := range rigPaths {
		rows = append(rows, fmt.Sprintf(`{"name":"rig%d","path":%q,"hq":false}`, i, path))
	}
	rigListJSON := "{\"rigs\":[\n" + strings.Join(rows, ",\n") + "\n]}"
	writeExecutable(t, filepath.Join(fixture.binDir, "gc"), fmt.Sprintf(`#!/bin/sh
printf 'gc %%s\n' "$*" >> %s
if [ "${1:-}" = "rig" ] && [ "${2:-}" = "list" ]; then
  cat <<'RIGS'
%s
RIGS
  exit 0
fi
if [ "${1:-}" = "mail" ] && [ "${2:-}" = "send" ]; then
  printf 'gc mail send: message sent\n'
fi
exit 0
`, shellQuote(fixture.gcLog), rigListJSON))
}

// serveOnlyCompactDatabases wraps the fixture's fake dolt so it answers like a
// sql-server that serves only the named databases. A query that selects any
// other database is logged like a served one and fails with the error the
// city's managed server returns for a database that lives on another endpoint.
func serveOnlyCompactDatabases(t *testing.T, fixture compactScriptFixture, served ...string) {
	t.Helper()
	inner := filepath.Join(fixture.binDir, "dolt-served")
	if err := os.Rename(filepath.Join(fixture.binDir, "dolt"), inner); err != nil {
		t.Fatalf("move fake dolt aside: %v", err)
	}
	writeExecutable(t, filepath.Join(fixture.binDir, "dolt"), fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail
args=("$@")
db=""
query=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --use-db) db="$2"; shift 2 ;;
    -q) query="$2"; shift 2 ;;
    *) shift ;;
  esac
done
case " %s " in
  *" $db "*) exec %s "${args[@]}" ;;
esac
printf 'db=%%s query=%%s\n' "$db" "$query" >> %s
printf 'error on line 1 for query %%s: Error 1049 (HY000): database not found: %%s\n' "$query" "$db" >&2
exit 1
`, strings.Join(served, " "), shellQuote(inner), shellQuote(fixture.doltLog)))
}

// makeCompactRigConfigUnreadable removes every permission from the rig's
// .beads/config.yaml until the test ends and returns its path. Root ignores
// file mode, so the test is skipped there.
func makeCompactRigConfigUnreadable(t *testing.T, rig string) string {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores file mode")
	}
	configPath := filepath.Join(rig, ".beads", "config.yaml")
	if err := os.Chmod(configPath, 0); err != nil {
		t.Fatalf("make rig config unreadable: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(configPath, 0o644) })
	return configPath
}

// assertCompactPinnedScopeSkipLine requires an output line that skips db
// because scope is pinned to endpoint. For a scope whose endpoint is unknown,
// endpoint is compactUnknownEndpointSkip.
func assertCompactPinnedScopeSkipLine(t *testing.T, out, db, scope, endpoint string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "compact: db="+db+" ") || !strings.Contains(line, "— skip") {
			continue
		}
		for _, want := range []string{"scope=" + scope, endpoint} {
			if !strings.Contains(line, want) {
				t.Fatalf("skip line for db=%s is missing %q:\n%s\noutput:\n%s", db, want, line, out)
			}
		}
		return
	}
	t.Fatalf("output has no skip line for db=%s naming %q:\n%s", db, endpoint, out)
}

// The reported failure: a managed city, one rig inheriting the managed
// endpoint (localrig), one rig pinned to a cloud Dolt host (cloudrig). Every
// compaction mode must leave cloudrig alone and still maintain the city's and
// localrig's databases.
func TestCompactScriptSkipsRigPinnedToExternalDoltEndpoint(t *testing.T) {
	cases := []struct {
		name string
		args []string
		env  []string
		// wantLocal is per-database evidence that the managed server's own
		// databases were still maintained.
		wantLocal []string
		// notWant must not appear: a mode that issues no SQL before deciding
		// still must not claim it would act on the pinned rig's database.
		notWant []string
	}{
		{
			name:      "scheduled flatten",
			env:       []string{"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500"},
			wantLocal: []string{"db=beads commits=600->600", "db=localrig commits=600->600"},
		},
		{
			name:      "gc-only reclaim",
			args:      []string{"--gc-only"},
			wantLocal: []string{"db=beads gc-only reclaim duration=", "db=localrig gc-only reclaim duration="},
		},
		{
			name:      "bare working-set GC",
			env:       []string{"GC_DOLT_COMPACT_BARE_GC=1"},
			wantLocal: []string{"db=beads bare-gc duration=", "db=localrig bare-gc duration="},
		},
		{
			name:      "gc-only dry run",
			args:      []string{"--gc-only", "--dry-run"},
			wantLocal: []string{"db=beads — dry-run (would reclaim", "db=localrig — dry-run (would reclaim"},
			notWant:   []string{"db=cloudrig — dry-run"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			localRig := writeCompactRigScope(t, t.TempDir(), "localrig", compactInheritedConfig("lr"))
			if err := os.MkdirAll(filepath.Join(fixture.dataDir, "localrig", ".dolt"), 0o755); err != nil {
				t.Fatalf("mkdir localrig db: %v", err)
			}
			cloudRig := writeCompactRigScope(t, t.TempDir(), "cloudrig", compactCloudPinConfig("cr"))
			installCompactRigListGC(t, fixture, localRig, cloudRig)
			serveOnlyCompactDatabases(t, fixture, "beads", "localrig")

			out, err := fixture.runWithArgs(t, "success", tc.args, tc.env...)
			if err != nil {
				t.Fatalf("a rig pinned to an external Dolt endpoint must not fail the run: %v\n%s", err, out)
			}
			if log := readCompactDoltLog(t, fixture); strings.Contains(log, "db=cloudrig ") {
				t.Fatalf("cloudrig lives on %s:3306, but the compactor queried it on the managed server:\n%s\ndolt log:\n%s",
					compactCloudDoltHost, out, log)
			}
			assertCompactPinnedScopeSkipLine(t, out, "cloudrig", cloudRig, compactCloudDoltEndpoint)
			for _, want := range tc.wantLocal {
				if !strings.Contains(out, want) {
					t.Fatalf("a database on the managed server was not maintained, missing %q:\n%s", want, out)
				}
			}
			for _, unwanted := range tc.notWant {
				if strings.Contains(out, unwanted) {
					t.Fatalf("output claims work on the pinned rig's database (%q):\n%s", unwanted, out)
				}
			}
		})
	}
}

// The worse shape: the managed server still holds a database with the pinned
// rig's name, typically the copy left behind when the rig moved to the cloud
// endpoint. It is not the rig's store, so compacting it rewrites history
// nobody reads while the log reports the rig's database as compacted.
func TestCompactScriptSkipsManagedCopyOfPinnedRigDatabase(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	if err := os.MkdirAll(filepath.Join(fixture.dataDir, "cloudrig", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir stale cloudrig copy: %v", err)
	}
	cloudRig := writeCompactRigScope(t, t.TempDir(), "cloudrig", compactCloudPinConfig("cr"))
	installCompactRigListGC(t, fixture, cloudRig)

	out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("compact failed: %v\n%s", err, out)
	}
	if log := readCompactDoltLog(t, fixture); strings.Contains(log, "db=cloudrig ") {
		t.Fatalf("the managed server's cloudrig is not the store of the rig pinned to %s:3306, but it was compacted:\n%s\ndolt log:\n%s",
			compactCloudDoltHost, out, log)
	}
	assertCompactPinnedScopeSkipLine(t, out, "cloudrig", cloudRig, compactCloudDoltEndpoint)
	if !strings.Contains(out, "compact: db=cloudrig WARN:") || !strings.Contains(out, filepath.Join(fixture.dataDir, "cloudrig")) {
		t.Fatalf("output must warn that %s is a same-named copy, not the pinned rig's store:\n%s",
			filepath.Join(fixture.dataDir, "cloudrig"), out)
	}
	if !strings.Contains(out, "db=beads commits=600->600") {
		t.Fatalf("the city's own database was not compacted:\n%s", out)
	}
}

// A rig pinned to a Unix socket, which may be this server's, or to another
// port on this server's host, which may be stale, as bd's binding is once
// gc's server restarts on a fresh port, is skipped like any pinned rig. But
// the database of its name here may be its live store, so the warning asks
// for a check instead of calling that database a copy to drop. A rig pinned
// to another host keeps the copy wording. Both hold for --only-db too.
func TestCompactScriptWarnsPinnedRigDatabaseHereMayBeLive(t *testing.T) {
	// In metadata and endpoint, otherPortToken becomes a port the fixture does
	// not listen on.
	const otherPortToken = "{{other_port}}"
	cases := []struct {
		name string
		// runHost is the run's GC_DOLT_HOST; empty means the fixture's
		// 127.0.0.1.
		runHost  string
		metadata string
		config   string
		endpoint string
		// doubt is why the rig's endpoint may still lead to this server; empty
		// means the endpoint names another host.
		doubt string
	}{
		{
			name:    "bd binding to this server's host on another port",
			runHost: compactCityDoltHost,
			metadata: `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"rigdb",` +
				`"dolt_server_host":"` + compactCityDoltHost + `","dolt_server_port":` + otherPortToken + `}`,
			endpoint: compactCityDoltHost + ":" + otherPortToken,
			doubt:    "another port on this host and may be stale",
		},
		{
			name:     "rig-local server on another loopback port (--self)",
			config:   "gc.endpoint_origin: explicit\ngc.endpoint_status: verified\ndolt.host: 127.0.0.1\ndolt.port: 28232\n",
			endpoint: "127.0.0.1:28232",
			doubt:    "another port on this host and may be stale",
		},
		{
			name:     "legacy socket",
			config:   "issue_prefix: rg\ndolt.socket: /run/rig-dolt/mysql.sock\n",
			endpoint: "unix:/run/rig-dolt/mysql.sock",
			doubt:    "a Unix socket that may belong to this server",
		},
		{
			name:     "external pin to another host",
			config:   compactCloudPinConfig("rg"),
			endpoint: compactCloudDoltEndpoint,
		},
	}
	for _, tc := range cases {
		for _, onlyDB := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/only-db %t", tc.name, onlyDB), func(t *testing.T) {
				fixture := newCompactScriptFixture(t)
				expand := strings.NewReplacer(otherPortToken, fmt.Sprint(fixture.port+1)).Replace
				rig := filepath.Join(t.TempDir(), "rig")
				writeCompactRigScope(t, rig, "rigdb", tc.config)
				if tc.metadata != "" {
					if err := os.WriteFile(filepath.Join(rig, ".beads", "metadata.json"), []byte(expand(tc.metadata)), 0o644); err != nil {
						t.Fatalf("write rig metadata: %v", err)
					}
				}
				installCompactRigListGC(t, fixture, rig)
				copyPath := filepath.Join(fixture.dataDir, "rigdb")
				var args []string
				if onlyDB {
					args = []string{"--gc-only", "--only-db", "rigdb"}
				} else if err := os.MkdirAll(filepath.Join(copyPath, ".dolt"), 0o755); err != nil {
					t.Fatalf("mkdir managed rigdb: %v", err)
				}
				runHost := "127.0.0.1"
				env := []string{"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500"}
				if tc.runHost != "" {
					runHost = tc.runHost
					env = append(env, "GC_DOLT_HOST="+tc.runHost)
				}

				out, err := fixture.runWithArgs(t, "success", args, env...)
				if err != nil {
					t.Fatalf("compact failed: %v\n%s", err, out)
				}
				if log := readCompactDoltLog(t, fixture); strings.Contains(log, "db=rigdb ") {
					t.Fatalf("the rig is pinned elsewhere, but the managed server's rigdb was queried:\n%s\ndolt log:\n%s", out, log)
				}
				endpoint := expand(tc.endpoint)
				assertCompactPinnedScopeSkipLine(t, out, "rigdb", rig, endpoint)
				var want, unwanted string
				switch {
				case tc.doubt != "" && !onlyDB:
					want = "compact: db=rigdb WARN: " + copyPath + " on this server may still be the live store of scope=" + rig +
						", whose Dolt endpoint " + endpoint + " is " + tc.doubt +
						" — skip; check which server the scope uses before you drop it"
					unwanted = "drop or archive"
				case tc.doubt != "":
					want = "compact: db=rigdb WARN: requested by name, but it is the database of scope=" + rig +
						", whose Dolt endpoint " + endpoint + " is " + tc.doubt +
						"; this server, " + runHost + ":" + fmt.Sprint(fixture.port) +
						", may still hold its live store — skip; check which server the scope uses first"
					unwanted = "gc compacts only"
				case !onlyDB:
					want = "compact: db=rigdb WARN: " + copyPath + " on this server is not the store of scope=" + rig +
						", which is pinned to Dolt endpoint " + endpoint + " — skip; drop or archive the copy if it is stale"
					unwanted = "may still be the live store"
				default:
					want = "compact: db=rigdb WARN: requested by name, but it is the database of scope=" + rig +
						", which is pinned to Dolt endpoint " + endpoint + "; gc compacts only " + runHost + ":" + fmt.Sprint(fixture.port) + " — skip"
					unwanted = "may still hold its live store"
				}
				if !strings.Contains(out, want) {
					t.Fatalf("output is missing the warning %q:\n%s", want, out)
				}
				if strings.Contains(out, unwanted) {
					t.Fatalf("output must not say %q here:\n%s", unwanted, out)
				}
			})
		}
	}
}

// Which scopes count as pinned elsewhere. A scope is skipped only on
// unambiguous evidence that its database lives on another server; everything
// else keeps today's behavior and is compacted here.
func TestCompactScriptClassifiesRigEndpoints(t *testing.T) {
	// In metadata, config and wantEndpoint, portToken becomes the fixture's
	// managed port and otherPortToken a port it does not listen on.
	const (
		portToken      = "{{managed_port}}"
		otherPortToken = "{{other_port}}"
	)
	cases := []struct {
		name string
		// runHost is the run's GC_DOLT_HOST; empty means the fixture's
		// loopback host.
		runHost string
		// metadata is the rig's metadata.json; empty means the server-mode
		// default naming the rig's database.
		metadata string
		// config is the rig's config.yaml.
		config string
		// wantEndpoint is the endpoint a skipped rig is pinned to; empty means
		// the rig's database is compacted on the managed server.
		wantEndpoint string
	}{
		{
			name:         "external pin",
			config:       compactCloudPinConfig("rg"),
			wantEndpoint: compactCloudDoltEndpoint,
		},
		{
			name:         "external pin, nested config spelling",
			config:       "issue_prefix: rg\ngc:\n  endpoint_origin: explicit\n  endpoint_status: verified\ndolt:\n  host: " + compactCloudDoltHost + "\n  port: 3306\n",
			wantEndpoint: compactCloudDoltEndpoint,
		},
		{
			name:         "external pin, unverified and mixed-case host",
			config:       "gc.endpoint_origin: explicit\ngc.endpoint_status: unverified\ndolt.host: Beads.Dolt-Cloud.Example\ndolt.port: 3306\n",
			wantEndpoint: "Beads.Dolt-Cloud.Example:3306",
		},
		{
			name:         "rig-local server on another loopback port (--self)",
			config:       "gc.endpoint_origin: explicit\ngc.endpoint_status: verified\ndolt.host: 127.0.0.1\ndolt.port: 28232\n",
			wantEndpoint: "127.0.0.1:28232",
		},
		{
			name:         "external pin to another host on the managed port",
			config:       "gc.endpoint_origin: explicit\ngc.endpoint_status: verified\ndolt.host: " + compactCloudDoltHost + "\ndolt.port: " + portToken + "\n",
			wantEndpoint: compactCloudDoltHost + ":" + portToken,
		},
		{
			name:    "explicit pin naming a non-loopback managed host itself",
			runHost: compactCityDoltHost,
			config:  "gc.endpoint_origin: explicit\ngc.endpoint_status: verified\ndolt.host: Dolt.City.Example\ndolt.port: " + portToken + "\n",
		},
		{
			name:         "socket pin",
			config:       "gc.endpoint_origin: explicit\ngc.endpoint_status: verified\ndolt.socket: /run/rig-dolt/mysql.sock\n",
			wantEndpoint: "unix:/run/rig-dolt/mysql.sock",
		},
		{
			name:   "explicit pin naming the managed server itself",
			config: "gc.endpoint_origin: explicit\ngc.endpoint_status: verified\ndolt.host: localhost\ndolt.port: " + portToken + "\n",
		},
		{
			name:   "inherited rig",
			config: compactInheritedConfig("rg"),
		},
		{
			name:   "inherited rig keeps a stale bd binding to a remote host",
			config: compactInheritedConfig("rg"),
			metadata: `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"rigdb",` +
				`"dolt_server_host":"` + compactCloudDoltHost + `","dolt_server_port":3306}`,
		},
		{
			name:   "inherited rig mirroring an external endpoint",
			config: compactInheritedConfig("rg") + "dolt.host: " + compactCloudDoltHost + "\ndolt.port: 3306\n",
		},
		{
			name:   "legacy config with only a stale port mirror",
			config: "issue_prefix: rg\ndolt.port: 1\n",
		},
		{
			name:   "legacy config pinned to a loopback host",
			config: "issue_prefix: rg\ndolt.host: 127.0.0.1\ndolt.port: 1\n",
		},
		{
			name:         "legacy config pinned to a remote host",
			config:       "issue_prefix: rg\ndolt.host: " + compactCloudDoltHost + "\ndolt.port: 3306\n",
			wantEndpoint: compactCloudDoltEndpoint,
		},
		{
			name:         "legacy config pinned to a remote host with no port",
			config:       "issue_prefix: rg\ndolt.host: " + compactCloudDoltHost + "\n",
			wantEndpoint: compactCloudDoltHost + ":<unset>",
		},
		{
			name:         "legacy config naming a socket",
			config:       "issue_prefix: rg\ndolt.socket: /run/rig-dolt/mysql.sock\n",
			wantEndpoint: "unix:/run/rig-dolt/mysql.sock",
		},
		{
			name:         "legacy config naming a non-loopback managed host on another port",
			runHost:      compactCityDoltHost,
			config:       "issue_prefix: rg\ndolt.host: " + compactCityDoltHost + "\ndolt.port: " + otherPortToken + "\n",
			wantEndpoint: compactCityDoltHost + ":" + otherPortToken,
		},
		{
			name:    "legacy config naming a non-loopback managed host on the managed port",
			runHost: compactCityDoltHost,
			config:  "issue_prefix: rg\ndolt.host: " + compactCityDoltHost + "\ndolt.port: " + portToken + "\n",
		},
		{
			name:    "legacy config naming a non-loopback managed host with no port",
			runHost: compactCityDoltHost,
			config:  "issue_prefix: rg\ndolt.host: " + compactCityDoltHost + "\n",
		},
		{
			name: "bd-owned binding to a remote host",
			metadata: `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"rigdb",` +
				`"dolt_server_host":"` + compactCloudDoltHost + `","dolt_server_port":3306}`,
			wantEndpoint: compactCloudDoltEndpoint,
		},
		{
			name: "bd-owned binding to a remote host on the managed port",
			metadata: `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"rigdb",` +
				`"dolt_server_host":"` + compactCloudDoltHost + `","dolt_server_port":` + portToken + `}`,
			wantEndpoint: compactCloudDoltHost + ":" + portToken,
		},
		{
			name: "bd-owned binding to a loopback host",
			metadata: `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"rigdb",` +
				`"dolt_server_host":"127.0.0.1","dolt_server_port":1}`,
		},
		{
			name:    "bd-owned binding to a non-loopback managed host on another port",
			runHost: compactCityDoltHost,
			metadata: `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"rigdb",` +
				`"dolt_server_host":"` + compactCityDoltHost + `","dolt_server_port":` + otherPortToken + `}`,
			wantEndpoint: compactCityDoltHost + ":" + otherPortToken,
		},
		{
			name:    "bd-owned binding to a non-loopback managed host on the managed port",
			runHost: compactCityDoltHost,
			metadata: `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"rigdb",` +
				`"dolt_server_host":"` + compactCityDoltHost + `","dolt_server_port":` + portToken + `}`,
		},
		{
			name: "no endpoint evidence at all",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			expand := strings.NewReplacer(
				portToken, fmt.Sprint(fixture.port),
				otherPortToken, fmt.Sprint(fixture.port+1),
			).Replace
			rig := filepath.Join(t.TempDir(), "rig")
			writeCompactRigScope(t, rig, "rigdb", expand(tc.config))
			if tc.metadata != "" {
				if err := os.WriteFile(filepath.Join(rig, ".beads", "metadata.json"), []byte(expand(tc.metadata)), 0o644); err != nil {
					t.Fatalf("write rig metadata: %v", err)
				}
			}
			installCompactRigListGC(t, fixture, rig)
			wantEndpoint := expand(tc.wantEndpoint)
			served := []string{"beads"}
			if wantEndpoint == "" {
				served = append(served, "rigdb")
			}
			serveOnlyCompactDatabases(t, fixture, served...)

			env := []string{"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500", "GC_DOLT_COMPACT_DRY_RUN=1"}
			if tc.runHost != "" {
				env = append(env, "GC_DOLT_HOST="+tc.runHost)
			}
			out, err := fixture.run(t, "success", env...)
			if err != nil {
				t.Fatalf("compact failed: %v\n%s", err, out)
			}
			log := readCompactDoltLog(t, fixture)
			if !strings.Contains(out, "db=beads commits=600") {
				t.Fatalf("the city's own database was not reached:\n%s", out)
			}
			if wantEndpoint == "" {
				if !strings.Contains(log, "db=rigdb ") || strings.Contains(out, "db=rigdb scope=") {
					t.Fatalf("rigdb lives on the managed server and must be compacted there:\n%s", out)
				}
				return
			}
			if strings.Contains(log, "db=rigdb ") {
				t.Fatalf("rigdb lives on %s, but the managed server was queried for it:\n%s\ndolt log:\n%s", wantEndpoint, out, log)
			}
			assertCompactPinnedScopeSkipLine(t, out, "rigdb", rig, wantEndpoint)
		})
	}
}

// A rig config.yaml that cannot be read leaves the rig's endpoint unknown. The
// run warns, naming the file, and skips the rig's database as it skips a
// pinned rig's: if the rig lives on another endpoint, a same-named database
// here is a stale copy, and flattening it costs far more than one missed
// compaction. The run still succeeds and compacts the city's database, and a
// name that a scope on the managed server also claims is compacted as usual.
func TestCompactScriptUnreadableRigConfigSkipsRig(t *testing.T) {
	cases := []struct {
		name string
		// db is the rig's database; "beads" is also the city's.
		db     string
		config string
		// managedCopy puts a database named db in the managed data dir.
		managedCopy bool
	}{
		{name: "inherited rig", db: "rigdb", config: compactInheritedConfig("rg")},
		{
			name:        "external pin with a same-named copy on the managed server",
			db:          "cloudrig",
			config:      compactCloudPinConfig("cr"),
			managedCopy: true,
		},
		{name: "rig sharing the city's database name", db: "beads", config: compactInheritedConfig("rg")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			copyPath := filepath.Join(fixture.dataDir, tc.db)
			if tc.managedCopy {
				if err := os.MkdirAll(filepath.Join(copyPath, ".dolt"), 0o755); err != nil {
					t.Fatalf("mkdir managed copy: %v", err)
				}
			}
			rig := writeCompactRigScope(t, t.TempDir(), tc.db, tc.config)
			configPath := makeCompactRigConfigUnreadable(t, rig)
			installCompactRigListGC(t, fixture, rig)

			out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
			if err != nil {
				t.Fatalf("an unreadable rig config.yaml must not fail the run: %v\n%s", err, out)
			}
			if !strings.Contains(out, "scope="+rig+" WARN: cannot read "+configPath) {
				t.Fatalf("output must name the rig and the config.yaml it cannot read:\n%s", out)
			}
			assertCompactPinnedScopeSkipLine(t, out, tc.db, rig, compactUnknownEndpointSkip)
			if !strings.Contains(out, "db=beads commits=600->600") {
				t.Fatalf("the city's own database was not compacted:\n%s", out)
			}
			if tc.db == "beads" {
				if strings.Contains(out, "db=beads WARN:") {
					t.Fatalf("the city claims beads, so it is compacted with no copy warning:\n%s", out)
				}
				return
			}
			if log := readCompactDoltLog(t, fixture); strings.Contains(log, "db="+tc.db+" ") {
				t.Fatalf("the rig's endpoint is unknown, but the managed server's %s was compacted as its store:\n%s\ndolt log:\n%s",
					tc.db, out, log)
			}
			if tc.managedCopy && !strings.Contains(out, "compact: db="+tc.db+" WARN: "+copyPath+" on this server may not be the store of scope="+rig) {
				t.Fatalf("output must warn that %s may not be the rig's store:\n%s", copyPath, out)
			}
		})
	}
}

// --only-db cannot reach the database of a rig whose config.yaml cannot be
// read either: the request is refused with a warning naming the cause.
func TestCompactScriptOnlyDBRefusesUnknownEndpointRigDatabase(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	if err := os.MkdirAll(filepath.Join(fixture.dataDir, "rigdb", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir rigdb: %v", err)
	}
	rig := writeCompactRigScope(t, t.TempDir(), "rigdb", compactInheritedConfig("rg"))
	makeCompactRigConfigUnreadable(t, rig)
	installCompactRigListGC(t, fixture, rig)

	out, err := fixture.runWithArgs(t, "success", []string{"--gc-only", "--only-db", "rigdb"})
	if err != nil {
		t.Fatalf("compact failed: %v\n%s", err, out)
	}
	if log := readCompactDoltLog(t, fixture); strings.TrimSpace(log) != "" {
		t.Fatalf("--only-db of a rig whose endpoint is unknown must issue no SQL here:\n%s\ndolt log:\n%s", out, log)
	}
	if !strings.Contains(out, "compact: db=rigdb WARN: requested by name, but it is the database of scope="+rig+", whose Dolt endpoint is unknown") {
		t.Fatalf("output must explain why the requested database is refused:\n%s", out)
	}
}

// --only-db cannot reach a pinned rig's database through this server, with or
// without a same-named copy here: the request is refused with a warning, and
// the run still succeeds.
func TestCompactScriptOnlyDBRefusesPinnedRigDatabase(t *testing.T) {
	for _, managedCopy := range []bool{false, true} {
		t.Run(fmt.Sprintf("managed copy %t", managedCopy), func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			if managedCopy {
				if err := os.MkdirAll(filepath.Join(fixture.dataDir, "cloudrig", ".dolt"), 0o755); err != nil {
					t.Fatalf("mkdir cloudrig copy: %v", err)
				}
			}
			cloudRig := writeCompactRigScope(t, t.TempDir(), "cloudrig", compactCloudPinConfig("cr"))
			installCompactRigListGC(t, fixture, cloudRig)

			out, err := fixture.runWithArgs(t, "success", []string{"--gc-only", "--only-db", "cloudrig"})
			if err != nil {
				t.Fatalf("compact failed: %v\n%s", err, out)
			}
			if log := readCompactDoltLog(t, fixture); strings.TrimSpace(log) != "" {
				t.Fatalf("--only-db of a pinned rig's database must issue no SQL here:\n%s\ndolt log:\n%s", out, log)
			}
			if !strings.Contains(out, "compact: db=cloudrig WARN:") || !strings.Contains(out, compactCloudDoltEndpoint) {
				t.Fatalf("output must explain why the requested database is refused:\n%s", out)
			}
		})
	}
}

// A pinned rig whose database name is also claimed by a scope on the managed
// server does not hide that database: the managed scope's claim wins.
func TestCompactScriptPinnedRigDoesNotShadowManagedDatabaseOfTheSameName(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	// No dolt_database: the rig's database defaults to "beads", the city's name.
	cloudRig := filepath.Join(t.TempDir(), "cloudrig")
	writeCompactRigScope(t, cloudRig, "unused", compactCloudPinConfig("cr"))
	if err := os.WriteFile(filepath.Join(cloudRig, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt"}`), 0o644); err != nil {
		t.Fatalf("write rig metadata: %v", err)
	}
	installCompactRigListGC(t, fixture, cloudRig)

	out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("compact failed: %v\n%s", err, out)
	}
	assertCompactPinnedScopeSkipLine(t, out, "beads", cloudRig, compactCloudDoltEndpoint)
	if !strings.Contains(out, "db=beads commits=600->600") || strings.Contains(out, "db=beads WARN:") {
		t.Fatalf("the city's beads database must still be compacted, with no copy warning:\n%s", out)
	}
}

// When `gc rig list` fails, discovery falls back to scanning the city
// directory, and a pinned rig found there is skipped the same way.
func TestCompactScriptSkipsPinnedRigFoundByFilesystemFallback(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	writeExecutable(t, filepath.Join(fixture.binDir, "gc"), "#!/bin/sh\nexit 1\n")
	cloudRig := writeCompactRigScope(t, filepath.Join(fixture.cityPath, "rigs", "cloudrig"), "cloudrig",
		compactCloudPinConfig("cr"))
	serveOnlyCompactDatabases(t, fixture, "beads")

	out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("compact failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "falling back to local filesystem metadata scan") {
		t.Fatalf("expected the filesystem fallback:\n%s", out)
	}
	if log := readCompactDoltLog(t, fixture); strings.Contains(log, "db=cloudrig ") {
		t.Fatalf("pinned rig found by the fallback scan was queried on the managed server:\n%s\ndolt log:\n%s", out, log)
	}
	assertCompactPinnedScopeSkipLine(t, out, "cloudrig", cloudRig, compactCloudDoltEndpoint)
}
