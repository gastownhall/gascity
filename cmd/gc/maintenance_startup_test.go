package main

import (
	"strings"
	"testing"
	"time"
)

// TestMaintenanceStartupLine verifies the always-on startup banner reports
// the interval and distinguishes the active (GC wired) and observe-only
// (no-op) modes, so operators can confirm the loop initialized from the
// supervisor log. (gascity ga-tp7)
func TestMaintenanceStartupLine(t *testing.T) {
	t.Run("observe-only-when-not-active", func(t *testing.T) {
		got := maintenanceStartupLine(168*time.Hour, false)
		for _, want := range []string{"store-maintenance: loop started", "interval=168h", "observe-only"} {
			if !strings.Contains(got, want) {
				t.Errorf("startup line missing %q\ngot: %q", want, got)
			}
		}
		if strings.Contains(got, "mode=active") {
			t.Errorf("observe-only line must not claim active mode; got: %q", got)
		}
	})

	t.Run("active-when-wired", func(t *testing.T) {
		got := maintenanceStartupLine(24*time.Hour, true)
		for _, want := range []string{"store-maintenance: loop started", "interval=24h", "mode=active"} {
			if !strings.Contains(got, want) {
				t.Errorf("startup line missing %q\ngot: %q", want, got)
			}
		}
		if strings.Contains(got, "observe-only") {
			t.Errorf("active line must not claim observe-only; got: %q", got)
		}
	})
}
