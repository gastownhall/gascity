package runtimecontract

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime/runtimecontract/reference"
)

func runNonTmuxIdle(t *testing.T, override map[string]string) Result {
	t.Helper()
	b := reference.NonTmuxBodies()
	for k, v := range override {
		b[k] = v
	}
	state := t.TempDir()
	report, err := run(context.Background(), writeScript(t, state, b), Options{}, func(c Code) bool { return c == ReqConnectionIdleBoundary })
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
		{"is-running", "echo false"}, {"is-running", "echo maybe"}, {"is-running", "exit 2"},
		{"peek", "exit 2"}, {"peek", "exit 1"}, {"peek", "echo unrelated fixed text"}, {"peek", "exit 66"},
	} {
		t.Run(tc.op+tc.body, func(t *testing.T) {
			if r := runNonTmuxIdle(t, map[string]string{tc.op: tc.body}); r.Status != StatusFail {
				t.Errorf("mutant = %s, %s", r.Status, r.Detail)
			}
		})
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
