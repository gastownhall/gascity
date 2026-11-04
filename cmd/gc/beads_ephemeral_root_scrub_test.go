package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// bd treats BEADS_EPHEMERAL_ROOT=1 as "this root is throwaway" and refuses to
// pair it with an explicit never-idle proxied-server timeout (be-djq0v): an
// ephemeral proxy must be able to idle-exit. Every store gc initializes is
// durable, and a durable store may be configured never-idle
// ([beads] proxied_idle_timeout = "0"), so an ambient marker inherited from
// the operator's shell or an enclosing ephemeral workspace must not reach bd
// on those inits. Dropping the idle-timeout flag instead would hand the store
// bd's 45s ephemeral default, so the flag stays and the marker goes.

// writeMarkerRecordingBd writes a fake bd into dir that appends one line per
// call, "<BEADS_EPHEMERAL_ROOT as bd saw it>\t<argv>", to the returned log. It
// fails `bd context` so the script's already-initialized probe takes the init
// path.
func writeMarkerRecordingBd(t *testing.T, dir string) (bdPath, logPath string) {
	t.Helper()
	logPath = filepath.Join(dir, "bd-calls")
	bdPath = filepath.Join(dir, "bd-marker-recording")
	script := "#!/bin/sh\n" +
		"printf '%s\\t%s\\n' \"${BEADS_EPHEMERAL_ROOT-<unset>}\" \"$*\" >> " + strconv.Quote(logPath) + "\n" +
		"[ \"$1\" != context ] || exit 1\n"
	if err := os.WriteFile(bdPath, []byte(script), 0o755); err != nil { //nolint:gosec // fixture must be executable
		t.Fatal(err)
	}
	return bdPath, logPath
}

// requireDurableInitWithoutEphemeralMarker asserts that every `bd init` in the
// log saw no BEADS_EPHEMERAL_ROOT=1 and still asked for a never-idle proxy.
// bd reads the marker as on only when it equals "1", so unset and "0" both
// satisfy it.
func requireDurableInitWithoutEphemeralMarker(t *testing.T, logPath string) {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("bd was never invoked: %v", err)
	}
	inits := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		marker, args, _ := strings.Cut(line, "\t")
		if !strings.HasPrefix(args, "init ") {
			continue
		}
		inits++
		if marker == "1" {
			t.Errorf("bd init ran with BEADS_EPHEMERAL_ROOT=1 ambient, which bd rejects beside a never-idle timeout: %s", args)
		}
		if !strings.Contains(args, "--proxied-server-idle-timeout 0 ") {
			t.Errorf("durable init lost its never-idle timeout: %s", args)
		}
	}
	if inits == 0 {
		t.Fatalf("no bd init was recorded:\n%s", data)
	}
}

// The default rig-store initializer (gc rig add, gc init's own scopes) passes
// --proxied-server-idle-timeout 0 for a never-idle city and must not let an
// ambient BEADS_EPHEMERAL_ROOT=1 ride along.
func TestDefaultRigBdStoreInitDropsAmbientEphemeralRootForNeverIdleStore(t *testing.T) {
	clearGCEnv(t)
	t.Setenv(config.ProxiedIdleTimeoutEnv, "")
	t.Setenv("BEADS_EPHEMERAL_ROOT", "1")
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "r1")
	if err := os.MkdirAll(rig, 0o755); err != nil {
		t.Fatal(err)
	}
	writeIdleTimeoutCityToml(t, city, `proxied_idle_timeout = "0"`,
		"name = \"r1\"\npath = "+strconv.Quote(rig)+"\nprefix = \"r1\"\n")
	bdPath, logPath := writeMarkerRecordingBd(t, t.TempDir())
	t.Setenv("BD_BIN", bdPath)
	writeScopeBeadsMetadata(t, rig, `{"database":"dolt","backend":"dolt","dolt_mode":"proxied-server","dolt_database":"r1"}`)

	// The init does more than call bd; only what bd was handed matters here.
	_ = initDefaultRigBdStore(city, rig, "r1", "r1")

	requireDurableInitWithoutEphemeralMarker(t, logPath)
}

// The legacy script's proxied initializer (run_bd_init_proxied) has the same
// obligation: it is reached when a proxied scope is initialized without the
// provider-owned front door.
func TestLegacyScriptProxiedInitDropsAmbientEphemeralRoot(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "config.yaml"), []byte("issue_prefix: gc\n"), 0o644); err != nil { //nolint:gosec // fixture
		t.Fatal(err)
	}
	// The proxied binding lives in metadata.json; the script reads the mode
	// from there.
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "metadata.json"),
		[]byte(`{"database":"dolt","backend":"dolt","dolt_mode":"proxied-server","dolt_database":"hq"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	toolDir := t.TempDir()
	bdPath, logPath := writeMarkerRecordingBd(t, toolDir)
	gcPath := filepath.Join(toolDir, "gc-recording")
	if err := os.WriteFile(gcPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // fixture must be executable
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(repoRootForLint(t), "examples", "bd", "assets", "scripts", "gc-beads-bd.sh"), "init", cityDir, "gc", "hq")
	cmd.Env = append(os.Environ(),
		"HOME="+t.TempDir(),
		"GC_CITY_PATH="+cityDir,
		"GC_BIN="+gcPath,
		"BD_BIN="+bdPath,
		"GC_BEADS_PROXIED_IDLE_TIMEOUT=0",
		"GC_DOLT=",
		"BEADS_EPHEMERAL_ROOT=1",
	)
	// Only the bd invocation is under test; the rest of the op may stop early.
	_, _ = cmd.CombinedOutput()

	requireDurableInitWithoutEphemeralMarker(t, logPath)
}

// The provider-owned init op (gc init and gc rig add on an exec: provider)
// runs bd through run_provider_owned_bd with the same never-idle flag.
func TestProviderOwnedScriptInitDropsAmbientEphemeralRoot(t *testing.T) {
	cityDir := t.TempDir()
	scopeDir := filepath.Join(cityDir, "rigs", "r1")
	if err := os.MkdirAll(scopeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bdPath, logPath := writeMarkerRecordingBd(t, t.TempDir())
	cmd := exec.Command("bash", filepath.Join(repoRootForLint(t), "examples", "bd", "assets", "scripts", "gc-beads-bd.sh"), "init", scopeDir, "r1", "r1") //nolint:gosec // repository script under test
	cmd.Env = append(os.Environ(),
		"HOME="+t.TempDir(),
		"GC_CITY_PATH="+cityDir,
		"GC_BEADS_PROVIDER_OWNED=1",
		"GC_BEADS_TRANSPORT=proxied",
		"GC_BEADS_TARGET=local",
		"GC_BEADS_PROXIED_IDLE_TIMEOUT=0",
		"GC_DOLT=",
		"BEADS_DIR="+filepath.Join(scopeDir, ".beads"),
		"BD_BIN="+bdPath,
		"BEADS_EPHEMERAL_ROOT=1",
	)
	// Only the bd invocation is under test; the rest of the op may stop early.
	_, _ = cmd.CombinedOutput()

	requireDurableInitWithoutEphemeralMarker(t, logPath)
}
