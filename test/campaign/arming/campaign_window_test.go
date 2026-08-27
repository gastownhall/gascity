//go:build campaign

package arming

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCampaignWindowStaysArmed is the WD.15 arming driver. It runs for the whole
// campaign window against a live auto-mode city, keeps every configured template
// detail-armed, and fails if any sample interval ran unarmed — those cycles
// recorded nothing durable and must not reach the parity readout.
//
//	make test-campaign-arming GC_CAMPAIGN_CITY=/path/to/city
func TestCampaignWindowStaysArmed(t *testing.T) {
	cityPath := strings.TrimSpace(os.Getenv("GC_CAMPAIGN_CITY"))
	if cityPath == "" {
		t.Skip("GC_CAMPAIGN_CITY is unset; the arming harness needs a live campaign city")
	}
	cfg := Config{
		Binary:   envOr("GC_CAMPAIGN_BIN", "gc"),
		CityPath: cityPath,
		Window:   envDuration(t, "GC_CAMPAIGN_ARM_FOR", 30*time.Minute),
		Interval: envDuration(t, "GC_CAMPAIGN_INTERVAL", 5*time.Minute),
		Lead:     envDuration(t, "GC_CAMPAIGN_LEAD", 2*time.Minute),
	}
	window := envDuration(t, "GC_CAMPAIGN_WINDOW", 7*24*time.Hour)

	harness, err := New(cfg, nil, nil)
	if err != nil {
		t.Fatalf("building the arming harness: %v", err)
	}
	templates, err := harness.DiscoverTemplates(context.Background())
	if err != nil {
		t.Fatalf("discovering campaign templates: %v", err)
	}
	t.Logf("arming %d templates for %s in %s (sample every %s, arm for %s)",
		len(templates), window, cityPath, cfg.Interval, cfg.Window)

	report, runErr := harness.Run(context.Background(), window)
	writeCampaignReport(t, report)
	if runErr != nil {
		t.Fatalf("campaign arming run: %v (report: %+v)", runErr, report)
	}
	if !report.Armed {
		t.Fatalf("%d sample intervals ran unarmed and recorded nothing: %+v", len(report.Gaps), report.Gaps)
	}
	t.Logf("window stayed armed across %d boundaries with %d re-arms", report.Boundaries, report.Rearms)
}

func writeCampaignReport(t *testing.T, report Report) {
	t.Helper()
	dir := strings.TrimSpace(os.Getenv("GC_CAMPAIGN_REPORT_DIR"))
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Errorf("creating campaign report dir: %v", err)
		return
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Errorf("encoding campaign arming report: %v", err)
		return
	}
	path := filepath.Join(dir, "arming-report.json")
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Errorf("writing campaign arming report: %v", err)
		return
	}
	t.Logf("arming report written to %s", path)
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envDuration(t *testing.T, key string, fallback time.Duration) time.Duration {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		t.Fatalf("parsing %s=%q: %v", key, raw, err)
	}
	return value
}
