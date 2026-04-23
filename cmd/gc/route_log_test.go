package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestGCNoAPIEnvGate(t *testing.T) {
	cases := []struct {
		name        string
		value       string
		wantDisable bool
		wantWarn    bool
	}{
		{"empty", "", false, false},
		{"truthy-1", "1", true, false},
		{"truthy-true", "true", true, false},
		{"truthy-TRUE", "TRUE", true, false},
		{"truthy-yes", "yes", true, false},
		{"truthy-YES", "YES", true, false},
		{"truthy-padded", "  true  ", true, false},
		{"falsy-0", "0", false, false},
		{"falsy-false", "false", false, false},
		{"falsy-no", "no", false, false},
		{"unknown-garble", "please-disable", false, true},
		{"unknown-maybe", "maybe", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotDisable, gotWarn := classifyGCNoAPI(tc.value)
			if gotDisable != tc.wantDisable {
				t.Errorf("classifyGCNoAPI(%q) disabled = %v, want %v", tc.value, gotDisable, tc.wantDisable)
			}
			if (gotWarn != "") != tc.wantWarn {
				t.Errorf("classifyGCNoAPI(%q) warn = %q, wantWarn = %v", tc.value, gotWarn, tc.wantWarn)
			}
		})
	}
}

func TestLogRouteToFormat(t *testing.T) {
	// logRouteTo always writes (no env gating). Format: cmd=... route=...
	// [reason=...] [key=value ...].
	t.Run("happy-path-kv-extras", func(t *testing.T) {
		var buf bytes.Buffer
		logRouteTo(&buf, "session list", "fallback", "cache-not-live", "bead_count", "42")
		line := buf.String()
		for _, want := range []string{
			"cmd=session list",
			"route=fallback",
			"reason=cache-not-live",
			"bead_count=42",
		} {
			if !strings.Contains(line, want) {
				t.Errorf("logRouteTo output missing %q\ngot: %q", want, line)
			}
		}
		if strings.Count(line, "\n") != 1 {
			t.Errorf("logRouteTo emitted %d newlines, want 1; got=%q", strings.Count(line, "\n"), line)
		}
	})
	t.Run("empty-reason-omitted", func(t *testing.T) {
		var buf bytes.Buffer
		logRouteTo(&buf, "bead list", "api", "")
		if strings.Contains(buf.String(), "reason=") {
			t.Errorf("empty reason should be omitted; got %q", buf.String())
		}
	})
	t.Run("unpaired-extras-trail-verbatim", func(t *testing.T) {
		var buf bytes.Buffer
		logRouteTo(&buf, "bead list", "api", "", "x", "1", "trailing")
		line := buf.String()
		if !strings.Contains(line, "x=1") {
			t.Errorf("expected x=1 in output, got %q", line)
		}
		if !strings.Contains(line, "trailing") {
			t.Errorf("expected trailing token in output, got %q", line)
		}
	})
}

func TestAPIClientFallbackReason_EscapeHatch(t *testing.T) {
	// GC_NO_API truthy → escape-hatch reason regardless of controller state.
	t.Setenv("GC_NO_API", "1")
	if got := apiClientFallbackReason(t.TempDir()); got != "escape-hatch" {
		t.Errorf("got %q, want escape-hatch", got)
	}
}

func TestAPIClientFallbackReason_ControllerDown(t *testing.T) {
	// No controller → controller-down reason.
	t.Setenv("GC_NO_API", "")
	if got := apiClientFallbackReason(t.TempDir()); got != "controller-down" {
		t.Errorf("got %q, want controller-down", got)
	}
}

func TestRouteLogEnabled(t *testing.T) {
	cases := []struct {
		value string
		want  bool
	}{
		{"", false},
		{"0", false},
		{"false", false},
		{"no", false},
		{"1", true},
		{"true", true},
		{"yes", true},
		{"YES", true},
		{"  true  ", true},
		{"maybe", false},
	}
	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("GC_DEBUG", tc.value)
			if got := routeLogEnabled(); got != tc.want {
				t.Errorf("routeLogEnabled() with GC_DEBUG=%q = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}
