package main

import (
	"bytes"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
)

// writeManagedDirectCityFixture builds the canonical managed-direct city the
// port-withhold path targets: a bd provider, server-mode Dolt metadata, a
// reachable managed runtime state, and the .beads/dolt-server.port mirror
// syncConfiguredDoltPortFiles reconciles to the live port. It returns the
// live port, the one both the runtime state and the mirror carry.
func writeManagedDirectCityFixture(t *testing.T, cityPath string) string {
	t.Helper()
	t.Setenv("GC_BEADS", "bd")
	writeScopeMetadata(t, cityPath, map[string]string{
		"database":      "dolt",
		"backend":       "dolt",
		"dolt_mode":     "server",
		"dolt_database": "hq",
	})
	port := strconv.Itoa(writeReachableManagedDoltState(t, cityPath))
	requireSyncConfiguredDoltPortFiles(t, cityPath, "bd", config.DoltConfig{}, "gc", nil)
	return port
}

// assertWithheldDoltPort checks the withheld-port contract of
// gastownhall/gascity#1656: the three port keys stay present but empty, so
// tmux/subprocess providers unset any stale inherited value instead of
// freezing the spawn-time port into the session for its whole life, and
// every Dolt key carries the value runtimetest.WithheldManagedDoltEnv fixes
// for the runtime adapters that rebuild the target off the controller host.
func assertWithheldDoltPort(t *testing.T, env map[string]string, livePort string) {
	t.Helper()
	for _, key := range []string{"GC_DOLT_PORT", "BEADS_DOLT_SERVER_PORT", "BEADS_DOLT_PORT"} {
		value, projected := env[key]
		if !projected {
			t.Fatalf("%s absent from session env; want present and empty so providers unset a stale inherited value", key)
		}
		if value != "" {
			t.Fatalf("%s = %q, want withheld (empty): the scope mirror .beads/dolt-server.port carries the live port %s", key, value, livePort)
		}
	}
	for key, want := range runtimetest.WithheldManagedDoltEnv() {
		if got := env[key]; got != want {
			t.Fatalf("%s = %q, want %q: a withheld session env must match runtimetest.WithheldManagedDoltEnv, the shape the k8s and t3bridge adapters are tested against", key, got, want)
		}
	}
}

// assertNoManagedLocalDoltMarker checks that a session env whose port was not
// withheld pins GC_DOLT_MANAGED_LOCAL present and empty. passthroughEnv copies
// every GC_ key of the controller process into the session, so only an explicit
// empty value keeps an inherited marker from posing as a withheld port.
func assertNoManagedLocalDoltMarker(t *testing.T, env map[string]string) {
	t.Helper()
	value, present := env["GC_DOLT_MANAGED_LOCAL"]
	if !present || value != "" {
		t.Fatalf("GC_DOLT_MANAGED_LOCAL = %q (present=%t), want present and empty: only a withheld port marks the session's target managed-local", value, present)
	}
}

// resolveWorkerTemplateEnv resolves a city worker's template and returns the
// env a runtime provider receives for it (runtime.Config.Env is
// TemplateParams.Env).
func resolveWorkerTemplateEnv(t *testing.T, cityPath string) map[string]string {
	t.Helper()
	params := &agentBuildParams{
		cityName:   "city",
		cityPath:   cityPath,
		workspace:  &config.Workspace{Provider: "test"},
		providers:  map[string]config.ProviderSpec{"test": {Command: "echo", PromptMode: "none"}},
		lookPath:   func(string) (string, error) { return "/bin/echo", nil },
		fs:         fsys.OSFS{},
		beaconTime: time.Unix(0, 0),
		beadNames:  make(map[string]string),
		stderr:     io.Discard,
	}
	agent := &config.Agent{Name: "worker"}
	tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}
	return tp.Env
}

// TestSessionBackendEnvWithholdsMirroredCityManagedDoltPort pins AC-1 of
// gastownhall/gascity#1656: a city session whose scope mirror already carries
// the live managed port must not have that port frozen into its env. bd ranks
// the env port above the mirror, so a projected port keeps every session
// dialing the port dolt ran on at spawn time, long after a restart moved it;
// withholding the env port lets the session's raw `bd` follow the mirror gc
// rewrites on every runtime publication.
func TestSessionBackendEnvWithholdsMirroredCityManagedDoltPort(t *testing.T) {
	cityPath := t.TempDir()
	livePort := writeManagedDirectCityFixture(t, cityPath)
	// Stale spawn-time values, as a session environment captured before a
	// Dolt restart would carry them.
	t.Setenv("GC_DOLT_PORT", "9999")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "9999")
	t.Setenv("BEADS_DOLT_PORT", "9999")

	env, err := sessionBackendEnvWithError(cityPath, "", nil)
	if err != nil {
		t.Fatalf("sessionBackendEnvWithError: %v", err)
	}

	assertWithheldDoltPort(t, env, livePort)
	if got := env["BEADS_DOLT_AUTO_START"]; got != "0" {
		t.Fatalf("BEADS_DOLT_AUTO_START = %q, want 0", got)
	}
}

// TestSessionBackendEnvWithholdsMirroredInheritedRigDoltPort pins AC-2 of
// gastownhall/gascity#1656: a rig session under the canonical inherited-city
// contract must have the managed port withheld when the rig's own
// .beads/dolt-server.port mirror — the one syncConfiguredDoltPortFiles
// reconciles to the live port — already carries it, for the same reason as
// the city scope: bd ranks an env port above the mirror, so projecting it
// freezes the spawn-time port into the rig session across a managed Dolt
// restart onto a new port.
func TestSessionBackendEnvWithholdsMirroredInheritedRigDoltPort(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	rigRoot := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(rigRoot, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}

	// The rig contract gc publishes for an inherited-city rig — endpoint
	// origin inherited_city, no pinned port — with the port mirror carrying
	// the live managed port, as syncConfiguredDoltPortFiles reconciles it.
	livePort := strconv.Itoa(writeReachableManagedDoltState(t, cityPath))
	if err := os.WriteFile(filepath.Join(rigRoot, ".beads", "config.yaml"), []byte(`issue_prefix: repo
gc.endpoint_origin: inherited_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigRoot, ".beads", "dolt-server.port"), []byte(livePort), 0o644); err != nil {
		t.Fatal(err)
	}

	// Stale spawn-time values, as a session environment captured before a
	// Dolt restart would carry them.
	t.Setenv("GC_DOLT_PORT", "9999")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "9999")
	t.Setenv("BEADS_DOLT_PORT", "9999")

	env, err := sessionBackendEnvWithError(cityPath, rigRoot, []config.Rig{{Name: "repo", Path: rigRoot}})
	if err != nil {
		t.Fatalf("sessionBackendEnvWithError: %v", err)
	}

	assertWithheldDoltPort(t, env, livePort)
	if got := env["GC_DOLT_HOST"]; got != "" {
		t.Fatalf("GC_DOLT_HOST = %q, want empty for the managed loopback target", got)
	}
	if got := env["BEADS_DOLT_AUTO_START"]; got != "0" {
		t.Fatalf("BEADS_DOLT_AUTO_START = %q, want 0", got)
	}
}

// TestSessionBackendEnvWithheldPortPinsSharedServerOff pins AC-4 of
// gastownhall/gascity#1656: withholding the env port removes the one fact
// that kept bd off a shared server, so a user-level `dolt.shared-server`
// (or an inherited BEADS_DOLT_SHARED_SERVER=1) would now send the
// session's raw bd to ~/.beads/shared-server or port 3308 instead of the
// managed server the mirror tracks. The withhold must pin the mode off
// explicitly, the same split-brain rule applySessionSharedServerOptOut
// applies to proxied gc-owned scopes.
func TestSessionBackendEnvWithheldPortPinsSharedServerOff(t *testing.T) {
	cityPath := t.TempDir()
	livePort := writeManagedDirectCityFixture(t, cityPath)
	// An inherited shared-server preference, as a session spawned from a
	// process carrying it would pass down.
	t.Setenv("BEADS_DOLT_SHARED_SERVER", "1")

	env, err := sessionBackendEnvWithError(cityPath, "", nil)
	if err != nil {
		t.Fatalf("sessionBackendEnvWithError: %v", err)
	}

	assertWithheldDoltPort(t, env, livePort)
	if got := env["BEADS_DOLT_SHARED_SERVER"]; got != "0" {
		t.Fatalf("BEADS_DOLT_SHARED_SERVER = %q, want 0 pinned off beside a withheld port", got)
	}
	if got := env["BD_DOLT_SHARED_SERVER"]; got != "false" {
		t.Fatalf("BD_DOLT_SHARED_SERVER = %q, want false pinned off beside a withheld port", got)
	}
}

// TestResolveTemplateWithheldDoltEnvReachesRuntimeProvider pins the env a
// runtime provider receives for a withheld managed-direct session: the merged
// template env, not only the backend projection, must match
// runtimetest.WithheldManagedDoltEnv even when the controller process itself
// carries stale Dolt values. The k8s pod adapters and the t3bridge thread env
// read exactly this map; a projection that left the managed-local marker or
// the shared-server pin to the controller's own env would let them rebuild a
// target the session never had.
func TestResolveTemplateWithheldDoltEnvReachesRuntimeProvider(t *testing.T) {
	cityPath := t.TempDir()
	writeTemplateResolveCityConfig(t, cityPath, "")
	livePort := writeManagedDirectCityFixture(t, cityPath)
	t.Setenv("GC_DOLT_PORT", "9999")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "9999")
	t.Setenv("BEADS_DOLT_SHARED_SERVER", "1")

	assertWithheldDoltPort(t, resolveWorkerTemplateEnv(t, cityPath), livePort)
}

// TestResolveTemplateDropsInheritedManagedLocalDoltMarker guards the marker
// half of the withhold: GC_DOLT_MANAGED_LOCAL=1 tells the k8s pod adapters that
// a session with no Dolt port still targets the gc-managed local server, so
// only the withhold may set it. A controller started from inside a withheld
// session inherits "1", and passthroughEnv copies every GC_ key into the
// sessions it spawns; a session whose port is projected (here the mirror is
// absent) must still end up with the marker empty.
func TestResolveTemplateDropsInheritedManagedLocalDoltMarker(t *testing.T) {
	cityPath := t.TempDir()
	writeTemplateResolveCityConfig(t, cityPath, "")
	livePort := writeManagedDirectCityFixture(t, cityPath)
	if err := os.Remove(filepath.Join(cityPath, ".beads", "dolt-server.port")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_DOLT_MANAGED_LOCAL", "1")

	env := resolveWorkerTemplateEnv(t, cityPath)
	if got := env["GC_DOLT_PORT"]; got != livePort {
		t.Fatalf("GC_DOLT_PORT = %q, want projected %q when no mirror exists", got, livePort)
	}
	assertNoManagedLocalDoltMarker(t, env)
}

// TestSessionBackendEnvKeepsDoltPortWhenMirrorCannotStandIn guards AC-5 of
// gastownhall/gascity#1656: the port withhold fires only when the scope's own
// mirror can stand in for the projected port. Every row here is a scope where
// it cannot — the mirror is absent or stale, or the target is one bd's
// port-file path cannot serve — and each must keep today's projection,
// byte for byte, with and without the withhold in place.
func TestSessionBackendEnvKeepsDoltPortWhenMirrorCannotStandIn(t *testing.T) {
	writeRigConfig := func(rigRoot, configYAML string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(rigRoot, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rigRoot, ".beads", "config.yaml"), []byte(configYAML), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cityPath := t.TempDir()
	livePort := writeManagedDirectCityFixture(t, cityPath)

	t.Run("city mirror absent", func(t *testing.T) {
		if err := os.Remove(filepath.Join(cityPath, ".beads", "dolt-server.port")); err != nil {
			t.Fatal(err)
		}
		env, err := sessionBackendEnvWithError(cityPath, "", nil)
		if err != nil {
			t.Fatalf("sessionBackendEnvWithError: %v", err)
		}
		if got := env["GC_DOLT_PORT"]; got != livePort {
			t.Fatalf("GC_DOLT_PORT = %q, want projected %q when no mirror exists", got, livePort)
		}
		if got := env["BEADS_DOLT_SERVER_PORT"]; got != livePort {
			t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want projected %q when no mirror exists", got, livePort)
		}
		assertNoManagedLocalDoltMarker(t, env)
	})

	t.Run("rig mirror stale", func(t *testing.T) {
		rigRoot := filepath.Join(t.TempDir(), "repo")
		writeRigConfig(rigRoot, `issue_prefix: repo
gc.endpoint_origin: inherited_city
gc.endpoint_status: verified
dolt.auto-start: false
`)
		if err := os.WriteFile(filepath.Join(rigRoot, ".beads", "dolt-server.port"), []byte("31364"), 0o644); err != nil {
			t.Fatal(err)
		}
		env, err := sessionBackendEnvWithError(cityPath, rigRoot, []config.Rig{{Name: "repo", Path: rigRoot}})
		if err != nil {
			t.Fatalf("sessionBackendEnvWithError: %v", err)
		}
		if got := env["GC_DOLT_PORT"]; got != livePort {
			t.Fatalf("GC_DOLT_PORT = %q, want projected %q when the mirror reads a stale port", got, livePort)
		}
		if got := env["BEADS_DOLT_SERVER_PORT"]; got != livePort {
			t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want projected %q when the mirror reads a stale port", got, livePort)
		}
		assertNoManagedLocalDoltMarker(t, env)
	})

	t.Run("legacy port-only rig", func(t *testing.T) {
		rigRoot := filepath.Join(t.TempDir(), "legacy")
		if err := os.MkdirAll(rigRoot, 0o755); err != nil {
			t.Fatal(err)
		}
		env, err := sessionBackendEnvWithError(cityPath, rigRoot, []config.Rig{{Name: "legacy", Path: rigRoot, DoltPort: "3307"}})
		if err != nil {
			t.Fatalf("sessionBackendEnvWithError: %v", err)
		}
		if got := env["GC_DOLT_PORT"]; got != "3307" {
			t.Fatalf("GC_DOLT_PORT = %q, want the legacy rig's pinned 3307", got)
		}
		if got := env["BEADS_DOLT_SERVER_PORT"]; got != "3307" {
			t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want the legacy rig's pinned 3307", got)
		}
	})

	t.Run("explicit external rig", func(t *testing.T) {
		rigRoot := filepath.Join(t.TempDir(), "external")
		writeRigConfig(rigRoot, `issue_prefix: external
gc.endpoint_origin: explicit
gc.endpoint_status: verified
dolt.host: rig-db.example.test
dolt.port: 3307
dolt.auto-start: false
`)
		env, err := sessionBackendEnvWithError(cityPath, rigRoot, []config.Rig{{Name: "external", Path: rigRoot}})
		if err != nil {
			t.Fatalf("sessionBackendEnvWithError: %v", err)
		}
		if got := env["GC_DOLT_HOST"]; got != "rig-db.example.test" {
			t.Fatalf("GC_DOLT_HOST = %q, want the explicit external host", got)
		}
		if got := env["GC_DOLT_PORT"]; got != "3307" {
			t.Fatalf("GC_DOLT_PORT = %q, want the explicit external 3307", got)
		}
	})

	t.Run("explicit loopback rig", func(t *testing.T) {
		rigRoot := filepath.Join(t.TempDir(), "loopback")
		writeRigConfig(rigRoot, `issue_prefix: loopback
gc.endpoint_origin: explicit
gc.endpoint_status: verified
dolt.host: 127.0.0.1
dolt.port: 3307
dolt.auto-start: false
`)
		env, err := sessionBackendEnvWithError(cityPath, rigRoot, []config.Rig{{Name: "loopback", Path: rigRoot}})
		if err != nil {
			t.Fatalf("sessionBackendEnvWithError: %v", err)
		}
		// An explicit endpoint resolves External even on 127.0.0.1, so it is
		// not gc's mirror path; the projection keeps host and port.
		if got := env["GC_DOLT_HOST"]; got != "127.0.0.1" {
			t.Fatalf("GC_DOLT_HOST = %q, want the explicit 127.0.0.1 host", got)
		}
		if got := env["GC_DOLT_PORT"]; got != "3307" {
			t.Fatalf("GC_DOLT_PORT = %q, want the explicit 3307", got)
		}
	})

	t.Run("socket target", func(t *testing.T) {
		socketPath := filepath.Join(t.TempDir(), "dolt.sock")
		rigRoot := filepath.Join(t.TempDir(), "socket")
		writeRigConfig(rigRoot, "issue_prefix: socket\ngc.endpoint_origin: explicit\ngc.endpoint_status: verified\ndolt.mode: server\ndolt.socket: "+socketPath+"\ndolt.auto-start: false\n")
		env, err := sessionBackendEnvWithError(cityPath, rigRoot, []config.Rig{{Name: "socket", Path: rigRoot}})
		if err != nil {
			t.Fatalf("sessionBackendEnvWithError: %v", err)
		}
		if got := env["BEADS_DOLT_SERVER_SOCKET"]; got != socketPath {
			t.Fatalf("BEADS_DOLT_SERVER_SOCKET = %q, want %q", got, socketPath)
		}
		if got := env["GC_DOLT_PORT"]; got != "" {
			t.Fatalf("GC_DOLT_PORT = %q, want empty for a socket target", got)
		}
	})
}

// TestSessionBackendEnvLeavesUnmirroredTopologiesUnchanged guards AC-6 of
// gastownhall/gascity#1656: topologies that never project a managed TCP port —
// a proxied-server city, a storage-bound scope, and a doltlite city — get
// exactly the env base produced for them. The withhold's first precondition
// (a projected GC_DOLT_PORT) never holds, so neither the withhold nor the
// shared-server pin it carries may appear; the proxied city keeps the
// opt-out applySessionSharedServerOptOut already gave it at base.
func TestSessionBackendEnvLeavesUnmirroredTopologiesUnchanged(t *testing.T) {
	assertNoPortKeys := func(t *testing.T, env map[string]string) {
		t.Helper()
		for _, key := range []string{"GC_DOLT_PORT", "BEADS_DOLT_SERVER_PORT", "BEADS_DOLT_PORT"} {
			if got := env[key]; got != "" {
				t.Fatalf("%s = %q, want empty: this topology projects no managed TCP port", key, got)
			}
		}
		assertNoManagedLocalDoltMarker(t, env)
	}

	t.Run("proxied server city", func(t *testing.T) {
		cityPath, _ := proxiedEnvTestCity(t)
		// A gc-owned journaled proxied scope: the only proxied shape whose
		// session env pins the shared server off, and it does so at base
		// (applySessionSharedServerOptOut), not through this change.
		journalProxiedScope(t, cityPath, cityPath, true)
		env, err := sessionBackendEnvWithError(cityPath, "", nil)
		if err != nil {
			t.Fatalf("sessionBackendEnvWithError: %v", err)
		}
		assertNoPortKeys(t, env)
		assertSharedServerPinnedOff(t, "gc-owned proxied session env", env)
	})

	t.Run("storage-bound scope", func(t *testing.T) {
		t.Setenv("GC_BEADS", "bd")
		cityPath := t.TempDir()
		writeScopeBeadsMetadata(t, cityPath, `{"backend":"postgres","storage_endpoint":"postgres://bd@db.example.test:5432","storage_database":"beads_pg"}`)
		env, err := sessionBackendEnvWithError(cityPath, "", nil)
		if err != nil {
			t.Fatalf("sessionBackendEnvWithError: %v", err)
		}
		assertNoPortKeys(t, env)
		// A storage-bound scope is not proxied and not dolt: base pins no
		// shared-server value, and the withhold must not add one.
		if got := env["BEADS_DOLT_SHARED_SERVER"]; got != "" {
			t.Fatalf("BEADS_DOLT_SHARED_SERVER = %q, want unset as at base", got)
		}
		if got := env["BD_DOLT_SHARED_SERVER"]; got != "" {
			t.Fatalf("BD_DOLT_SHARED_SERVER = %q, want unset as at base", got)
		}
	})

	t.Run("doltlite city", func(t *testing.T) {
		cityPath := t.TempDir()
		writeDoltliteCityScaffold(t, cityPath)
		env, err := sessionBackendEnvWithError(cityPath, "", nil)
		if err != nil {
			t.Fatalf("sessionBackendEnvWithError: %v", err)
		}
		assertNoPortKeys(t, env)
		if got := env["BEADS_DOLT_SHARED_SERVER"]; got != "" {
			t.Fatalf("BEADS_DOLT_SHARED_SERVER = %q, want unset as at base", got)
		}
		if got := env["BD_DOLT_SHARED_SERVER"]; got != "" {
			t.Fatalf("BD_DOLT_SHARED_SERVER = %q, want unset as at base", got)
		}
	})
}

// TestWithholdDriftLogOmitsEnvValues pins the clear-text-logging boundary on
// the mirror-mismatch log line: the env map withholdMirroredManagedDoltPort
// reads also carries Dolt credentials (GC_DOLT_PASSWORD/BEADS_DOLT_PASSWORD,
// projected in bd_env.go), so the drift line must be built only from the
// scope path, the mirror file contents, and the read error — never from a
// value read out of that map. CodeQL's go/clear-text-logging taints every
// read from a map a password is written into, so logging env["GC_DOLT_PORT"]
// reads as clear-text logging of sensitive information.
func TestWithholdDriftLogOmitsEnvValues(t *testing.T) {
	cityPath := t.TempDir()
	livePort := writeManagedDirectCityFixture(t, cityPath)
	// Force the mismatch path: the mirror reads a different port than the one
	// the env projects.
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "dolt-server.port"), []byte("31364"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"GC_DOLT_PORT":     livePort,
		"GC_DOLT_PASSWORD": "s3cret-probe",
	}

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	withholdMirroredManagedDoltPort(env, cityPath, "")

	out := buf.String()
	if out == "" {
		t.Fatal("expected a drift log line when the mirror cannot stand in for the projected port")
	}
	if strings.Contains(out, livePort) {
		t.Fatalf("drift log contains the env-derived port %q: %s", livePort, out)
	}
	if strings.Contains(out, "s3cret-probe") {
		t.Fatalf("drift log contains an env-derived credential: %s", out)
	}
	// The mismatch must keep the projection, logged or not.
	if got := env["GC_DOLT_PORT"]; got != livePort {
		t.Fatalf("GC_DOLT_PORT = %q, want projected %q kept when the mirror cannot stand in", got, livePort)
	}
}
