package main

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
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

// stubStandaloneListener makes apiRouteStandaloneListenerHook report that a
// listener accepts connections at every standalone [api] endpoint, so a test
// about routing can name a fixed [api] port without binding it, and without
// dialing whatever happens to listen on that port of the host. It restores the
// hook when the test ends.
func stubStandaloneListener(t *testing.T) {
	t.Helper()
	orig := apiRouteStandaloneListenerHook
	t.Cleanup(func() { apiRouteStandaloneListenerHook = orig })
	apiRouteStandaloneListenerHook = func(string) bool { return true }
}

// unboundLoopbackPort returns a loopback TCP port nothing listens on: it binds
// an ephemeral port and closes the listener at once.
func unboundLoopbackPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a loopback port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("releasing loopback port %d: %v", port, err)
	}
	return port
}

// apiPortCityTOML returns a minimal city.toml for city "t" whose loopback
// [api] port is port.
func apiPortCityTOML(port int) string {
	return "[workspace]\nname = \"t\"\n\n[api]\nport = " + strconv.Itoa(port) + "\n"
}

// TestStandaloneControllerClient covers the endpoint apiClient resolves for a
// standalone controller (or one predating identify): a client is built only
// when city.toml names a usable [api] port on a loopback bind (or allows
// mutations) and a listener accepts connections at that host:port. apiClient
// returns each nil to its caller, which takes its local fallback. (gascity
// ga-tp7, #5761)
func TestStandaloneControllerClient(t *testing.T) {
	cases := []struct {
		name    string
		toml    string
		write   bool
		refuse  bool
		wantNil bool
		// wantAddr is the endpoint the listener probe is asked about; "" means
		// it must not be probed.
		wantAddr string
	}{
		{name: "no-city-toml", write: false, wantNil: true},
		{name: "no-api-section", toml: "name = \"t\"\n", write: true, wantNil: true},
		{name: "api-port-zero", toml: "name = \"t\"\n[api]\nport = 0\n", write: true, wantNil: true},
		{name: "loopback-port", toml: "name = \"t\"\n[api]\nport = 8080\n", write: true, wantNil: false, wantAddr: "127.0.0.1:8080"},
		{name: "explicit-localhost", toml: "name = \"t\"\n[api]\nport = 8080\nbind = \"localhost\"\n", write: true, wantNil: false, wantAddr: "localhost:8080"},
		{name: "non-loopback-no-mutations", toml: "name = \"t\"\n[api]\nport = 8080\nbind = \"0.0.0.0\"\n", write: true, wantNil: true},
		{name: "non-loopback-allow-mutations", toml: "name = \"t\"\n[api]\nport = 8080\nbind = \"0.0.0.0\"\nallow_mutations = true\n", write: true, wantNil: false, wantAddr: "0.0.0.0:8080"},
		{name: "ipv6-loopback", toml: "name = \"t\"\n[api]\nport = 8080\nbind = \"::1\"\n", write: true, wantNil: false, wantAddr: "[::1]:8080"},
		{name: "hook-refuses", toml: "name = \"t\"\n[api]\nport = 8080\n", write: true, refuse: true, wantNil: true, wantAddr: "127.0.0.1:8080"},
	}
	orig := apiRouteStandaloneListenerHook
	t.Cleanup(func() { apiRouteStandaloneListenerHook = orig })
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var probed []string
			apiRouteStandaloneListenerHook = func(addr string) bool {
				probed = append(probed, addr)
				return !tc.refuse
			}
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
			switch {
			case tc.wantAddr == "" && len(probed) != 0:
				t.Fatalf("listener probed at %v, want no probe without a usable [api] endpoint", probed)
			case tc.wantAddr != "" && (len(probed) != 1 || probed[0] != tc.wantAddr):
				t.Fatalf("listener probed at %v, want exactly [%s]", probed, tc.wantAddr)
			}
		})
	}
}

// TestStandaloneAPIListenerAccepts covers the real probe: true for a loopback
// port with a listener, false for one nothing listens on.
func TestStandaloneAPIListenerAccepts(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening on loopback: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	if !standaloneAPIListenerAccepts(ln.Addr().String()) {
		t.Fatalf("standaloneAPIListenerAccepts(%s) = false, want true for a bound listener", ln.Addr())
	}
	unbound := net.JoinHostPort("127.0.0.1", strconv.Itoa(unboundLoopbackPort(t)))
	if standaloneAPIListenerAccepts(unbound) {
		t.Fatalf("standaloneAPIListenerAccepts(%s) = true, want false with nothing listening", unbound)
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
	// The rows naming [api] port 8080 are about routing, not the listener.
	stubStandaloneListener(t)

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
		// client — among them gc mail check, which would otherwise fail.
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
			t.Fatalf("mailCheckAPIClient = (%p, %q), want (nil, \"grant-required\") so gc mail check reads locally", c, reason)
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
	const (
		readOnlyBind = "name = \"t\"\n[api]\nport = 9443\nbind = \"0.0.0.0\"\n"
		noAPIPort    = "name = \"t\"\n"
	)
	cases := []struct {
		name string
		// down reports no live controller socket.
		down   bool
		mode   controllerHostingMode
		grants bool
		toml   string
		want   string
	}{
		{name: "standalone-read-only-bind", mode: controllerHostingStandalone, toml: readOnlyBind, want: "non-loopback-bind"},
		{name: "unknown-read-only-bind", mode: controllerHostingUnknown, toml: readOnlyBind, want: "non-loopback-bind"},
		{name: "supervisor-hosted-ignores-api-bind", mode: controllerHostingSupervisor, toml: readOnlyBind, want: "controller-down"},
		{name: "supervisor-hosted-requiring-grants", mode: controllerHostingSupervisor, grants: true, toml: readOnlyBind, want: "grant-required"},
		{name: "standalone-no-api-port", mode: controllerHostingStandalone, toml: noAPIPort, want: "controller-down"},
		{name: "unknown-no-api-port", mode: controllerHostingUnknown, toml: noAPIPort, want: "controller-down"},
		{name: "no-controller", down: true, toml: apiPortCityTOML(9443), want: "controller-down"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_NO_API", "")
			apiRouteControllerIdentityHook = func(string) controllerIdentityReply {
				if tc.down {
					return controllerIdentityReply{}
				}
				return controllerIdentityReply{PID: 4242, HostingMode: tc.mode}
			}
			apiRouteSupervisorGrantsHook = func() bool { return tc.grants }
			dir := writeCityTOMLForRoute(t, t.TempDir(), tc.toml)
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

// TestSessionListAPIClientRouting covers the client gc session list resolves:
// it routes as apiClient does (#8453). A supervisor-hosted city lists through
// the supervisor API, with or without an [api] port; a supervisor that
// requires grants, or a supervisor client that cannot be resolved, sends the
// listing local. A state filter that can keep a closed session lists locally
// wherever the API would have served it, since the API reads the whole
// session history again for every page; where no API serves it, the reason a
// nil client gives stands. The supervisor client is never resolved under a
// grant requirement or the GC_NO_API escape hatch.
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
	// The rows naming [api] port 9443 are about routing, not the listener.
	stubStandaloneListener(t)
	cases := []struct {
		name string
		// state is the --state filter.
		state string
		// escapeHatch sets GC_NO_API=1, under which nothing may be probed.
		escapeHatch bool
		pid         int
		mode        controllerHostingMode
		// supervisor is whether the supervisor client may be resolved; it is
		// the sentinel when it may.
		supervisor bool
		// supervisorNil makes the supervisor client hook return nil: no
		// supervisor-managed client is available.
		supervisorNil bool
		grants        bool
		toml          string
		want          string // "supervisor", "standalone" or "" (nil)
		wantReason    string
	}{
		{name: "supervisor-hosted-uses-supervisor", pid: 4242, mode: controllerHostingSupervisor, supervisor: true, toml: noAPIPort, want: "supervisor"},
		{name: "supervisor-hosted-with-api-port-uses-supervisor", pid: 4242, mode: controllerHostingSupervisor, supervisor: true, toml: apiPort, want: "supervisor"},
		{name: "supervisor-hosted-requiring-grants-lists-locally", pid: 4242, mode: controllerHostingSupervisor, grants: true, toml: noAPIPort, wantReason: "grant-required"},
		{name: "supervisor-hosted-supervisor-unavailable-lists-locally", pid: 4242, mode: controllerHostingSupervisor, supervisor: true, supervisorNil: true, toml: noAPIPort, wantReason: "controller-down"},
		{name: "standalone-with-api-port-uses-standalone", pid: 4242, mode: controllerHostingStandalone, toml: apiPort, want: "standalone"},
		{name: "unknown-with-api-port-uses-standalone", pid: 4242, mode: controllerHostingUnknown, toml: apiPort, want: "standalone"},
		{name: "standalone-no-api-port-lists-locally", pid: 4242, mode: controllerHostingStandalone, toml: noAPIPort, wantReason: "controller-down"},
		{name: "controller-down-uses-supervisor", supervisor: true, toml: apiPort, want: "supervisor"},
		{name: "escape-hatch-lists-locally", escapeHatch: true, pid: 4242, mode: controllerHostingSupervisor, toml: noAPIPort, wantReason: "escape-hatch"},
		{name: "supervisor-hosted-all-lists-locally", state: "all", pid: 4242, mode: controllerHostingSupervisor, supervisor: true, toml: noAPIPort, wantReason: sessionListClosedHistoryReason},
		{name: "supervisor-hosted-closed-lists-locally", state: "closed", pid: 4242, mode: controllerHostingSupervisor, supervisor: true, toml: noAPIPort, wantReason: sessionListClosedHistoryReason},
		{name: "supervisor-hosted-active-or-closed-lists-locally", state: "active,closed", pid: 4242, mode: controllerHostingSupervisor, supervisor: true, toml: noAPIPort, wantReason: sessionListClosedHistoryReason},
		{name: "supervisor-hosted-active-uses-supervisor", state: "active", pid: 4242, mode: controllerHostingSupervisor, supervisor: true, toml: noAPIPort, want: "supervisor"},
		{name: "standalone-with-api-port-all-lists-locally", state: "all", pid: 4242, mode: controllerHostingStandalone, toml: apiPort, wantReason: sessionListClosedHistoryReason},
		{name: "controller-down-closed-lists-locally", state: "closed", supervisor: true, toml: apiPort, wantReason: sessionListClosedHistoryReason},
		{name: "supervisor-hosted-requiring-grants-all-keeps-grant-required", state: "all", pid: 4242, mode: controllerHostingSupervisor, grants: true, toml: noAPIPort, wantReason: "grant-required"},
		{name: "escape-hatch-all-keeps-escape-hatch", state: "all", escapeHatch: true, pid: 4242, mode: controllerHostingSupervisor, toml: noAPIPort, wantReason: "escape-hatch"},
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
				if tc.supervisorNil {
					return nil
				}
				return sentinel
			}
			apiRouteSupervisorGrantsHook = func() bool { return tc.grants }
			dir := writeCityTOMLForRoute(t, t.TempDir(), tc.toml)
			c, reason := sessionListAPIClient(dir, tc.state)
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

// TestStandaloneAPIClientNeverResolvesTheSupervisor pins standaloneAPIClient:
// a standalone controller's [api] endpoint, and nil wherever apiClient would
// hand back the supervisor-managed client, without resolving it.
func TestStandaloneAPIClientNeverResolvesTheSupervisor(t *testing.T) {
	origIdentity, origSup := apiRouteControllerIdentityHook, apiRouteSupervisorClientHook
	t.Cleanup(func() { apiRouteControllerIdentityHook, apiRouteSupervisorClientHook = origIdentity, origSup })
	stubStandaloneListener(t)
	sentinel := api.NewClient("http://supervisor.sentinel:1")
	for _, tc := range []struct {
		name     string
		identity controllerIdentityReply
		cityTOML string
		want     string // "nil" or "standalone"
	}{
		{"supervisor-hosted", controllerIdentityReply{PID: 4242, HostingMode: controllerHostingSupervisor}, "name = \"t\"\n", "nil"},
		{"supervisor-hosted with a stale api port", controllerIdentityReply{PID: 4242, HostingMode: controllerHostingSupervisor}, "name = \"t\"\n[api]\nport = 8080\n", "nil"},
		{"no live controller", controllerIdentityReply{}, "name = \"t\"\n[api]\nport = 8080\n", "nil"},
		{"controller predating identify, no api port", controllerIdentityReply{PID: 4242}, "name = \"t\"\n", "nil"},
		{"standalone with an api port", controllerIdentityReply{PID: 4242, HostingMode: controllerHostingStandalone}, "name = \"t\"\n[api]\nport = 8080\n", "standalone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_NO_API", "")
			identity := tc.identity
			apiRouteControllerIdentityHook = func(string) controllerIdentityReply { return identity }
			apiRouteSupervisorClientHook = func(string) *api.Client {
				t.Errorf("standaloneAPIClient resolved the supervisor-managed client")
				return sentinel
			}
			got := standaloneAPIClient(writeCityTOMLForRoute(t, t.TempDir(), tc.cityTOML))
			switch {
			case tc.want == "nil" && got != nil:
				t.Fatalf("standaloneAPIClient = %p, want nil", got)
			case tc.want == "standalone" && (got == nil || got == sentinel):
				t.Fatalf("standaloneAPIClient = %p, want the standalone controller's client", got)
			}
		})
	}
}

// TestMailCheckInjectProbesOnlyAStandaloneController pins the client each
// form of `gc mail check` routes through in a supervisor-hosted city. The
// per-turn --inject hook reads the mailbox in process either way, so it does
// not probe the supervisor first: that probe is a full inbox listing on every
// agent prompt. A plain check still asks the supervisor.
func TestMailCheckInjectProbesOnlyAStandaloneController(t *testing.T) {
	origIdentity, origSup, origGrants := apiRouteControllerIdentityHook, apiRouteSupervisorClientHook, apiRouteSupervisorGrantsHook
	t.Cleanup(func() {
		apiRouteControllerIdentityHook, apiRouteSupervisorClientHook, apiRouteSupervisorGrantsHook = origIdentity, origSup, origGrants
	})
	t.Setenv("GC_NO_API", "")
	sentinel := api.NewClient("http://supervisor.sentinel:1")
	apiRouteControllerIdentityHook = func(string) controllerIdentityReply {
		return controllerIdentityReply{PID: 4242, HostingMode: controllerHostingSupervisor}
	}
	apiRouteSupervisorGrantsHook = func() bool { return false }
	dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n")

	apiRouteSupervisorClientHook = func(string) *api.Client {
		t.Errorf("gc mail check --inject resolved the supervisor-managed client")
		return sentinel
	}
	if c, _ := mailCheckClient(dir, true); c != nil {
		t.Fatalf("mailCheckClient(inject) = %p, want nil: the hook reads the mailbox in process", c)
	}

	apiRouteSupervisorClientHook = func(string) *api.Client { return sentinel }
	if c, reason := mailCheckClient(dir, false); c != sentinel {
		t.Fatalf("mailCheckClient(plain) = (%p, %q), want the supervisor-managed client", c, reason)
	}
}

// TestSupervisorFallthroughUnknownHostingUnboundAPIPortReachesSupervisor pins
// #5761 for the supervisor fall-through callers: a live controller of unknown
// hosting mode (a supervisor predating identify) whose city.toml names a
// loopback [api] port nothing listens on routes to the supervisor-managed
// client, the same answer as with no [api] port, never to the dead port.
func TestSupervisorFallthroughUnknownHostingUnboundAPIPortReachesSupervisor(t *testing.T) {
	sentinel := api.NewClient("http://supervisor.sentinel:1")
	origIdentity, origSup := apiRouteControllerIdentityHook, apiRouteSupervisorClientHook
	origGrants, origWriteGrants := apiRouteSupervisorGrantsHook, apiRouteSupervisorWriteGrantsHook
	t.Cleanup(func() {
		apiRouteControllerIdentityHook, apiRouteSupervisorClientHook = origIdentity, origSup
		apiRouteSupervisorGrantsHook, apiRouteSupervisorWriteGrantsHook = origGrants, origWriteGrants
	})
	t.Setenv("GC_NO_API", "")
	apiRouteControllerIdentityHook = func(string) controllerIdentityReply { return controllerIdentityReply{PID: 4242} }
	apiRouteSupervisorClientHook = func(string) *api.Client { return sentinel }
	apiRouteSupervisorGrantsHook = func() bool {
		t.Errorf("supervisor grant requirement consulted for a controller of unknown hosting mode")
		return true
	}
	apiRouteSupervisorWriteGrantsHook = func() bool {
		t.Errorf("supervisor write-grant requirement consulted for a controller of unknown hosting mode")
		return true
	}
	dir := writeCityTOMLForRoute(t, t.TempDir(), apiPortCityTOML(unboundLoopbackPort(t)))

	for _, tc := range []struct {
		name    string
		resolve func(string) (*api.Client, string)
	}{
		{"supervisorFallthroughAPIClient", supervisorFallthroughAPIClient},
		{"supervisorFallthroughMutationAPIClient", supervisorFallthroughMutationAPIClient},
		{"maintenanceAPIClient", maintenanceAPIClient},
		{"maintenanceDoltGCAPIClient", maintenanceDoltGCAPIClient},
	} {
		c, reason := tc.resolve(dir)
		if c != sentinel || reason != "" {
			t.Errorf("%s = (%p, %q), want (supervisor sentinel %p, \"\"): nothing listens on the [api] port", tc.name, c, reason, sentinel)
		}
	}
}

// hostingModeName names mode for a subtest; the unknown mode is "".
func hostingModeName(mode controllerHostingMode) string {
	if mode == controllerHostingUnknown {
		return "unknown"
	}
	return string(mode)
}

// TestAPIClientUnboundAPIPortTakesLocalFallback pins #5761 for apiClient: a
// live controller that identifies as standalone, or one of unknown hosting
// mode, whose city.toml names a loopback [api] port nothing listens on gets
// nil (the caller's local fallback), the same answer as with no [api] port,
// without resolving the supervisor-managed client.
func TestAPIClientUnboundAPIPortTakesLocalFallback(t *testing.T) {
	origIdentity, origSup := apiRouteControllerIdentityHook, apiRouteSupervisorClientHook
	t.Cleanup(func() { apiRouteControllerIdentityHook, apiRouteSupervisorClientHook = origIdentity, origSup })
	for _, mode := range []controllerHostingMode{controllerHostingStandalone, controllerHostingUnknown} {
		t.Run(hostingModeName(mode), func(t *testing.T) {
			t.Setenv("GC_NO_API", "")
			apiRouteControllerIdentityHook = func(string) controllerIdentityReply {
				return controllerIdentityReply{PID: 4242, HostingMode: mode}
			}
			apiRouteSupervisorClientHook = func(string) *api.Client {
				t.Errorf("supervisor client resolved for apiClient")
				return api.NewClient("http://supervisor.sentinel:1")
			}
			dir := writeCityTOMLForRoute(t, t.TempDir(), apiPortCityTOML(unboundLoopbackPort(t)))
			if c := apiClient(dir); c != nil {
				t.Fatalf("apiClient = %p, want nil: nothing listens on the [api] port", c)
			}
		})
	}
}

// TestAPIClientFallbackReasonUnboundAPIPort pins the reason a nil client
// reports when a live non-supervisor-hosted controller's city.toml names a
// usable [api] port nothing listens on: "api-port-unbound", for apiClient and
// for every route a controller that identifies as standalone resolves nil on.
func TestAPIClientFallbackReasonUnboundAPIPort(t *testing.T) {
	origIdentity := apiRouteControllerIdentityHook
	t.Cleanup(func() { apiRouteControllerIdentityHook = origIdentity })
	for _, mode := range []controllerHostingMode{controllerHostingStandalone, controllerHostingUnknown} {
		t.Run(hostingModeName(mode), func(t *testing.T) {
			t.Setenv("GC_NO_API", "")
			apiRouteControllerIdentityHook = func(string) controllerIdentityReply {
				return controllerIdentityReply{PID: 4242, HostingMode: mode}
			}
			dir := writeCityTOMLForRoute(t, t.TempDir(), apiPortCityTOML(unboundLoopbackPort(t)))
			if got := apiClientFallbackReason(dir); got != "api-port-unbound" {
				t.Fatalf("apiClientFallbackReason = %q, want \"api-port-unbound\"", got)
			}
			if mode != controllerHostingStandalone {
				return
			}
			for _, route := range []apiRoute{apiRoutePreferSupervisor, apiRoutePreferSupervisorMutation, apiRouteStandaloneOnly} {
				if got := routeFallbackReason(dir, route); got != "api-port-unbound" {
					t.Fatalf("routeFallbackReason(route %d) = %q, want \"api-port-unbound\"", route, got)
				}
			}
		})
	}
}
