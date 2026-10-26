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

// TestStandaloneControllerClient covers the endpoint apiClient resolves for a
// standalone controller (or one predating identify): a client is built only
// when city.toml names a usable [api] port on a loopback bind (or allows
// mutations). apiClient returns each nil to its caller, which takes its local
// fallback. (gascity ga-tp7)
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

// TestAPIClientRouting covers apiClient's routing, keyed on the hosting mode
// the per-city controller socket reports through identify. A supervisor-hosted
// city routes to the supervisor-managed client (nil when it is unavailable),
// never to its [api] port, which supervisor mode does not serve. A standalone
// controller, or one predating identify (hosting mode unknown), routes to its
// configured [api] port and returns nil (the caller's local fallback) without
// one. A down socket routes to the supervisor client, and GC_NO_API returns nil
// before any probe. The supervisor fall-through for a live controller of
// unknown mode with no [api] port is scoped to the
// supervisorFallthroughAPIClient callers — see
// TestMaintenanceAPIClientRoutesToSupervisor and
// TestCityStatusAPIClientRoutesToSupervisor. (gascity ga-tp7, #5262)
func TestAPIClientRouting(t *testing.T) {
	sentinel := api.NewClient("http://supervisor.sentinel:1")
	supervisorHosted := func(string) controllerIdentityReply {
		return controllerIdentityReply{PID: 4242, HostingMode: controllerHostingSupervisor}
	}
	standaloneHosted := func(string) controllerIdentityReply {
		return controllerIdentityReply{PID: 4242, HostingMode: controllerHostingStandalone}
	}
	// A controller predating identify answers only ping: alive, mode unknown.
	legacyHosted := func(string) controllerIdentityReply { return controllerIdentityReply{PID: 4242} }
	supervisorClient := func(string) *api.Client { return sentinel }

	restore := func(identity func(string) controllerIdentityReply, sup func(string) *api.Client) {
		apiRouteControllerIdentityHook = identity
		apiRouteSupervisorClientHook = sup
	}
	origIdentity, origSup := apiRouteControllerIdentityHook, apiRouteSupervisorClientHook
	t.Cleanup(func() { restore(origIdentity, origSup) })

	t.Run("controller-alive-no-api-port-returns-nil", func(t *testing.T) {
		// General commands have a local fallback, so apiClient returns nil for a
		// controller predating identify (no global supervisor fall-through).
		t.Setenv("GC_NO_API", "")
		restore(legacyHosted, supervisorClient)
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n")
		if got := apiClient(dir); got != nil {
			t.Fatalf("apiClient = %p, want nil (general commands use local fallback)", got)
		}
	})

	t.Run("controller-alive-with-api-port-uses-standalone", func(t *testing.T) {
		t.Setenv("GC_NO_API", "")
		restore(standaloneHosted, supervisorClient)
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
		restore(func(string) controllerIdentityReply { return controllerIdentityReply{} }, supervisorClient)
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n[api]\nport = 8080\n")
		if got := apiClient(dir); got != sentinel {
			t.Fatalf("apiClient = %p, want supervisor sentinel %p", got, sentinel)
		}
	})

	t.Run("escape-hatch-returns-nil", func(t *testing.T) {
		t.Setenv("GC_NO_API", "1")
		restore(func(string) controllerIdentityReply {
			t.Errorf("controller identity probed under the GC_NO_API escape hatch")
			return controllerIdentityReply{PID: 4242, HostingMode: controllerHostingSupervisor}
		}, supervisorClient)
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n")
		if got := apiClient(dir); got != nil {
			t.Fatalf("apiClient = %p, want nil under GC_NO_API escape hatch", got)
		}
	})

	t.Run("supervisor-hosted-with-api-port-uses-supervisor", func(t *testing.T) {
		// AC-1 (#5262): supervisor mode ignores the city's [api] port, so a
		// client for 9443 would target a listener nothing serves.
		t.Setenv("GC_NO_API", "")
		restore(supervisorHosted, supervisorClient)
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n[api]\nport = 9443\n")
		if got := apiClient(dir); got != sentinel {
			t.Fatalf("apiClient = %p, want supervisor sentinel %p (the supervisor does not serve [api] port 9443)", got, sentinel)
		}
	})
}

// TestMaintenanceAPIClientRoutesToSupervisor proves the maintenance-scoped
// fall-through: when the controller socket is alive with an unknown hosting
// mode (a supervisor predating identify) and the city omits a standalone [api]
// port, maintenanceAPIClient routes to the supervisor-managed client
// (maintenance has no local fallback), where general commands' apiClient
// returns nil. (gascity ga-tp7)
func TestMaintenanceAPIClientRoutesToSupervisor(t *testing.T) {
	sentinel := api.NewClient("http://supervisor.sentinel:1")
	origIdentity, origSup := apiRouteControllerIdentityHook, apiRouteSupervisorClientHook
	t.Cleanup(func() {
		apiRouteControllerIdentityHook = origIdentity
		apiRouteSupervisorClientHook = origSup
	})

	t.Run("alive-no-api-port-routes-to-supervisor", func(t *testing.T) {
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
// must resolve the supervisor client in that same shape (a live controller of
// unknown hosting mode, no [api] port) instead of nil, and for a
// supervisor-hosted city whether or not it sets an [api] port.
func TestCityStatusAPIClientRoutesToSupervisor(t *testing.T) {
	sentinel := api.NewClient("http://supervisor.sentinel:1")
	origIdentity, origSup := apiRouteControllerIdentityHook, apiRouteSupervisorClientHook
	t.Cleanup(func() {
		apiRouteControllerIdentityHook = origIdentity
		apiRouteSupervisorClientHook = origSup
	})

	t.Run("alive-no-api-port-routes-to-supervisor", func(t *testing.T) {
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

	t.Run("supervisor-hosted-with-api-port-routes-to-supervisor", func(t *testing.T) {
		// The supervisor ignores the city's [api] port, so a supervisor-hosted
		// city with one set still resolves the supervisor client. (#5262)
		t.Setenv("GC_NO_API", "")
		apiRouteControllerIdentityHook = func(string) controllerIdentityReply {
			return controllerIdentityReply{PID: 4242, HostingMode: controllerHostingSupervisor}
		}
		apiRouteSupervisorClientHook = func(string) *api.Client { return sentinel }
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n[api]\nport = 9443\n")
		c, reason := cityStatusAPIClient(dir)
		if c != sentinel {
			t.Fatalf("cityStatusAPIClient client = %p, want supervisor sentinel %p ([api] port 9443 is not served)", c, sentinel)
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
}
