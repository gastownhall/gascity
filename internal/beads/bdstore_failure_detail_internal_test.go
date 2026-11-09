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

// bdGTRootEnvFileNotice is bd v1.3.3's stderr line for the GT_ROOT key that
// `gc rig add` writes to a rig's .beads/.env. A bd command that opens the
// store prints it twice.
const bdGTRootEnvFileNotice = "beads: ignoring GT_ROOT from .beads/.env (only BEADS_*, BD_*, and the Dolt remote credential variables (DOLT_REMOTE_USER, DOLT_REMOTE_PASSWORD) are allowed in .beads/.env)"

// TestBdFailureDetailEnvFileNoticesDoNotHideTheStdoutEnvelope: in a rig, bd
// v1.3.3 prints its .beads/.env notice for GT_ROOT on every command. A --json
// failure that bd reports only on stdout (bd dep list, close, label add)
// therefore arrives with stderr holding nothing but notices. The envelope must
// still reach the error text, ahead of the notices, or the classifiers that read
// that text stop recognizing the failure.
func TestBdFailureDetailEnvFileNoticesDoNotHideTheStdoutEnvelope(t *testing.T) {
	stderr := bdGTRootEnvFileNotice + "\n" + bdGTRootEnvFileNotice + "\n"
	cases := []struct {
		name     string
		out      string
		classify func(error) bool
	}{
		{"not found", `{"error":"resolving rt-zzz: no issue found matching \"rt-zzz\"","schema_version":1}`, isBdNotFound},
		{"serialization failure", `{"error":"failed to commit: Error 1213 (40001): serialization failure","schema_version":1}`, isBdTransientWriteError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyForTest(t, "bd", []byte(tc.out), stderr)
			if !tc.classify(err) {
				t.Fatalf("the classifier no longer recognizes the wrapped error: %v", err)
			}
			first, rest, _ := strings.Cut(err.Error(), "\n")
			if strings.Contains(first, "beads: ignoring") {
				t.Errorf("first line of the wrapped error = %q, want bd's own error", first)
			}
			if strings.Count(rest, bdGTRootEnvFileNotice) != 2 {
				t.Errorf("both notices must follow the error; nothing bd wrote is dropped: %v", err)
			}
		})
	}
}

// TestBdFailureDetailEnvFileNoticesFollowBdsError: when bd writes its error to
// stderr, the notices printed before it move behind it, so a single-line
// render of the wrapped error says why the command failed.
func TestBdFailureDetailEnvFileNoticesFollowBdsError(t *testing.T) {
	stderr := bdGTRootEnvFileNotice + "\n" + bdGTRootEnvFileNotice + "\nError resolving rt-zzz: no issue found matching \"rt-zzz\"\n"
	err := classifyForTest(t, "bd", nil, stderr)
	want := "exit status 1: Error resolving rt-zzz: no issue found matching \"rt-zzz\"\n" + bdGTRootEnvFileNotice + "\n" + bdGTRootEnvFileNotice
	if err.Error() != want {
		t.Fatalf("wrapped error =\n%q\nwant\n%q", err.Error(), want)
	}
}

// TestBdFailureDetailOnlyEnvFileNotices: notices and no envelope on stdout is
// all bd said, so the detail is the notices.
func TestBdFailureDetailOnlyEnvFileNotices(t *testing.T) {
	err := classifyForTest(t, "bd", nil, bdGTRootEnvFileNotice+"\n")
	if want := "exit status 1: " + bdGTRootEnvFileNotice; err.Error() != want {
		t.Fatalf("wrapped error = %q, want %q", err.Error(), want)
	}
}
