package config

import (
	"strings"
	"testing"
	"time"
)

func TestACPSessionConfigStopGraceParsesFromTOML(t *testing.T) {
	cfg, err := Parse([]byte(`
[workspace]
name = "test"

[session.acp]
stop_grace = "30s"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Session.ACP.StopGrace != "30s" {
		t.Fatalf("StopGrace = %q, want 30s", cfg.Session.ACP.StopGrace)
	}
	if got := cfg.Session.ACP.StopGraceDuration(); got != 30*time.Second {
		t.Fatalf("StopGraceDuration() = %v, want 30s", got)
	}
}

func TestACPSessionConfigStopGraceDuration(t *testing.T) {
	cases := []struct {
		raw  string
		want time.Duration
	}{
		{"", DefaultACPStopGrace},
		{"250ms", 250 * time.Millisecond},
		{"2m", 2 * time.Minute},
		{"0s", DefaultACPStopGrace},
		{"-1s", DefaultACPStopGrace},
		{"soon", DefaultACPStopGrace},
	}
	for _, c := range cases {
		a := ACPSessionConfig{StopGrace: c.raw}
		if got := a.StopGraceDuration(); got != c.want {
			t.Errorf("StopGraceDuration(%q) = %v, want %v", c.raw, got, c.want)
		}
	}
	if DefaultACPStopGrace != 5*time.Second {
		t.Errorf("DefaultACPStopGrace = %v, want 5s", DefaultACPStopGrace)
	}
}

func TestValidateDurationsWarnsOnNonPositiveACPStopGrace(t *testing.T) {
	for _, raw := range []string{"0s", "-5s", "soon"} {
		cfg := &City{Session: SessionConfig{ACP: ACPSessionConfig{StopGrace: raw}}}
		warnings := ValidateDurations(cfg, "city.toml")
		if len(warnings) != 1 {
			t.Fatalf("stop_grace=%q: expected 1 warning, got %d: %v", raw, len(warnings), warnings)
		}
		if !strings.Contains(warnings[0], "[session.acp] stop_grace") {
			t.Errorf("stop_grace=%q: warning should name the field: %s", raw, warnings[0])
		}
	}
	cfg := &City{Session: SessionConfig{ACP: ACPSessionConfig{StopGrace: "10s"}}}
	if warnings := ValidateDurations(cfg, "city.toml"); len(warnings) != 0 {
		t.Errorf("valid stop_grace: expected no warnings, got %v", warnings)
	}
}

func TestACPSessionConfigCancelTimeoutParsesFromTOML(t *testing.T) {
	cfg, err := Parse([]byte(`
[workspace]
name = "test"

[session.acp]
cancel_timeout = "3s"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Session.ACP.CancelTimeout != "3s" {
		t.Fatalf("CancelTimeout = %q, want 3s", cfg.Session.ACP.CancelTimeout)
	}
	if got := cfg.Session.ACP.CancelTimeoutDuration(); got != 3*time.Second {
		t.Fatalf("CancelTimeoutDuration() = %v, want 3s", got)
	}
}

func TestACPSessionConfigCancelTimeoutDuration(t *testing.T) {
	cases := []struct {
		raw  string
		want time.Duration
	}{
		{"", DefaultACPCancelTimeout},
		{"300ms", 300 * time.Millisecond},
		{"1m", time.Minute},
		{"0s", DefaultACPCancelTimeout},
		{"-1s", DefaultACPCancelTimeout},
		{"later", DefaultACPCancelTimeout},
	}
	for _, c := range cases {
		a := ACPSessionConfig{CancelTimeout: c.raw}
		if got := a.CancelTimeoutDuration(); got != c.want {
			t.Errorf("CancelTimeoutDuration(%q) = %v, want %v", c.raw, got, c.want)
		}
	}
	if DefaultACPCancelTimeout != 10*time.Second {
		t.Errorf("DefaultACPCancelTimeout = %v, want 10s", DefaultACPCancelTimeout)
	}
}

func TestValidateDurationsWarnsOnNonPositiveACPCancelTimeout(t *testing.T) {
	for _, raw := range []string{"0s", "-5s", "later"} {
		cfg := &City{Session: SessionConfig{ACP: ACPSessionConfig{CancelTimeout: raw}}}
		warnings := ValidateDurations(cfg, "city.toml")
		if len(warnings) != 1 {
			t.Fatalf("cancel_timeout=%q: expected 1 warning, got %d: %v", raw, len(warnings), warnings)
		}
		if !strings.Contains(warnings[0], "[session.acp] cancel_timeout") {
			t.Errorf("cancel_timeout=%q: warning should name the field: %s", raw, warnings[0])
		}
	}
	cfg := &City{Session: SessionConfig{ACP: ACPSessionConfig{CancelTimeout: "10s"}}}
	if warnings := ValidateDurations(cfg, "city.toml"); len(warnings) != 0 {
		t.Errorf("valid cancel_timeout: expected no warnings, got %v", warnings)
	}
}
