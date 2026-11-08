package runtimecontract

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime/runtimecontract/reference"
)

func runNonTmuxIdle(t *testing.T, override map[string]string) Result {
	t.Helper()
	return runNonTmuxIdleOptions(t, override, Options{OpTimeout: 500 * time.Millisecond})
}

func runNonTmuxIdleOptions(t *testing.T, override map[string]string, opts Options) Result {
	t.Helper()
	b := reference.NonTmuxBodies()
	for k, v := range override {
		b[k] = v
	}
	state := t.TempDir()
	report, err := run(context.Background(), writeScript(t, state, b), opts, func(c Code) bool { return c == ReqConnectionIdleBoundary })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(state, "tmux-rejected")); !os.IsNotExist(err) {
		t.Error("ops probe invoked tmux")
	}
	for _, r := range report.Results {
		if r.Code == ReqConnectionIdleBoundary {
			return r
		}
	}
	t.Fatal("missing RPP-CONN-002")
	return Result{}
}

func TestNonTmuxIdleBoundaryIsGated(t *testing.T) {
	if r := runNonTmuxIdle(t, nil); r.Status != StatusPass {
		t.Fatalf("golden = %s, %s", r.Status, r.Detail)
	}
	for _, tc := range []struct{ op, body string }{
		{"is-running", "echo false"},
		{"is-running", "echo maybe"},
		{"is-running", "exit 2"},
		{"peek", "exit 2"},
		{"peek", "exit 1"},
		{"peek", "echo unrelated fixed text"},
		{"peek", "exit 66"},
	} {
		t.Run(tc.op+tc.body, func(t *testing.T) {
			if r := runNonTmuxIdle(t, map[string]string{tc.op: tc.body}); r.Status != StatusFail {
				t.Errorf("mutant = %s, %s", r.Status, r.Detail)
			}
		})
	}
}

func TestNonTmuxIdleBoundaryWaitsForScreen(t *testing.T) {
	peek := `if [ ! -f "$S/peek-called" ]; then touch "$S/peek-called"; echo 'screen not rendered'; exit 0; fi
` + reference.NonTmuxBodies()["peek"]
	if r := runNonTmuxIdle(t, map[string]string{"peek": peek}); r.Status != StatusPass {
		t.Fatalf("delayed screen = %s, %s", r.Status, r.Detail)
	}
}

func TestNonTmuxIdleBoundaryFailureDetails(t *testing.T) {
	for _, tc := range []struct {
		name, op, body, detail string
	}{
		{"start-error", "start", "echo start-exploded >&2; exit 1", "start-exploded"},
		{"start-unsupported", "start", "exit 2", "start returned exit 2"},
		{"last-screen", "peek", "echo still-rendering", "still-rendering"},
		{"last-error", "peek", "echo peek-exploded >&2; exit 1", "peek-exploded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runNonTmuxIdle(t, map[string]string{tc.op: tc.body})
			if r.Status != StatusFail || !strings.Contains(r.Detail, tc.detail) {
				t.Errorf("got %s, %s; want FAIL with %q", r.Status, r.Detail, tc.detail)
			}
		})
	}
}

// A peek the window's deadline cuts short must not replace the last real
// read in the failure detail: the first peek shows the screen, every later
// one hangs past the window.
// Kills: a canceled peek's empty output or context error reported instead
// of the screen the pack actually showed.
func TestNonTmuxIdleBoundaryKeepsTheLastRealRead(t *testing.T) {
	peek := `if [ -f "$box/peeked" ]; then exec sleep 30; fi; : > "$box/peeked"; echo first-screen`
	r := runNonTmuxIdleOptions(t, map[string]string{"peek": peek}, Options{OpTimeout: 500 * time.Millisecond})
	if r.Status != StatusFail || !strings.Contains(r.Detail, "first-screen") || strings.Contains(r.Detail, "canceled") {
		t.Errorf("got %s, %s; want FAIL naming first-screen, not the canceled peek", r.Status, r.Detail)
	}
}

func TestNonTmuxIdleBoundaryPeekBound(t *testing.T) {
	start := time.Now()
	r := runNonTmuxIdleOptions(t, map[string]string{"peek": "exec sleep 30"}, Options{OpTimeout: 100 * time.Millisecond})
	if r.Status != StatusFail || !strings.Contains(r.Detail, "timed out") {
		t.Errorf("hung peek = %s, %s", r.Status, r.Detail)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("peek exceeded short bound: %s", elapsed)
	}
}

func TestNonTmuxIdleBoundaryGatesOnTheDeclaration(t *testing.T) {
	for _, tc := range []struct {
		name, caps, peek string
		want             Status
	}{
		{"undeclared", `"carrier.ops"`, "exit 2", StatusSkip},
		{"missing peer broken", `"carrier.ops","idle-boundary"`, "exit 2", StatusFail},
		{"missing peer working", `"carrier.ops","idle-boundary"`, reference.NonTmuxBodies()["peek"], StatusPass},
		{"duplicate", `"carrier.ops","carrier.ops","idle-boundary","report-activity","report-attachment"`, reference.NonTmuxBodies()["peek"], StatusPass},
		{"unknown alongside", `"carrier.ops","carrier.future","idle-boundary","report-activity","report-attachment"`, reference.NonTmuxBodies()["peek"], StatusPass},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runNonTmuxIdle(t, map[string]string{"protocol": `echo '{"version":0,"capabilities":[` + tc.caps + `]}'`, "peek": tc.peek})
			if r.Status != tc.want {
				t.Errorf("got %s, %s, want %s", r.Status, r.Detail, tc.want)
			}
			if tc.name == "missing peer working" && !strings.Contains(r.Detail, "takes effect only with") {
				t.Error("missing prerequisite note")
			}
		})
	}
}
