//go:build integration

package tmux

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestNudgeSessionAuthFailureOnRealPane retains the public call-site proof:
// Enter drains a real pane's composer, but no busy row ever appears.
func TestNudgeSessionAuthFailureOnRealPane(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Fatal("python3 is required for the auth-submit TUI fixture")
	}
	script, err := filepath.Abs(filepath.Join("testdata", "nudge_auth_submit.py"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		mode string
		want error
	}{
		{name: "auth banner and drained composer", mode: "auth", want: ErrNudgeSubmitAuthFailed},
		{name: "drained composer without auth", mode: "drained", want: ErrNudgeSubmitDeliveredUnobserved},
		{name: "staged composer without auth", mode: "staged", want: ErrNudgeSubmitUnconfirmed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tm := testTmux()
			session := fmt.Sprintf("gt-test-auth-submit-%d", time.Now().UnixNano())
			command := "python3 " + shellQuote(script) + " " + tc.mode
			if err := tm.NewSessionWithCommandAndEnv(session, t.TempDir(), command, map[string]string{"GC_PROVIDER": "claude"}); err != nil {
				t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
			}
			t.Cleanup(func() {
				if err := tm.KillSession(session); err != nil {
					t.Errorf("KillSession: %v", err)
				}
			})
			deadline := time.After(5 * time.Second)
			poll := time.NewTicker(10 * time.Millisecond)
			defer poll.Stop()
			for {
				out, err := tm.CapturePaneAll(session)
				if err != nil {
					t.Fatalf("CapturePaneAll: %v", err)
				}
				if strings.Contains(out, "auth-submit-ready") {
					break
				}
				select {
				case <-poll.C:
				case <-deadline:
					t.Fatal("auth-submit TUI never became ready")
				}
			}

			err := tm.NudgeSession(session, "auth-submit-probe")
			if !errors.Is(err, tc.want) {
				t.Fatalf("NudgeSession = %v, want %v", err, tc.want)
			}
			out, err := tm.CapturePaneAll(session)
			if err != nil {
				t.Fatalf("CapturePaneAll after nudge: %v", err)
			}
			if !strings.Contains(out, "ENTER-RECEIVED") {
				t.Fatalf("fixture never received Enter:\n%s", out)
			}
			if tc.mode == "auth" && (!strings.Contains(out, "Login expired") || !paneShowsDrainedComposer(strings.Split(out, "\n"))) {
				t.Fatalf("fixture did not retain the auth banner and drained composer:\n%s", out)
			}
		})
	}
}
