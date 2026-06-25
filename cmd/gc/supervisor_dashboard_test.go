package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/api"
)

type fakeDashResolver struct{ cities []api.CityInfo }

func (f fakeDashResolver) ListCities() []api.CityInfo { return f.cities }
func (f fakeDashResolver) CityState(string) api.State { return nil }

func TestDashboardLoopbackBaseURL(t *testing.T) {
	cases := map[string]struct {
		bind string
		port int
		want string
	}{
		"loopback":     {"127.0.0.1", 8372, "http://127.0.0.1:8372"},
		"empty bind":   {"", 8372, "http://127.0.0.1:8372"},
		"wildcard v4":  {"0.0.0.0", 8372, "http://127.0.0.1:8372"},
		"localhost":    {"localhost", 9000, "http://127.0.0.1:9000"},
		"wildcard v6":  {"::", 8372, "http://[::1]:8372"},
		"explicit v6":  {"::1", 8372, "http://[::1]:8372"},
		"explicit lan": {"192.168.1.5", 8080, "http://192.168.1.5:8080"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := dashboardLoopbackBaseURL(tc.bind, tc.port); got != tc.want {
				t.Errorf("dashboardLoopbackBaseURL(%q,%d) = %q, want %q", tc.bind, tc.port, got, tc.want)
			}
		})
	}
}

// TestDashboardDepsWiresSupervisorBaseURL is the regression guard for the
// red-team HIGH finding: attachDashboard must give the plane a non-empty
// SupervisorBaseURL, or the host-side samplers ship permanently degraded.
func TestDashboardDepsWiresSupervisorBaseURL(t *testing.T) {
	deps := dashboardDeps(fakeDashResolver{}, false, "127.0.0.1", 8372)
	if deps.SupervisorBaseURL == "" {
		t.Fatal("dashboardDeps left SupervisorBaseURL empty; samplers would never read /v0/.../status")
	}
	if deps.SupervisorBaseURL != "http://127.0.0.1:8372" {
		t.Errorf("SupervisorBaseURL = %q, want http://127.0.0.1:8372", deps.SupervisorBaseURL)
	}
	if deps.Resolver == nil {
		t.Error("Resolver not wired")
	}
}

func TestRunCwdAllowedRootsFromEnv(t *testing.T) {
	t.Setenv("RUN_CWD_ALLOWED_ROOTS", "/srv/a:/srv/b: :relative:/srv/c")
	got := runCwdAllowedRootsFromEnv()
	want := []string{"/srv/a", "/srv/b", "/srv/c"}
	if len(got) != len(want) {
		t.Fatalf("roots = %v, want %v (relative/blank entries dropped)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("roots[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	t.Setenv("RUN_CWD_ALLOWED_ROOTS", "")
	if got := runCwdAllowedRootsFromEnv(); got != nil {
		t.Errorf("empty env should yield nil, got %v", got)
	}
}

func TestDashboardEnabledToggle(t *testing.T) {
	t.Setenv("GC_SUPERVISOR_DASHBOARD", "0")
	if dashboardEnabled() {
		t.Error("GC_SUPERVISOR_DASHBOARD=0 should disable the dashboard")
	}
	t.Setenv("GC_SUPERVISOR_DASHBOARD", "")
	if !dashboardEnabled() {
		t.Error("unset GC_SUPERVISOR_DASHBOARD should default to enabled")
	}
}
