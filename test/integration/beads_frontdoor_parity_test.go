//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestBeadsFrontDoorDirectAndProxiedParity is the first live acceptance slice
// for ga-p9iuv.5. It deliberately invokes the real pinned bd binary for both
// storage modes; the Gas City BdStore wrapper is not involved. This catches a
// proxy handler that happens to satisfy an injected interface while the public
// CLI shape or persistence semantics diverge.
func TestBeadsFrontDoorDirectAndProxiedParity(t *testing.T) {
	requireDoltIntegration(t)
	baseEnv := newIsolatedToolEnv(t, true)
	baseEnv = filterEnvMany(baseEnv,
		"BEADS_DOLT_PROXIED_SERVER",
		"BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT", "BEADS_DOLT_SERVER_USER",
		"BEADS_DOLT_HOST", "BEADS_DOLT_PORT", "BEADS_DOLT_USER", "BEADS_DOLT_DATABASE",
		"DOLT_HOST", "DOLT_PORT", "DOLT_USER", "DOLT_PASSWORD")

	direct := newFrontDoorFixture(t, baseEnv, "direct")
	directSnapshot := exerciseFrontDoor(t, direct)
	for _, mode := range []string{"proxied-managed", "proxied-external-host", "proxied-external-socket"} {
		t.Run(mode, func(t *testing.T) {
			proxied := newFrontDoorFixture(t, baseEnv, mode)
			proxiedSnapshot := exerciseFrontDoor(t, proxied)
			if reflect.DeepEqual(directSnapshot, proxiedSnapshot) {
				return
			}
			directJSON, _ := json.MarshalIndent(directSnapshot, "", "  ")
			proxiedJSON, _ := json.MarshalIndent(proxiedSnapshot, "", "  ")
			t.Fatalf("direct and %s front-door results diverged\ndirect:\n%s\n%s:\n%s", mode, directJSON, mode, proxiedJSON)
		})
	}
}

// TestBeadsFrontDoorProxiedRefusalsAreSafe pins the negative contract for the
// commands RC1 intentionally withholds. Every refusal must be explicit and
// must leave the authoritative issue unchanged.
func TestBeadsFrontDoorProxiedRefusalsAreSafe(t *testing.T) {
	requireDoltIntegration(t)
	baseEnv := newIsolatedToolEnv(t, true)
	baseEnv = filterEnvMany(baseEnv,
		"BEADS_DOLT_PROXIED_SERVER",
		"BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT", "BEADS_DOLT_SERVER_USER",
		"BEADS_DOLT_HOST", "BEADS_DOLT_PORT", "BEADS_DOLT_USER", "BEADS_DOLT_DATABASE",
		"DOLT_HOST", "DOLT_PORT", "DOLT_USER", "DOLT_PASSWORD")
	fixture := newFrontDoorFixture(t, baseEnv, "proxied-managed")
	root := createFrontDoorIssue(t, fixture, "refusal sentinel")
	before := showFrontDoorIssue(t, fixture, root)

	for _, tc := range []struct {
		name        string
		args        []string
		wantNonzero bool
	}{
		{name: "doctor", args: []string{"doctor", "--readonly", "--json"}},
		{name: "backup sync", args: []string{"backup", "sync"}, wantNonzero: true},
		{name: "show watch", args: []string{"show", root, "--watch"}, wantNonzero: true},
		{name: "rename prefix", args: []string{"rename-prefix", "other"}, wantNonzero: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, err := runFrontDoor(t, fixture, tc.args...)
			if tc.wantNonzero && err == nil {
				t.Fatalf("bd %s unexpectedly succeeded\nstdout:\n%s\nstderr:\n%s", tc.name, stdout, stderr)
			}
			if !tc.wantNonzero && err != nil {
				t.Fatalf("bd %s returned %v; RC1's doctor refusal is informational and must remain a successful, explicit response\nstdout:\n%s\nstderr:\n%s", tc.name, err, stdout, stderr)
			}
			combined := strings.ToLower(stdout + "\n" + stderr)
			if !strings.Contains(combined, "not supported") && !strings.Contains(combined, "not yet supported") {
				t.Fatalf("bd %s failed without an explicit proxied capability refusal: %q", tc.name, stdout+stderr)
			}
			after := showFrontDoorIssue(t, fixture, root)
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("bd %s mutated the issue despite refusing\nbefore=%s\nafter=%s", tc.name, before, after)
			}
		})
	}
}

func TestGasCityGcBdExternalUnixSocketFrontDoor(t *testing.T) {
	requireDoltIntegration(t)
	baseEnv := filterEnvMany(newIsolatedToolEnv(t, true), "BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT", "GC_DOLT_HOST", "GC_DOLT_PORT")
	fixture := newFrontDoorFixture(t, baseEnv, "proxied-external-socket")
	env := append([]string{}, fixture.env...)
	env = append(env, "GC_CITY_PATH="+fixture.dir)
	if _, err := os.Stat(filepath.Join(fixture.dir, ".gc", "runtime", "packs", "dolt", "dolt-state.json")); !os.IsNotExist(err) {
		t.Fatalf("external proxy unexpectedly materialized Gas City Dolt runtime state: %v", err)
	}
	before, err := os.ReadFile(filepath.Join(fixture.dir, ".beads", "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := runCommandStdout(fixture.dir, env, 20*time.Second, gcBinary, "doctor", "--json")
	assertDoctorDoltResult(t, out, "dolt-server", fixture.socketServer.socketPath)
	assertDoctorDoltResult(t, out, "beads-store", fixture.socketServer.socketPath)
	for _, args := range [][]string{{"bd", "create", "front door", "-t", "task", "--json"}, {"bd", "list", "--json"}} {
		if out, err := runCommand(fixture.dir, env, 20*time.Second, gcBinary, args...); err != nil {
			t.Fatalf("gc %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	out, _ = runCommandStdout(fixture.dir, env, 20*time.Second, gcBinary, "doctor", "--json")
	assertDoctorDoltResult(t, out, "dolt-server", fixture.socketServer.socketPath)
	assertDoctorDoltResult(t, out, "beads-store", fixture.socketServer.socketPath)
	beforeTree := snapshotFrontDoorTree(t, filepath.Join(fixture.dir, ".beads"))
	fixture.socketServer.stop(t)
	if out, err := runCommand(fixture.dir, env, 3*time.Second, gcBinary, "bd", "list", "--json"); err == nil || (!strings.Contains(strings.ToLower(out), "socket") && !strings.Contains(strings.ToLower(out), "invalid connection") && !strings.Contains(strings.ToLower(out), "failed to open")) {
		t.Fatalf("gc bd unexpectedly succeeded after socket outage or lacked actionable transport error: %s", out)
	}
	if got := snapshotFrontDoorTree(t, filepath.Join(fixture.dir, ".beads")); !reflect.DeepEqual(beforeTree, got) {
		var changed []string
		for path, before := range beforeTree {
			if after, ok := got[path]; !ok || !bytes.Equal(before, after) {
				changed = append(changed, path)
			}
		}
		for path := range got {
			if _, ok := beforeTree[path]; !ok {
				changed = append(changed, path)
			}
		}
		sort.Strings(changed)
		t.Fatalf("beads files mutated during outage: %v", changed)
	}
	server := startDoltSocketServer(t, fixture.env, filepath.Join(filepath.Dir(fixture.dir), "dolt"))
	defer server.stop(t)
	if out, err := runCommand(fixture.dir, env, 20*time.Second, gcBinary, "bd", "list", "--json"); err != nil {
		t.Fatalf("gc bd after endpoint restart: %v\n%s", err, out)
	}
	got, err := os.ReadFile(filepath.Join(fixture.dir, ".beads", "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, got) {
		t.Fatalf("metadata mutated during outage")
	}
}

func snapshotFrontDoorTree(t *testing.T, root string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	if err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		// Proxy diagnostics are intentionally append-only and may record the
		// outage itself; semantic store/config artifacts are checked below.
		if strings.HasSuffix(strings.ToLower(rel), ".log") {
			return nil
		}
		out[rel] = b
		return nil
	}); err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return out
}

func assertDoctorDoltResult(t *testing.T, output, name, endpoint string) {
	t.Helper()
	raw := []byte(output)
	if start := bytes.Index(raw, []byte("{\"blocking_failed\"")); start >= 0 {
		raw = raw[start:]
	}
	value := decodeFrontDoorJSON(t, raw, "gc doctor")
	root, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("gc doctor JSON is not an object: %#v", value)
	}
	results, ok := root["results"].([]any)
	if !ok {
		t.Fatalf("gc doctor JSON has no results array: %#v", root)
	}
	for _, raw := range results {
		result, ok := raw.(map[string]any)
		if !ok || result["name"] != name {
			continue
		}
		status, ok := result["status"].(string)
		if !ok || (status != "ok" && !(name == "beads-store" && status == "warning")) {
			t.Fatalf("gc doctor %s status is not OK: %#v", name, result)
		}
		message, _ := result["message"].(string)
		if name == "dolt-server" && !strings.Contains(message, endpoint) {
			t.Fatalf("gc doctor %s message %q does not report endpoint %q", name, message, endpoint)
		}
		if name == "beads-store" && (strings.Contains(message, "resolve dolt target") || strings.Contains(message, "runtime state unavailable")) {
			t.Fatalf("gc doctor %s still reports local-runtime failure: %q", name, message)
		}
		return
	}
	t.Fatalf("gc doctor output missing %s result: %#v", name, root)
}

type frontDoorFixture struct {
	dir          string
	env          []string
	mode         string
	socketServer *doltSocketServer
}

func newFrontDoorFixture(t *testing.T, baseEnv []string, mode string) *frontDoorFixture {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "workspace")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create %s workspace: %v", mode, err)
	}
	gitInitWorkspace(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[workspace]\nname = \"frontdoor\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	env := append([]string(nil), baseEnv...)
	env = append(env, "HOME="+filepath.Join(root, "home"), "BEADS_DIR="+filepath.Join(dir, ".beads"))
	var socketServer *doltSocketServer
	switch mode {
	case "direct":
		port := startSharedDoltServer(t, baseEnv, filepath.Join(root, "dolt"))
		runFrontDoorMust(t, &frontDoorFixture{dir: dir, env: env},
			"init", "--server", "--server-host", "127.0.0.1", "--server-port", port,
			"-p", "parity", "--skip-hooks", "--skip-agents", "--quiet")
	case "proxied-managed":
		runFrontDoorMust(t, &frontDoorFixture{dir: dir, env: env},
			"init", "--proxied-server", "--database", "parity_proxy",
			"-p", "parity", "--skip-hooks", "--skip-agents", "--non-interactive", "--quiet")
	case "proxied-external-host":
		port := startSharedDoltServer(t, baseEnv, filepath.Join(root, "dolt"))
		database := "parity_proxy"
		runFrontDoorMust(t, &frontDoorFixture{dir: dir, env: env},
			"init", "--proxied-server", "--database", database,
			"--proxied-server-external-host", "127.0.0.1",
			"--proxied-server-external-port", port,
			"-p", "parity", "--skip-hooks", "--skip-agents", "--non-interactive", "--quiet")
	case "proxied-external-socket":
		socketServer = startDoltSocketServer(t, baseEnv, filepath.Join(root, "dolt"))
		socketPath := socketServer.socketPath
		runFrontDoorMust(t, &frontDoorFixture{dir: dir, env: env},
			"init", "--proxied-server", "--database", "parity_proxy",
			"--proxied-server-external-socket-path", socketPath,
			"-p", "parity", "--skip-hooks", "--skip-agents", "--non-interactive", "--quiet")
	default:
		t.Fatalf("unknown front-door fixture mode %q", mode)
	}
	fixture := &frontDoorFixture{dir: dir, env: env, mode: mode, socketServer: socketServer}
	if _, err := os.Stat(filepath.Join(dir, ".beads", "metadata.json")); err != nil {
		t.Fatalf("%s init did not create metadata.json: %v", mode, err)
	}
	return fixture
}

// startSharedDoltSocketServer starts an explicit Dolt SQL server on a Unix
// socket and returns the socket path. It is the socket counterpart to the
// TCP helper in bdstore_test.go and proves that a proxy can front a remote
// (externally managed) SQL server without changing the bd front door.
func startSharedDoltSocketServer(t *testing.T, env []string, dataDir string) string {
	return startDoltSocketServer(t, env, dataDir).socketPath
}

type doltSocketServer struct {
	socketPath string
	cancel     context.CancelFunc
	waitCh     <-chan error
	logFile    *os.File
	stopped    bool
}

func (s *doltSocketServer) stop(t *testing.T) {
	t.Helper()
	if s.stopped {
		return
	}
	s.stopped = true
	s.cancel()
	if err := <-s.waitCh; err != nil && !strings.Contains(err.Error(), "signal: killed") {
		t.Fatalf("stop dolt socket server: %v", err)
	}
	if err := s.logFile.Close(); err != nil {
		t.Fatalf("close dolt socket log: %v", err)
	}
}

func startDoltSocketServer(t *testing.T, env []string, dataDir string) *doltSocketServer {
	t.Helper()
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("creating dolt data dir: %v", err)
	}
	socketPath := filepath.Join(dataDir, "dolt.sock")
	logPath := filepath.Join(dataDir, "sql-server.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("creating dolt log file: %v", err)
	}
	// Dolt versions used by the RC require a TCP port even when a Unix socket
	// is enabled. Reserve an ephemeral port so parallel fixtures never collide;
	// the test intentionally filters TCP environment and uses only the socket.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = logFile.Close()
		t.Fatalf("reserve dolt tcp port: %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, doltBinary, "sql-server", "--socket", socketPath, "--port", strconv.Itoa(port), "--data-dir", dataDir)
	cmd.Env = env
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatalf("starting dolt socket server: %v", err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	startupCtx, startupCancel := context.WithTimeout(context.Background(), doltServerStartupLimit)
	defer startupCancel()
	if err := waitForUnixSocket(startupCtx, socketPath); err == nil {
		server := &doltSocketServer{socketPath: socketPath, cancel: cancel, waitCh: waitCh, logFile: logFile}
		t.Cleanup(func() { server.stop(t) })
		return server
	}

	cancel()
	<-waitCh
	_ = logFile.Close()
	logBytes, _ := os.ReadFile(logPath)
	t.Fatalf("dolt sql-server did not become ready on unix socket %s within %s:\n%s", socketPath, doltServerStartupLimit, logBytes)
	return nil
}

// waitForUnixSocket waits for a black-box SQL listener to accept a connection.
// The listener has no readiness notification, so this shared, bounded helper
// uses a context-aware ticker and reports the final dial error to its caller.
func waitForUnixSocket(ctx context.Context, socketPath string) error {
	var lastErr error
	probe := func() bool {
		conn, err := net.DialTimeout("unix", socketPath, 200*time.Millisecond)
		if err != nil {
			lastErr = err
			return false
		}
		_ = conn.Close()
		return true
	}
	if probe() {
		return nil
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("waiting for Unix socket %s: %w", socketPath, lastErr)
			}
			return ctx.Err()
		case <-ticker.C:
			if probe() {
				return nil
			}
		}
	}
}

func exerciseFrontDoor(t *testing.T, fixture *frontDoorFixture) map[string]any {
	t.Helper()
	root := createFrontDoorIssue(t, fixture, "parity root")
	child := createFrontDoorIssue(t, fixture, "parity child")
	wisp := createFrontDoorIssue(t, fixture, "parity wisp", "--ephemeral")
	runFrontDoorMust(t, fixture, "dep", "add", child, root)
	runFrontDoorMust(t, fixture, "comment", root, "parity comment")
	runFrontDoorMust(t, fixture, "update", root, "--add-label", "parity", "--set-metadata", "parity=true")
	runFrontDoorMust(t, fixture, "update", root, "--claim", "--actor", "parity-agent")
	claimed := showFrontDoorIssue(t, fixture, root)
	runFrontDoorMust(t, fixture, "unclaim", root, "--actor", "parity-agent")
	runFrontDoorMust(t, fixture, "close", root)
	runFrontDoorMust(t, fixture, "reopen", root)
	gate := decodeFrontDoorJSON(t, runFrontDoorMust(t, fixture, "gate", "create", "--blocks", root, "--type", "human", "--json"), "gate create")
	gateID := frontDoorJSONID(t, gate, "gate create")
	gateList := decodeFrontDoorJSON(t, runFrontDoorMust(t, fixture, "gate", "list", "--json"), "gate list")
	runFrontDoorMust(t, fixture, "gate", "resolve", gateID)
	runFrontDoorMust(t, fixture, "delete", child, "--force")
	if _, _, err := runFrontDoor(t, fixture, "show", child, "--json"); err == nil {
		t.Fatalf("deleted child %s is still visible", child)
	}
	runFrontDoorMust(t, fixture, "config", "set", "parity.mode", fixture.mode)
	configValue := strings.TrimSpace(string(runFrontDoorMust(t, fixture, "config", "get", "parity.mode")))
	status := decodeFrontDoorJSON(t, runFrontDoorMust(t, fixture, "status", "--json", "--no-activity"), "status")
	exportPath := filepath.Join(fixture.dir, "parity-export.jsonl")
	runFrontDoorMust(t, fixture, "export", "--all", "-o", exportPath)
	exportData, err := os.ReadFile(exportPath)
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	if len(bytes.TrimSpace(exportData)) == 0 {
		t.Fatal("export produced an empty JSONL file")
	}
	runFrontDoorMust(t, fixture, "import", "--input", exportPath, "--json")

	show := showFrontDoorIssue(t, fixture, root, "--include-comments")
	list := decodeFrontDoorJSON(t, runFrontDoorMust(t, fixture, "list", "--json", "--all", "--limit=0"), "list")
	sql := decodeFrontDoorJSON(t, runFrontDoorMust(t, fixture, "sql", "--json", "SELECT title,status FROM issues ORDER BY title"), "sql")
	wispShow := showFrontDoorIssue(t, fixture, wisp)
	return normalizeFrontDoorJSON(map[string]any{
		"show": show, "list": list, "sql": sql, "claimed": claimed,
		"gate": gate, "gate_list": gateList, "config": configValue,
		"status": status, "wisp": wispShow,
	}, map[string]string{
		root:        "<root>",
		child:       "<child>",
		wisp:        "<wisp>",
		gateID:      "<gate>",
		configValue: "<configured>",
	})
}

func createFrontDoorIssue(t *testing.T, fixture *frontDoorFixture, title string, extra ...string) string {
	t.Helper()
	args := []string{"create", "--json", "--type", "task", "--priority", "1", "--description", "front-door parity"}
	args = append(args, title)
	args = append(args, extra...)
	out := runFrontDoorMust(t, fixture, args...)
	v := decodeFrontDoorJSON(t, out, "create")
	if list, ok := v.([]any); ok && len(list) > 0 {
		v = list[0]
	}
	obj, ok := v.(map[string]any)
	if !ok || obj["id"] == nil {
		t.Fatalf("create JSON has no issue id: %#v", v)
	}
	id, ok := obj["id"].(string)
	if !ok || id == "" {
		t.Fatalf("create JSON id = %#v, want non-empty string", obj["id"])
	}
	return id
}

func frontDoorJSONID(t *testing.T, value any, operation string) string {
	t.Helper()
	obj, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s JSON is not an object: %#v", operation, value)
	}
	id, ok := obj["id"].(string)
	if !ok || id == "" {
		t.Fatalf("%s JSON id = %#v, want non-empty string", operation, obj["id"])
	}
	return id
}

func showFrontDoorIssue(t *testing.T, fixture *frontDoorFixture, id string, extra ...string) any {
	t.Helper()
	args := append([]string{"show", id, "--json"}, extra...)
	return decodeFrontDoorJSON(t, runFrontDoorMust(t, fixture, args...), "show")
}

func runFrontDoorMust(t *testing.T, fixture *frontDoorFixture, args ...string) []byte {
	t.Helper()
	stdout, stderr, err := runFrontDoor(t, fixture, args...)
	if err != nil {
		t.Fatalf("bd %s (%s) failed: %v\nstdout:\n%s\nstderr:\n%s", fixture.mode, strings.Join(args, " "), err, stdout, stderr)
	}
	return []byte(stdout)
}

func runFrontDoor(t *testing.T, fixture *frontDoorFixture, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := exec.Command(realBDBinary, args...)
	cmd.Dir = fixture.dir
	cmd.Env = fixture.env
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err = cmd.Run()
	return out.String(), errOut.String(), err
}

func decodeFrontDoorJSON(t *testing.T, raw []byte, operation string) any {
	t.Helper()
	trimmed := bytes.TrimSpace(raw)
	start := bytes.IndexAny(trimmed, "[{\"")
	if start < 0 {
		t.Fatalf("%s emitted no JSON: %q", operation, string(raw))
	}
	var value any
	if err := json.Unmarshal(trimmed[start:], &value); err != nil {
		t.Fatalf("%s emitted invalid JSON: %v\nraw: %s", operation, err, raw)
	}
	return value
}

func normalizeFrontDoorJSON(value any, ids map[string]string) map[string]any {
	normalized, ok := normalizeFrontDoorValue(value, ids).(map[string]any)
	if !ok {
		return map[string]any{"value": normalized}
	}
	return normalized
}

func normalizeFrontDoorValue(value any, ids map[string]string) any {
	switch v := value.(type) {
	case string:
		if replacement, ok := ids[v]; ok {
			return replacement
		}
		for id, replacement := range ids {
			v = strings.ReplaceAll(v, id, replacement)
		}
		if _, err := time.Parse(time.RFC3339Nano, v); err == nil {
			return "<timestamp>"
		}
		return v
	case []any:
		out := make([]any, len(v))
		for i := range v {
			out[i] = normalizeFrontDoorValue(v[i], ids)
		}
		// SQL and list results are both sets for this contract. Sorting their
		// canonical JSON makes the comparison independent of backend ordering.
		sort.SliceStable(out, func(i, j int) bool { return fmt.Sprint(out[i]) < fmt.Sprint(out[j]) })
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			switch key {
			case "id", "created_at", "updated_at", "closed_at", "created_by", "updated_by", "revision":
				continue
			default:
				out[key] = normalizeFrontDoorValue(item, ids)
			}
		}
		return out
	default:
		return value
	}
}
