package config

import (
	"testing"
	"time"
)

func TestParseMaintenanceDoltFullSection(t *testing.T) {
	data := `
[maintenance.dolt]
enabled = true
interval = "168h"
alert_to = "gascity/mayor"
gc_timeout = "10m"
min_store_mb = 2048
`
	cfg, err := Parse([]byte(data))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if !cfg.Maintenance.Dolt.Enabled {
		t.Errorf("expected Enabled=true, got false")
	}
	if cfg.Maintenance.Dolt.Interval != "168h" {
		t.Errorf("expected Interval=168h, got %q", cfg.Maintenance.Dolt.Interval)
	}
	if cfg.Maintenance.Dolt.AlertTo != "gascity/mayor" {
		t.Errorf("expected AlertTo=gascity/mayor, got %q", cfg.Maintenance.Dolt.AlertTo)
	}
	if cfg.Maintenance.Dolt.GCTimeout != "10m" {
		t.Errorf("expected GCTimeout=10m, got %q", cfg.Maintenance.Dolt.GCTimeout)
	}
	if cfg.Maintenance.Dolt.MinStoreMB == nil || *cfg.Maintenance.Dolt.MinStoreMB != 2048 {
		t.Errorf("expected MinStoreMB=2048, got %v", cfg.Maintenance.Dolt.MinStoreMB)
	}
}

func TestParseMaintenanceDoltOmittedSection(t *testing.T) {
	data := `
[workspace]
name = "test"
`
	cfg, err := Parse([]byte(data))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if cfg.Maintenance.Dolt.Enabled {
		t.Errorf("expected Enabled=false (zero value), got true")
	}
	if cfg.Maintenance.Dolt.Interval != "" {
		t.Errorf("expected Interval empty, got %q", cfg.Maintenance.Dolt.Interval)
	}
	if cfg.Maintenance.Dolt.AlertTo != "" {
		t.Errorf("expected AlertTo empty, got %q", cfg.Maintenance.Dolt.AlertTo)
	}
	if cfg.Maintenance.Dolt.GCTimeout != "" {
		t.Errorf("expected GCTimeout empty, got %q", cfg.Maintenance.Dolt.GCTimeout)
	}
	if cfg.Maintenance.Dolt.MinStoreMB != nil {
		t.Errorf("expected MinStoreMB nil (zero value), got %v", *cfg.Maintenance.Dolt.MinStoreMB)
	}
}

func TestParseMaintenanceDoltPartialSection(t *testing.T) {
	data := `
[maintenance.dolt]
enabled = true
`
	cfg, err := Parse([]byte(data))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if !cfg.Maintenance.Dolt.Enabled {
		t.Errorf("expected Enabled=true, got false")
	}
	if cfg.Maintenance.Dolt.Interval != "" {
		t.Errorf("expected Interval empty (no default applied at parse time), got %q", cfg.Maintenance.Dolt.Interval)
	}
}

func TestDoltMaintenanceIntervalOrDefault(t *testing.T) {
	cases := []struct {
		name     string
		interval string
		want     time.Duration
	}{
		{"empty uses default", "", 168 * time.Hour},
		{"explicit weekly", "168h", 168 * time.Hour},
		{"explicit fortnightly", "336h", 336 * time.Hour},
		{"invalid falls back to default", "5mins", 168 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := DoltMaintenance{Interval: tc.interval}
			if got := d.IntervalOrDefault(); got != tc.want {
				t.Errorf("IntervalOrDefault() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDoltMaintenanceGCTimeoutOrDefault(t *testing.T) {
	cases := []struct {
		name    string
		timeout string
		want    time.Duration
	}{
		{"empty uses default", "", 10 * time.Minute},
		{"explicit 5m", "5m", 5 * time.Minute},
		{"explicit 30m", "30m", 30 * time.Minute},
		{"invalid falls back to default", "10mins", 10 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := DoltMaintenance{GCTimeout: tc.timeout}
			if got := d.GCTimeoutOrDefault(); got != tc.want {
				t.Errorf("GCTimeoutOrDefault() = %v, want %v", got, tc.want)
			}
		})
	}
}

func intPtr(v int) *int { return &v }

func TestDoltMaintenanceMinStoreBytesOrDefault(t *testing.T) {
	const mib = int64(1) << 20
	cases := []struct {
		name string
		mb   *int
		want int64
	}{
		{"nil uses ~1GiB default", nil, 1024 * mib},
		{"explicit zero disables gate", intPtr(0), 0},
		{"explicit negative disables gate", intPtr(-5), 0},
		{"explicit 512 MiB", intPtr(512), 512 * mib},
		{"explicit 4096 MiB", intPtr(4096), 4096 * mib},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := DoltMaintenance{MinStoreMB: tc.mb}
			if got := d.MinStoreBytesOrDefault(); got != tc.want {
				t.Errorf("MinStoreBytesOrDefault() = %d, want %d", got, tc.want)
			}
		})
	}
}
