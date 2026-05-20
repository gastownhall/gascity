package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/logutil"
)

func TestStartOutputProxyDedupsWarningsAndDefersFatal(t *testing.T) {
	var stderr bytes.Buffer
	proxy := newStartOutputProxy(&stderr, startOutputProxyOptions{})

	writeStartOutputTestLine(t, proxy, "warning: deprecated order path /tmp/orders/digest.order.toml; rename to orders/digest.toml")
	writeStartOutputTestLine(t, proxy, "gc start: waiting for supervisor")
	writeStartOutputTestLine(t, proxy, "warning: deprecated order path /tmp/orders/digest.order.toml; rename to orders/digest.toml")
	writeStartOutputTestLine(t, proxy, "gc-fatal: agent \"worker\": pack v1/v2 layout collision")

	if err := proxy.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	out := stderr.String()
	if got := strings.Count(out, "deprecated order path"); got != 1 {
		t.Fatalf("deprecated warning count = %d, want 1; output:\n%s", got, out)
	}
	if !strings.Contains(out, "(suppressed 1 more)") {
		t.Fatalf("output missing suppression suffix:\n%s", out)
	}
	lines := nonEmptyLines(out)
	want := `gc-fatal: agent "worker": pack v1/v2 layout collision see: ` + logutil.WalkthroughURL["duplicate_name_v1v2"]
	if got := lines[len(lines)-1]; got != want {
		t.Fatalf("last line = %q, want %q", got, want)
	}
	if got, want := proxy.WarningCount(), 2; got != want {
		t.Fatalf("WarningCount() = %d, want %d", got, want)
	}
	if got, want := proxy.FatalCause(), "pack-v1-v2-collision"; got != want {
		t.Fatalf("FatalCause() = %q, want %q", got, want)
	}
}

func TestStartOutputProxyVerboseDoesNotDedupWarnings(t *testing.T) {
	var stderr bytes.Buffer
	proxy := newStartOutputProxy(&stderr, startOutputProxyOptions{Verbose: true})

	writeStartOutputTestLine(t, proxy, "warning: duplicated")
	writeStartOutputTestLine(t, proxy, "warning: duplicated")

	if err := proxy.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := strings.Count(stderr.String(), "warning: duplicated"); got != 2 {
		t.Fatalf("warning count = %d, want 2; output:\n%s", got, stderr.String())
	}
	if strings.Contains(stderr.String(), "suppressed") {
		t.Fatalf("verbose output should not include suppression suffix:\n%s", stderr.String())
	}
}

func TestStartSummaryLineHasStableKeys(t *testing.T) {
	got := startSummaryLine(startSummary{
		PID:      42,
		Binary:   "/tmp/gc",
		Build:    "abcdef123456",
		Drift:    "unknown",
		Warnings: 3,
		Fatal:    "startup-failed",
	})
	want := "gc-start: pid=42 binary=/tmp/gc build=abcdef123456 drift=unknown warnings=3 fatal=startup-failed"
	if got != want {
		t.Fatalf("startSummaryLine() = %q, want %q", got, want)
	}
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, line)
	}
	return out
}

func writeStartOutputTestLine(t *testing.T, proxy *startOutputProxy, line string) {
	t.Helper()
	if _, err := fmt.Fprintln(proxy, line); err != nil {
		t.Fatalf("writing proxy line: %v", err)
	}
}
