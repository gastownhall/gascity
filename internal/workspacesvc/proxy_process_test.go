package workspacesvc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/supervisor"
)

const proxyProcessPythonHelper = `
import json
import os
import socketserver
import sys
from http.server import BaseHTTPRequestHandler

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/healthz":
            self.send_response(204)
            self.end_headers()
            return
        if self.path == "/env":
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps({
                "GC_SERVICE_PUBLIC_URL": os.environ.get("GC_SERVICE_PUBLIC_URL", ""),
            }).encode("utf-8"))
            return
        self.send_response(404)
        self.end_headers()

    def log_message(self, format, *args):
        return

sock = os.environ["GC_SERVICE_SOCKET"]
fail_once = os.environ.get("GC_SERVICE_FAIL_ONCE_FILE", "")
if fail_once and os.path.exists(fail_once):
    try:
        os.unlink(fail_once)
    except FileNotFoundError:
        pass
    sys.exit(1)
try:
    os.unlink(sock)
except FileNotFoundError:
    pass

class Server(socketserver.UnixStreamServer):
    allow_reuse_address = True

Server(sock, Handler).serve_forever()
`

func requirePython3(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not in PATH")
	}
}

// helperPassthroughForTests is the passthrough list proxy_process helper
// subprocesses need through internal/testenv's scrub: the city identity vars
// that proxy_process.go intentionally seeds into the helper env. Other GC_*
// leak vectors stay scrubbed. GC_SERVICE_* vars are not leak vectors so they
// flow through untouched without being listed here.
const helperPassthroughForTests = "GC_CITY,GC_CITY_PATH,GC_CITY_RUNTIME_DIR,GC_CONTROL_DISPATCHER_TRACE_DEFAULT"

// setHelperPassthrough installs extraHelperEnv so proxy_process.start()
// appends the passthrough var to the helper subprocess env. Tests run
// serially so the package-level var is safe to mutate.
func setHelperPassthrough(t *testing.T) {
	t.Helper()
	prev := extraHelperEnv
	extraHelperEnv = []string{"GC_TESTENV_PASSTHROUGH=" + helperPassthroughForTests}
	t.Cleanup(func() { extraHelperEnv = prev })
}

func TestManagerReloadProxyProcessStartsAndProxies(t *testing.T) {
	t.Setenv("GC_SERVICE_HELPER", "1")
	// The helper subprocess is the same test binary. proxy_process.go seeds
	// GC_CITY / GC_CITY_PATH / GC_CITY_RUNTIME_DIR /
	// GC_CONTROL_DISPATCHER_TRACE_DEFAULT into the child env; without a
	// passthrough declaration the child's internal/testenv init() would strip
	// them.
	setHelperPassthrough(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}

	rt := &testRuntime{
		cityPath: t.TempDir(),
		cityName: "test-city",
		cfg: &config.City{
			Services: []config.Service{{
				Name: "bridge",
				Kind: "proxy_process",
				Process: config.ServiceProcessConfig{
					Command:    []string{exe, "-test.run=^TestProxyProcessHelper$", "--"},
					HealthPath: "/healthz",
				},
			}},
		},
		sp:    runtime.NewFake(),
		store: beads.NewMemStore(),
	}
	mgr := NewManager(rt)
	if err := mgr.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	defer mgr.Close() //nolint:errcheck // best-effort cleanup

	status, ok := mgr.Get("bridge")
	if !ok {
		t.Fatal("service status missing")
	}
	if status.LocalState != "ready" {
		logData, _ := os.ReadFile(filepath.Join(rt.cityPath, ".gc", "services", "bridge", "logs", "service.log"))
		t.Fatalf("LocalState = %q, want ready (reason=%q, log=%q)", status.LocalState, status.Reason, string(logData))
	}

	req := httptest.NewRequest(http.MethodPost, "/svc/bridge/hooks/example", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	if ok := mgr.ServeHTTP(rec, req); !ok {
		t.Fatal("ServeHTTP returned false, want true")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if strings.TrimSpace(rec.Body.String()) != "POST /hooks/example" {
		t.Fatalf("body = %q, want %q", rec.Body.String(), "POST /hooks/example")
	}
}

func TestProxyProcessHelper(t *testing.T) {
	if os.Getenv("GC_SERVICE_HELPER") != "1" {
		t.Skip("helper process")
	}
	socketPath := os.Getenv("GC_SERVICE_SOCKET")
	if socketPath == "" {
		t.Fatal("GC_SERVICE_SOCKET not set")
	}
	_ = os.Remove(socketPath)

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	defer ln.Close() //nolint:errcheck // best-effort cleanup

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/env", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"GC_CITY":                             os.Getenv("GC_CITY"),
			"GC_CITY_PATH":                        os.Getenv("GC_CITY_PATH"),
			"GC_CITY_RUNTIME_DIR":                 os.Getenv("GC_CITY_RUNTIME_DIR"),
			"GC_CONTROL_DISPATCHER_TRACE_DEFAULT": os.Getenv("GC_CONTROL_DISPATCHER_TRACE_DEFAULT"),
			"GC_SERVICE_NAME":                     os.Getenv("GC_SERVICE_NAME"),
			"GC_SERVICE_STATE_ROOT":               os.Getenv("GC_SERVICE_STATE_ROOT"),
			"GC_SERVICE_URL_PREFIX":               os.Getenv("GC_SERVICE_URL_PREFIX"),
			"GC_SERVICE_PUBLIC_URL":               os.Getenv("GC_SERVICE_PUBLIC_URL"),
			"GC_SERVICE_VISIBILITY":               os.Getenv("GC_SERVICE_VISIBILITY"),
			"GC_PUBLISHED_SERVICES_DIR":           os.Getenv("GC_PUBLISHED_SERVICES_DIR"),
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s %s", r.Method, r.URL.Path) //nolint:errcheck // test helper
	})

	srv := &http.Server{Handler: mux}
	err = srv.Serve(ln)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("serve: %v", err)
	}
}

func TestProxyProcessPublishesServiceEnv(t *testing.T) {
	t.Setenv("GC_SERVICE_HELPER", "1")
	// The helper subprocess is the same test binary. proxy_process.go seeds
	// GC_CITY / GC_CITY_PATH / GC_CITY_RUNTIME_DIR /
	// GC_CONTROL_DISPATCHER_TRACE_DEFAULT into the child env; without a
	// passthrough declaration the child's internal/testenv init() would strip
	// them.
	setHelperPassthrough(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}

	rt := &testRuntime{
		cityPath: t.TempDir(),
		cityName: "test-city",
		cfg: &config.City{
			Workspace: config.Workspace{Name: "demo-app"},
			Services: []config.Service{{
				Name: "bridge",
				Kind: "proxy_process",
				Publication: config.ServicePublicationConfig{
					Visibility: "public",
				},
				Process: config.ServiceProcessConfig{
					Command:    []string{exe, "-test.run=^TestProxyProcessHelper$", "--"},
					HealthPath: "/healthz",
				},
			}},
		},
		pubCfg: supervisor.PublicationConfig{
			Provider:         "hosted",
			TenantSlug:       "acme",
			PublicBaseDomain: "apps.example.com",
		},
		sp:    runtime.NewFake(),
		store: beads.NewMemStore(),
	}
	writePublicationStoreForTest(t, rt.cityPath, `[
  {
    "service_name": "bridge",
    "visibility": "public",
    "url": "https://bridge--acme--deadbeef.apps.example.com"
  }
]`)

	mgr := NewManager(rt)
	if err := mgr.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	defer mgr.Close() //nolint:errcheck // best-effort cleanup

	req := httptest.NewRequest(http.MethodGet, "/svc/bridge/env", nil)
	rec := httptest.NewRecorder()
	if ok := mgr.ServeHTTP(rec, req); !ok {
		t.Fatal("ServeHTTP returned false, want true")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var env map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode env: %v", err)
	}
	if env["GC_CITY"] != rt.cityPath {
		t.Fatalf("GC_CITY = %q, want %q", env["GC_CITY"], rt.cityPath)
	}
	if env["GC_CITY_PATH"] != rt.cityPath {
		t.Fatalf("GC_CITY_PATH = %q, want %q", env["GC_CITY_PATH"], rt.cityPath)
	}
	if env["GC_CITY_RUNTIME_DIR"] != filepath.Join(rt.cityPath, ".gc", "runtime") {
		t.Fatalf("GC_CITY_RUNTIME_DIR = %q, want %q", env["GC_CITY_RUNTIME_DIR"], filepath.Join(rt.cityPath, ".gc", "runtime"))
	}
	if env["GC_CONTROL_DISPATCHER_TRACE_DEFAULT"] != filepath.Join(rt.cityPath, ".gc", "runtime", "control-dispatcher-trace.log") {
		t.Fatalf("GC_CONTROL_DISPATCHER_TRACE_DEFAULT = %q, want %q", env["GC_CONTROL_DISPATCHER_TRACE_DEFAULT"], filepath.Join(rt.cityPath, ".gc", "runtime", "control-dispatcher-trace.log"))
	}
	if env["GC_SERVICE_NAME"] != "bridge" {
		t.Fatalf("GC_SERVICE_NAME = %q, want bridge", env["GC_SERVICE_NAME"])
	}
	if env["GC_SERVICE_STATE_ROOT"] != filepath.Join(rt.cityPath, ".gc", "services", "bridge") {
		t.Fatalf("GC_SERVICE_STATE_ROOT = %q, want %q", env["GC_SERVICE_STATE_ROOT"], filepath.Join(rt.cityPath, ".gc", "services", "bridge"))
	}
	if env["GC_SERVICE_PUBLIC_URL"] != "https://bridge--acme--deadbeef.apps.example.com" {
		t.Fatalf("GC_SERVICE_PUBLIC_URL = %q, want authoritative route", env["GC_SERVICE_PUBLIC_URL"])
	}
	if env["GC_SERVICE_VISIBILITY"] != "public" {
		t.Fatalf("GC_SERVICE_VISIBILITY = %q, want public", env["GC_SERVICE_VISIBILITY"])
	}
	if env["GC_PUBLISHED_SERVICES_DIR"] != citylayout.PublishedServicesDir(rt.cityPath) {
		t.Fatalf("GC_PUBLISHED_SERVICES_DIR = %q, want %q", env["GC_PUBLISHED_SERVICES_DIR"], citylayout.PublishedServicesDir(rt.cityPath))
	}
	// Must be supervisor-routable; the per-city /svc/<name> form 404s on inbound.
	wantPrefix := citylayout.PublicServiceMountPath(rt.cityName, "bridge")
	if env["GC_SERVICE_URL_PREFIX"] != wantPrefix {
		t.Fatalf("GC_SERVICE_URL_PREFIX = %q, want %q", env["GC_SERVICE_URL_PREFIX"], wantPrefix)
	}
}

func TestProxyProcessReloadRefreshesPublicationEnv(t *testing.T) {
	t.Setenv("GC_SERVICE_HELPER", "1")
	// The helper subprocess is the same test binary. proxy_process.go seeds
	// GC_CITY / GC_CITY_PATH / GC_CITY_RUNTIME_DIR into the child env; without
	// a passthrough declaration the child's internal/testenv init() would
	// strip them.
	setHelperPassthrough(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}

	rt := &testRuntime{
		cityPath: t.TempDir(),
		cityName: "test-city",
		cfg: &config.City{
			Workspace: config.Workspace{Name: "demo-app"},
			Services: []config.Service{{
				Name: "bridge",
				Kind: "proxy_process",
				Publication: config.ServicePublicationConfig{
					Visibility: "public",
				},
				Process: config.ServiceProcessConfig{
					Command:    []string{exe, "-test.run=^TestProxyProcessHelper$", "--"},
					HealthPath: "/healthz",
				},
			}},
		},
		pubCfg: supervisor.PublicationConfig{
			Provider:         "hosted",
			TenantSlug:       "acme",
			PublicBaseDomain: "apps.example.com",
		},
		sp:    runtime.NewFake(),
		store: beads.NewMemStore(),
	}
	writePublicationStoreForTest(t, rt.cityPath, `[
  {
    "service_name": "bridge",
    "visibility": "public",
    "url": "https://bridge--acme--deadbeef.apps.example.com"
  }
]`)

	mgr := NewManager(rt)
	if err := mgr.Reload(); err != nil {
		t.Fatalf("first Reload: %v", err)
	}
	defer mgr.Close() //nolint:errcheck // best-effort cleanup

	loadEnv := func() map[string]string {
		deadline := time.Now().Add(2 * time.Second)
		for {
			req := httptest.NewRequest(http.MethodGet, "/svc/bridge/env", nil)
			rec := httptest.NewRecorder()
			if ok := mgr.ServeHTTP(rec, req); ok && rec.Code == http.StatusOK {
				var env map[string]string
				if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
					t.Fatalf("decode env: %v", err)
				}
				return env
			}
			if time.Now().After(deadline) {
				status, _ := mgr.Get("bridge")
				logData, _ := os.ReadFile(filepath.Join(rt.cityPath, ".gc", "services", "bridge", "logs", "service.log"))
				t.Fatalf("timed out waiting for proxy process env endpoint: status=%+v log=%q", status, string(logData))
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	first := loadEnv()
	writePublicationStoreForTest(t, rt.cityPath, `[
  {
    "service_name": "bridge",
    "visibility": "public",
    "url": "https://bridge--beta--feedface.apps.example.com"
  }
]`)
	if err := mgr.Reload(); err != nil {
		t.Fatalf("second Reload: %v", err)
	}
	second := loadEnv()

	if first["GC_SERVICE_PUBLIC_URL"] == second["GC_SERVICE_PUBLIC_URL"] {
		t.Fatalf("GC_SERVICE_PUBLIC_URL did not change across reload: %q", first["GC_SERVICE_PUBLIC_URL"])
	}
	if second["GC_SERVICE_PUBLIC_URL"] != "https://bridge--beta--feedface.apps.example.com" {
		t.Fatalf("GC_SERVICE_PUBLIC_URL = %q, want updated authoritative route", second["GC_SERVICE_PUBLIC_URL"])
	}
}

func TestProxyProcessTickRefreshesPublicationEnvFromAuthoritativeStore(t *testing.T) {
	requirePython3(t)
	cityPath := t.TempDir()

	rt := &testRuntime{
		cityPath: cityPath,
		cityName: "test-city",
		cfg: &config.City{
			Workspace: config.Workspace{Name: "demo-app"},
			Services: []config.Service{{
				Name: "bridge",
				Kind: "proxy_process",
				Publication: config.ServicePublicationConfig{
					Visibility: "public",
				},
				Process: config.ServiceProcessConfig{
					Command:    []string{"python3", "-c", proxyProcessPythonHelper},
					HealthPath: "/healthz",
				},
			}},
		},
		pubCfg: supervisor.PublicationConfig{
			Provider:         "hosted",
			TenantSlug:       "acme",
			PublicBaseDomain: "apps.example.com",
		},
		sp:    runtime.NewFake(),
		store: beads.NewMemStore(),
	}
	mgr := NewManager(rt)
	writePublicationStoreForTest(t, rt.cityPath, `[
  {
    "service_name": "bridge",
    "visibility": "public",
    "url": "https://bridge--acme--11111111.apps.example.com"
  }
]`)
	if err := mgr.Reload(); err != nil {
		t.Fatalf("first Reload: %v", err)
	}
	defer mgr.Close() //nolint:errcheck // best-effort cleanup

	loadEnv := func() map[string]string {
		deadline := time.Now().Add(2 * time.Second)
		for {
			req := httptest.NewRequest(http.MethodGet, "/svc/bridge/env", nil)
			rec := httptest.NewRecorder()
			if ok := mgr.ServeHTTP(rec, req); ok && rec.Code == http.StatusOK {
				var env map[string]string
				if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
					t.Fatalf("decode env: %v", err)
				}
				return env
			}
			if time.Now().After(deadline) {
				status, _ := mgr.Get("bridge")
				logData, _ := os.ReadFile(filepath.Join(rt.cityPath, ".gc", "services", "bridge", "logs", "service.log"))
				t.Fatalf("timed out waiting for proxy process env endpoint: status=%+v log=%q", status, string(logData))
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	first := loadEnv()
	writePublicationStoreForTest(t, rt.cityPath, `[
  {
    "service_name": "bridge",
    "visibility": "public",
    "url": "https://bridge--acme--deadbeef.apps.example.com"
  }
]`)
	refs := loadPublicationRefs(rt.PublicationStorePath(), rt.CityPath())
	if refs.err != nil {
		t.Fatalf("loadPublicationRefs: %v", refs.err)
	}
	if refs.refs["bridge"].URL != "https://bridge--acme--deadbeef.apps.example.com" {
		t.Fatalf("authoritative ref URL = %q, want stored route", refs.refs["bridge"].URL)
	}

	mgr.Tick(context.Background(), time.Now().UTC())
	second := loadEnv()

	if first["GC_SERVICE_PUBLIC_URL"] == second["GC_SERVICE_PUBLIC_URL"] {
		t.Fatalf("GC_SERVICE_PUBLIC_URL did not change across tick: %q", first["GC_SERVICE_PUBLIC_URL"])
	}
	if second["GC_SERVICE_PUBLIC_URL"] != "https://bridge--acme--deadbeef.apps.example.com" {
		t.Fatalf("GC_SERVICE_PUBLIC_URL = %q, want authoritative route", second["GC_SERVICE_PUBLIC_URL"])
	}
}

func TestProxyProcessTickRetriesPublicationRefreshWithoutLosingCurrentURL(t *testing.T) {
	requirePython3(t)
	rt := &testRuntime{
		cityPath: t.TempDir(),
		cityName: "test-city",
		cfg: &config.City{
			Workspace: config.Workspace{Name: "demo-app"},
			Services: []config.Service{{
				Name: "bridge",
				Kind: "proxy_process",
				Publication: config.ServicePublicationConfig{
					Visibility: "public",
				},
				Process: config.ServiceProcessConfig{
					Command:    []string{"python3", "-c", proxyProcessPythonHelper},
					HealthPath: "/healthz",
				},
			}},
		},
		pubCfg: supervisor.PublicationConfig{
			Provider:         "hosted",
			TenantSlug:       "acme",
			PublicBaseDomain: "apps.example.com",
		},
		sp:    runtime.NewFake(),
		store: beads.NewMemStore(),
	}
	writePublicationStoreForTest(t, rt.cityPath, `[
  {
    "service_name": "bridge",
    "visibility": "public",
    "url": "https://bridge--acme--11111111.apps.example.com"
  }
]`)

	mgr := NewManager(rt)
	if err := mgr.Reload(); err != nil {
		t.Fatalf("first Reload: %v", err)
	}
	defer mgr.Close() //nolint:errcheck // best-effort cleanup

	loadEnv := func() map[string]string {
		deadline := time.Now().Add(2 * time.Second)
		for {
			req := httptest.NewRequest(http.MethodGet, "/svc/bridge/env", nil)
			rec := httptest.NewRecorder()
			if ok := mgr.ServeHTTP(rec, req); ok && rec.Code == http.StatusOK {
				var env map[string]string
				if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
					t.Fatalf("decode env: %v", err)
				}
				return env
			}
			if time.Now().After(deadline) {
				status, _ := mgr.Get("bridge")
				logData, _ := os.ReadFile(filepath.Join(rt.cityPath, ".gc", "services", "bridge", "logs", "service.log"))
				t.Fatalf("timed out waiting for proxy process env endpoint: status=%+v log=%q", status, string(logData))
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	first := loadEnv()
	failOnce := filepath.Join(rt.cityPath, ".gc", "services", "bridge", "fail-once")
	t.Setenv("GC_SERVICE_FAIL_ONCE_FILE", failOnce)
	if err := os.MkdirAll(filepath.Dir(failOnce), 0o750); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(failOnce), err)
	}
	if err := os.WriteFile(failOnce, []byte("1"), 0o640); err != nil {
		t.Fatalf("WriteFile(%q): %v", failOnce, err)
	}
	writePublicationStoreForTest(t, rt.cityPath, `[
  {
    "service_name": "bridge",
    "visibility": "public",
    "url": "https://bridge--acme--deadbeef.apps.example.com"
  }
]`)

	mgr.Tick(context.Background(), time.Now().UTC())
	status, ok := mgr.Get("bridge")
	if !ok {
		t.Fatal("service status missing after failed tick")
	}
	if status.URL != first["GC_SERVICE_PUBLIC_URL"] {
		t.Fatalf("URL = %q, want current URL %q after failed restart", status.URL, first["GC_SERVICE_PUBLIC_URL"])
	}
	if status.PublicationState != "published" {
		t.Fatalf("PublicationState = %q, want published after failed restart", status.PublicationState)
	}

	mgr.Tick(context.Background(), time.Now().UTC())
	second := loadEnv()
	if second["GC_SERVICE_PUBLIC_URL"] == first["GC_SERVICE_PUBLIC_URL"] {
		t.Fatalf("GC_SERVICE_PUBLIC_URL did not change after retry: %q", second["GC_SERVICE_PUBLIC_URL"])
	}
	if second["GC_SERVICE_PUBLIC_URL"] != "https://bridge--acme--deadbeef.apps.example.com" {
		t.Fatalf("GC_SERVICE_PUBLIC_URL = %q, want authoritative route", second["GC_SERVICE_PUBLIC_URL"])
	}

	status, ok = mgr.Get("bridge")
	if !ok {
		t.Fatal("service status missing after retry")
	}
	if status.URL != second["GC_SERVICE_PUBLIC_URL"] {
		t.Fatalf("status URL = %q, want %q after retry", status.URL, second["GC_SERVICE_PUBLIC_URL"])
	}
}

func TestNewProxyProcessInstanceCleansUpSocketDirOnStartFailure(t *testing.T) {
	requirePython3(t)
	cityPath := t.TempDir()
	failOnce := filepath.Join(cityPath, ".gc", "services", "bridge", "fail-once")
	if err := os.MkdirAll(filepath.Dir(failOnce), 0o750); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(failOnce), err)
	}
	if err := os.WriteFile(failOnce, []byte("1"), 0o640); err != nil {
		t.Fatalf("WriteFile(%q): %v", failOnce, err)
	}
	t.Setenv("GC_SERVICE_FAIL_ONCE_FILE", failOnce)

	sum := sha256.Sum256([]byte(cityPath))
	socketDir := filepath.Join(
		os.TempDir(),
		fmt.Sprintf("gcsvc-%d", os.Getuid()),
		hex.EncodeToString(sum[:4]),
	)

	rt := &testRuntime{
		cityPath: cityPath,
		cityName: "test-city",
		cfg:      &config.City{},
		sp:       runtime.NewFake(),
		store:    beads.NewMemStore(),
	}
	svc := config.Service{
		Name: "bridge",
		Kind: "proxy_process",
		Process: config.ServiceProcessConfig{
			Command:    []string{"python3", "-c", proxyProcessPythonHelper},
			HealthPath: "/healthz",
		},
	}
	if _, err := ensureStateRoot(cityPath, svc); err != nil {
		t.Fatalf("ensureStateRoot: %v", err)
	}

	inst, err := newProxyProcessInstance(rt, svc, Status{ServiceName: svc.Name, Kind: svc.Kind})
	if err == nil {
		if inst != nil {
			_ = inst.Close()
		}
		t.Fatal("expected start failure")
	}
	if !errors.Is(err, errProxyProcessExitedEarly) {
		t.Fatalf("err = %v, want %v", err, errProxyProcessExitedEarly)
	}
	if _, statErr := os.Stat(socketDir); !os.IsNotExist(statErr) {
		t.Fatalf("socket dir still exists after failed start: %v", statErr)
	}
}

func TestProxyProcessTickUsesCachedPublicationRefsOnReadError(t *testing.T) {
	t.Setenv("GC_SERVICE_HELPER", "1")
	// The helper subprocess is the same test binary. proxy_process.go seeds
	// GC_CITY / GC_CITY_PATH / GC_CITY_RUNTIME_DIR into the child env; without
	// a passthrough declaration the child's internal/testenv init() would
	// strip them.
	setHelperPassthrough(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}

	rt := &testRuntime{
		cityPath: t.TempDir(),
		cityName: "test-city",
		cfg: &config.City{
			Workspace: config.Workspace{Name: "demo-app"},
			Services: []config.Service{{
				Name: "bridge",
				Kind: "proxy_process",
				Publication: config.ServicePublicationConfig{
					Visibility: "public",
				},
				Process: config.ServiceProcessConfig{
					Command:    []string{exe, "-test.run=^TestProxyProcessHelper$", "--"},
					HealthPath: "/healthz",
				},
			}},
		},
		pubCfg: supervisor.PublicationConfig{
			Provider:         "hosted",
			TenantSlug:       "acme",
			PublicBaseDomain: "apps.example.com",
		},
		sp:    runtime.NewFake(),
		store: beads.NewMemStore(),
	}
	writePublicationStoreForTest(t, rt.cityPath, `[
  {
    "service_name": "bridge",
    "visibility": "public",
    "url": "https://bridge--acme--deadbeef.apps.example.com"
  }
]`)

	mgr := NewManager(rt)
	if err := mgr.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	defer mgr.Close() //nolint:errcheck // best-effort cleanup

	loadEnv := func() map[string]string {
		deadline := time.Now().Add(2 * time.Second)
		for {
			req := httptest.NewRequest(http.MethodGet, "/svc/bridge/env", nil)
			rec := httptest.NewRecorder()
			if ok := mgr.ServeHTTP(rec, req); ok && rec.Code == http.StatusOK {
				var env map[string]string
				if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
					t.Fatalf("decode env: %v", err)
				}
				return env
			}
			if time.Now().After(deadline) {
				status, _ := mgr.Get("bridge")
				t.Fatalf("timed out waiting for proxy process env endpoint: status=%+v", status)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	currentSocketPath := func() string {
		t.Helper()
		mgr.mu.RLock()
		defer mgr.mu.RUnlock()
		inst, ok := mgr.entries["bridge"].inst.(*proxyProcessInstance)
		if !ok {
			t.Fatal("bridge instance missing or wrong type")
		}
		return inst.socketPath
	}

	first := loadEnv()
	firstSocket := currentSocketPath()
	if first["GC_SERVICE_PUBLIC_URL"] != "https://bridge--acme--deadbeef.apps.example.com" {
		t.Fatalf("first public URL = %q, want authoritative route", first["GC_SERVICE_PUBLIC_URL"])
	}
	if err := os.WriteFile(rt.PublicationStorePath(), []byte("{"), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", rt.PublicationStorePath(), err)
	}

	mgr.Tick(context.Background(), time.Now().UTC())

	second := loadEnv()
	secondSocket := currentSocketPath()
	if second["GC_SERVICE_PUBLIC_URL"] != first["GC_SERVICE_PUBLIC_URL"] {
		t.Fatalf("public URL = %q, want cached %q", second["GC_SERVICE_PUBLIC_URL"], first["GC_SERVICE_PUBLIC_URL"])
	}
	if secondSocket != firstSocket {
		t.Fatalf("socket path changed on publication store read error: %q -> %q", firstSocket, secondSocket)
	}
}

func TestProxyProcessSwapAndCloseCleanUpSocketFiles(t *testing.T) {
	t.Setenv("GC_SERVICE_HELPER", "1")
	// The helper subprocess is the same test binary. proxy_process.go seeds
	// GC_CITY / GC_CITY_PATH / GC_CITY_RUNTIME_DIR into the child env; without
	// a passthrough declaration the child's internal/testenv init() would
	// strip them.
	setHelperPassthrough(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}

	rt := &testRuntime{
		cityPath: t.TempDir(),
		cityName: "test-city",
		cfg: &config.City{
			Workspace: config.Workspace{Name: "demo-app"},
			Services: []config.Service{{
				Name: "bridge",
				Kind: "proxy_process",
				Publication: config.ServicePublicationConfig{
					Visibility: "public",
				},
				Process: config.ServiceProcessConfig{
					Command:    []string{exe, "-test.run=^TestProxyProcessHelper$", "--"},
					HealthPath: "/healthz",
				},
			}},
		},
		pubCfg: supervisor.PublicationConfig{
			Provider:         "hosted",
			TenantSlug:       "acme",
			PublicBaseDomain: "apps.example.com",
		},
		sp:    runtime.NewFake(),
		store: beads.NewMemStore(),
	}

	mgr := NewManager(rt)
	if err := mgr.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	loadEnv := func() {
		deadline := time.Now().Add(2 * time.Second)
		for {
			req := httptest.NewRequest(http.MethodGet, "/svc/bridge/env", nil)
			rec := httptest.NewRecorder()
			if ok := mgr.ServeHTTP(rec, req); ok && rec.Code == http.StatusOK {
				return
			}
			if time.Now().After(deadline) {
				status, _ := mgr.Get("bridge")
				t.Fatalf("timed out waiting for proxy process env endpoint: status=%+v", status)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	currentSocketPath := func() string {
		t.Helper()
		mgr.mu.RLock()
		defer mgr.mu.RUnlock()
		inst, ok := mgr.entries["bridge"].inst.(*proxyProcessInstance)
		if !ok {
			t.Fatal("bridge instance missing or wrong type")
		}
		return inst.socketPath
	}

	loadEnv()
	firstSocket := currentSocketPath()
	if _, err := os.Stat(firstSocket); err != nil {
		t.Fatalf("first socket missing: %v", err)
	}

	writePublicationStoreForTest(t, rt.cityPath, `[
  {
    "service_name": "bridge",
    "visibility": "public",
    "url": "https://bridge--acme--deadbeef.apps.example.com"
  }
]`)
	mgr.Tick(context.Background(), time.Now().UTC())
	loadEnv()

	secondSocket := currentSocketPath()
	if secondSocket == firstSocket {
		t.Fatalf("socket path did not change across swap: %q", firstSocket)
	}
	if _, err := os.Stat(firstSocket); !os.IsNotExist(err) {
		t.Fatalf("old socket still exists after swap: %v", err)
	}
	if _, err := os.Stat(secondSocket); err != nil {
		t.Fatalf("new socket missing: %v", err)
	}

	if err := mgr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(secondSocket); !os.IsNotExist(err) {
		t.Fatalf("socket still exists after close: %v", err)
	}
}

// TestManagerReloadProxyProcess_ConstructionFailureSchedulesRetry verifies
// the #1774 fix: when proxy_process construction fails during Reload,
// the entry is stored with inst=nil but nextConstructionRetry is set
// to a time in the future. Without this scheduling, Tick would skip
// the entry forever and the service would stay dead until manual
// Restart.
func TestManagerReloadProxyProcess_ConstructionFailureSchedulesRetry(t *testing.T) {
	rt := &testRuntime{
		cityPath: t.TempDir(),
		cityName: "test-city",
		cfg: &config.City{
			Services: []config.Service{{
				Name: "doomed",
				Kind: "proxy_process",
				Process: config.ServiceProcessConfig{
					Command:    []string{"/this/binary/does/not/exist"},
					HealthPath: "/healthz",
				},
			}},
		},
		sp:    runtime.NewFake(),
		store: beads.NewMemStore(),
	}
	mgr := NewManager(rt)
	defer mgr.Close() //nolint:errcheck // best-effort cleanup

	before := time.Now().UTC()
	if err := mgr.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	mgr.mu.RLock()
	e, ok := mgr.entries["doomed"]
	mgr.mu.RUnlock()
	if !ok {
		t.Fatal("entry missing after Reload")
	}
	if e.inst != nil {
		t.Fatalf("inst should be nil after construction failure; got %T", e.inst)
	}
	if e.status.LocalState != "config_error" {
		t.Fatalf("LocalState = %q, want config_error", e.status.LocalState)
	}
	if e.nextConstructionRetry.IsZero() {
		t.Fatal("nextConstructionRetry should be set; got zero (would prevent Tick from ever retrying)")
	}
	if !e.nextConstructionRetry.After(before) {
		t.Fatalf("nextConstructionRetry = %v, want > %v", e.nextConstructionRetry, before)
	}
}

// TestManagerTickProxyProcess_RetriesAfterDeadline verifies the #1774
// fix: Tick re-attempts construction on a nil-inst proxy_process entry
// once nextConstructionRetry has elapsed. The retry still fails (the
// binary is still missing) so we assert the deadline was bumped — the
// key invariant is "Tick is willing to retry" rather than success.
func TestManagerTickProxyProcess_RetriesAfterDeadline(t *testing.T) {
	rt := &testRuntime{
		cityPath: t.TempDir(),
		cityName: "test-city",
		cfg: &config.City{
			Services: []config.Service{{
				Name: "doomed",
				Kind: "proxy_process",
				Process: config.ServiceProcessConfig{
					Command:    []string{"/this/binary/does/not/exist"},
					HealthPath: "/healthz",
				},
			}},
		},
		sp:    runtime.NewFake(),
		store: beads.NewMemStore(),
	}
	mgr := NewManager(rt)
	defer mgr.Close() //nolint:errcheck // best-effort cleanup
	if err := mgr.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	mgr.mu.Lock()
	e := mgr.entries["doomed"]
	originalDeadline := e.nextConstructionRetry
	// Simulate sufficient time having passed without sleeping the test.
	e.nextConstructionRetry = time.Time{}
	mgr.mu.Unlock()

	mgr.Tick(context.Background(), time.Now().UTC().Add(time.Hour))

	mgr.mu.RLock()
	e = mgr.entries["doomed"]
	mgr.mu.RUnlock()
	if e == nil {
		t.Fatal("entry vanished after Tick")
	}
	if e.inst != nil {
		t.Fatalf("inst should still be nil (binary still missing); got %T", e.inst)
	}
	if e.nextConstructionRetry.IsZero() {
		t.Fatal("nextConstructionRetry should be re-armed after a failed retry; got zero")
	}
	// Deadline should have moved forward — i.e. not still equal to original.
	// We can't assert exact value because the test uses Tick's own clock.
	if e.nextConstructionRetry.Equal(originalDeadline) {
		t.Fatalf("nextConstructionRetry = %v unchanged; should have been bumped", e.nextConstructionRetry)
	}
}

// TestManagerTickProxyProcess_RetryRespectsDeadline verifies that Tick
// does NOT re-attempt construction before nextConstructionRetry has
// elapsed. Bypassing the deadline would spam the supervisor with
// per-Tick retry attempts when the precondition is permanently broken.
func TestManagerTickProxyProcess_RetryRespectsDeadline(t *testing.T) {
	rt := &testRuntime{
		cityPath: t.TempDir(),
		cityName: "test-city",
		cfg: &config.City{
			Services: []config.Service{{
				Name: "doomed",
				Kind: "proxy_process",
				Process: config.ServiceProcessConfig{
					Command:    []string{"/this/binary/does/not/exist"},
					HealthPath: "/healthz",
				},
			}},
		},
		sp:    runtime.NewFake(),
		store: beads.NewMemStore(),
	}
	mgr := NewManager(rt)
	defer mgr.Close() //nolint:errcheck // best-effort cleanup
	if err := mgr.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	mgr.mu.Lock()
	originalDeadline := mgr.entries["doomed"].nextConstructionRetry
	mgr.mu.Unlock()

	// Tick at a time well before the deadline.
	mgr.Tick(context.Background(), originalDeadline.Add(-1*time.Second))

	mgr.mu.RLock()
	deadlineAfter := mgr.entries["doomed"].nextConstructionRetry
	mgr.mu.RUnlock()
	if !deadlineAfter.Equal(originalDeadline) {
		t.Fatalf("nextConstructionRetry changed before deadline elapsed: %v -> %v", originalDeadline, deadlineAfter)
	}
}
