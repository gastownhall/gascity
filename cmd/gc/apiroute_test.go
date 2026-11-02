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
	supervisorUnconsulted := func(t *testing.T) func(string) *api.Client {
		return func(string) *api.Client {
			t.Errorf("supervisor client consulted for a controller that serves its own [api] port")
			return sentinel
		}
	}

	// restore also resets the supervisor to one that requires no city grants;
	// the rows about a grant-gated supervisor set that hook after it.
	restore := func(identity func(string) controllerIdentityReply, sup func(string) *api.Client) {
		apiRouteControllerIdentityHook = identity
		apiRouteSupervisorClientHook = sup
		apiRouteSupervisorGrantsHook = func() bool { return false }
	}
	origIdentity, origSup, origGrants := apiRouteControllerIdentityHook, apiRouteSupervisorClientHook, apiRouteSupervisorGrantsHook
	t.Cleanup(func() {
		restore(origIdentity, origSup)
		apiRouteSupervisorGrantsHook = origGrants
	})

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

	t.Run("supervisor-hosted-no-api-port-uses-supervisor", func(t *testing.T) {
		// AC-2 (#5262): the supervisor is the only endpoint serving the city, so
		// nil (the local fallback) cannot reach its process-owned runtime state.
		t.Setenv("GC_NO_API", "")
		restore(supervisorHosted, supervisorClient)
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n")
		if got := apiClient(dir); got != sentinel {
			t.Fatalf("apiClient = %p, want supervisor sentinel %p for a supervisor-hosted city", got, sentinel)
		}
	})

	t.Run("supervisor-hosted-supervisor-unavailable-returns-nil", func(t *testing.T) {
		// AC-3 (#5262): with no supervisor-managed client the answer is nil,
		// never a client for the unserved [api] port.
		t.Setenv("GC_NO_API", "")
		restore(supervisorHosted, func(string) *api.Client { return nil })
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n[api]\nport = 9443\n")
		if got := apiClient(dir); got != nil {
			t.Fatalf("apiClient = %p, want nil (no supervisor client; [api] port 9443 is not served)", got)
		}
	})

	t.Run("supervisor-hosted-requiring-grants-returns-nil", func(t *testing.T) {
		// A supervisor that requires a signed grant on city routes refuses this
		// client's gated calls with a 401 no caller falls back from, so general
		// commands keep their local fallback without resolving the supervisor
		// client — among them the per-turn gc mail check --inject hook, which
		// would otherwise stop injecting mail.
		t.Setenv("GC_NO_API", "")
		restore(supervisorHosted, func(string) *api.Client {
			t.Errorf("supervisor client resolved for a supervisor that requires city grants")
			return sentinel
		})
		apiRouteSupervisorGrantsHook = func() bool { return true }
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n")
		if got := apiClient(dir); got != nil {
			t.Fatalf("apiClient = %p, want nil (the supervisor requires grants this client never mints)", got)
		}
		if c, reason := mailCheckAPIClient(dir); c != nil || reason != "grant-required" {
			t.Fatalf("mailCheckAPIClient = (%p, %q), want (nil, \"grant-required\") so gc mail check --inject injects locally", c, reason)
		}
	})

	t.Run("standalone-no-api-port-returns-nil", func(t *testing.T) {
		// AC-4: a standalone controller serves only its own [api] port.
		t.Setenv("GC_NO_API", "")
		restore(standaloneHosted, supervisorClient)
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n")
		if got := apiClient(dir); got != nil {
			t.Fatalf("apiClient = %p, want nil (standalone controller without an [api] port)", got)
		}
	})

	t.Run("standalone-with-api-port-skips-supervisor", func(t *testing.T) {
		// AC-4: a standalone controller's [api] port is used without consulting
		// the supervisor.
		t.Setenv("GC_NO_API", "")
		restore(standaloneHosted, supervisorUnconsulted(t))
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n[api]\nport = 8080\n")
		if got := apiClient(dir); got == nil || got == sentinel {
			t.Fatalf("apiClient = %p, want the standalone client for [api] port 8080", got)
		}
	})

	t.Run("unknown-with-api-port-uses-standalone", func(t *testing.T) {
		// AC-4: a controller predating identify keeps today's standalone routing;
		// no hosting role is inferred for it.
		t.Setenv("GC_NO_API", "")
		restore(legacyHosted, supervisorUnconsulted(t))
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n[api]\nport = 8080\n")
		if got := apiClient(dir); got == nil || got == sentinel {
			t.Fatalf("apiClient = %p, want the standalone client for [api] port 8080", got)
		}
	})
}

// TestMaintenanceAPIClientRoutesToSupervisor proves the maintenance-scoped
// fall-through: when the controller socket is alive with an unknown hosting
// mode (a supervisor predating identify) and the city omits a standalone [api]
// port, or when the supervisor hosting the city requires city grants,
// maintenanceAPIClient routes to the supervisor-managed client (maintenance
// has no local fallback), where general commands' apiClient returns nil. A
// controller that identifies as standalone is never sent to the supervisor,
// and the supervisor-managed client is resolved at most once. The fall-through
// never consults the supervisor's grant requirement. (gascity ga-tp7)
func TestMaintenanceAPIClientRoutesToSupervisor(t *testing.T) {
	sentinel := api.NewClient("http://supervisor.sentinel:1")
	origIdentity, origSup, origGrants := apiRouteControllerIdentityHook, apiRouteSupervisorClientHook, apiRouteSupervisorGrantsHook
	t.Cleanup(func() {
		apiRouteControllerIdentityHook = origIdentity
		apiRouteSupervisorClientHook = origSup
		apiRouteSupervisorGrantsHook = origGrants
	})
	apiRouteSupervisorGrantsHook = func() bool {
		t.Errorf("supervisor grant requirement consulted on the fall-through path")
		return true
	}

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

	t.Run("supervisor-hosted-requiring-grants-routes-to-supervisor", func(t *testing.T) {
		// Maintenance has no local fallback, so it asks a supervisor that
		// requires city grants and reports its refusal like any API error.
		t.Setenv("GC_NO_API", "")
		apiRouteControllerIdentityHook = func(string) controllerIdentityReply {
			return controllerIdentityReply{PID: 4242, HostingMode: controllerHostingSupervisor}
		}
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

	t.Run("standalone-no-api-port-skips-supervisor", func(t *testing.T) {
		// A controller that identifies as standalone serves only its own [api]
		// port; the supervisor does not host its city.
		t.Setenv("GC_NO_API", "")
		apiRouteControllerIdentityHook = func(string) controllerIdentityReply {
			return controllerIdentityReply{PID: 4242, HostingMode: controllerHostingStandalone}
		}
		apiRouteSupervisorClientHook = func(string) *api.Client {
			t.Errorf("supervisor client consulted for a controller that identifies as standalone")
			return sentinel
		}
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n")
		c, reason := maintenanceAPIClient(dir)
		if c != nil {
			t.Fatalf("maintenanceAPIClient client = %p, want nil (standalone controller without an [api] port)", c)
		}
		if reason != "controller-down" {
			t.Fatalf("maintenanceAPIClient reason = %q, want \"controller-down\"", reason)
		}
	})

	t.Run("supervisor-hosted-supervisor-unavailable-resolves-once", func(t *testing.T) {
		// With no supervisor-managed client the answer is nil after a single
		// resolution, never a second identify/registry/ListCities round, and the
		// city's [api] bind (which the supervisor ignores) is not the reason.
		t.Setenv("GC_NO_API", "")
		apiRouteControllerIdentityHook = func(string) controllerIdentityReply {
			return controllerIdentityReply{PID: 4242, HostingMode: controllerHostingSupervisor}
		}
		resolutions := 0
		apiRouteSupervisorClientHook = func(string) *api.Client {
			resolutions++
			return nil
		}
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n[api]\nport = 9443\nbind = \"0.0.0.0\"\n")
		c, reason := maintenanceAPIClient(dir)
		if c != nil {
			t.Fatalf("maintenanceAPIClient client = %p, want nil (no supervisor-managed client)", c)
		}
		if resolutions != 1 {
			t.Fatalf("supervisor client resolved %d times, want 1", resolutions)
		}
		if reason != "controller-down" {
			t.Fatalf("maintenanceAPIClient reason = %q, want \"controller-down\"", reason)
		}
	})
}

// TestAPIClientFallbackReason covers the reason apiClient's callers log for its
// nil, read from the hosting mode the controller socket reports: a standalone
// controller's read-only [api] bind is "non-loopback-bind", a supervisor-hosted
// city never blames the [api] bind the supervisor ignores, and a supervisor
// that requires city grants is "grant-required". (#5262)
func TestAPIClientFallbackReason(t *testing.T) {
	origIdentity, origGrants := apiRouteControllerIdentityHook, apiRouteSupervisorGrantsHook
	t.Cleanup(func() {
		apiRouteControllerIdentityHook = origIdentity
		apiRouteSupervisorGrantsHook = origGrants
	})
	const readOnlyBind = "name = \"t\"\n[api]\nport = 9443\nbind = \"0.0.0.0\"\n"
	cases := []struct {
		name   string
		mode   controllerHostingMode
		grants bool
		want   string
	}{
		{name: "standalone-read-only-bind", mode: controllerHostingStandalone, want: "non-loopback-bind"},
		{name: "unknown-read-only-bind", mode: controllerHostingUnknown, want: "non-loopback-bind"},
		{name: "supervisor-hosted-ignores-api-bind", mode: controllerHostingSupervisor, want: "controller-down"},
		{name: "supervisor-hosted-requiring-grants", mode: controllerHostingSupervisor, grants: true, want: "grant-required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_NO_API", "")
			apiRouteControllerIdentityHook = func(string) controllerIdentityReply {
				return controllerIdentityReply{PID: 4242, HostingMode: tc.mode}
			}
			apiRouteSupervisorGrantsHook = func() bool { return tc.grants }
			dir := writeCityTOMLForRoute(t, t.TempDir(), readOnlyBind)
			if got := apiClientFallbackReason(dir); got != tc.want {
				t.Fatalf("apiClientFallbackReason = %q, want %q", got, tc.want)
			}
		})
	}
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

// TestSessionListAPIClientRouting covers the client gc session list resolves.
// A supervisor-hosted city lists its sessions locally, with or without an
// [api] port and whatever the supervisor's grant requirement, and the
// supervisor client is never resolved for it: the local rows carry the
// session list contract (agent_name, closed, the summary) that the API's rows
// do not, and the API fills each active session's active bead by reading every
// rig store, a suspended rig's included. Every other shape routes as apiClient
// does. (#5262)
func TestSessionListAPIClientRouting(t *testing.T) {
	sentinel := api.NewClient("http://supervisor.sentinel:1")
	origIdentity, origSup, origGrants := apiRouteControllerIdentityHook, apiRouteSupervisorClientHook, apiRouteSupervisorGrantsHook
	t.Cleanup(func() {
		apiRouteControllerIdentityHook = origIdentity
		apiRouteSupervisorClientHook = origSup
		apiRouteSupervisorGrantsHook = origGrants
	})
	const (
		noAPIPort = "name = \"t\"\n"
		apiPort   = "name = \"t\"\n[api]\nport = 9443\n"
	)
	cases := []struct {
		name string
		// escapeHatch sets GC_NO_API=1, under which nothing may be probed.
		escapeHatch bool
		pid         int
		mode        controllerHostingMode
		// supervisor is whether the supervisor client may be resolved; it is
		// the sentinel when it may.
		supervisor bool
		grants     bool
		toml       string
		want       string // "supervisor", "standalone" or "" (nil)
		wantReason string
	}{
		{name: "supervisor-hosted-lists-locally", pid: 4242, mode: controllerHostingSupervisor, toml: noAPIPort, wantReason: "supervisor-hosted"},
		{name: "supervisor-hosted-with-api-port-lists-locally", pid: 4242, mode: controllerHostingSupervisor, toml: apiPort, wantReason: "supervisor-hosted"},
		{name: "supervisor-hosted-requiring-grants-lists-locally", pid: 4242, mode: controllerHostingSupervisor, grants: true, toml: noAPIPort, wantReason: "supervisor-hosted"},
		{name: "standalone-with-api-port-uses-standalone", pid: 4242, mode: controllerHostingStandalone, toml: apiPort, want: "standalone"},
		{name: "unknown-with-api-port-uses-standalone", pid: 4242, mode: controllerHostingUnknown, toml: apiPort, want: "standalone"},
		{name: "standalone-no-api-port-lists-locally", pid: 4242, mode: controllerHostingStandalone, toml: noAPIPort, wantReason: "controller-down"},
		{name: "controller-down-uses-supervisor", supervisor: true, toml: apiPort, want: "supervisor"},
		{name: "escape-hatch-lists-locally", escapeHatch: true, pid: 4242, mode: controllerHostingSupervisor, toml: noAPIPort, wantReason: "escape-hatch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_NO_API", "")
			if tc.escapeHatch {
				t.Setenv("GC_NO_API", "1")
			}
			apiRouteControllerIdentityHook = func(string) controllerIdentityReply {
				if tc.escapeHatch {
					t.Errorf("controller identity probed under the GC_NO_API escape hatch")
				}
				return controllerIdentityReply{PID: tc.pid, HostingMode: tc.mode}
			}
			apiRouteSupervisorClientHook = func(string) *api.Client {
				if !tc.supervisor {
					t.Errorf("supervisor client resolved for gc session list")
				}
				return sentinel
			}
			apiRouteSupervisorGrantsHook = func() bool { return tc.grants }
			dir := writeCityTOMLForRoute(t, t.TempDir(), tc.toml)
			c, reason := sessionListAPIClient(dir)
			switch tc.want {
			case "supervisor":
				if c != sentinel {
					t.Fatalf("sessionListAPIClient client = %p, want supervisor sentinel %p", c, sentinel)
				}
			case "standalone":
				if c == nil || c == sentinel {
					t.Fatalf("sessionListAPIClient client = %p, want the standalone client for [api] port 9443", c)
				}
			default:
				if c != nil {
					t.Fatalf("sessionListAPIClient client = %p, want nil so gc session list lists locally", c)
				}
			}
			if reason != tc.wantReason {
				t.Fatalf("sessionListAPIClient reason = %q, want %q", reason, tc.wantReason)
			}
		})
	}
}
