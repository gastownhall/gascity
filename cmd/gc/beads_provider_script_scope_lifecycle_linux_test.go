// This file is Linux-only by its filename, for the same reason as
// beads_provider_missing_scope_stop_linux_test.go: it executes the POSIX
// provider script, and 07-design scopes the proxied lifecycle to Linux.
package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/testutil"
)

// providerScriptPath is the real script every test in this file drives. The
// decisions under test are shell decisions — a `cd`, a sidecar read, a case arm
// — so only the shell can prove them.
func providerScriptPath(t *testing.T) string {
	t.Helper()
	script := filepath.Join("..", "..", "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	if _, err := os.Stat(script); err != nil {
		t.Skipf("provider script not available: %v", err)
	}
	return script
}

// runProviderOwnedScriptOp runs one provider-owned op, with its positional
// arguments, against the real script.
//
// It is the single subprocess call site for the provider-script tests on purpose:
// the source resource census ratchet is shrink-only, so a new shape is covered by
// another call to this helper rather than another exec.Command.
func runProviderOwnedScriptOp(t *testing.T, env []string, op string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{providerScriptPath(t), op}, args...)...) //nolint:gosec // repository script under test
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// providerOwnedScriptEnv is the environment gc's provider adapter passes for a
// proxied, provider-owned scope.
func providerOwnedScriptEnv(city, scope, bdBin string) []string {
	return append(os.Environ(),
		"GC_CITY_PATH="+city,
		"GC_BEADS_PROVIDER_OWNED=1",
		"GC_BEADS_TRANSPORT=proxied",
		"GC_BEADS_PROXIED_IDLE_TIMEOUT=0",
		"GC_BEADS_TARGET=local",
		"BEADS_DOLT_PROXIED_SERVER=1",
		"GC_DOLT=",
		"BEADS_DIR="+filepath.Join(scope, ".beads"),
		"BD_BIN="+bdBin,
	)
}

// writeProxiedSidecar plants the bd client-info sidecar that names the physical
// Dolt root a proxied scope's lifecycle commands act on.
func writeProxiedSidecar(t *testing.T, scope, rootPath string) {
	t.Helper()
	beads := filepath.Join(scope, ".beads")
	if err := os.MkdirAll(beads, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"root_path": "` + rootPath + `", "idle_timeout": -1}`
	if err := os.WriteFile(filepath.Join(beads, "proxied_server_client_info.json"), []byte(body), 0o644); err != nil { //nolint:gosec // fixture
		t.Fatal(err)
	}
}

// writeRecordingBd installs a bd that logs its argv and succeeds, so a test can
// assert on the lifecycle commands the script chose to issue.
func writeRecordingBd(t *testing.T, dir string) (bin, logPath string) {
	t.Helper()
	logPath = filepath.Join(dir, "bd-calls.log")
	bin = filepath.Join(dir, "bd")
	body := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + logPath + "\nexit 0\n"
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil { //nolint:gosec // fixture must be executable
		t.Fatal(err)
	}
	return bin, logPath
}

func bdCalls(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			calls = append(calls, line)
		}
	}
	return calls
}

// TestProviderScriptRecoverLeavesTheSharedCityProxyAlone pins the invariant that
// a single flaky rig must not cycle the city's Dolt and every other rig's.
//
// `gc beads city migrate-proxied` points a rig's Dolt data dir at the city's
// .beads/dolt, and bd resolves the root for `bd dolt stop` from the sidecar
// rather than from BEADS_DIR. So a rig-scoped recover used to shut down the one
// proxy and Dolt child serving hq and every other rig, under live agents, to
// recover one rig — and the ping that follows cold-starts it while the rig-local
// cause is still there, so the next health pass does it again.
func TestProviderScriptRecoverLeavesTheSharedCityProxyAlone(t *testing.T) {
	for _, tc := range []struct {
		name string
		// rootPath is the rig sidecar's root_path, relative to the rig or absolute.
		rigRootPath func(city, rig string) string
		wantStop    bool
	}{
		{
			// Three segments, not two: bd resolves a relative root_path
			// against BEADS_DIR (`<rig>/.beads`), so the city's root is three
			// levels up from a rig at <city>/rigs/api. The two-segment spelling
			// this used to pin resolves to <city>/rigs/.beads/dolt for bd —
			// a root the rig would own — and only matched because the script
			// joined it to the scope dir instead.
			name:        "shared city root via a relative data dir",
			rigRootPath: func(_, _ string) string { return filepath.Join("..", "..", "..", ".beads", "dolt") },
			wantStop:    false,
		},
		{
			name:        "shared city root spelled absolutely",
			rigRootPath: func(city, _ string) string { return filepath.Join(city, ".beads", "dolt") },
			wantStop:    false,
		},
		{
			name:        "the rig's own root is the rig's to cycle",
			rigRootPath: func(_, rig string) string { return filepath.Join(rig, ".beads", "dolt") },
			wantStop:    true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city := t.TempDir()
			rig := filepath.Join(city, "rigs", "api")
			for _, dir := range []string{
				filepath.Join(city, ".beads", "dolt"),
				filepath.Join(rig, ".beads", "dolt"),
			} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			writeProxiedSidecar(t, city, filepath.Join(city, ".beads", "dolt"))
			writeProxiedSidecar(t, rig, tc.rigRootPath(city, rig))

			bin, logPath := writeRecordingBd(t, city)
			out, err := runProviderOwnedScriptOp(t, providerOwnedScriptEnv(city, rig, bin), "recover")
			if err != nil {
				t.Fatalf("recover on the rig: %v\n%s", err, out)
			}

			calls := bdCalls(t, logPath)
			var stopped bool
			for _, call := range calls {
				if strings.HasPrefix(call, "dolt stop") {
					stopped = true
				}
			}
			if stopped != tc.wantStop {
				t.Fatalf("rig recover issued `bd dolt stop` = %v, want %v; bd calls %q\n%s", stopped, tc.wantStop, calls, out)
			}
			// Either way recover must still re-establish the rig's own store.
			var pinged bool
			for _, call := range calls {
				if call == "ping" {
					pinged = true
				}
			}
			if !pinged {
				t.Fatalf("rig recover never pinged the scope; bd calls %q\n%s", calls, out)
			}
		})
	}
}

// TestProviderScriptRecoverStillCyclesTheCityScopesOwnProxy is the control for
// the guard above: the city scope owns the shared root, so its own recover must
// keep retiring it. A guard that silenced the city's recover too would leave
// nothing able to cycle a wedged shared proxy.
func TestProviderScriptRecoverStillCyclesTheCityScopesOwnProxy(t *testing.T) {
	city := t.TempDir()
	if err := os.MkdirAll(filepath.Join(city, ".beads", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeProxiedSidecar(t, city, filepath.Join(city, ".beads", "dolt"))

	bin, logPath := writeRecordingBd(t, city)
	out, err := runProviderOwnedScriptOp(t, providerOwnedScriptEnv(city, city, bin), "recover")
	if err != nil {
		t.Fatalf("recover on the city: %v\n%s", err, out)
	}
	var stopped bool
	for _, call := range bdCalls(t, logPath) {
		if strings.HasPrefix(call, "dolt stop") {
			stopped = true
		}
	}
	if !stopped {
		t.Fatalf("city recover did not retire its own proxy; bd calls %q\n%s", bdCalls(t, logPath), out)
	}
}

// bd honors BEADS_DIR only when that directory already holds a project file;
// otherwise it walks up from the CWD and binds whatever bd workspace sits above
// the scope. Provider-owned init must therefore hand bd a scope that already
// carries one, and must not leave that anchor behind when bd init fails, since
// a stray config.yaml reads as a persisted beads identity to gc.
func TestGcBeadsBdProviderOwnedInitAnchorsBeadsDirBeforeBdInit(t *testing.T) {
	const userConfig = "dolt.auto-start: true\n"
	for _, tt := range []struct {
		name          string
		transport     string
		seed          map[string]string // .beads files present before the op
		bdExit        int
		bdWritesMeta  bool   // bd writes metadata.json before exiting
		wantSeen      string // what bd must see at invocation
		wantAfter     bool   // config.yaml present after the op
		wantAfterStr  string
		wantAfterPerm os.FileMode // 0 = not checked
	}{
		{name: "proxied fresh scope", transport: "proxied", wantSeen: "config.yaml=empty", wantAfter: true, wantAfterPerm: 0o600},
		{name: "direct fresh scope", transport: "direct", wantSeen: "config.yaml=empty", wantAfter: true, wantAfterPerm: 0o600},
		{name: "failed init removes the anchor", transport: "proxied", bdExit: 1, wantSeen: "config.yaml=empty"},
		{
			name: "init that wrote metadata then failed keeps the anchor", transport: "proxied", bdExit: 1, bdWritesMeta: true,
			wantSeen: "config.yaml=empty", wantAfter: true,
		},
		{
			name: "retry after a leftover empty anchor", transport: "proxied", seed: map[string]string{"config.yaml": ""},
			wantSeen: "config.yaml=empty", wantAfter: true,
		},
		{
			name: "failed retry leaves a leftover anchor it did not create", transport: "direct", seed: map[string]string{"config.yaml": ""}, bdExit: 1,
			wantSeen: "config.yaml=empty", wantAfter: true,
		},
		{
			name: "existing db file already anchors the scope", transport: "direct", seed: map[string]string{"beads.db": ""},
			wantSeen: "config.yaml=missing",
		},
		{
			name: "vc.db and backups do not count as project files", transport: "proxied",
			seed:     map[string]string{"vc.db": "", "issues.backup.db": ""},
			wantSeen: "config.yaml=empty", wantAfter: true, wantAfterPerm: 0o600,
		},
		{
			name: "existing config is left alone", transport: "proxied", seed: map[string]string{"config.yaml": userConfig},
			wantSeen: "config.yaml=present", wantAfter: true, wantAfterStr: userConfig,
		},
		{
			name: "existing config survives a failed init", transport: "direct", seed: map[string]string{"config.yaml": userConfig}, bdExit: 1,
			wantSeen: "config.yaml=present", wantAfter: true, wantAfterStr: userConfig,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cityDir := t.TempDir()
			scopeDir := filepath.Join(cityDir, "rigs", "anchored")
			beadsDir := filepath.Join(scopeDir, ".beads")
			if err := os.MkdirAll(scopeDir, 0o755); err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(beadsDir, "config.yaml")
			for name, content := range tt.seed {
				if err := os.MkdirAll(beadsDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(beadsDir, name), []byte(content), 0o640); err != nil {
					t.Fatal(err)
				}
			}
			logPath := filepath.Join(t.TempDir(), "bd.log")
			bdPath := filepath.Join(t.TempDir(), "bd")
			fakeBD := "#!/bin/sh\n" +
				"if [ ! -f \"$BEADS_DIR/config.yaml\" ]; then s=missing; elif [ -s \"$BEADS_DIR/config.yaml\" ]; then s=present; else s=empty; fi\n" +
				"printf 'config.yaml=%s\\n' \"$s\" >> " + strconv.Quote(logPath) + "\n"
			if tt.bdWritesMeta {
				fakeBD += "printf '{}\\n' > \"$BEADS_DIR/metadata.json\"\n"
			}
			fakeBD += "exit " + strconv.Itoa(tt.bdExit) + "\n"
			if err := os.WriteFile(bdPath, []byte(fakeBD), 0o755); err != nil { //nolint:gosec // fixture must be executable
				t.Fatal(err)
			}
			env := sanitizedBaseEnv(
				"GC_CITY_PATH="+cityDir,
				"BEADS_DIR="+beadsDir,
				"BD_BIN="+bdPath,
				"GC_BEADS_PROVIDER_OWNED=1",
				"GC_BEADS_TRANSPORT="+tt.transport,
				"GC_BEADS_TARGET=local",
				"GC_BEADS_PROXIED_IDLE_TIMEOUT=0",
			)
			out, err := runProviderOwnedScriptOp(t, env, "init", scopeDir, "anc", "hq")
			if tt.bdExit == 0 && err != nil {
				t.Fatalf("gc-beads-bd init: %v\n%s", err, out)
			}
			if tt.bdExit != 0 && err == nil {
				t.Fatalf("gc-beads-bd init succeeded although bd init failed:\n%s", out)
			}
			data, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(string(data)); got != tt.wantSeen {
				t.Fatalf("bd init saw %q, want %q", got, tt.wantSeen)
			}
			after, err := os.ReadFile(configPath)
			if !tt.wantAfter {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("config.yaml after init = %q (err %v), want absent", after, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("config.yaml after init: %v", err)
			}
			if string(after) != tt.wantAfterStr {
				t.Fatalf("config.yaml after init = %q, want %q", after, tt.wantAfterStr)
			}
			if tt.wantAfterPerm != 0 {
				info, err := os.Stat(configPath)
				if err != nil {
					t.Fatal(err)
				}
				if got := info.Mode().Perm(); got != tt.wantAfterPerm {
					t.Fatalf("anchor mode = %v, want %v", got, tt.wantAfterPerm)
				}
			}
		})
	}
}

// This test stubs the canonical gc helper at the process boundary; the helper's
// reconciliation behavior has its own command tests. Here we pin that provider
// init calls it after bd succeeds with the scope metadata and selected endpoint.
func TestGcBeadsBdProviderOwnedInitEnsuresProjectIdentityAfterSuccessfulBdInit(t *testing.T) {
	const (
		// Loopback IPv6 catches accidental connect_host normalization when
		// reconciling a provider-owned direct/external init binding.
		host      = "::1"
		port      = "3344"
		user      = "provider-user"
		prefix    = "px"
		database  = "provider_db"
		projectID = "provider-owned-project-id"
		bdFailure = 23
		gcFailure = 29
	)
	for _, tc := range []struct {
		name       string
		bdExit     int
		gcExit     int
		wantCalls  []string
		wantAnchor bool
	}{
		{
			name:       "successful init reconciles identity after bd",
			wantCalls:  []string{"bd init", "gc dolt-state ensure-project-id"},
			wantAnchor: true,
		},
		{
			name:       "failed bd init skips identity and removes only the fresh anchor",
			bdExit:     bdFailure,
			wantCalls:  []string{"bd init"},
			wantAnchor: false,
		},
		{
			name:       "identity failure keeps successful init state",
			gcExit:     gcFailure,
			wantCalls:  []string{"bd init", "gc dolt-state ensure-project-id"},
			wantAnchor: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityDir := t.TempDir()
			scopeDir := filepath.Join(cityDir, "rigs", "provider")
			if err := os.MkdirAll(scopeDir, 0o755); err != nil {
				t.Fatal(err)
			}
			captureDir := t.TempDir()
			bdPath := writeNamedTestScript(t, "provider-owned-bd.sh", "#!/bin/sh\nset -eu\ncapture_dir="+strconv.Quote(captureDir)+"\n"+`
printf 'bd init\n' >> "$capture_dir/calls"
printf '%s\n' "$@" > "$capture_dir/bd-args"
if [ "${BD_INIT_EXIT:-0}" -ne 0 ]; then
  exit "$BD_INIT_EXIT"
fi
scope=""
for arg in "$@"; do
  scope="$arg"
done
mkdir -p "$scope/.beads"
cat > "$scope/.beads/metadata.json" <<JSON
{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"provider_db","dolt_server_host":"${GC_DOLT_HOST}","dolt_server_port":${GC_DOLT_PORT},"issue_prefix":"px"}
JSON
`)

			gcPath := writeNamedTestScript(t, "provider-owned-gc-helper.sh", "#!/bin/sh\nset -eu\ncapture_dir="+strconv.Quote(captureDir)+"\n"+`
command="${1:-} ${2:-}"
case "$command" in
  "dolt-state allocate-port")
    printf '%s\n' "${GC_DOLT_PORT:-}"
    exit 0
    ;;
  "dolt-state ensure-project-id")
    shift 2
    printf 'gc dolt-state ensure-project-id\n' >> "$capture_dir/calls"
    printf '%s\n' "$@" > "$capture_dir/gc-args"
    if [ "${GC_HELPER_EXIT:-0}" -ne 0 ]; then
      exit "$GC_HELPER_EXIT"
    fi
    cat > "$BEADS_DIR/identity.toml" <<'TOML'
[project]
id = "provider-owned-project-id"
TOML
    python3 - "$BEADS_DIR/metadata.json" <<'PY'
import json, pathlib, sys
path = pathlib.Path(sys.argv[1])
data = json.loads(path.read_text())
data["project_id"] = "provider-owned-project-id"
path.write_text(json.dumps(data, indent=2) + "\n")
PY
    ;;
  *)
    echo "unexpected gc helper command: $command" >&2
    exit 64
  ;;
esac
`)

			env := sanitizedBaseEnv(
				"GC_CITY_PATH="+cityDir,
				"BEADS_DIR="+filepath.Join(scopeDir, ".beads"),
				"GC_BIN="+gcPath,
				"BD_BIN="+bdPath,
				"GC_BEADS_PROVIDER_OWNED=1",
				"GC_BEADS_TRANSPORT=direct",
				"GC_BEADS_TARGET=external",
				"GC_DOLT_HOST="+host,
				"GC_DOLT_PORT="+port,
				"GC_DOLT_USER="+user,
				"BD_INIT_EXIT="+strconv.Itoa(tc.bdExit),
				"GC_HELPER_EXIT="+strconv.Itoa(tc.gcExit),
			)
			out, err := runProviderOwnedScriptOp(t, env, "init", scopeDir, prefix, database)
			if tc.bdExit == 0 && tc.gcExit == 0 && err != nil {
				t.Fatalf("provider-owned init: %v\n%s", err, out)
			}
			if (tc.bdExit != 0 || tc.gcExit != 0) && err == nil {
				t.Fatalf("provider-owned init succeeded despite configured failure:\n%s", out)
			}
			if tc.bdExit != 0 {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != tc.bdExit {
					t.Fatalf("provider-owned init error = %v, want bd exit %d\n%s", err, tc.bdExit, out)
				}
			}

			calls, err := os.ReadFile(filepath.Join(captureDir, "calls"))
			if err != nil {
				t.Fatalf("read calls: %v", err)
			}
			if got, want := strings.TrimSpace(string(calls)), strings.Join(tc.wantCalls, "\n"); got != want {
				t.Fatalf("calls = %q, want %q", got, want)
			}

			anchorPath := filepath.Join(scopeDir, ".beads", "config.yaml")
			_, anchorErr := os.Stat(anchorPath)
			if tc.wantAnchor && anchorErr != nil {
				t.Fatalf("fresh anchor missing after init: %v", anchorErr)
			}
			if !tc.wantAnchor && !errors.Is(anchorErr, os.ErrNotExist) {
				t.Fatalf("fresh anchor after failed bd init = %v, want removed", anchorErr)
			}

			bdArgs, err := os.ReadFile(filepath.Join(captureDir, "bd-args"))
			if err != nil {
				t.Fatalf("read bd args: %v", err)
			}
			wantBDArgs := []string{
				"init", "--init-if-missing", "--quiet", "--server", "--external",
				"--server-host", host, "--server-port", port,
				"-p", prefix, "--skip-hooks", "--skip-agents", "--database", database, scopeDir,
			}
			if got, want := strings.TrimSpace(string(bdArgs)), strings.Join(wantBDArgs, "\n"); got != want {
				t.Fatalf("bd args = %q, want %q", got, want)
			}

			metadataPath := filepath.Join(scopeDir, ".beads", "metadata.json")
			if tc.bdExit != 0 {
				if _, err := os.Stat(metadataPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("metadata after failed bd init: err=%v, want absent", err)
				}
				if _, err := os.Stat(filepath.Join(captureDir, "gc-args")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("gc helper args after failed bd init: err=%v, want absent", err)
				}
				return
			}

			if tc.gcExit != 0 {
				if _, err := os.Stat(metadataPath); err != nil {
					t.Fatalf("metadata after identity failure: %v", err)
				}
				if _, err := os.Stat(filepath.Join(scopeDir, ".beads", "identity.toml")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("identity after failed reconciliation: err=%v, want absent", err)
				}
				return
			}

			gcArgs, err := os.ReadFile(filepath.Join(captureDir, "gc-args"))
			if err != nil {
				t.Fatalf("read gc args: %v", err)
			}
			wantGCArgs := []string{
				"--city", cityDir,
				"--metadata", metadataPath,
				"--host", host,
				"--port", port,
				"--user", user,
				"--database", database,
			}
			if got, want := strings.TrimSpace(string(gcArgs)), strings.Join(wantGCArgs, "\n"); got != want {
				t.Fatalf("gc args = %q, want %q", got, want)
			}

			meta, ok, err := contract.LoadMetadataState(fsys.OSFS{}, metadataPath)
			if err != nil || !ok {
				t.Fatalf("metadata after init: ok=%v err=%v", ok, err)
			}
			if meta.DoltMode != "server" || meta.DoltDatabase != database {
				t.Fatalf("server metadata = %#v, want database %q in server mode", meta, database)
			}
			binding, ok, err := contract.ReadPersistedServerBinding(fsys.OSFS{}, metadataPath)
			if err != nil || !ok {
				t.Fatalf("server binding after init: ok=%v err=%v", ok, err)
			}
			if binding.DoltHost != host || binding.DoltPort != port {
				t.Fatalf("server binding = %q:%q, want %q:%q", binding.DoltHost, binding.DoltPort, host, port)
			}
			metadataID, err := readManagedMetadataProjectID(metadataPath)
			if err != nil {
				t.Fatalf("read metadata project identity: %v", err)
			}
			identityID, ok, err := contract.ReadProjectIdentity(fsys.OSFS{}, scopeDir)
			if err != nil || !ok {
				t.Fatalf("canonical project identity: ok=%v err=%v", ok, err)
			}
			if metadataID != projectID || identityID != projectID || metadataID != identityID {
				t.Fatalf("project identity metadata=%q canonical=%q, want %q", metadataID, identityID, projectID)
			}
		})
	}
}

func TestGcBeadsBdProviderOwnedLocalInitDefersEndpointResolutionToGc(t *testing.T) {
	for _, transport := range []string{"direct", "proxied"} {
		t.Run(transport, func(t *testing.T) {
			cityDir := t.TempDir()
			scopeDir := filepath.Join(cityDir, "rigs", "provider")
			if err := os.MkdirAll(scopeDir, 0o755); err != nil {
				t.Fatal(err)
			}
			captureDir := t.TempDir()
			bdPath := writeNamedTestScript(t, "provider-owned-local-bd.sh", "#!/bin/sh\nset -eu\ncapture_dir="+strconv.Quote(captureDir)+"\n"+`
printf 'bd init\n' >> "$capture_dir/calls"
scope=""
for arg in "$@"; do scope="$arg"; done
mkdir -p "$scope/.beads"
mode=server
if [ "${GC_BEADS_TRANSPORT:-}" = proxied ]; then mode=proxied-server; fi
printf '{"database":"dolt","backend":"dolt","dolt_mode":"%s","dolt_database":"provider_db"}\n' "$mode" > "$scope/.beads/metadata.json"
`)
			gcPath := writeNamedTestScript(t, "provider-owned-local-gc-helper.sh", "#!/bin/sh\nset -eu\ncapture_dir="+strconv.Quote(captureDir)+"\n"+`
if [ "${1:-} ${2:-}" != "dolt-state ensure-project-id" ]; then
  echo "unexpected gc helper command: $*" >&2
  exit 64
fi
shift 2
printf '%s\n' "$@" > "$capture_dir/gc-args"
`)
			env := sanitizedBaseEnv(
				"GC_CITY_PATH="+cityDir,
				"BEADS_DIR="+filepath.Join(scopeDir, ".beads"),
				"GC_BIN="+gcPath,
				"BD_BIN="+bdPath,
				"GC_BEADS_PROVIDER_OWNED=1",
				"GC_BEADS_TRANSPORT="+transport,
				"GC_BEADS_TARGET=local",
				"GC_BEADS_PROXIED_IDLE_TIMEOUT=0",
			)
			out, err := runProviderOwnedScriptOp(t, env, "init", scopeDir, "px", "provider_db")
			if err != nil {
				t.Fatalf("provider-owned local init: %v\n%s", err, out)
			}
			data, err := os.ReadFile(filepath.Join(captureDir, "gc-args"))
			if err != nil {
				t.Fatalf("read gc args: %v", err)
			}
			args := strings.Split(strings.TrimSpace(string(data)), "\n")
			for _, arg := range args {
				if arg == "--host" || arg == "--port" {
					t.Fatalf("gc helper received managed endpoint arg %q for provider-owned %s init: %q", arg, transport, args)
				}
			}
			wantArgs := []string{
				"--city", cityDir,
				"--metadata", filepath.Join(scopeDir, ".beads", "metadata.json"),
				"--user", "root",
				"--database", "provider_db",
			}
			if got, want := strings.Join(args, "\n"), strings.Join(wantArgs, "\n"); got != want {
				t.Fatalf("gc helper args = %q, want %q", got, want)
			}
		})
	}
}

// A city (or rig) created anywhere below another bd workspace — inside a repo
// that uses beads, or under a home directory holding ~/.beads — must get its
// own store. Before the anchor, bd v1.3.0 ignored the scope's still-empty
// BEADS_DIR, walked up to the ancestor, and `bd init --init-if-missing
// --database hq` aborted with `workspace already initialized as database
// "ancestor"`, failing every fresh `gc init` there (v1.4.2 regression).
func TestGcBeadsBdProviderOwnedRealInitIgnoresAncestorBeadsWorkspace(t *testing.T) {
	skipSlowCmdGCTest(t, "runs the real bd provider init; run make test-cmd-gc-process for full coverage")
	bdPath := waitTestRealBDPath(t)
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt not installed")
	}
	for _, transport := range []string{"direct", "proxied"} {
		t.Run(transport, func(t *testing.T) {
			parent := t.TempDir()
			home := filepath.Join(t.TempDir(), "home")
			if err := os.MkdirAll(home, 0o755); err != nil {
				t.Fatal(err)
			}
			testutil.RunGit(t, parent, "init", "-q")
			// The ancestor workspace, in the on-disk shape of an ordinary
			// embedded bd project (the pinned test bd is built without the
			// embedded engine, so it cannot create one itself). bd's
			// already-initialized probe keys on exactly these two artifacts.
			ancestorBeads := filepath.Join(parent, ".beads")
			if err := os.MkdirAll(filepath.Join(ancestorBeads, "embeddeddolt", "ancestor", ".dolt"), 0o700); err != nil {
				t.Fatal(err)
			}
			ancestorMetadata := []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"embedded","dolt_database":"ancestor"}` + "\n")
			if err := os.WriteFile(filepath.Join(ancestorBeads, "metadata.json"), ancestorMetadata, 0o600); err != nil {
				t.Fatal(err)
			}

			dir := filepath.Join(parent, "city")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			env := sanitizedBaseEnv("HOME="+home, "DOLT_ROOT_PATH="+home, "GC_CITY_PATH="+dir, "BEADS_DIR="+filepath.Join(dir, ".beads"), "BD_BIN="+bdPath, "GC_BEADS_PROVIDER_OWNED=1", "GC_BEADS_TRANSPORT="+transport, "GC_BEADS_TARGET=local", "GC_BEADS_PROXIED_IDLE_TIMEOUT=0")
			t.Cleanup(func() {
				_, _ = runProviderOwnedScriptOp(t, env, "stop")
				for _, root := range []string{dir, parent} {
					if leaked := providerOwnedDoltPIDsUnderRoot(t, root); len(leaked) != 0 {
						if errs := reapDoltLeakPIDs(leaked); len(errs) != 0 {
							t.Errorf("reap processes under %s: %v", root, errs)
						}
					}
				}
			})
			if out, err := runProviderOwnedScriptOp(t, env, "init", dir, "hq", "hq"); err != nil {
				t.Fatalf("gc-beads-bd init below an ancestor bd workspace: %v\n%s", err, out)
			}
			meta, ok, err := contract.LoadMetadataState(fsys.OSFS{}, filepath.Join(dir, ".beads", "metadata.json"))
			if err != nil || !ok {
				t.Fatalf("scope metadata after init: ok=%v err=%v", ok, err)
			}
			if meta.DoltDatabase != "hq" {
				t.Fatalf("scope dolt_database = %q, want hq (bound to the ancestor?)", meta.DoltDatabase)
			}
			wantMode := "server"
			if transport == "proxied" {
				wantMode = "proxied-server"
			}
			if meta.DoltMode != wantMode {
				t.Fatalf("scope dolt_mode = %q, want %q", meta.DoltMode, wantMode)
			}
			if after, err := os.ReadFile(filepath.Join(ancestorBeads, "metadata.json")); err != nil || !bytes.Equal(after, ancestorMetadata) {
				t.Fatalf("ancestor metadata changed by scope init:\nbefore: %s\nafter:  %s (err %v)", ancestorMetadata, after, err)
			}
			if out, err := runProviderOwnedScriptOp(t, env, "health"); err != nil {
				t.Fatalf("gc-beads-bd health after init: %v\n%s", err, out)
			}
		})
	}
}

// providerOwnedDoltPIDsUnderRoot lists live bd proxy / Dolt server processes
// whose command line names root. It reads /proc directly so this file keeps a
// single subprocess call site (see runProviderOwnedScriptOp).
func providerOwnedDoltPIDsUnderRoot(t *testing.T, root string) []int {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatalf("read process table: %v", err)
	}
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil {
			continue // exited between the listing and the read
		}
		args := string(bytes.ReplaceAll(raw, []byte{0}, []byte{' '}))
		if !strings.Contains(args, root) {
			continue
		}
		if strings.Contains(args, "db-proxy-child") || strings.Contains(args, "sql-server") {
			pids = append(pids, pid)
		}
	}
	return pids
}
