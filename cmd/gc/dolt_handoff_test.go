package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func decodeHandoffResponse(t *testing.T, raw []byte) handoffProtocolResponse {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var response handoffProtocolResponse
	if err := decoder.Decode(&response); err != nil {
		t.Fatalf("decode handoff response %q: %v", raw, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("handoff response has trailing JSON: %v", err)
	}
	return response
}

func handoffTestArgs(operation, city string) []string {
	return []string{
		"dolt-state", operation, "--json", "--city", city, "--scope-root", city,
		"--database", "beads", "--workspace", "test", "--host", "127.0.0.1", "--port", "3307",
	}
}

func TestDoltStateHandoffRefusalIsStrictJSON(t *testing.T) {
	city := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := run(handoffTestArgs("handoff-inspect", city), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run() = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	response := decodeHandoffResponse(t, stdout.Bytes())
	if response.SchemaVersion != handoffProtocolSchemaVersion || response.Operation != "handoff-inspect" {
		t.Fatalf("response envelope = %+v", response)
	}
	if response.Result != "refused" || response.ErrorCode != "state_missing" {
		t.Fatalf("response = %+v, want state_missing refusal", response)
	}
	if stderr.Len() != 0 {
		t.Fatalf("handoff protocol wrote stderr: %q", stderr.String())
	}
}

func TestDoltStateHandoffInspectDoesNotCreateLifecycleFiles(t *testing.T) {
	city := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := run(handoffTestArgs("handoff-inspect", city), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run() = %d, want 1", code)
	}
	layout, err := resolveManagedDoltRuntimeLayout(city)
	if err != nil {
		t.Fatalf("resolve layout: %v", err)
	}
	if _, err := os.Stat(layout.LockFile); !os.IsNotExist(err) {
		t.Fatalf("inspect created lifecycle lock %q (err=%v)", layout.LockFile, err)
	}
	if _, err := os.Stat(layout.PackStateDir); !os.IsNotExist(err) {
		t.Fatalf("inspect created runtime directory %q (err=%v)", layout.PackStateDir, err)
	}
}

func TestDoltStateHandoffRejectsSocketBeforeLifecycleMutation(t *testing.T) {
	city := t.TempDir()
	layout, err := resolveManagedDoltRuntimeLayout(city)
	if err != nil {
		t.Fatalf("resolve layout: %v", err)
	}
	socket := filepath.Join(city, "dolt.sock")
	args := []string{
		"dolt-state", "handoff-inspect", "--json", "--city", city, "--scope-root", city,
		"--database", "beads", "--workspace", "test", "--socket", socket,
	}
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run() = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	response := decodeHandoffResponse(t, stdout.Bytes())
	if response.Result != "refused" || response.ErrorCode != "unsupported_scope" || response.Mutates {
		t.Fatalf("response = %+v, want unsupported_scope refusal with mutates=false", response)
	}
	if stderr.Len() != 0 {
		t.Fatalf("handoff protocol wrote stderr: %q", stderr.String())
	}
	for name, path := range map[string]string{
		"pack state dir": layout.PackStateDir,
		"data dir":       layout.DataDir,
		"state file":     layout.StateFile,
		"pid file":       layout.PIDFile,
		"lock file":      layout.LockFile,
		"config file":    layout.ConfigFile,
	} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatalf("socket refusal created or changed %s %q (stat err=%v)", name, path, statErr)
		}
	}
}

func TestDoltStateHandoffStopRejectsSocketBeforeLifecycleMutation(t *testing.T) {
	city := t.TempDir()
	layout, err := resolveManagedDoltRuntimeLayout(city)
	if err != nil {
		t.Fatalf("resolve layout: %v", err)
	}
	args := []string{
		"dolt-state", "handoff-stop", "--json", "--city", city, "--scope-root", city,
		"--database", "beads", "--workspace", "test", "--socket", filepath.Join(city, "dolt.sock"),
	}
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run() = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	response := decodeHandoffResponse(t, stdout.Bytes())
	if response.Result != "refused" || response.ErrorCode != "unsupported_scope" || response.Mutates {
		t.Fatalf("response = %+v, want unsupported_scope refusal with mutates=false", response)
	}
	if stderr.Len() != 0 {
		t.Fatalf("handoff protocol wrote stderr: %q", stderr.String())
	}
	for name, path := range map[string]string{
		"pack state dir": layout.PackStateDir,
		"data dir":       layout.DataDir,
		"state file":     layout.StateFile,
		"pid file":       layout.PIDFile,
		"lock file":      layout.LockFile,
		"config file":    layout.ConfigFile,
	} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatalf("socket refusal created or changed %s %q (stat err=%v)", name, path, statErr)
		}
	}
}

func TestDoltStateHandoffRejectsMalformedPersistedState(t *testing.T) {
	city := t.TempDir()
	layout, err := resolveManagedDoltRuntimeLayout(city)
	if err != nil {
		t.Fatalf("resolve layout: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(layout.StateFile), 0o755); err != nil {
		t.Fatalf("create state dir: %v", err)
	}
	if err := os.WriteFile(layout.StateFile, []byte("not-json\n"), 0o644); err != nil {
		t.Fatalf("write malformed state: %v", err)
	}
	var stdout, stderr bytes.Buffer
	code := run(handoffTestArgs("handoff-inspect", city), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run() = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	response := decodeHandoffResponse(t, stdout.Bytes())
	if response.ErrorCode != "state_missing" || response.Result != "refused" {
		t.Fatalf("response = %+v, want state_missing refusal", response)
	}
	if stderr.Len() != 0 {
		t.Fatalf("handoff protocol wrote stderr: %q", stderr.String())
	}
}

func TestDoltStateHandoffRefusesBusyLifecycle(t *testing.T) {
	city := t.TempDir()
	lock, _, err := openManagedDoltLifecycleLock(city)
	if err != nil {
		t.Fatalf("open lifecycle lock: %v", err)
	}
	locked, err := tryManagedDoltLifecycleLock(lock)
	if err != nil || !locked {
		t.Fatalf("acquire lifecycle lock: locked=%t err=%v", locked, err)
	}
	t.Cleanup(func() { releaseManagedDoltLifecycleLock(lock) })

	var stdout, stderr bytes.Buffer
	code := run(handoffTestArgs("handoff-inspect", city), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run() = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	response := decodeHandoffResponse(t, stdout.Bytes())
	if response.ErrorCode != "lifecycle_busy" || response.Result != "refused" {
		t.Fatalf("response = %+v, want lifecycle_busy refusal", response)
	}
	if stderr.Len() != 0 {
		t.Fatalf("handoff protocol wrote stderr: %q", stderr.String())
	}
}

func TestHandoffIdentityTokenChangesWithProcessIdentity(t *testing.T) {
	identity := handoffProtocolIdentity{
		CityRoot: "/city", ScopeRoot: "/city", Database: "beads", Workspace: "test",
		Endpoint: handoffProtocolEndpoint{Host: "127.0.0.1", Port: 3307}, DataDir: "/city/.beads/dolt", ConfigFile: "/city/.gc/dolt.yaml",
		PID: 42, StartTimeTicks: 100,
	}
	first := handoffIdentityToken(identity)
	identity.PID = 43
	if second := handoffIdentityToken(identity); second == first {
		t.Fatalf("identity token did not change when PID changed: %q", first)
	}
}

func TestDoltStateHandoffInspectAndStopOwnedProcess(t *testing.T) {
	city := t.TempDir()
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"test\"\n"), 0o644); err != nil {
		t.Fatalf("write city config: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(city, ".beads"), 0o755); err != nil {
		t.Fatalf("create beads dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(city, ".beads", "config.yaml"), []byte("gc.endpoint_origin: managed_city\ngc.endpoint_status: verified\n"), 0o644); err != nil {
		t.Fatalf("write beads config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(city, ".beads", "metadata.json"), []byte(`{"database":"beads","backend":"dolt","dolt_mode":"server","dolt_database":"beads","project_id":"test"}`), 0o644); err != nil {
		t.Fatalf("write beads metadata: %v", err)
	}
	if err := os.WriteFile(filepath.Join(city, ".beads", "identity.toml"), []byte("[project]\nid = \"test\"\n"), 0o644); err != nil {
		t.Fatalf("write project identity: %v", err)
	}
	layout, err := resolveManagedDoltRuntimeLayout(city)
	if err != nil {
		t.Fatalf("resolve layout: %v", err)
	}
	if err := os.MkdirAll(layout.DataDir, 0o755); err != nil {
		t.Fatalf("create data dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(layout.ConfigFile), 0o755); err != nil {
		t.Fatalf("create config dir: %v", err)
	}
	if err := os.WriteFile(layout.ConfigFile, []byte("listener: 127.0.0.1\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	lock, _, err := openManagedDoltLifecycleLock(city)
	if err != nil {
		t.Fatalf("create lifecycle lock: %v", err)
	}
	releaseManagedDoltLifecycleLock(lock)
	port := reserveRandomTCPPort(t)
	// Give the child the same argv shape as production's `dolt sql-server
	// --config ...` while using Python as a deterministic TCP fixture.
	childScript := `
import signal, socket, sys, time
port = int(sys.argv[1])
sock = socket.socket()
sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
sock.bind(("127.0.0.1", port))
sock.listen(4)
signal.signal(signal.SIGTERM, lambda *_: sys.exit(0))
signal.signal(signal.SIGINT, lambda *_: sys.exit(0))
while True:
    conn, _ = sock.accept()
    conn.close()
`
	proc := exec.Command("bash", "-c", `exec -a "$1" python3 -c "$2" "$3"`, "handoff-fixture",
		"dolt sql-server --config "+layout.ConfigFile, childScript, strconv.Itoa(port))
	proc.Dir = layout.DataDir
	if err := proc.Start(); err != nil {
		t.Fatalf("start managed fixture: %v", err)
	}
	t.Cleanup(func() {
		if proc.Process != nil {
			_ = proc.Process.Kill()
		}
		_ = proc.Wait()
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, dialErr := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 100*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if conn, dialErr := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 100*time.Millisecond); dialErr != nil {
		t.Fatalf("managed fixture did not become reachable: %v", dialErr)
	} else {
		_ = conn.Close()
	}
	if err := writeDoltRuntimeStateFile(layout.StateFile, doltRuntimeState{Running: true, PID: proc.Process.Pid, Port: port, DataDir: layout.DataDir}); err != nil {
		t.Fatalf("write runtime state: %v", err)
	}

	inspectArgs := handoffTestArgs("handoff-inspect", city)
	inspectArgs[len(inspectArgs)-1] = strconv.Itoa(port)
	var inspectOut, inspectErr bytes.Buffer
	code := run(inspectArgs, &inspectOut, &inspectErr)
	if code != 0 {
		t.Fatalf("inspect run() = %d; stdout=%s stderr=%s", code, inspectOut.String(), inspectErr.String())
	}
	inspect := decodeHandoffResponse(t, inspectOut.Bytes())
	if inspect.Result != "eligible" || inspect.Identity.PID != proc.Process.Pid || inspect.IdentityToken == "" {
		t.Fatalf("inspect response = %+v, want eligible identity", inspect)
	}

	stopArgs := handoffTestArgs("handoff-stop", city)
	stopArgs[len(stopArgs)-1] = strconv.Itoa(port)
	stopArgs = append(stopArgs, "--identity-token", inspect.IdentityToken)
	var stopOut, stopErr bytes.Buffer
	code = run(stopArgs, &stopOut, &stopErr)
	if code != 0 {
		t.Fatalf("stop run() = %d; stdout=%s stderr=%s", code, stopOut.String(), stopErr.String())
	}
	stopped := decodeHandoffResponse(t, stopOut.Bytes())
	if stopped.Result != "stopped" || !stopped.Mutates || stopped.IdentityToken != inspect.IdentityToken {
		t.Fatalf("stop response = %+v, want stopped with matching token", stopped)
	}
}

func writeHandoffPersistedFixture(t *testing.T, city, backend, mode, database, projectID string) managedDoltRuntimeLayout {
	t.Helper()
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"test\"\n"), 0o644); err != nil {
		t.Fatalf("write city config: %v", err)
	}
	beadsDir := filepath.Join(city, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("create beads dir: %v", err)
	}
	configBody := "gc.endpoint_origin: managed_city\ngc.endpoint_status: verified\n"
	if mode != "" {
		configBody += "dolt.mode: " + mode + "\n"
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte(configBody), 0o644); err != nil {
		t.Fatalf("write beads config: %v", err)
	}
	metadata := `{"database":"` + database + `","backend":"` + backend + `","dolt_database":"` + database + `","project_id":"` + projectID + `"}`
	if mode != "" {
		metadata = `{"database":"` + database + `","backend":"` + backend + `","dolt_mode":"` + mode + `","dolt_database":"` + database + `","project_id":"` + projectID + `"}`
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0o644); err != nil {
		t.Fatalf("write beads metadata: %v", err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "identity.toml"), []byte("[project]\nid = \""+projectID+"\"\n"), 0o644); err != nil {
		t.Fatalf("write project identity: %v", err)
	}
	layout, err := resolveCanonicalManagedDoltRuntimeLayout(city)
	if err != nil {
		t.Fatalf("resolve canonical layout: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(layout.StateFile), 0o755); err != nil {
		t.Fatalf("create runtime dir: %v", err)
	}
	if err := os.WriteFile(layout.StateFile, []byte(`{"running":false,"pid":0,"port":3307,"data_dir":"`+layout.DataDir+`"}`), 0o644); err != nil {
		t.Fatalf("write runtime state: %v", err)
	}
	lock, err := os.OpenFile(layout.LockFile, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("create lifecycle lock: %v", err)
	}
	_ = lock.Close()
	return layout
}

func TestDoltStateHandoffRejectsPersistedIdentityMismatch(t *testing.T) {
	tests := []struct {
		name      string
		backend   string
		mode      string
		database  string
		projectID string
		mutate    func([]string) []string
		wantCode  string
	}{
		{name: "database", backend: "dolt", database: "beads", projectID: "test", mutate: func(args []string) []string {
			out := append([]string(nil), args...)
			for i := range out {
				if out[i] == "beads" && i > 0 && out[i-1] == "--database" {
					out[i] = "other"
				}
			}
			return out
		}, wantCode: "identity_changed"},
		{name: "workspace", backend: "dolt", database: "beads", projectID: "test", mutate: func(args []string) []string {
			out := append([]string(nil), args...)
			for i := range out {
				if out[i] == "test" && i > 0 && out[i-1] == "--workspace" {
					out[i] = "other"
				}
			}
			return out
		}, wantCode: "identity_changed"},
		{name: "doltlite", backend: "doltlite", database: "beads", projectID: "test", wantCode: "unsupported_scope"},
		{name: "embedded", backend: "dolt", mode: "embedded", database: "beads", projectID: "test", wantCode: "unsupported_scope"},
		{name: "proxied", backend: "dolt", mode: "proxied-server", database: "beads", projectID: "test", wantCode: "unsupported_scope"},
		{name: "unknown mode", backend: "dolt", mode: "mystery", database: "beads", projectID: "test", wantCode: "unsupported_scope"},
		{name: "legacy metadata", backend: "legacy", database: "beads", projectID: "test", wantCode: "process_missing"},
		{name: "foreign backend", backend: "postgres", database: "beads", projectID: "test", wantCode: "unsupported_scope"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			city := t.TempDir()
			layout := writeHandoffPersistedFixture(t, city, tc.backend, tc.mode, tc.database, tc.projectID)
			args := handoffTestArgs("handoff-inspect", city)
			if tc.mutate != nil {
				args = tc.mutate(args)
			}
			var stdout, stderr bytes.Buffer
			code := run(args, &stdout, &stderr)
			if code != 1 {
				t.Fatalf("run() = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			response := decodeHandoffResponse(t, stdout.Bytes())
			if response.Result != "refused" || response.ErrorCode != tc.wantCode || response.Mutates {
				t.Fatalf("response = %+v, want %s refusal with mutates=false", response, tc.wantCode)
			}
			if stderr.Len() != 0 {
				t.Fatalf("handoff protocol wrote stderr: %q", stderr.String())
			}
			if data, err := os.ReadFile(layout.StateFile); err != nil || !bytes.Contains(data, []byte(`"running":false`)) {
				t.Fatalf("runtime state changed after refusal: %q (err=%v)", data, err)
			}
		})
	}
}

func TestDoltStateHandoffRejectsUnknownDoltModeWithoutMutation(t *testing.T) {
	city := t.TempDir()
	layout := writeHandoffPersistedFixture(t, city, "dolt", "mystery", "beads", "test")
	var stdout, stderr bytes.Buffer
	code := run(handoffTestArgs("handoff-inspect", city), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run() = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	response := decodeHandoffResponse(t, stdout.Bytes())
	if response.Result != "refused" || response.ErrorCode != "unsupported_scope" || response.Mutates {
		t.Fatalf("response = %+v, want unsupported_scope refusal with mutates=false", response)
	}
	if stderr.Len() != 0 {
		t.Fatalf("handoff protocol wrote stderr: %q", stderr.String())
	}
	data, err := os.ReadFile(layout.StateFile)
	if err != nil {
		t.Fatalf("read runtime state after refusal: %v", err)
	}
	if !bytes.Contains(data, []byte(`"running":false`)) {
		t.Fatalf("runtime state changed after refusal: %q", data)
	}
}

func TestDoltStateHandoffRejectsUndeclaredScopeAndIgnoresAmbientLayout(t *testing.T) {
	city := t.TempDir()
	layout := writeHandoffPersistedFixture(t, city, "dolt", "", "beads", "test")
	foreign := t.TempDir()
	foreignLayout := writeHandoffPersistedFixture(t, foreign, "dolt", "", "beads", "test")
	if err := writeDoltRuntimeStateFile(foreignLayout.StateFile, doltRuntimeState{Running: true, PID: os.Getpid(), Port: 3307, DataDir: foreignLayout.DataDir}); err != nil {
		t.Fatalf("write foreign runtime state: %v", err)
	}
	t.Setenv("GC_PACK_STATE_DIR", foreignLayout.PackStateDir)
	t.Setenv("GC_DOLT_DATA_DIR", foreignLayout.DataDir)
	t.Setenv("GC_DOLT_STATE_FILE", foreignLayout.StateFile)
	t.Setenv("GC_DOLT_PID_FILE", foreignLayout.PIDFile)
	t.Setenv("GC_DOLT_LOCK_FILE", foreignLayout.LockFile)
	t.Setenv("GC_DOLT_CONFIG_FILE", foreignLayout.ConfigFile)
	t.Setenv("GC_DOLT_LOG_FILE", foreignLayout.LogFile)
	args := handoffTestArgs("handoff-inspect", city)
	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != 1 {
		t.Fatalf("run() = %d, want refusal", code)
	}
	response := decodeHandoffResponse(t, stdout.Bytes())
	if response.ErrorCode != "process_missing" || response.Mutates {
		t.Fatalf("response = %+v, want canonical process_missing refusal", response)
	}
	if _, err := os.Stat(layout.StateFile); err != nil {
		t.Fatalf("canonical runtime state missing after refusal: %v", err)
	}

	undeclared := filepath.Join(city, "invented")
	if err := os.MkdirAll(undeclared, 0o755); err != nil {
		t.Fatalf("create undeclared scope: %v", err)
	}
	args = handoffTestArgs("handoff-inspect", city)
	for i := range args {
		if args[i] == city && i > 0 && args[i-1] == "--scope-root" {
			args[i] = undeclared
		}
	}
	stdout.Reset()
	stderr.Reset()
	if code := run(args, &stdout, &stderr); code != 1 {
		t.Fatalf("undeclared scope run() = %d, want refusal", code)
	}
	response = decodeHandoffResponse(t, stdout.Bytes())
	if response.ErrorCode != "unsupported_scope" || response.Mutates {
		t.Fatalf("undeclared scope response = %+v, want unsupported_scope refusal", response)
	}
}
