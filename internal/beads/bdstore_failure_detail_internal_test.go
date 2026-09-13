package beads

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

const bdOtelDeprecationWarning = "warning: BD_OTEL_* environment variables are deprecated. Replace with BD_OTEL_ENABLED=true plus the standard OpenTelemetry SDK variables. Translated for this run: BD_OTEL_METRICS_URL"

func classifyForTest(t *testing.T, name string, out []byte, stderr string) error {
	t.Helper()
	ctx := context.Background()
	_, _, resultErr := classifyBDExecResult(ctx, ctx, name, time.Minute, time.Now(), out, stderr, errors.New("exit status 1"))
	return resultErr
}

// TestBdFailureDetailLeadsWithBdsErrorLine is the diagnosis half of the
// maintainer-city outage: bd prints its BD_OTEL_* deprecation warning during
// startup, BEFORE the command runs, so the actionable line is the SECOND line of
// stderr. Every single-line render of the wrapped error — the cache problem
// tile, a journal grep — then showed a telemetry nag where it used to show the
// reason the read failed, and the real cause went unfound for a night.
//
// The detail keeps every byte bd wrote and only reorders it, so the actionable
// line leads.
func TestBdFailureDetailLeadsWithBdsErrorLine(t *testing.T) {
	stderr := bdOtelDeprecationWarning + "\nError: 'bd sql' is not yet supported in embedded mode\n"

	err := classifyForTest(t, "bd", nil, stderr)

	first, _, _ := strings.Cut(err.Error(), "\n")
	if !strings.Contains(first, "not yet supported in embedded mode") {
		t.Fatalf("first line of the wrapped error = %q, want bd's actionable Error: line", first)
	}
	if !strings.Contains(err.Error(), "BD_OTEL_*") {
		t.Errorf("reordering dropped the warning; the detail must keep every byte bd wrote: %v", err)
	}
	if !isBdSQLUnsupportedInEmbeddedMode(err) {
		t.Errorf("the embedded-mode classifier no longer recognizes the wrapped error: %v", err)
	}
}

// TestBdFailureDetailIsUnchangedWithoutNoise is the byte-identity guard: every
// shape that is not "a warning printed before bd's own error line" composes
// exactly the detail it composed before.
func TestBdFailureDetailIsUnchangedWithoutNoise(t *testing.T) {
	cases := []struct {
		name   string
		cmd    string
		out    []byte
		stderr string
		want   string
	}{
		{
			name:   "single stderr line",
			cmd:    "bd",
			stderr: "Error: 'bd sql' is not yet supported in embedded mode\n",
			want:   "exit status 1: Error: 'bd sql' is not yet supported in embedded mode",
		},
		{
			name:   "error line already leads",
			cmd:    "bd",
			stderr: "Error: no issue found matching \"tst-1\"\nHint: run bd list\n",
			want:   "exit status 1: Error: no issue found matching \"tst-1\"\nHint: run bd list",
		},
		{
			name:   "no bd error line at all",
			cmd:    "bd",
			stderr: "first\nsecond\n",
			want:   "exit status 1: first\nsecond",
		},
		{
			name: "empty stderr falls back to the stdout envelope",
			cmd:  "bd",
			out:  []byte(`{"error":"database is locked","schema_version":1}`),
			want: "exit status 1: database is locked",
		},
		{
			name:   "non-bd command is passed through",
			cmd:    "dolt",
			stderr: "warning: something\nError: real cause\n",
			want:   "exit status 1: warning: something\nError: real cause",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyForTest(t, tc.cmd, tc.out, tc.stderr).Error(); got != tc.want {
				t.Fatalf("wrapped error =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}
