//go:build windows

package herdr

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestSetupCommandsRefusePOSIXShell pins that pre_start and session_setup
// commands naming a POSIX shell are refused rather than handed to whatever sh
// PATH offers, as hook, order and pool commands are.
func TestSetupCommandsRefusePOSIXShell(t *testing.T) {
	p := newTestProvider(t, 10*time.Second)
	err := p.runPreStart(context.Background(), runtime.Config{PreStart: []string{fixtureSucceed, "bash ./prepare.sh"}})
	if err == nil || !strings.Contains(err.Error(), "pre_start[1]") || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("runPreStart = %v, want pre_start[1] refused", err)
	}
	if err := p.runSetupCommand(context.Background(), "sh -c 'exit 0'", nil); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("runSetupCommand = %v, want a refusal", err)
	}
}

// TestLaunchRawRefusesPOSIXShell pins that a raw command naming a POSIX shell
// is refused before anything is typed into the pane.
func TestLaunchRawRefusesPOSIXShell(t *testing.T) {
	p := newTestProvider(t, time.Second)
	if err := p.launchRaw(context.Background(), "unused-pane", "bash"); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("launchRaw = %v, want a refusal", err)
	}
}

// TestStartRefusesBeforeAnySideEffect pins that Start refuses a command the
// platform refuses before it runs pre_start, places a pane or publishes the
// session. A refusal that left a live shell and seeded metadata behind read to
// the session manager as a started session, and each retry ran pre_start
// again.
func TestStartRefusesBeforeAnySideEffect(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  func(marker string) runtime.Config
		want string
	}{
		{"raw command", func(marker string) runtime.Config {
			return runtime.Config{Command: "bash", PreStart: []string{fixtureTouch(marker)}}
		}, "raw command refused"},
		{"later pre_start", func(marker string) runtime.Config {
			return runtime.Config{Command: "claude", PreStart: []string{fixtureTouch(marker), "bash ./prepare.sh"}}
		}, "pre_start[1] refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "prestart-ran")
			f := newFakeHerdr(t, marker)
			p := newFakeStartProvider(t, f)
			const name = "gastown__worker"
			cfg := tc.cfg(marker)
			cfg.WorkDir = t.TempDir()
			cfg.Env = map[string]string{"GC_SESSION_ID": "s-1", "GC_INSTANCE_TOKEN": "token-1"}
			err := p.start(context.Background(), name, cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("start = %v, want %q", err, tc.want)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Error("pre_start ran before the refusal")
			}
			lines := f.logLines(t)
			for _, call := range []string{"workspace create", "tab create", "agent start", "pane run"} {
				if logIndex(lines, call) >= 0 {
					t.Errorf("herdr %s ran before the refusal; log:\n%s", call, strings.Join(lines, "\n"))
				}
			}
			if id, _ := p.GetMeta(name, "GC_SESSION_ID"); id != "" {
				t.Errorf("session metadata published before the refusal: GC_SESSION_ID=%q", id)
			}
		})
	}
}
