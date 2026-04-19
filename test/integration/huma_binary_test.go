//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestHumaBinary_SupervisorBootsAndServesSpec builds `gc`, starts the
// supervisor against an isolated GC_HOME, polls /health, and asserts
// that /openapi.json returns a non-empty spec whose paths include
// /v0/cities. This proves the whole stack wires end-to-end through a
// real binary and a real socket — that the typed-API path generators,
// Huma registration, and listener bootstrap all agree.
//
// The test is build-tagged `integration` so it doesn't run in the
// default `go test ./...` pass; run it explicitly via:
//
//	go test -tags=integration ./test/integration/ -run TestHumaBinary
func TestHumaBinary_SupervisorBootsAndServesSpec(t *testing.T) {
	bin := buildGCBinary(t)

	gcHome := t.TempDir()
	// macOS caps AF_UNIX paths at ~104 chars. t.TempDir() paths on
	// macOS are long enough that <tempdir>/gc/supervisor.sock blows
	// past the limit. Use a short /tmp-rooted dir for XDG_RUNTIME_DIR
	// so the socket path stays under the limit.
	runtimeDir := shortTempDir(t)
	port := reserveFreePort(t)
	writeSupervisorConfig(t, gcHome, port)

	baseURL := "http://127.0.0.1:" + strconv.Itoa(port)
	cityRoot := filepath.Join(gcHome, "city")
	env := append(os.Environ(),
		"GC_HOME="+gcHome,
		"XDG_RUNTIME_DIR="+runtimeDir,
		"GC_BEADS=file",
		"GC_DOLT=skip",
	)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, bin, "supervisor", "run")
	cmd.Env = env
	// Capture supervisor stderr for triage on failure.
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start supervisor: %v", err)
	}
	var supervisorLog strings.Builder
	go func() { _, _ = io.Copy(&supervisorLog, stderr) }()
	cityRegistered := false
	t.Cleanup(func() {
		if cityRegistered {
			runCLIAllowError(t, bin, env, "gc unregister", "unregister", cityRoot)
		}
		runCLIAllowError(t, bin, env, "gc supervisor stop --wait", "supervisor", "stop", "--wait")
		cancel()
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("supervisor stderr:\n%s", supervisorLog.String())
		}
	})

	// Poll /health up to 10 seconds.
	waitHTTP(t, baseURL+"/health", 10*time.Second)

	// Hit /openapi.json and assert the spec looks plausible.
	resp, err := http.Get(baseURL + "/openapi.json")
	if err != nil {
		t.Fatalf("GET /openapi.json: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/openapi.json status %d", resp.StatusCode)
	}
	var spec struct {
		Paths map[string]any `json:"paths"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&spec); err != nil {
		t.Fatalf("decode spec: %v", err)
	}
	if len(spec.Paths) == 0 {
		t.Fatalf("spec has no paths")
	}
	if _, ok := spec.Paths["/v0/cities"]; !ok {
		t.Fatalf("spec missing /v0/cities; got %d paths", len(spec.Paths))
	}

	// Each CLI subcommand below talks to the running supervisor over
	// its real socket. Together these prove the full stack wires through
	// the typed API for both supervisor-scope and per-city commands.

	// 1) `gc cities` — supervisor scope, no city required.
	runCLI(t, bin, env, "gc cities", "cities")

	// 2) Create a minimal city the supervisor can register without relying on
	// any real provider or agent runtime.
	if err := os.MkdirAll(cityRoot, 0o755); err != nil {
		t.Fatalf("create city root: %v", err)
	}
	cityConfig := "[workspace]\nname = \"humatest\"\n\n[beads]\nprovider = \"file\"\n"
	if err := os.WriteFile(filepath.Join(cityRoot, "city.toml"), []byte(cityConfig), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	runCLI(t, bin, env, "gc register", "register", cityRoot, "--name", "humatest")
	cityRegistered = true

	// Give the supervisor a moment to pick up the registered city.
	cityListURL := baseURL + "/v0/cities"
	waitForCityRegistered(t, cityListURL, "humatest", 5*time.Second)

	// 3) `gc city status` — resolves the city path, then calls per-city status.
	runCLI(t, bin, env, "gc city status", "--city", cityRoot, "status")

	// 4) `gc session list` — per-city, exercises a different domain handler.
	runCLI(t, bin, env, "gc session list", "--city", cityRoot, "session", "list")
}

// runCLI executes a gc subcommand against the live supervisor and fails
// the test if the command returns non-zero. label is included in error
// messages to identify which command failed.
func runCLI(t *testing.T, bin string, env []string, label string, args ...string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\noutput: %s", label, err, string(out))
	}
	if len(out) == 0 {
		t.Fatalf("%s produced no output", label)
	}
}

func runCLIAllowError(t *testing.T, bin string, env []string, label string, args ...string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Logf("%s during cleanup: %v\noutput: %s", label, err, string(out))
	}
}

// waitForCityRegistered polls the supervisor's /v0/cities endpoint until
// the named city appears or the deadline expires.
func waitForCityRegistered(t *testing.T, url, city string, deadline time.Duration) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		resp, err := http.Get(url)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if strings.Contains(string(body), `"name":"`+city+`"`) {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for city %q to register at %s", city, url)
}

// buildGCBinary builds cmd/gc into a tempdir and returns the path.
// Caching across subtests is unnecessary — one build per test is <1s.
func buildGCBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "gc")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/gc")
	cmd.Dir = findRepoRoot(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build gc: %v\n%s", err, string(out))
	}
	return bin
}

// findRepoRoot walks up from the test binary's working directory until
// a go.mod is found. The go test runner cds into the test's package dir,
// so the repo root is two parents up.
func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod ancestor of %s", dir)
		}
		dir = parent
	}
}

// reserveFreePort asks the kernel for a free TCP port on loopback, then
// releases it. The caller uses the port number to spawn the supervisor.
// There's a small race between release and bind; in practice it's fine
// for test runs.
func reserveFreePort(t *testing.T) int {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := lis.Addr().(*net.TCPAddr).Port
	_ = lis.Close()
	return port
}

// writeSupervisorConfig writes a minimal ~/.gc/supervisor.toml pinning
// the port. Pre-writing this file prevents the seeding path from
// picking its own port and leaves the test in control of the URL.
func writeSupervisorConfig(t *testing.T, gcHome string, port int) {
	t.Helper()
	if err := os.MkdirAll(gcHome, 0o700); err != nil {
		t.Fatalf("mkdir gc home: %v", err)
	}
	cfg := "[supervisor]\nport = " + strconv.Itoa(port) + "\n"
	if err := os.WriteFile(filepath.Join(gcHome, "supervisor.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("write supervisor.toml: %v", err)
	}
}

// shortTempDir creates a /tmp-rooted dir with a short name suitable
// for XDG_RUNTIME_DIR on macOS where AF_UNIX paths are capped at
// ~104 chars.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "gcit-")
	if err != nil {
		t.Fatalf("short tmp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// waitHTTP polls url until it returns 2xx or deadline expires. Honors
// the test's context so a cancelled parent aborts the loop promptly
// rather than burning the whole deadline.
func waitHTTP(t *testing.T, url string, deadline time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s", url)
		case <-ticker.C:
		}
	}
}
