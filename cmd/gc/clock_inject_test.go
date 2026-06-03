package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestFormatClockLine(t *testing.T) {
	if _, err := time.LoadLocation("America/Los_Angeles"); err != nil {
		t.Skip("no tzdata for America/Los_Angeles")
	}
	t.Setenv("GC_OPERATOR_TZ", "America/Los_Angeles")
	now := time.Date(2026, 6, 3, 21, 23, 13, 0, time.UTC) // 2:23 PM PDT
	got := formatClockLine(now)
	for _, want := range []string{
		"Current time: ",
		"Wed 2026-06-03 2:23PM PDT",
		"2026-06-03 21:23Z UTC",
		fmt.Sprintf("(epoch %d)", now.Unix()),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("formatClockLine() = %q, missing %q", got, want)
		}
	}
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("formatClockLine should end with newline, got %q", got)
	}
}

func TestFormatClockLineInvalidTZFallsBackToLocal(t *testing.T) {
	t.Setenv("GC_OPERATOR_TZ", "Not/AZone")
	now := time.Date(2026, 6, 3, 21, 23, 13, 0, time.UTC)
	got := formatClockLine(now) // must not panic; UTC + epoch still present
	if !strings.Contains(got, "2026-06-03 21:23Z UTC") ||
		!strings.Contains(got, fmt.Sprintf("epoch %d", now.Unix())) {
		t.Errorf("fallback render wrong: %q", got)
	}
}

func TestEmitClockInjectClaude(t *testing.T) {
	t.Setenv("GC_INJECT_CLOCK", "")
	var buf bytes.Buffer
	emitClockInject("", &buf)
	if !strings.Contains(buf.String(), "Current time: ") {
		t.Errorf("emitClockInject (claude) should emit a clock line, got %q", buf.String())
	}
}

func TestEmitClockInjectDisabled(t *testing.T) {
	t.Setenv("GC_INJECT_CLOCK", "0")
	var buf bytes.Buffer
	emitClockInject("", &buf)
	if buf.Len() != 0 {
		t.Errorf("emitClockInject disabled should emit nothing, got %q", buf.String())
	}
}

func TestEmitClockInjectCodexIsJSON(t *testing.T) {
	t.Setenv("GC_INJECT_CLOCK", "")
	var buf bytes.Buffer
	emitClockInject("codex", &buf)
	s := buf.String()
	if !strings.Contains(s, "hookSpecificOutput") || !strings.Contains(s, "Current time:") {
		t.Errorf("codex format should be JSON with additionalContext, got %q", s)
	}
}
