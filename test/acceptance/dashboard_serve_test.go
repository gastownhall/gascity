//go:build acceptance_a

// Dashboard acceptance tests.
//
// The dashboard is no longer a standalone static server. The compiled SPA is
// embedded in the gc binary and served same-origin by the supervisor, so
// `gc dashboard` is now an informational command that resolves and prints the
// supervisor URL (or tells the user how to start the supervisor). These tests
// assert that shim contract:
//
//   - `gc dashboard` exits 0 and prints where the dashboard is served.
//   - When the supervisor is running, the printed notice carries the
//     supervisor's URL.
//
// The behavioral tests that previously asserted on the static server's wire
// surface (served /dashboard.js, injected supervisor-url meta tag, 404 on the
// legacy /api/* proxy) are gone with that server; same-origin SPA rendering is
// covered by the browser-level suite against the live supervisor.
package acceptance_test

import (
	"os"
	"strings"
	"testing"
	"time"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

func TestDashboard_PrintsSupervisorNotice(t *testing.T) {
	c := newShortDashboardCity(t)
	startCityUnderSupervisor(t, c)

	// --no-open keeps this out-of-process command from launching a real
	// browser in CI; the served notice still names the supervisor as the
	// dashboard host and carries its resolved URL. (The default browser-open
	// path is covered in-process by cmd/gc/cmd_dashboard_test.go, which can
	// stub the launch hook.)
	out, err := c.GC("dashboard", "--no-open", "--city", c.Dir)
	if err != nil {
		t.Fatalf("gc dashboard failed: %v\n%s", err, out)
	}

	// The notice always names the supervisor as the dashboard host and,
	// because the city is running under the supervisor, carries an http
	// URL pointing at it.
	if !strings.Contains(out, "supervisor") {
		t.Fatalf("gc dashboard output did not mention the supervisor:\n%s", out)
	}
	if !strings.Contains(out, "http://") {
		t.Fatalf("gc dashboard output did not include a resolved supervisor URL:\n%s", out)
	}
}

func newShortDashboardCity(t *testing.T) *helpers.City {
	t.Helper()

	shortRoot, err := os.MkdirTemp("", "gca-dashboard-*")
	if err != nil {
		t.Fatalf("creating short city root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(shortRoot) })

	c := helpers.NewCityInRoot(t, testEnv, shortRoot)
	c.InitNoStart("claude")
	return c
}

func startCityUnderSupervisor(t *testing.T, c *helpers.City) string {
	t.Helper()

	stopOut, stopErr := c.GC("stop", c.Dir)
	if stopErr != nil {
		t.Fatalf("gc stop before supervisor handoff failed: %v\n%s", stopErr, stopOut)
	}

	if !c.WaitForCondition(func() bool {
		out, err := c.GC("status", c.Dir)
		if err != nil {
			return false
		}
		return !strings.Contains(out, "Controller: standalone-managed")
	}, 20*time.Second) {
		out, err := c.GC("status", c.Dir)
		t.Fatalf("standalone controller did not stop before supervisor handoff: %v\n%s", err, out)
	}

	startOut, startErr := c.GC("start", c.Dir)
	if startErr != nil {
		t.Fatalf("gc start under supervisor failed: %v\n%s", startErr, startOut)
	}
	return startOut
}
