package dashboardbff

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParsePingCheckNormalizesHealthyAndFailure(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		res  execResult
		want string
	}{
		{name: "healthy", res: execResult{stdout: `{"status":"ok"}`}, want: "ok"},
		{name: "provider failure", res: execResult{exitCode: 1, stdout: `{"status":"error","error":"proxy unavailable"}`}, want: "error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checks, ok := parsePingCheck(&tt.res)
			if !ok || len(checks) != 1 || checks[0].Status != tt.want {
				t.Fatalf("parsePingCheck() = (%v, %v), want one %s check", checks, ok, tt.want)
			}
			if checks[0].Category != "Beads" || checks[0].Name != pingConnectivityCheck {
				t.Fatalf("check identity = %+v", checks[0])
			}
		})
	}
}

func TestParsePingCheckRejectsMalformedOutput(t *testing.T) {
	t.Parallel()
	for _, stdout := range []string{"", "not json", `{"status":""}`, `[]`} {
		if checks, ok := parsePingCheck(&execResult{stdout: stdout}); ok || checks != nil {
			t.Errorf("parsePingCheck(%q) = (%v, %v), want nil,false", stdout, checks, ok)
		}
	}
}

func TestExecBdPingUsesProviderNeutralArgsAndIsolatesSocket(t *testing.T) {
	root := t.TempDir()
	beadsPath := filepath.Join(root, ".beads")
	if err := os.Mkdir(beadsPath, 0o755); err != nil {
		t.Fatal(err)
	}
	argsFile := filepath.Join(root, "args")
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + argsFile + "\"\nprintf 'socket=%s\\n' \"${BEADS_DOLT_SERVER_SOCKET-unset}\" >> \"" + argsFile + "\"\nprintf '%s' '{\"status\":\"ok\"}'\n"
	if err := os.WriteFile(filepath.Join(bin, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ADMIN_PATH", bin)
	// Ambient transport must never leak into dashboard probes. The target
	// store's metadata (not the caller environment) owns transport selection.
	t.Setenv("BEADS_DOLT_SERVER_SOCKET", "/tmp/ambient-dolt.sock")
	// exec.Command resolves the executable using the parent PATH before the
	// runner applies its scrubbed child environment.
	t.Setenv("PATH", bin)
	before, err := os.ReadDir(beadsPath)
	if err != nil {
		t.Fatal(err)
	}
	res, err := newExecRunner().execBdPing(context.Background(), beadsPath)
	if err != nil {
		t.Fatalf("execBdPing() error = %v", err)
	}
	if res.exitCode != 0 {
		t.Fatalf("execBdPing() exit = %d stdout=%q stderr=%q", res.exitCode, res.stdout, res.stderr)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(args)), "\n")
	if len(lines) != 5 || !equalStrings(lines[:4], []string{"ping", "--db", beadsPath, "--json"}) || lines[4] != "socket=unset" {
		t.Fatalf("bd argv/environment = %v", lines)
	}
	after, err := os.ReadDir(beadsPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatalf("probe mutated .beads directory: before=%d after=%d", len(before), len(after))
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(res.stdout), &payload); err != nil || payload["status"] != "ok" {
		t.Fatalf("ping output = %q", res.stdout)
	}
}

func TestExecBdPingHonorsCancellation(t *testing.T) {
	root := t.TempDir()
	beadsPath := filepath.Join(root, ".beads")
	if err := os.Mkdir(beadsPath, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newExecRunner().execBdPing(ctx, beadsPath); err == nil {
		t.Fatal("execBdPing() with canceled context returned nil error")
	}
}

func TestProbeRigReportsPingFailureAsDown(t *testing.T) {
	root := t.TempDir()
	rig := filepath.Join(root, "rig")
	beadsPath := filepath.Join(rig, ".beads")
	if err := os.MkdirAll(beadsPath, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "bd"), []byte("#!/bin/sh\nprintf '%s' '{\"status\":\"error\",\"error\":\"proxy unavailable\"}'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("ADMIN_PATH", bin)
	rep := newSamplerManager(Deps{}, newExecRunner()).probeRig(context.Background(), "r1", rig)
	if rep.Rollup != "down" || !rep.Reachable || len(rep.Problems) != 1 || rep.Problems[0].Status != "error" {
		t.Fatalf("probeRig() = %+v, want reachable/down with one error", rep)
	}
	if !strings.Contains(rep.Problems[0].Message, "proxy unavailable") {
		t.Fatalf("probeRig problem = %+v, want provider error", rep.Problems[0])
	}
	if rep.DoltConnected == nil || *rep.DoltConnected {
		t.Fatalf("probeRig DoltConnected = %v, want non-nil false", rep.DoltConnected)
	}
}

// TestProbeRigReportsPingStderrFailureAsDown covers the failure shape ping
// actually produces when the store cannot be opened at all: plain text on
// stderr, empty stdout, non-zero exit. Nothing is parseable, so the probe must
// synthesize the connectivity check itself rather than reporting an empty
// "warn" and discarding the provider's error.
func TestProbeRigReportsPingStderrFailureAsDown(t *testing.T) {
	rig, bin := newProbeRigFixture(t)
	writeFakeBd(t, bin, "#!/bin/sh\nprintf '%s' 'failed to open store: connection refused' >&2\nexit 1\n")

	rep := newSamplerManager(Deps{}, newExecRunner()).probeRig(context.Background(), "r1", rig)
	if rep.Rollup != "down" || !rep.Reachable || len(rep.Problems) != 1 {
		t.Fatalf("probeRig() = %+v, want reachable/down with one problem", rep)
	}
	if rep.Problems[0].Status != "error" || rep.Problems[0].Name != pingConnectivityCheck {
		t.Fatalf("probeRig problem = %+v, want an error connectivity check", rep.Problems[0])
	}
	if !strings.Contains(rep.Problems[0].Message, "connection refused") {
		t.Fatalf("probeRig problem message = %q, want the stderr provider error", rep.Problems[0].Message)
	}
	if rep.DoltConnected == nil || *rep.DoltConnected {
		t.Fatalf("probeRig DoltConnected = %v, want non-nil false", rep.DoltConnected)
	}
}

// TestProbeRigReportsDoltConnectedFromPing pins the proxied case this change
// exists to serve: a rig with no dolt-server.port file has no endpoint to
// dial, so connectivity must come from the ping check itself instead of
// reporting "unknown" for a store whose connectivity was just proven.
func TestProbeRigReportsDoltConnectedFromPing(t *testing.T) {
	rig, bin := newProbeRigFixture(t)
	writeFakeBd(t, bin, "#!/bin/sh\nprintf '%s' '{\"status\":\"ok\"}'\n")

	rep := newSamplerManager(Deps{}, newExecRunner()).probeRig(context.Background(), "r1", rig)
	if rep.DoltEndpoint != nil {
		t.Fatalf("probeRig DoltEndpoint = %v, want nil without a dolt-server.port file", *rep.DoltEndpoint)
	}
	if rep.DoltConnected == nil || !*rep.DoltConnected {
		t.Fatalf("probeRig DoltConnected = %v, want non-nil true", rep.DoltConnected)
	}
	if rep.Rollup != "ok" || len(rep.Problems) != 0 || rep.Note != "" {
		t.Fatalf("probeRig() = %+v, want a clean ok report", rep)
	}
}

func TestProbeRigProxiedModeIgnoresStalePortArtifact(t *testing.T) {
	rig, bin := newProbeRigFixture(t)
	if err := os.WriteFile(filepath.Join(rig, ".beads", "metadata.json"), []byte(`{"dolt_mode":"proxied-server"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rig, ".beads", "dolt-server.port"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeFakeBd(t, bin, "#!/bin/sh\nprintf '%s' '{\"status\":\"ok\"}'\n")

	rep := newSamplerManager(Deps{}, newExecRunner()).probeRig(context.Background(), "r1", rig)
	if rep.DoltEndpoint != nil {
		t.Fatalf("proxied stale endpoint = %v, want nil", *rep.DoltEndpoint)
	}
	if rep.DoltConnected == nil || !*rep.DoltConnected || rep.Rollup != "ok" {
		t.Fatalf("proxied stale-port report = %+v, want ping-backed healthy status", rep)
	}
}

func TestProbeRigConfigMarkerIgnoresStalePortArtifact(t *testing.T) {
	rig, bin := newProbeRigFixture(t)
	if err := os.WriteFile(filepath.Join(rig, ".beads", "config.yaml"), []byte("dolt.mode: proxied-server\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rig, ".beads", "dolt-server.port"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeFakeBd(t, bin, "#!/bin/sh\nprintf '%s' '{\"status\":\"ok\"}'\n")
	rep := newSamplerManager(Deps{}, newExecRunner()).probeRig(context.Background(), "r1", rig)
	if rep.DoltEndpoint != nil || rep.DoltConnected == nil || !*rep.DoltConnected || rep.Rollup != "ok" {
		t.Fatalf("config-marker report = %+v, want ping-backed healthy status without endpoint", rep)
	}
}

func TestProbeRigMalformedMetadataFailsClosedOnStalePort(t *testing.T) {
	rig, bin := newProbeRigFixture(t)
	if err := os.WriteFile(filepath.Join(rig, ".beads", "metadata.json"), []byte("{not-json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rig, ".beads", "dolt-server.port"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeFakeBd(t, bin, "#!/bin/sh\nprintf '%s' '{\"status\":\"ok\"}'\n")
	rep := newSamplerManager(Deps{}, newExecRunner()).probeRig(context.Background(), "r1", rig)
	if rep.DoltEndpoint != nil {
		t.Fatalf("malformed metadata endpoint = %v, want nil", *rep.DoltEndpoint)
	}
	if rep.DoltConnected == nil || !*rep.DoltConnected || rep.Rollup != "ok" {
		t.Fatalf("malformed metadata report = %+v, want ping-backed healthy status", rep)
	}
}

func TestProbeRigUnsafeModeMarkersIgnoreStalePortArtifact(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		contents string
	}{
		{name: "unknown metadata mode", filename: "metadata.json", contents: `{"dolt_mode":"mystery"}`},
		{name: "embedded metadata mode", filename: "metadata.json", contents: `{"dolt_mode":"embedded"}`},
		{name: "malformed config without mode", filename: "config.yaml", contents: "dolt: [\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rig, bin := newProbeRigFixture(t)
			if err := os.WriteFile(filepath.Join(rig, ".beads", tt.filename), []byte(tt.contents), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(rig, ".beads", "dolt-server.port"), []byte("1"), 0o644); err != nil {
				t.Fatal(err)
			}
			writeFakeBd(t, bin, "#!/bin/sh\nprintf '%s' '{\"status\":\"ok\"}'\n")

			rep := newSamplerManager(Deps{}, newExecRunner()).probeRig(context.Background(), "r1", rig)
			if rep.DoltEndpoint != nil {
				t.Fatalf("unsafe marker endpoint = %v, want nil", *rep.DoltEndpoint)
			}
			if rep.DoltConnected == nil || !*rep.DoltConnected || rep.Rollup != "ok" {
				t.Fatalf("unsafe marker report = %+v, want ping-backed healthy status without endpoint", rep)
			}
		})
	}
}

// newProbeRigFixture builds a rig directory with an empty .beads store and a
// PATH containing only a fake bd, so a probeRig test never reaches a real one.
func newProbeRigFixture(t *testing.T) (rigPath, binDir string) {
	t.Helper()
	root := t.TempDir()
	rigPath = filepath.Join(root, "rig")
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir = filepath.Join(root, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	t.Setenv("ADMIN_PATH", binDir)
	return rigPath, binDir
}

// writeFakeBd installs script as the bd on the fixture PATH.
func writeFakeBd(t *testing.T, binDir, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// recordingRoundTripper is a fake in-process transport standing in for the
// supervisor's LoopbackTransport: it records the request path and returns a
// canned response without touching the network, so a test can prove the
// samplers dispatch loopback reads through Deps.SelfReadTransport.
type recordingRoundTripper struct {
	gotPath string
	status  int
	body    string
}

func (rt *recordingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.gotPath = req.URL.Path
	code := rt.status
	if code == 0 {
		code = http.StatusOK
	}
	return &http.Response{
		StatusCode: code,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(rt.body)),
		Request:    req,
	}, nil
}

// TestSamplersUseSelfReadTransport is the regression test for the read-auth
// finding at the sampler layer: fetchStatus must dispatch its loopback status
// read through Deps.SelfReadTransport (the supervisor's in-process transport),
// not the network. The base URL is deliberately unroutable, so a networked read
// would fail; the canned status body proves the transport was used.
func TestSamplersUseSelfReadTransport(t *testing.T) {
	rt := &recordingRoundTripper{status: http.StatusOK, body: `{"store_health":{"size_bytes":42}}`}
	m := newSamplerManager(Deps{SupervisorBaseURL: "http://supervisor.invalid", SelfReadTransport: rt}, newExecRunner())

	raw, err := m.fetchStatus(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("fetchStatus via self-read transport: %v", err)
	}
	if rt.gotPath != "/v0/city/alpha/status" {
		t.Fatalf("transport saw path %q, want /v0/city/alpha/status", rt.gotPath)
	}
	if !strings.Contains(string(raw), "size_bytes") {
		t.Fatalf("fetchStatus body = %q, want the transport's canned status", raw)
	}
}

// statusServer returns an httptest server that serves a fixed supervisor status
// body at /v0/city/{name}/status, so refresh()'s fetchStatus succeeds.
func statusServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

// TestRefreshReadersDoNotBlockOnProbe is the regression test for the HIGH
// finding: refresh() must not hold the per-city write lock across the blocking
// rig probe. beforeProbe blocks the probe pass mid-flight while a reader calls
// supervisorStatus(); if the write lock were held across probeRig, the reader's
// RLock would block until the probe is released and the deadline would elapse.
func TestRefreshReadersDoNotBlockOnProbe(t *testing.T) {
	srv := statusServer(t, `{"store_health":{"size_bytes":100},"rig_details":[{"name":"r1","path":"/dashboardbff-nonexistent-rig"}]}`)
	defer srv.Close()

	m := newSamplerManager(Deps{SupervisorBaseURL: srv.URL}, newExecRunner())
	cs := &citySampler{name: "alpha", mgr: m}

	probing := make(chan struct{}) // closed once the probe pass is in-flight
	release := make(chan struct{}) // test closes this to let the probe finish
	cs.beforeProbe = func() {
		close(probing)
		<-release
	}

	done := make(chan struct{})
	go func() {
		cs.refresh(context.Background())
		close(done)
	}()

	select {
	case <-probing:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh never reached the rig probe")
	}

	// The probe is mid-flight. A reader must still return promptly — proving no
	// write lock is held across probeRig.
	got := make(chan supervisorStatusReport, 1)
	go func() { got <- cs.supervisorStatus() }()
	select {
	case <-got:
		// reader returned while the probe is blocked: contract upheld.
	case <-time.After(time.Second):
		t.Fatal("supervisorStatus() blocked while a probe was in flight: write lock held across probeRig")
	}

	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh did not finish after probe released")
	}
}

// TestRefreshPublishesUnderLock confirms the happy path still publishes status,
// the dolt ring, and the rig report after one refresh.
func TestRefreshPublishesUnderLock(t *testing.T) {
	srv := statusServer(t, `{"store_health":{"size_bytes":4096},"rig_details":[{"name":"r1","path":"/dashboardbff-nonexistent-rig"}]}`)
	defer srv.Close()

	m := newSamplerManager(Deps{SupervisorBaseURL: srv.URL}, newExecRunner())
	cs := &citySampler{name: "alpha", mgr: m}
	cs.refresh(context.Background())

	if rep := cs.supervisorStatus(); !rep.Available {
		t.Errorf("supervisorStatus available = false, want true after a good fetch")
	}
	trend := cs.doltTrend()
	if !trend.Available || len(trend.Samples) != 1 || trend.Samples[0].Bytes != 4096 {
		t.Errorf("doltTrend = %+v, want one 4096-byte sample available", trend)
	}
	rig := cs.rigStoreHealth()
	if !rig.Available || len(rig.Rigs) != 1 {
		t.Errorf("rigStoreHealth = %+v, want one rig available", rig)
	}
	// The probed rig dir does not exist, so it rolls up down/unreachable.
	if rig.Rigs[0].Reachable {
		t.Errorf("rig reachable = true, want false for a missing .beads dir")
	}
}

// TestRefreshDegradesNotBlankOnFetchError verifies a failed status fetch retains
// the last-good snapshot (status flips to unavailable, dolt/rig data survives).
func TestRefreshDegradesNotBlankOnFetchError(t *testing.T) {
	srv := statusServer(t, `{"store_health":{"size_bytes":2048},"rig_details":[{"name":"r1","path":"/dashboardbff-nonexistent-rig"}]}`)
	m := newSamplerManager(Deps{SupervisorBaseURL: srv.URL}, newExecRunner())
	cs := &citySampler{name: "alpha", mgr: m}

	cs.refresh(context.Background()) // seed last-good
	srv.Close()                      // next fetch fails
	cs.refresh(context.Background())

	if rep := cs.supervisorStatus(); rep.Available {
		t.Errorf("supervisorStatus available = true, want false after fetch failure")
	} else if rep.Reason != "status_read_failed" {
		t.Errorf("reason = %q, want status_read_failed", rep.Reason)
	}
	// Last-good dolt + rig data must survive the failed fetch (degrade, not blank).
	if trend := cs.doltTrend(); len(trend.Samples) != 1 {
		t.Errorf("doltTrend samples = %d, want 1 retained after fetch failure", len(trend.Samples))
	}
	if rig := cs.rigStoreHealth(); len(rig.Rigs) != 1 {
		t.Errorf("rigStoreHealth rigs = %d, want 1 retained after fetch failure", len(rig.Rigs))
	}
}

// TestRefreshCadenceGates confirms the dolt ring only appends on its 10-min
// cadence: two back-to-back refreshes append once (the second is inside the
// window), while the rig probe (5-min cadence) likewise runs once.
func TestRefreshCadenceGates(t *testing.T) {
	srv := statusServer(t, `{"store_health":{"size_bytes":100},"rig_details":[]}`)
	defer srv.Close()

	m := newSamplerManager(Deps{SupervisorBaseURL: srv.URL}, newExecRunner())
	cs := &citySampler{name: "alpha", mgr: m}

	cs.refresh(context.Background())
	first := cs.doltTrend()
	cs.refresh(context.Background()) // within doltAppendInterval: no new sample
	second := cs.doltTrend()

	if len(first.Samples) != 1 || len(second.Samples) != 1 {
		t.Errorf("dolt ring grew inside the append window: first=%d second=%d", len(first.Samples), len(second.Samples))
	}
}

// TestEnsureDoesNotStoreCityPath documents that ensure no longer tracks the
// city path (the dead cs.path reassignment was removed); the sampler keys off
// cs.name and rig paths come from the status body.
func TestEnsureDoesNotStoreCityPath(t *testing.T) {
	m := newSamplerManager(Deps{}, newExecRunner())
	cs := m.ensure("alpha")
	if cs.name != "alpha" {
		t.Errorf("ensure name = %q, want alpha", cs.name)
	}
	// Calling ensure again returns the same sampler instance.
	if again := m.ensure("alpha"); again != cs {
		t.Error("ensure should return the cached sampler for a known city")
	}
}
