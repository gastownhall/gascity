package rppcheck

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime/runtimecontract/reference"
)

func checkOpsReference(t *testing.T, overrides map[string]string, opts Options) Result {
	t.Helper()
	b := reference.NonTmuxBodies()
	for k, v := range overrides {
		b[k] = v
	}
	path := filepath.Join(t.TempDir(), "golden")
	if err := os.WriteFile(path, []byte(reference.Script(t.TempDir(), b)), 0o755); err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), path, opts)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func TestRun_CarrierOpsIsKnown(t *testing.T) {
	for _, extra := range []string{"", `,"carrier.future"`} {
		t.Run(extra, func(t *testing.T) {
			r := checkOpsReference(t, map[string]string{"protocol": `echo '{"version":0,"capabilities":["carrier.ops"` + extra + `]}'`}, Options{})
			detail := findCheck(t, r, handshakeCheck).Detail
			unknown := ""
			if _, tail, ok := strings.Cut(detail, "ignoring unknown capabilities"); ok {
				unknown = tail
			}
			if strings.Contains(unknown, "carrier.ops") {
				t.Errorf("carrier.ops reported unknown: %s", detail)
			}
			if strings.Contains(unknown, "carrier.future") != (extra != "") {
				t.Errorf("unknown token handling: %s", detail)
			}
		})
	}
}
func TestRun_NonTmuxGoldenFullyConformant(t *testing.T) {
	r := checkOpsReference(t, nil, Options{})
	if r.Failed() {
		t.Fatalf("non-tmux golden failed checker: %+v", r.Checks)
	}
	for _, op := range []string{"nudge", "peek", "send-keys", "interrupt", "clear-scrollback"} {
		if c := findCheck(t, r, "capability carrier.ops: "+op); c.Status != StatusPass {
			t.Errorf("%s = %s, %s", op, c.Status, c.Detail)
		}
	}
}
func TestRun_CarrierOpsRequiresDrivingOps(t *testing.T) {
	for _, op := range []string{"nudge", "peek", "send-keys", "interrupt", "clear-scrollback"} {
		for _, body := range []string{"exit 2", "exit 1"} {
			t.Run(op+body, func(t *testing.T) {
				r := checkOpsReference(t, map[string]string{op: body}, Options{})
				if c := findCheck(t, r, "capability carrier.ops: "+op); c.Status != StatusFail {
					t.Errorf("%s = %s, %s", op, c.Status, c.Detail)
				}
			})
		}
	}
	// The existing optional names and exit-2 treatment stay intact when unselected.
	r := checkOpsReference(t, map[string]string{"protocol": `echo '{"version":0}'`, "nudge": "exit 2", "peek": "exit 2", "interrupt": "exit 2"}, Options{})
	for _, op := range []string{"nudge", "peek", "interrupt"} {
		if c := findCheck(t, r, "optional: "+op); c.Status != StatusSkip {
			t.Errorf("optional %s = %s", op, c.Status)
		}
	}
	if hasCheck(r, "capability carrier.ops: send-keys") {
		t.Error("extra checks for undeclared carrier")
	}
}
func TestRun_CarrierOpsNamingPrecedesRequireNudge(t *testing.T) {
	for _, failStart := range []bool{false, true} {
		t.Run(map[bool]string{false: "running", true: "failed-start"}[failStart], func(t *testing.T) {
			overrides := map[string]string{"nudge": "exit 2"}
			if failStart {
				overrides["start"] = "cat >/dev/null; exit 1"
			}
			r := checkOpsReference(t, overrides, Options{RequireNudge: true})
			want := StatusFail
			if failStart {
				want = StatusSkip
			}
			if c := findCheck(t, r, "capability carrier.ops: nudge"); c.Status != want {
				t.Errorf("nudge = %s, want %s", c.Status, want)
			}
			if hasCheck(r, "required: nudge") || hasCheck(r, "optional: nudge") {
				t.Error("carrier.ops naming did not win")
			}
			if failStart {
				for _, op := range []string{"peek", "send-keys", "interrupt", "clear-scrollback"} {
					if c := findCheck(t, r, "capability carrier.ops: "+op); c.Status != StatusSkip {
						t.Errorf("%s = %s", op, c.Status)
					}
				}
			}
		})
	}
}
func TestRun_IdleBoundaryWarningFollowsTheCarrier(t *testing.T) {
	for _, tc := range []struct {
		caps     string
		warn     bool
		contains string
	}{
		{`"proc.exec","report-activity","report-attachment"`, true, "carrier.ops"},
		{`"carrier.ops","report-activity","report-attachment"`, true, "carrier.ops"},
		{`"carrier.ops","idle-boundary","report-activity","report-attachment"`, false, ""},
	} {
		t.Run(tc.caps, func(t *testing.T) {
			r := checkOpsReference(t, map[string]string{"protocol": `echo '{"version":0,"capabilities":[` + tc.caps + `]}'`}, Options{})
			detail := findCheck(t, r, handshakeCheck).Detail
			if strings.Contains(detail, "warning:") != tc.warn || !strings.Contains(detail, tc.contains) {
				t.Errorf("warning: %s", detail)
			}
			if strings.Contains(detail, "expected for a runtime whose box runs no tmux") {
				t.Errorf("stale warning: %s", detail)
			}
		})
	}
}
