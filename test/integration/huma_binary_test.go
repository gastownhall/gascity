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

	// macOS caps AF_UNIX paths at ~104 chars. t.TempDir() paths on
	// macOS are long enough that <tempdir>/supervisor.sock blows past
	// the limit. An isolated GC_HOME override keeps the supervisor
	// socket under GC_HOME, so both GC_HOME and XDG_RUNTIME_DIR must
	// live under the short /tmp-rooted test directory.
	root := shortTempDir(t)
	gcHome := filepath.Join(root, "home")
	runtimeDir := filepath.Join(root, "run")
	for _, dir := range []string{gcHome, runtimeDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	port := reserveFreePort(t)
	writeSupervisorConfig(t, gcHome, port)
	if err := seedDoltIdentityForRoot(gcHome); err != nil {
		t.Fatalf("seed dolt identity: %v", err)
	}

	baseURL := "http://127.0.0.1:" + strconv.Itoa(port)
	cityRoot := filepath.Join(gcHome, "city")
	env := integrationEnvFor(gcHome, runtimeDir, true)

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

	// 1) `gc cities list` — supervisor scope, no city required.
	runCLI(t, bin, env, "gc cities list", "cities", "list")

	// 2) `gc cities` (default action) — legacy alias still must work.
	runCLI(t, bin, env, "gc cities", "cities")

	// 3) Create a minimal city the supervisor can register without relying on
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

	// 4) `gc city status --city <path>` — resolves the city path and calls the
	// per-city status endpoint through the supervisor.
	runCLI(t, bin, env, "gc status", "--city", cityRoot, "status")

	// 5) `gc session list --city <path>` — per-city, exercises a different
	// domain handler through the supervisor.
	runCLI(t, bin, env, "gc session list", "--city", cityRoot, "session", "list", "--state", "all")
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

// TestHumaBinary_CityCreateAsync exercises the async POST /v0/city
// contract end-to-end against a live supervisor: subscribe to
// /v0/events/stream, POST /v0/city, verify the handler returns 202
// immediately with {ok, name, path}, then assert a city.ready event
// for that city name arrives on the SSE stream. This is the test MC's
// live contract harness implicitly needs — without it, any
// regression in Scaffold, the reconciler's city.ready emission, or
// the supervisor event multiplexer would ship unnoticed.
//
// Build-tagged `integration`; run with:
//
//	go test -tags=integration ./test/integration/ -run TestHumaBinary_CityCreateAsync
func TestHumaBinary_CityCreateAsync(t *testing.T) {
	bin := buildGCBinary(t)

	root := shortTempDir(t)
	gcHome := filepath.Join(root, "home")
	runtimeDir := filepath.Join(root, "run")
	for _, dir := range []string{gcHome, runtimeDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	port := reserveFreePort(t)
	writeSupervisorConfig(t, gcHome, port)
	if err := seedDoltIdentityForRoot(gcHome); err != nil {
		t.Fatalf("seed dolt identity: %v", err)
	}

	baseURL := "http://127.0.0.1:" + strconv.Itoa(port)
	env := integrationEnvFor(gcHome, runtimeDir, true)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, bin, "supervisor", "run")
	cmd.Env = env
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start supervisor: %v", err)
	}
	var supervisorLog strings.Builder
	go func() { _, _ = io.Copy(&supervisorLog, stderr) }()
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("supervisor stderr:\n%s", supervisorLog.String())
		}
	})

	waitHTTP(t, baseURL+"/health", 10*time.Second)

	// 1. POST /v0/city. Expected: 202 Accepted, body contains name
	// matching the directory basename. We POST first because the
	// supervisor event stream rejects subscriptions when no event
	// providers are registered (503 no_providers), which is the
	// case before any city exists.
	cityDir := filepath.Join(gcHome, "async-test-city")
	body := `{"dir":"` + cityDir + `","provider":"claude"}`
	postReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v0/city", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build post request: %v", err)
	}
	postReq.Header.Set("Content-Type", "application/json")
	postReq.Header.Set("X-GC-Request", "true")
	postStart := time.Now()
	postResp, err := http.DefaultClient.Do(postReq)
	if err != nil {
		t.Fatalf("POST /v0/city: %v", err)
	}
	postDur := time.Since(postStart)
	postBody, _ := io.ReadAll(postResp.Body)
	_ = postResp.Body.Close()
	if postResp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /v0/city status = %d, want 202; body: %s", postResp.StatusCode, string(postBody))
	}
	if postDur > 20*time.Second {
		t.Errorf("POST /v0/city took %s, want fast scaffold response (<20s); async contract is broken", postDur)
	}
	var createResp struct {
		OK   bool   `json:"ok"`
		Name string `json:"name"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal(postBody, &createResp); err != nil {
		t.Fatalf("decode create response: %v; body: %s", err, string(postBody))
	}
	if !createResp.OK {
		t.Errorf("ok = false; body: %s", string(postBody))
	}
	if createResp.Name == "" {
		t.Fatalf("empty city name in response; body: %s", string(postBody))
	}
	if createResp.Path != cityDir {
		t.Errorf("path = %q, want %q", createResp.Path, cityDir)
	}
	t.Logf("POST /v0/city returned 202 in %s for city %q", postDur.Round(time.Millisecond), createResp.Name)

	// 2. Subscribe to /v0/events/stream. No retry: Scaffold writes
	// the city to cities.toml synchronously before POST returns, and
	// TransientCityEventProviders reads cities.toml directly, so the
	// mux contains this city's event provider by the time the client
	// receives 202. after_cursor=0 requests replay from the start
	// so the client doesn't miss city.ready if it fires between POST
	// return and subscribe.
	streamCtx, streamCancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(streamCancel)
	streamReq, err := http.NewRequestWithContext(streamCtx, http.MethodGet, baseURL+"/v0/events/stream?after_cursor=0", nil)
	if err != nil {
		t.Fatalf("build stream request: %v", err)
	}
	streamReq.Header.Set("Accept", "text/event-stream")
	streamResp, err := http.DefaultClient.Do(streamReq)
	if err != nil {
		t.Fatalf("GET /v0/events/stream: %v", err)
	}
	defer streamResp.Body.Close() //nolint:errcheck
	if streamResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(streamResp.Body)
		t.Fatalf("GET /v0/events/stream status = %d, want 200; body: %s", streamResp.StatusCode, string(body))
	}

	// Collect events on a background goroutine; surface them via a
	// channel so the test body can block until the expected one
	// arrives (or a timeout fires).
	eventLines := make(chan string, 128)
	go readSSEFrames(streamResp.Body, eventLines)

	// 3. Wait for city.ready (or city.init_failed) on the SSE stream
	// whose envelope Subject == createResp.Name. This is the async
	// completion contract the MC live harness relies on.
	deadline := time.After(120 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for city.ready for %q; collected %d lines so far", createResp.Name, len(eventLines))
		case line, ok := <-eventLines:
			if !ok {
				t.Fatalf("SSE stream closed before city.ready for %q arrived", createResp.Name)
			}
			// SSE "data:" lines carry JSON envelopes. Ignore
			// heartbeats, comments, framing lines.
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			payload := strings.TrimPrefix(line, "data: ")
			var env struct {
				Type    string `json:"type"`
				Subject string `json:"subject"`
			}
			if err := json.Unmarshal([]byte(payload), &env); err != nil {
				continue
			}
			if env.Subject != createResp.Name {
				continue
			}
			switch env.Type {
			case "city.ready":
				t.Logf("received city.ready for %q — async contract satisfied", createResp.Name)
				return
			case "city.init_failed":
				t.Fatalf("received city.init_failed for %q: %s", createResp.Name, payload)
			}
		}
	}
}

// TestHumaBinary_CityUnregisterAsync exercises the async
// POST /v0/city/{cityName}/unregister contract end-to-end against a
// live supervisor. Creates a city, waits for city.ready, then POSTs
// unregister and asserts city.unregistered arrives on the same SSE
// stream. Symmetric with TestHumaBinary_CityCreateAsync.
//
// Build-tagged `integration`; run with:
//
//	go test -tags=integration ./test/integration/ -run TestHumaBinary_CityUnregisterAsync
func TestHumaBinary_CityUnregisterAsync(t *testing.T) {
	bin := buildGCBinary(t)

	root := shortTempDir(t)
	gcHome := filepath.Join(root, "home")
	runtimeDir := filepath.Join(root, "run")
	for _, dir := range []string{gcHome, runtimeDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	port := reserveFreePort(t)
	writeSupervisorConfig(t, gcHome, port)
	if err := seedDoltIdentityForRoot(gcHome); err != nil {
		t.Fatalf("seed dolt identity: %v", err)
	}

	baseURL := "http://127.0.0.1:" + strconv.Itoa(port)
	env := integrationEnvFor(gcHome, runtimeDir, true)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, bin, "supervisor", "run")
	cmd.Env = env
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start supervisor: %v", err)
	}
	var supervisorLog strings.Builder
	go func() { _, _ = io.Copy(&supervisorLog, stderr) }()
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("supervisor stderr:\n%s", supervisorLog.String())
		}
	})

	waitHTTP(t, baseURL+"/health", 10*time.Second)

	// 1. Create a city.
	cityDir := filepath.Join(gcHome, "unregister-test-city")
	body := `{"dir":"` + cityDir + `","provider":"claude"}`
	postReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v0/city", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build post request: %v", err)
	}
	postReq.Header.Set("Content-Type", "application/json")
	postReq.Header.Set("X-GC-Request", "true")
	postResp, err := http.DefaultClient.Do(postReq)
	if err != nil {
		t.Fatalf("POST /v0/city: %v", err)
	}
	postBody, _ := io.ReadAll(postResp.Body)
	_ = postResp.Body.Close()
	if postResp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /v0/city status = %d, want 202; body: %s", postResp.StatusCode, string(postBody))
	}
	var createResp struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal(postBody, &createResp); err != nil {
		t.Fatalf("decode create response: %v; body: %s", err, string(postBody))
	}

	// 2. Subscribe to /v0/events/stream and wait for city.ready so
	// we know the reconciler has fully adopted the city (the
	// unregister reconcile path we're testing operates on the
	// running set).
	streamCtx, streamCancel := context.WithTimeout(context.Background(), 180*time.Second)
	t.Cleanup(streamCancel)
	streamReq, err := http.NewRequestWithContext(streamCtx, http.MethodGet, baseURL+"/v0/events/stream?after_cursor=0", nil)
	if err != nil {
		t.Fatalf("build stream request: %v", err)
	}
	streamReq.Header.Set("Accept", "text/event-stream")
	streamResp, err := http.DefaultClient.Do(streamReq)
	if err != nil {
		t.Fatalf("GET /v0/events/stream: %v", err)
	}
	defer streamResp.Body.Close() //nolint:errcheck
	if streamResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(streamResp.Body)
		t.Fatalf("GET /v0/events/stream status = %d, want 200; body: %s", streamResp.StatusCode, string(b))
	}

	eventLines := make(chan string, 256)
	go readSSEFrames(streamResp.Body, eventLines)

	readyDeadline := time.After(120 * time.Second)
ready:
	for {
		select {
		case <-readyDeadline:
			t.Fatalf("timed out waiting for city.ready for %q", createResp.Name)
		case line, ok := <-eventLines:
			if !ok {
				t.Fatalf("SSE stream closed before city.ready for %q arrived", createResp.Name)
			}
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var env struct {
				Type    string `json:"type"`
				Subject string `json:"subject"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &env); err != nil {
				continue
			}
			if env.Subject == createResp.Name && env.Type == "city.ready" {
				break ready
			}
		}
	}
	t.Logf("city %q ready; issuing unregister", createResp.Name)

	// 3. POST /v0/city/{cityName}/unregister. Expect 202.
	unregURL := baseURL + "/v0/city/" + createResp.Name + "/unregister"
	unregReq, err := http.NewRequestWithContext(ctx, http.MethodPost, unregURL, nil)
	if err != nil {
		t.Fatalf("build unregister request: %v", err)
	}
	unregReq.Header.Set("X-GC-Request", "true")
	unregStart := time.Now()
	unregResp, err := http.DefaultClient.Do(unregReq)
	if err != nil {
		t.Fatalf("POST unregister: %v", err)
	}
	unregDur := time.Since(unregStart)
	unregBody, _ := io.ReadAll(unregResp.Body)
	_ = unregResp.Body.Close()
	if unregResp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST unregister status = %d, want 202; body: %s", unregResp.StatusCode, string(unregBody))
	}
	if unregDur > 20*time.Second {
		t.Errorf("POST unregister took %s, want fast response (<20s)", unregDur)
	}
	var unregBodyDecoded struct {
		OK   bool   `json:"ok"`
		Name string `json:"name"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal(unregBody, &unregBodyDecoded); err != nil {
		t.Fatalf("decode unregister response: %v; body: %s", err, string(unregBody))
	}
	// macOS resolves /tmp -> /private/tmp at some boundaries; strip
	// either prefix so the test survives wherever the canonicalization
	// kicked in.
	canonicalize := func(p string) string { return strings.TrimPrefix(p, "/private") }
	if !unregBodyDecoded.OK || unregBodyDecoded.Name != createResp.Name || canonicalize(unregBodyDecoded.Path) != canonicalize(createResp.Path) {
		t.Errorf("unregister response mismatch: got %+v, want ok=true name=%q path=%q", unregBodyDecoded, createResp.Name, createResp.Path)
	}
	t.Logf("POST unregister returned 202 in %s", unregDur.Round(time.Millisecond))

	// 4. Wait for city.unregistered (or city.unregister_failed) on
	// the SSE stream.
	unregDeadline := time.After(120 * time.Second)
	for {
		select {
		case <-unregDeadline:
			t.Fatalf("timed out waiting for city.unregistered for %q", createResp.Name)
		case line, ok := <-eventLines:
			if !ok {
				t.Fatalf("SSE stream closed before city.unregistered for %q arrived", createResp.Name)
			}
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var env struct {
				Type    string `json:"type"`
				Subject string `json:"subject"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &env); err != nil {
				continue
			}
			if env.Subject != createResp.Name {
				continue
			}
			switch env.Type {
			case "city.unregistered":
				t.Logf("received city.unregistered for %q — async unregister contract satisfied", createResp.Name)
				return
			case "city.unregister_failed":
				t.Fatalf("received city.unregister_failed for %q: %s", createResp.Name, strings.TrimPrefix(line, "data: "))
			}
		}
	}
}

// readSSEFrames scans a text/event-stream body line-by-line and ships
// each line to out. Returns when the underlying reader closes (EOF or
// connection drop). The channel is closed to signal "no more frames".
func readSSEFrames(body io.ReadCloser, out chan<- string) {
	defer close(out)
	buf := make([]byte, 0, 4096)
	chunk := make([]byte, 4096)
	for {
		n, err := body.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			for {
				i := strings.IndexByte(string(buf), '\n')
				if i < 0 {
					break
				}
				line := strings.TrimRight(string(buf[:i]), "\r")
				buf = buf[i+1:]
				out <- line
			}
		}
		if err != nil {
			return
		}
	}
}
