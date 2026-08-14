package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
)

// writeCityTOMLForRoute writes a minimal city.toml into dir and returns dir.
func writeCityTOMLForRoute(t *testing.T, dir, body string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("writing city.toml: %v", err)
	}
	return dir
}

// withAPIRouteHooks pins every seam the routing ladder consults so a subtest is
// hermetic no matter which one the implementation reads: the controller
// identity probe (hosting mode plus the liveness PID), the legacy liveness
// hook that maintenanceAPIClient still uses, and the supervisor client builder.
// A pid of 0 means no controller is answering the socket.
func withAPIRouteHooks(t *testing.T, pid int, mode controllerHostingMode, supervisor *api.Client) {
	t.Helper()
	withControllerHosting(t, pid, mode)
	origAlive, origSup := apiRouteControllerAliveHook, apiRouteSupervisorClientHook
	apiRouteControllerAliveHook = func(string) int { return pid }
	apiRouteSupervisorClientHook = func(string) *api.Client { return supervisor }
	t.Cleanup(func() {
		apiRouteControllerAliveHook = origAlive
		apiRouteSupervisorClientHook = origSup
	})
}

// TestStandaloneControllerClient covers the decision that gates apiClient's
// fall-through: a standalone controller endpoint is built only when city.toml
// names a usable [api] port on a loopback bind (or allows mutations). Every
// nil return is a signal for apiClient to try the supervisor-managed client
// instead. (gascity ga-tp7)
func TestStandaloneControllerClient(t *testing.T) {
	cases := []struct {
		name    string
		toml    string
		write   bool
		wantNil bool
	}{
		{name: "no-city-toml", write: false, wantNil: true},
		{name: "no-api-section", toml: "name = \"t\"\n", write: true, wantNil: true},
		{name: "api-port-zero", toml: "name = \"t\"\n[api]\nport = 0\n", write: true, wantNil: true},
		{name: "loopback-port", toml: "name = \"t\"\n[api]\nport = 8080\n", write: true, wantNil: false},
		{name: "explicit-localhost", toml: "name = \"t\"\n[api]\nport = 8080\nbind = \"localhost\"\n", write: true, wantNil: false},
		{name: "non-loopback-no-mutations", toml: "name = \"t\"\n[api]\nport = 8080\nbind = \"0.0.0.0\"\n", write: true, wantNil: true},
		{name: "non-loopback-allow-mutations", toml: "name = \"t\"\n[api]\nport = 8080\nbind = \"0.0.0.0\"\nallow_mutations = true\n", write: true, wantNil: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.write {
				writeCityTOMLForRoute(t, dir, tc.toml)
			}
			got := standaloneControllerClient(dir)
			if tc.wantNil && got != nil {
				t.Fatalf("standaloneControllerClient = non-nil, want nil")
			}
			if !tc.wantNil && got == nil {
				t.Fatalf("standaloneControllerClient = nil, want non-nil")
			}
		})
	}
}

// TestAPIClientRouting covers apiClient's routing ladder, which keys on the
// controller's self-reported hosting mode: the supervisor client when the
// supervisor hosts the city (its standalone [api] port is ignored in that
// mode), the standalone endpoint for a standalone controller with an [api]
// port, nil (the caller's local fallback) when no usable endpoint exists, the
// supervisor client when the socket is down, and nil under the GC_NO_API
// escape hatch. A controller predating the identity command reports an unknown
// mode and keeps the pre-existing standalone-only routing; its supervisor
// fall-through stays scoped to maintenance — see
// TestMaintenanceAPIClientRoutesToSupervisor. (gascity ga-tp7)
func TestAPIClientRouting(t *testing.T) {
	sentinel := api.NewClient("http://supervisor.sentinel:1")

	t.Run("supervisor-hosted-uses-supervisor-client", func(t *testing.T) {
		t.Setenv("GC_NO_API", "")
		withAPIRouteHooks(t, 4242, controllerHostingSupervisor, sentinel)
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n")
		if got := apiClient(dir); got != sentinel {
			t.Fatalf("apiClient = %p, want supervisor sentinel %p", got, sentinel)
		}
	})

	// Regression: a supervisor-managed city may still carry an [api] port in
	// city.toml, but supervisor mode ignores it, so nothing listens there.
	// Routing to that dead endpoint made every mutating command fail its API
	// call and drop into a local fallback, which cannot reach a process-owned
	// runtime (an ACP session lives in the supervisor's memory).
	t.Run("supervisor-hosted-ignores-standalone-api-port", func(t *testing.T) {
		t.Setenv("GC_NO_API", "")
		withAPIRouteHooks(t, 4242, controllerHostingSupervisor, sentinel)
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n[api]\nport = 9443\n")
		if got := apiClient(dir); got != sentinel {
			t.Fatalf("apiClient = %p, want supervisor sentinel %p (supervisor mode ignores [api] port)", got, sentinel)
		}
	})

	t.Run("supervisor-hosted-without-supervisor-client-returns-nil", func(t *testing.T) {
		t.Setenv("GC_NO_API", "")
		withAPIRouteHooks(t, 4242, controllerHostingSupervisor, nil)
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n[api]\nport = 9443\n")
		if got := apiClient(dir); got != nil {
			t.Fatalf("apiClient = %p, want nil; the standalone port is not served under supervisor hosting", got)
		}
	})

	t.Run("standalone-hosted-with-api-port-uses-standalone", func(t *testing.T) {
		t.Setenv("GC_NO_API", "")
		withAPIRouteHooks(t, 4242, controllerHostingStandalone, sentinel)
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n[api]\nport = 8080\n")
		got := apiClient(dir)
		if got == nil {
			t.Fatalf("apiClient = nil, want standalone client")
		}
		if got == sentinel {
			t.Fatalf("apiClient returned supervisor sentinel, want standalone client (no regression)")
		}
	})

	t.Run("standalone-hosted-without-api-port-returns-nil", func(t *testing.T) {
		t.Setenv("GC_NO_API", "")
		withAPIRouteHooks(t, 4242, controllerHostingStandalone, sentinel)
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n")
		if got := apiClient(dir); got != nil {
			t.Fatalf("apiClient = %p, want nil (general commands use local fallback)", got)
		}
	})

	t.Run("legacy-unknown-hosting-with-api-port-uses-standalone", func(t *testing.T) {
		t.Setenv("GC_NO_API", "")
		withAPIRouteHooks(t, 4242, controllerHostingUnknown, sentinel)
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n[api]\nport = 8080\n")
		got := apiClient(dir)
		if got == nil {
			t.Fatalf("apiClient = nil, want standalone client")
		}
		if got == sentinel {
			t.Fatalf("apiClient returned supervisor sentinel, want standalone client")
		}
	})

	t.Run("legacy-unknown-hosting-without-api-port-returns-nil", func(t *testing.T) {
		// General commands have a local fallback, so apiClient returns nil here
		// (no global supervisor fall-through).
		t.Setenv("GC_NO_API", "")
		withAPIRouteHooks(t, 4242, controllerHostingUnknown, sentinel)
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n")
		if got := apiClient(dir); got != nil {
			t.Fatalf("apiClient = %p, want nil (general commands use local fallback)", got)
		}
	})

	t.Run("controller-down-uses-supervisor", func(t *testing.T) {
		t.Setenv("GC_NO_API", "")
		withAPIRouteHooks(t, 0, controllerHostingUnknown, sentinel)
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n[api]\nport = 8080\n")
		if got := apiClient(dir); got != sentinel {
			t.Fatalf("apiClient = %p, want supervisor sentinel %p", got, sentinel)
		}
	})

	t.Run("escape-hatch-returns-nil", func(t *testing.T) {
		t.Setenv("GC_NO_API", "1")
		withAPIRouteHooks(t, 4242, controllerHostingSupervisor, sentinel)
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n")
		if got := apiClient(dir); got != nil {
			t.Fatalf("apiClient = %p, want nil under GC_NO_API escape hatch", got)
		}
	})
}

// TestMaintenanceAPIClientRoutesToSupervisor proves the maintenance-scoped
// fall-through that survives for controllers predating the identity command:
// the socket is alive but reports an unknown hosting mode and the city omits a
// standalone [api] port, so apiClient returns nil while maintenanceAPIClient
// (which has no local fallback) still reaches the supervisor-managed client.
// A controller that reports supervisor hosting is routed by apiClient itself —
// see TestAPIClientRouting. (gascity ga-tp7)
func TestMaintenanceAPIClientRoutesToSupervisor(t *testing.T) {
	sentinel := api.NewClient("http://supervisor.sentinel:1")
	origAlive, origSup := apiRouteControllerAliveHook, apiRouteSupervisorClientHook
	t.Cleanup(func() {
		apiRouteControllerAliveHook = origAlive
		apiRouteSupervisorClientHook = origSup
	})

	t.Run("alive-unknown-hosting-no-api-port-routes-to-supervisor", func(t *testing.T) {
		t.Setenv("GC_NO_API", "")
		withControllerHosting(t, 4242, controllerHostingUnknown)
		apiRouteControllerAliveHook = func(string) int { return 4242 }
		apiRouteSupervisorClientHook = func(string) *api.Client { return sentinel }
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n")
		c, reason := maintenanceAPIClient(dir)
		if c != sentinel {
			t.Fatalf("maintenanceAPIClient client = %p, want supervisor sentinel %p", c, sentinel)
		}
		if reason != "" {
			t.Fatalf("maintenanceAPIClient reason = %q, want empty", reason)
		}
	})

	t.Run("escape-hatch-skips-supervisor", func(t *testing.T) {
		t.Setenv("GC_NO_API", "1")
		withControllerHosting(t, 4242, controllerHostingUnknown)
		apiRouteControllerAliveHook = func(string) int { return 4242 }
		apiRouteSupervisorClientHook = func(string) *api.Client { return sentinel }
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n")
		c, reason := maintenanceAPIClient(dir)
		if c != nil {
			t.Fatalf("maintenanceAPIClient client = %p, want nil under GC_NO_API", c)
		}
		if reason != "escape-hatch" {
			t.Fatalf("maintenanceAPIClient reason = %q, want \"escape-hatch\"", reason)
		}
	})
}
