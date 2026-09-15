package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The custom-types check shells out to `bd` in the scope's directory, and used
// to do it with nothing but the ambient process environment. For a city that
// was fine by accident: gc's own process usually carries the managed city's
// coordinates. For an inherited rig it was never fine — a legacy rig's config
// records no endpoint at all, because under a gc-managed city the endpoint is
// gc's to resolve — and the accident stops holding the moment the city's owner
// changes. After an ownership handoff gc's runtime publication is retired, and
// the check reported:
//
//	custom-types:<rig> — could not read types.custom: exit status 1
//
// on a city that was working perfectly, because bare `bd` in the rig directory
// had nothing to connect to.
//
// So the check resolves the scope's endpoint the same way every other reader
// does, and hands it to bd. The three states below are the ones that matter:
// gc runs the server, bd runs it, and gc runs it again after a rollback.

func writeScopeConfigFile(t *testing.T, scopeRoot, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(scopeRoot, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scopeRoot, ".beads", "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeScopeMetadataFile(t *testing.T, scopeRoot, database string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(scopeRoot, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":%q}`, database)
	if err := os.WriteFile(filepath.Join(scopeRoot, ".beads", "metadata.json"), []byte(body+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// legacyCityWithInheritedRig is the shape the handoff inherits: a managed city
// and a rig that tracks no endpoint of its own.
func legacyCityWithInheritedRig(t *testing.T) (string, string) {
	t.Helper()
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "testrig")
	writeScopeConfigFile(t, city, "issue_prefix: gc\ngc.endpoint_origin: managed_city\ngc.endpoint_status: verified\n")
	writeScopeMetadataFile(t, city, "hq")
	writeScopeConfigFile(t, rig, "issue_prefix: ma\ngc.endpoint_origin: inherited_city\ngc.endpoint_status: verified\n")
	writeScopeMetadataFile(t, rig, "ma")
	return city, rig
}

func writeGCRuntimeState(t *testing.T, city, port string) {
	t.Helper()
	dir := filepath.Join(city, ".gc", "runtime", "packs", "dolt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"running":true,"pid":%d,"port":%s,"data_dir":%q}`,
		os.Getpid(), port, filepath.Join(city, ".beads", "dolt"))
	if err := os.WriteFile(filepath.Join(dir, "dolt-state.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeBdServerRecord(t *testing.T, scopeRoot, port string) {
	t.Helper()
	beadsDir := filepath.Join(scopeRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "dolt-server.pid"), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "dolt-server.port"), []byte(port+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func envPort(t *testing.T, env []string) string {
	t.Helper()
	for _, entry := range env {
		if strings.HasPrefix(entry, "BEADS_DOLT_SERVER_PORT=") {
			return strings.TrimPrefix(entry, "BEADS_DOLT_SERVER_PORT=")
		}
	}
	return ""
}

func TestScopeDoltEnvSendsARigAtTheCitysManagedServer(t *testing.T) {
	city, rig := legacyCityWithInheritedRig(t)
	port := listenLoopbackPort(t)
	writeGCRuntimeState(t, city, port)

	if got := envPort(t, scopeDoltEnv(city, rig)); got != port {
		t.Fatalf("rig env port = %q, want gc's published %q", got, port)
	}
}

// After a handoff gc's publication is gone and bd's record is the city's
// endpoint. The rig has to follow the city, because a rig has no server of its
// own — which is exactly why it carries no endpoint in its config.
func TestScopeDoltEnvSendsARigAtTheCitysHandedOffServer(t *testing.T) {
	city, rig := legacyCityWithInheritedRig(t)
	port := listenLoopbackPort(t)
	writeBdServerRecord(t, city, port)

	if got := envPort(t, scopeDoltEnv(city, rig)); got != port {
		t.Fatalf("rig env port = %q, want the city's replacement server %q", got, port)
	}
}

// And a rollback puts gc's publication back, which the rig follows in turn.
func TestScopeDoltEnvFollowsTheCityBackAfterARollback(t *testing.T) {
	city, rig := legacyCityWithInheritedRig(t)
	managedPort := listenLoopbackPort(t)
	writeGCRuntimeState(t, city, managedPort)
	writeBdServerRecord(t, city, listenLoopbackPort(t))

	if got := envPort(t, scopeDoltEnv(city, rig)); got != managedPort {
		t.Fatalf("rig env port = %q, want gc's republished %q", got, managedPort)
	}
}

// A scope whose endpoint cannot be resolved gets no override. Inventing one
// would send bd at a port nothing is serving, which is worse than letting bd
// resolve for itself and say what it found.
func TestScopeDoltEnvIsEmptyWhenNothingResolves(t *testing.T) {
	city, rig := legacyCityWithInheritedRig(t)

	if env := scopeDoltEnv(city, rig); len(env) != 0 {
		t.Fatalf("scopeDoltEnv = %v, want nothing for a city with no running server", env)
	}
	if env := scopeDoltEnv("", rig); len(env) != 0 {
		t.Fatalf("scopeDoltEnv with no city = %v, want nothing", env)
	}
}
