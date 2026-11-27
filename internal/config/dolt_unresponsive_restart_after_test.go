package config

import (
	"strings"
	"testing"
	"time"
)

func TestDoltUnresponsiveRestartAfterDuration(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw     string
		want    time.Duration
		wantErr string
	}{
		{name: "absent selects the default", raw: "", want: DefaultDoltUnresponsiveRestartAfter},
		{name: "valid duration selects that window", raw: "45s", want: 45 * time.Second},
		{name: "zero disables the listening-server delay", raw: "0s", want: 0},
		{name: "malformed is an error", raw: "soon", wantErr: "dolt_unresponsive_restart_after"},
		{name: "negative is an error", raw: "-5s", wantErr: "must not be negative"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := DaemonConfig{DoltUnresponsiveRestartAfter: tc.raw}
			got, err := d.DoltUnresponsiveRestartAfterDuration()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("DoltUnresponsiveRestartAfterDuration(%q) error = %v, want %q", tc.raw, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("DoltUnresponsiveRestartAfterDuration(%q): %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("DoltUnresponsiveRestartAfterDuration(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
	if DefaultDoltUnresponsiveRestartAfter != 180*time.Second {
		t.Fatalf("DefaultDoltUnresponsiveRestartAfter = %v, want 180s", DefaultDoltUnresponsiveRestartAfter)
	}
}

func TestValidateDoltUnresponsiveRestartAfter(t *testing.T) {
	cfg := &City{Daemon: DaemonConfig{DoltUnresponsiveRestartAfter: "-1s"}}
	err := ValidateNonNegativeDurations(cfg, "city.toml")
	if err == nil || !strings.Contains(err.Error(), "dolt_unresponsive_restart_after") {
		t.Fatalf("ValidateNonNegativeDurations error = %v, want dolt_unresponsive_restart_after rejection", err)
	}
	cfg.Daemon.DoltUnresponsiveRestartAfter = "eventually"
	warnings := ValidateDurations(cfg, "city.toml")
	if !strings.Contains(strings.Join(warnings, "\n"), "dolt_unresponsive_restart_after") {
		t.Fatalf("ValidateDurations warnings = %v, want dolt_unresponsive_restart_after warning", warnings)
	}
}
