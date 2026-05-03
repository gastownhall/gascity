package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/supervisor"
)

func TestCmdReloadApplied(t *testing.T) {
	dir := shortSocketTempDir(t, "gc-reload-cli-")
	writeCityTOML(t, dir, "test", "mayor")

	oldSend := sendReloadControlRequestHook
	oldUnavailable := reloadUnavailableMessageHook
	t.Cleanup(func() {
		sendReloadControlRequestHook = oldSend
		reloadUnavailableMessageHook = oldUnavailable
	})

	sendReloadControlRequestHook = func(cityPath string, req reloadControlRequest) (reloadControlReply, error) {
		if !samePath(cityPath, dir) {
			t.Fatalf("cityPath = %q, want %q", cityPath, canonicalTestPath(dir))
		}
		if !req.Wait || req.Timeout != "30s" {
			t.Fatalf("req = %+v, want wait=true timeout=30s", req)
		}
		return reloadControlReply{
			Outcome:  reloadOutcomeApplied,
			Message:  "Config reloaded: 1 agents, 0 rigs (rev abc123def456)",
			Revision: "abc123def4567890",
			Warnings: []string{"service reload: boom"},
		}, nil
	}
	reloadUnavailableMessageHook = func(string) string { return "" }

	var stdout, stderr bytes.Buffer
	if code := cmdReload([]string{dir}, false, "30s", true, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdReload = %d; stderr=%s", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "Config reloaded: 1 agents, 0 rigs (rev abc123def456)" {
		t.Fatalf("stdout = %q", got)
	}
	if got := strings.TrimSpace(stderr.String()); got != "gc reload: warning: service reload: boom" {
		t.Fatalf("stderr = %q", got)
	}
}

func TestCmdReloadAsyncExplicitTimeoutInvalid(t *testing.T) {
	dir := shortSocketTempDir(t, "gc-reload-flags-")
	writeCityTOML(t, dir, "test", "mayor")

	var stdout, stderr bytes.Buffer
	if code := cmdReload([]string{dir}, true, "30s", true, &stdout, &stderr); code != 1 {
		t.Fatalf("cmdReload = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "--async and --timeout cannot be used together") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestCmdReloadControllerUnavailableUsesRicherMessage(t *testing.T) {
	dir := shortSocketTempDir(t, "gc-reload-unavail-")
	writeCityTOML(t, dir, "test", "mayor")

	oldSend := sendReloadControlRequestHook
	oldUnavailable := reloadUnavailableMessageHook
	t.Cleanup(func() {
		sendReloadControlRequestHook = oldSend
		reloadUnavailableMessageHook = oldUnavailable
	})

	sendReloadControlRequestHook = func(string, reloadControlRequest) (reloadControlReply, error) {
		return reloadControlReply{}, controllerCommandError{
			op:           "connecting to controller",
			err:          errors.New("dial failed"),
			unavailable:  true,
			unresponsive: false,
		}
	}
	reloadUnavailableMessageHook = func(string) string {
		return "city failed to start under supervisor: fetching packs: auth denied"
	}

	var stdout, stderr bytes.Buffer
	if code := cmdReload([]string{dir}, false, "5s", true, &stdout, &stderr); code != 1 {
		t.Fatalf("cmdReload = %d, want 1", code)
	}
	if got := strings.TrimSpace(stderr.String()); got != "gc reload: city failed to start under supervisor: fetching packs: auth denied: connecting to controller: dial failed" {
		t.Fatalf("stderr = %q", got)
	}
}

func TestCmdReloadControllerUnresponsiveUsesRicherMessage(t *testing.T) {
	dir := shortSocketTempDir(t, "gc-reload-unresponsive-")
	writeCityTOML(t, dir, "test", "mayor")

	oldSend := sendReloadControlRequestHook
	oldUnavailable := reloadUnavailableMessageHook
	t.Cleanup(func() {
		sendReloadControlRequestHook = oldSend
		reloadUnavailableMessageHook = oldUnavailable
	})

	sendReloadControlRequestHook = func(string, reloadControlRequest) (reloadControlReply, error) {
		return reloadControlReply{}, controllerCommandError{
			op:           "reading response",
			err:          errors.New("i/o timeout"),
			unresponsive: true,
		}
	}
	reloadUnavailableMessageHook = func(string) string {
		return "controller is running but not responding"
	}

	var stdout, stderr bytes.Buffer
	if code := cmdReload([]string{dir}, false, "5s", true, &stdout, &stderr); code != 1 {
		t.Fatalf("cmdReload = %d, want 1", code)
	}
	if got := strings.TrimSpace(stderr.String()); got != "gc reload: controller is running but not responding: reading response: i/o timeout" {
		t.Fatalf("stderr = %q", got)
	}
}

func TestCmdReloadPreservesProtocolErrors(t *testing.T) {
	dir := shortSocketTempDir(t, "gc-reload-protocol-")
	writeCityTOML(t, dir, "test", "mayor")

	oldSend := sendReloadControlRequestHook
	oldUnavailable := reloadUnavailableMessageHook
	t.Cleanup(func() {
		sendReloadControlRequestHook = oldSend
		reloadUnavailableMessageHook = oldUnavailable
	})

	sendReloadControlRequestHook = func(string, reloadControlRequest) (reloadControlReply, error) {
		return reloadControlReply{}, errors.New("parsing response: invalid character 'o' in literal null")
	}
	reloadUnavailableMessageHook = func(string) string {
		return "city is still starting under supervisor"
	}

	var stdout, stderr bytes.Buffer
	if code := cmdReload([]string{dir}, false, "5s", true, &stdout, &stderr); code != 1 {
		t.Fatalf("cmdReload = %d, want 1", code)
	}
	if got := strings.TrimSpace(stderr.String()); got != "gc reload: parsing response: invalid character 'o' in literal null" {
		t.Fatalf("stderr = %q", got)
	}
}

func TestCmdReloadFailedReplyPrintsWarnings(t *testing.T) {
	dir := shortSocketTempDir(t, "gc-reload-failed-warnings-")
	writeCityTOML(t, dir, "test", "mayor")

	oldSend := sendReloadControlRequestHook
	oldUnavailable := reloadUnavailableMessageHook
	t.Cleanup(func() {
		sendReloadControlRequestHook = oldSend
		reloadUnavailableMessageHook = oldUnavailable
	})

	sendReloadControlRequestHook = func(string, reloadControlRequest) (reloadControlReply, error) {
		return reloadControlReply{
			Outcome: reloadOutcomeFailed,
			Warnings: []string{
				`workspace.install_agent_hooks redefined by "override.toml"`,
			},
			Error: "strict mode: 1 collision warning(s)",
		}, nil
	}
	reloadUnavailableMessageHook = func(string) string { return "" }

	var stdout, stderr bytes.Buffer
	if code := cmdReload([]string{dir}, false, "5s", true, &stdout, &stderr); code != 1 {
		t.Fatalf("cmdReload = %d, want 1", code)
	}
	got := stderr.String()
	if !strings.Contains(got, `gc reload: warning: workspace.install_agent_hooks redefined by "override.toml"`) {
		t.Fatalf("stderr = %q, want warning detail", got)
	}
	if !strings.Contains(got, "strict mode: 1 collision warning(s)") {
		t.Fatalf("stderr = %q, want strict error", got)
	}
}

func TestHandleReloadSocketCmdAsyncAccepted(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close() //nolint:errcheck

	reloadReqCh := make(chan reloadRequest)
	done := make(chan struct{})
	go func() {
		handleReloadSocketCmd(server, `{"wait":false}`, reloadReqCh)
		close(done)
	}()

	req := <-reloadReqCh
	if req.wait {
		t.Fatal("req.wait = true, want false")
	}
	req.acceptedCh <- reloadControlReply{
		Outcome: reloadOutcomeAccepted,
		Message: "Reload requested.",
	}

	reply := readReloadSocketReply(t, client)
	if reply.Outcome != reloadOutcomeAccepted {
		t.Fatalf("reply.Outcome = %q, want %q", reply.Outcome, reloadOutcomeAccepted)
	}
	if reply.Message != "Reload requested." {
		t.Fatalf("reply.Message = %q", reply.Message)
	}

	client.Close() //nolint:errcheck
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reload socket handler did not exit")
	}
}

func TestHandleReloadSocketCmdAsyncIgnoresInvalidTimeout(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close() //nolint:errcheck

	reloadReqCh := make(chan reloadRequest)
	done := make(chan struct{})
	go func() {
		handleReloadSocketCmd(server, `{"wait":false,"timeout":"bad"}`, reloadReqCh)
		close(done)
	}()

	req := <-reloadReqCh
	if req.wait {
		t.Fatal("req.wait = true, want false")
	}
	if req.timeout != 0 {
		t.Fatalf("req.timeout = %s, want 0 for async request", req.timeout)
	}
	req.acceptedCh <- reloadControlReply{
		Outcome: reloadOutcomeAccepted,
		Message: "Reload requested.",
	}

	reply := readReloadSocketReply(t, client)
	if reply.Outcome != reloadOutcomeAccepted {
		t.Fatalf("reply.Outcome = %q, want %q", reply.Outcome, reloadOutcomeAccepted)
	}

	client.Close() //nolint:errcheck
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reload socket handler did not exit")
	}
}

func TestHandleReloadSocketCmdSyncTimeout(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close() //nolint:errcheck

	reloadReqCh := make(chan reloadRequest)
	done := make(chan struct{})
	go func() {
		handleReloadSocketCmd(server, `{"wait":true,"timeout":"20ms"}`, reloadReqCh)
		close(done)
	}()

	req := <-reloadReqCh
	req.acceptedCh <- reloadControlReply{
		Outcome: reloadOutcomeAccepted,
		Message: "Reload requested.",
	}

	reply := readReloadSocketReply(t, client)
	if reply.Outcome != reloadOutcomeTimeout {
		t.Fatalf("reply.Outcome = %q, want %q", reply.Outcome, reloadOutcomeTimeout)
	}
	if !strings.Contains(reply.Message, "may still complete later") {
		t.Fatalf("reply.Message = %q", reply.Message)
	}

	client.Close() //nolint:errcheck
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reload socket handler did not exit")
	}
}

func TestHandleReloadSocketCmdBusyOnAcceptTimeout(t *testing.T) {
	oldAccept := controllerReloadAcceptTimeout
	controllerReloadAcceptTimeout = 20 * time.Millisecond
	t.Cleanup(func() { controllerReloadAcceptTimeout = oldAccept })

	server, client := net.Pipe()
	defer client.Close() //nolint:errcheck

	reloadReqCh := make(chan reloadRequest)

	done := make(chan struct{})
	go func() {
		handleReloadSocketCmd(server, `{"wait":false}`, reloadReqCh)
		close(done)
	}()

	reply := readReloadSocketReply(t, client)
	if reply.Outcome != reloadOutcomeBusy {
		t.Fatalf("reply.Outcome = %q, want %q", reply.Outcome, reloadOutcomeBusy)
	}

	client.Close() //nolint:errcheck
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reload socket handler did not exit")
	}
	select {
	case req := <-reloadReqCh:
		t.Fatalf("unexpected queued reload request after busy reply: %+v", req)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestHandleReloadSocketCmdWaitsForAcceptedAfterHandoff(t *testing.T) {
	oldAccept := controllerReloadAcceptTimeout
	controllerReloadAcceptTimeout = 200 * time.Millisecond
	t.Cleanup(func() { controllerReloadAcceptTimeout = oldAccept })

	server, client := net.Pipe()
	defer client.Close() //nolint:errcheck

	reloadReqCh := make(chan reloadRequest)
	done := make(chan struct{})
	go func() {
		handleReloadSocketCmd(server, `{"wait":false}`, reloadReqCh)
		close(done)
	}()

	time.Sleep(180 * time.Millisecond)
	req := <-reloadReqCh
	time.Sleep(50 * time.Millisecond)
	req.acceptedCh <- reloadControlReply{
		Outcome: reloadOutcomeAccepted,
		Message: "Reload requested.",
	}

	reply := readReloadSocketReply(t, client)
	if reply.Outcome != reloadOutcomeAccepted {
		t.Fatalf("reply.Outcome = %q, want %q", reply.Outcome, reloadOutcomeAccepted)
	}

	client.Close() //nolint:errcheck
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reload socket handler did not exit")
	}
}

func TestControllerReloadAcceptTimeoutDefault(t *testing.T) {
	if controllerReloadAcceptTimeout != 60*time.Second {
		t.Fatalf("controllerReloadAcceptTimeout = %s, want 60s", controllerReloadAcceptTimeout)
	}
}

func TestReloadControlReadTimeoutAsyncOutlastsAcceptAndAckWindow(t *testing.T) {
	readTimeout, err := reloadControlReadTimeout(reloadControlRequest{Wait: false})
	if err != nil {
		t.Fatal(err)
	}
	if readTimeout <= 15*time.Second {
		t.Fatalf("async read timeout = %s, want above old 15s client deadline", readTimeout)
	}
	if wantMin := 2*controllerReloadAcceptTimeout + 5*time.Second; readTimeout <= wantMin {
		t.Fatalf("async read timeout = %s, want above controller window %s", readTimeout, wantMin)
	}
}

func TestReloadControlReadTimeoutWaitIncludesRequestedTimeout(t *testing.T) {
	oldAccept := controllerReloadAcceptTimeout
	controllerReloadAcceptTimeout = 20 * time.Millisecond
	t.Cleanup(func() { controllerReloadAcceptTimeout = oldAccept })

	readTimeout, err := reloadControlReadTimeout(reloadControlRequest{Wait: true, Timeout: "40ms"})
	if err != nil {
		t.Fatal(err)
	}
	want := 2*controllerReloadAcceptTimeout + 40*time.Millisecond + 10*time.Second
	if readTimeout != want {
		t.Fatalf("read timeout = %s, want %s", readTimeout, want)
	}
}

func TestSendReloadControlRequestNoChange(t *testing.T) {
	sp := runtime.NewFake()

	var reconcileCount atomic.Int32
	buildFn := func(c *config.City, _ runtime.Provider, _ beads.Store) DesiredStateResult {
		reconcileCount.Add(1)
		ds := make(map[string]TemplateParams)
		for _, a := range c.Agents {
			if a.Implicit {
				continue
			}
			ds[a.Name] = TemplateParams{SessionName: a.Name, TemplateName: a.Name, Command: "echo hello"}
		}
		return DesiredStateResult{State: ds}
	}

	dir := shortSocketTempDir(t, "gc-reload-no-change-")
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	tomlPath := writeCityTOML(t, dir, "test", "mayor")
	cfg, prov, err := config.LoadWithIncludes(osFS{}, tomlPath)
	if err != nil {
		t.Fatal(err)
	}
	configRev := config.Revision(osFS{}, prov, cfg, dir)

	var stdout, stderr bytes.Buffer
	done := make(chan struct{})
	go func() {
		runController(dir, tomlPath, cfg, configRev, buildFn, nil, sp, nil, nil, nil, nil, events.Discard, nil, &stdout, &stderr)
		close(done)
	}()
	t.Cleanup(func() {
		tryStopController(dir, &bytes.Buffer{})
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})

	waitForController(t, dir)
	deadline := time.After(5 * time.Second)
	for reconcileCount.Load() < 1 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for initial reconcile")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}

	reply, err := sendReloadControlRequest(dir, reloadControlRequest{Wait: true, Timeout: "1s"})
	if err != nil {
		t.Fatalf("sendReloadControlRequest: %v", err)
	}
	if reply.Outcome != reloadOutcomeNoChange {
		t.Fatalf("reply.Outcome = %q, want %q", reply.Outcome, reloadOutcomeNoChange)
	}
	if reply.Message != "No config changes detected." {
		t.Fatalf("reply.Message = %q", reply.Message)
	}
	if len(reply.Warnings) != 0 {
		t.Fatalf("reply.Warnings = %v, want none", reply.Warnings)
	}
}

func TestSendReloadControlRequestInvalidConfig(t *testing.T) {
	sp := runtime.NewFake()

	var reconcileCount atomic.Int32
	buildFn := func(c *config.City, _ runtime.Provider, _ beads.Store) DesiredStateResult {
		reconcileCount.Add(1)
		ds := make(map[string]TemplateParams)
		for _, a := range c.Agents {
			if a.Implicit {
				continue
			}
			ds[a.Name] = TemplateParams{SessionName: a.Name, TemplateName: a.Name, Command: "echo hello"}
		}
		return DesiredStateResult{State: ds}
	}

	dir := shortSocketTempDir(t, "gc-reload-invalid-")
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	tomlPath := writeCityTOML(t, dir, "test", "mayor")
	cfg, prov, err := config.LoadWithIncludes(osFS{}, tomlPath)
	if err != nil {
		t.Fatal(err)
	}
	configRev := config.Revision(osFS{}, prov, cfg, dir)

	var stdout, stderr bytes.Buffer
	done := make(chan struct{})
	go func() {
		runController(dir, tomlPath, cfg, configRev, buildFn, nil, sp, nil, nil, nil, nil, events.Discard, nil, &stdout, &stderr)
		close(done)
	}()
	t.Cleanup(func() {
		tryStopController(dir, &bytes.Buffer{})
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})

	waitForController(t, dir)
	deadline := time.After(5 * time.Second)
	for reconcileCount.Load() < 1 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for initial reconcile")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}

	if err := os.WriteFile(tomlPath, []byte("[[[ bad toml"), 0o644); err != nil {
		t.Fatal(err)
	}

	reply, err := sendReloadControlRequest(dir, reloadControlRequest{Wait: true, Timeout: "1s"})
	if err != nil {
		t.Fatalf("sendReloadControlRequest: %v", err)
	}
	if reply.Outcome != reloadOutcomeFailed {
		t.Fatalf("reply.Outcome = %q, want %q", reply.Outcome, reloadOutcomeFailed)
	}
	if !strings.Contains(reply.Error, "parsing city.toml") {
		t.Fatalf("reply.Error = %q", reply.Error)
	}
	if strings.Contains(stdout.String(), "Config reloaded:") {
		t.Fatalf("stdout unexpectedly contains reload success: %q", stdout.String())
	}
}

func readReloadSocketReply(t *testing.T, conn net.Conn) reloadControlReply {
	t.Helper()
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			t.Fatalf("read reply: %v", err)
		}
		t.Fatal("read reply: connection closed")
	}
	var reply reloadControlReply
	if err := json.Unmarshal(scanner.Bytes(), &reply); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	return reply
}

func TestSupervisorCityInfoMatchesNormalizedPath(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())

	realDir := shortSocketTempDir(t, "gc-reload-supervisor-real-")
	linkDir := filepath.Join(t.TempDir(), "city-link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Dir(supervisor.RegistryPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(realDir, "test"); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/cities" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		payload := map[string]any{
			"items": []api.CityInfo{{
				Name:   "test",
				Path:   linkDir,
				Status: "starting_agents",
			}},
		}
		if err := json.NewEncoder(w).Encode(payload); err != nil {
			t.Fatalf("encode cities: %v", err)
		}
	}))
	defer server.Close()

	oldAlive := supervisorAliveHook
	oldBaseURL := supervisorAPIBaseURLHook
	t.Cleanup(func() {
		supervisorAliveHook = oldAlive
		supervisorAPIBaseURLHook = oldBaseURL
	})
	supervisorAliveHook = func() int { return 4242 }
	supervisorAPIBaseURLHook = func() (string, error) { return server.URL, nil }

	info, ok := supervisorCityInfo(realDir)
	if !ok {
		t.Fatal("supervisorCityInfo returned ok=false")
	}
	if info.Path != linkDir {
		t.Fatalf("info.Path = %q, want %q", info.Path, linkDir)
	}
}
