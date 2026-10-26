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

// TestStandaloneControllerClient covers the decision that gates the standalone
// route: a standalone controller endpoint is built only when city.toml names
// a usable [api] port on a loopback bind (or allows mutations). Every nil
// return is apiClient's answer to its caller — the local-fallback signal;
// apiClient consults this path only for a standalone or legacy controller.
// (gascity ga-tp7)
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

// TestAPIClientRouting covers apiClient's routing: the supervisor-managed
// client for a supervisor-hosted city (the supervisor hosts the controller
// in-process and ignores the city's [api] port; #5262), the standalone
// endpoint when a standalone — or legacy, pre-identify — controller is live
// with an [api] port configured, nil (the caller's local fallback) when such
// a controller has no usable port, the supervisor client when the socket is
// down, and nil under the GC_NO_API escape hatch. (gascity ga-tp7)
func TestAPIClientRouting(t *testing.T) {
	sentinel := api.NewClient("http://supervisor.sentinel:1")

	restore := func(identity func(string) controllerIdentityReply, sup func(string) *api.Client) {
		apiRouteControllerIdentityHook = identity
		apiRouteSupervisorClientHook = sup
	}
	origIdentity, origSup := apiRouteControllerIdentityHook, apiRouteSupervisorClientHook
	t.Cleanup(func() { restore(origIdentity, origSup) })

	t.Run("controller-alive-no-api-port-returns-nil", func(t *testing.T) {
		// A controller predating identify answers only ping: PID set, hosting
		// mode unknown. General commands have a local fallback, so apiClient
		// returns nil here (no global supervisor fall-through).
		t.Setenv("GC_NO_API", "")
		restore(func(string) controllerIdentityReply { return controllerIdentityReply{PID: 4242} },
			func(string) *api.Client { return sentinel })
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n")
		if got := apiClient(dir); got != nil {
			t.Fatalf("apiClient = %p, want nil (general commands use local fallback)", got)
		}
	})

	t.Run("controller-alive-with-api-port-uses-standalone", func(t *testing.T) {
		t.Setenv("GC_NO_API", "")
		restore(func(string) controllerIdentityReply {
			return controllerIdentityReply{PID: 4242, HostingMode: controllerHostingStandalone}
		}, func(string) *api.Client { return sentinel })
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n[api]\nport = 8080\n")
		got := apiClient(dir)
		if got == nil {
			t.Fatalf("apiClient = nil, want standalone client")
		}
		if got == sentinel {
			t.Fatalf("apiClient returned supervisor sentinel, want standalone client (no regression)")
		}
	})

	t.Run("controller-down-uses-supervisor", func(t *testing.T) {
		t.Setenv("GC_NO_API", "")
		restore(func(string) controllerIdentityReply { return controllerIdentityReply{} },
			func(string) *api.Client { return sentinel })
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n[api]\nport = 8080\n")
		if got := apiClient(dir); got != sentinel {
			t.Fatalf("apiClient = %p, want supervisor sentinel %p", got, sentinel)
		}
	})

	t.Run("escape-hatch-returns-nil", func(t *testing.T) {
		// The gate short-circuits before any probe: no identity round-trip
		// under the operator escape hatch.
		t.Setenv("GC_NO_API", "1")
		restore(func(string) controllerIdentityReply {
			t.Fatalf("apiClient probed controller identity under the GC_NO_API escape hatch")
			return controllerIdentityReply{}
		}, func(string) *api.Client { return sentinel })
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n")
		if got := apiClient(dir); got != nil {
			t.Fatalf("apiClient = %p, want nil under GC_NO_API escape hatch", got)
		}
	})

	t.Run("supervisor-hosted-with-api-port-uses-supervisor", func(t *testing.T) {
		// #5262: the supervisor hosts the city's controller in-process and
		// ignores the city's [api] port, so the supervisor client is the only
		// endpoint serving the city.
		t.Setenv("GC_NO_API", "")
		restore(func(string) controllerIdentityReply {
			return controllerIdentityReply{PID: 4242, HostingMode: controllerHostingSupervisor}
		}, func(string) *api.Client { return sentinel })
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n[api]\nport = 9443\n")
		if got := apiClient(dir); got != sentinel {
			t.Fatalf("apiClient = %p, want supervisor sentinel %p for a supervisor-hosted city (its [api] port 9443 is not served)", got, sentinel)
		}
	})

	t.Run("supervisor-hosted-no-api-port-uses-supervisor", func(t *testing.T) {
		t.Setenv("GC_NO_API", "")
		restore(func(string) controllerIdentityReply {
			return controllerIdentityReply{PID: 4242, HostingMode: controllerHostingSupervisor}
		}, func(string) *api.Client { return sentinel })
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n")
		if got := apiClient(dir); got != sentinel {
			t.Fatalf("apiClient = %p, want supervisor sentinel %p for a supervisor-hosted city", got, sentinel)
		}
	})

	t.Run("supervisor-hosted-supervisor-unavailable-returns-nil", func(t *testing.T) {
		// City not registered or not running under the supervisor: nil is the
		// caller's local-fallback signal — never a client for the unserved
		// [api] port.
		t.Setenv("GC_NO_API", "")
		restore(func(string) controllerIdentityReply {
			return controllerIdentityReply{PID: 4242, HostingMode: controllerHostingSupervisor}
		}, func(string) *api.Client { return nil })
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n[api]\nport = 9443\n")
		if got := apiClient(dir); got != nil {
			t.Fatalf("apiClient = %p, want nil when the supervisor-managed client is unavailable", got)
		}
	})

	t.Run("standalone-no-api-port-returns-nil", func(t *testing.T) {
		t.Setenv("GC_NO_API", "")
		restore(func(string) controllerIdentityReply {
			return controllerIdentityReply{PID: 4242, HostingMode: controllerHostingStandalone}
		}, func(string) *api.Client {
			t.Fatalf("apiClient consulted the supervisor client for a standalone controller")
			return sentinel
		})
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n")
		if got := apiClient(dir); got != nil {
			t.Fatalf("apiClient = %p, want nil for a standalone controller without an [api] port", got)
		}
	})

	t.Run("unknown-with-api-port-uses-standalone", func(t *testing.T) {
		// A controller predating identify: PID set, hosting mode unknown. It
		// keeps the historical standalone routing.
		t.Setenv("GC_NO_API", "")
		restore(func(string) controllerIdentityReply { return controllerIdentityReply{PID: 4242} },
			func(string) *api.Client {
				t.Fatalf("apiClient consulted the supervisor client for a legacy controller with a usable [api] port")
				return sentinel
			})
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n[api]\nport = 8080\n")
		got := apiClient(dir)
		if got == nil {
			t.Fatalf("apiClient = nil, want standalone client")
		}
		if got == sentinel {
			t.Fatalf("apiClient returned supervisor sentinel, want standalone client")
		}
	})
}

// TestMaintenanceAPIClientRoutesToSupervisor proves the maintenance-scoped
// fall-through: when the controller socket is alive but the supervisor-managed
// city omits a standalone [api] port, maintenanceAPIClient routes to the
// supervisor-managed client (maintenance has no local fallback), where general
// commands' apiClient returns nil. (gascity ga-tp7)
func TestMaintenanceAPIClientRoutesToSupervisor(t *testing.T) {
	sentinel := api.NewClient("http://supervisor.sentinel:1")
	origIdentity, origSup := apiRouteControllerIdentityHook, apiRouteSupervisorClientHook
	t.Cleanup(func() {
		apiRouteControllerIdentityHook = origIdentity
		apiRouteSupervisorClientHook = origSup
	})

	t.Run("alive-no-api-port-routes-to-supervisor", func(t *testing.T) {
		// A controller predating identify reports liveness (PID) with an
		// unknown hosting mode and no [api] port — the fall-through's original
		// ra-r9hm6v shape.
		t.Setenv("GC_NO_API", "")
		apiRouteControllerIdentityHook = func(string) controllerIdentityReply { return controllerIdentityReply{PID: 4242} }
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
		apiRouteControllerIdentityHook = func(string) controllerIdentityReply { return controllerIdentityReply{PID: 4242} }
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

// TestCityStatusAPIClientRoutesToSupervisor is the ra-r9hm6v regression test.
// Before this fix, `gc status` resolved its client through plain apiClient,
// which returns nil whenever the per-city controller socket is alive but the
// city has no standalone [api] port — exactly the shape of a supervisor-
// managed city like a default `gc init`'d city (no [api] section in
// city.toml). That nil forced every `gc status` invocation onto the local
// fallback (open the bead/dolt store, rescan event archives to rebuild store
// health), measured at ~9.5s of CPU on a 26-agent/1.2GB city versus ~0.35s
// for the supervisor's already-cached response. cityStatusAPIClient now
// shares supervisorFallthroughAPIClient with maintenanceAPIClient, so it
// must resolve the supervisor client in that same shape instead of nil.
func TestCityStatusAPIClientRoutesToSupervisor(t *testing.T) {
	sentinel := api.NewClient("http://supervisor.sentinel:1")
	origIdentity, origSup := apiRouteControllerIdentityHook, apiRouteSupervisorClientHook
	t.Cleanup(func() {
		apiRouteControllerIdentityHook = origIdentity
		apiRouteSupervisorClientHook = origSup
	})

	t.Run("alive-no-api-port-routes-to-supervisor", func(t *testing.T) {
		// A controller predating identify reports liveness (PID) with an
		// unknown hosting mode and no [api] port — the fall-through's original
		// ra-r9hm6v shape.
		t.Setenv("GC_NO_API", "")
		apiRouteControllerIdentityHook = func(string) controllerIdentityReply { return controllerIdentityReply{PID: 4242} }
		apiRouteSupervisorClientHook = func(string) *api.Client { return sentinel }
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n")
		c, reason := cityStatusAPIClient(dir)
		if c != sentinel {
			t.Fatalf("cityStatusAPIClient client = %p, want supervisor sentinel %p (ra-r9hm6v regression: must not fall back to nil/local)", c, sentinel)
		}
		if reason != "" {
			t.Fatalf("cityStatusAPIClient reason = %q, want empty", reason)
		}
	})

	t.Run("escape-hatch-skips-supervisor", func(t *testing.T) {
		t.Setenv("GC_NO_API", "1")
		apiRouteControllerIdentityHook = func(string) controllerIdentityReply { return controllerIdentityReply{PID: 4242} }
		apiRouteSupervisorClientHook = func(string) *api.Client { return sentinel }
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n")
		c, reason := cityStatusAPIClient(dir)
		if c != nil {
			t.Fatalf("cityStatusAPIClient client = %p, want nil under GC_NO_API", c)
		}
		if reason != "escape-hatch" {
			t.Fatalf("cityStatusAPIClient reason = %q, want \"escape-hatch\"", reason)
		}
	})

	t.Run("controller-fully-down-still-falls-back-local", func(t *testing.T) {
		// No socket at all (not even a supervisor-managed one): this is the
		// genuine "nothing is running" case, where the local fallback remains
		// correct and necessary — the fix must not paper over it.
		t.Setenv("GC_NO_API", "")
		apiRouteControllerIdentityHook = func(string) controllerIdentityReply { return controllerIdentityReply{} }
		apiRouteSupervisorClientHook = func(string) *api.Client { return nil }
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n")
		c, reason := cityStatusAPIClient(dir)
		if c != nil {
			t.Fatalf("cityStatusAPIClient client = %p, want nil when nothing is running", c)
		}
		if reason != "controller-down" {
			t.Fatalf("cityStatusAPIClient reason = %q, want \"controller-down\"", reason)
		}
	})

	t.Run("supervisor-hosted-with-api-port-routes-to-supervisor", func(t *testing.T) {
		// #5262: a supervisor-hosted city with an [api] port set — the port is
		// ignored under supervisor mode, so routing must still reach the
		// supervisor client instead of a dead standalone endpoint.
		t.Setenv("GC_NO_API", "")
		apiRouteControllerIdentityHook = func(string) controllerIdentityReply {
			return controllerIdentityReply{PID: 4242, HostingMode: controllerHostingSupervisor}
		}
		apiRouteSupervisorClientHook = func(string) *api.Client { return sentinel }
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n[api]\nport = 9443\n")
		c, reason := cityStatusAPIClient(dir)
		if c != sentinel {
			t.Fatalf("cityStatusAPIClient client = %p, want supervisor sentinel %p (the [api] port is not served under supervisor mode)", c, sentinel)
		}
		if reason != "" {
			t.Fatalf("cityStatusAPIClient reason = %q, want empty", reason)
		}
	})
}
