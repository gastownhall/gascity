package doctor

import (
	"errors"
	"strings"
	"testing"
)

func TestSupervisorBinaryCheck_Metadata(t *testing.T) {
	c := NewSupervisorBinaryCheck(false, 0, SupervisorBinary{})
	if c.Name() != "supervisor-binary" {
		t.Errorf("Name() = %q, want %q", c.Name(), "supervisor-binary")
	}
	if c.CanFix() {
		t.Error("CanFix() = true, want false")
	}
	if c.WarmupEligible() {
		t.Error("WarmupEligible() = true, want false")
	}
}

// TestSupervisorBinaryCheckRun covers the stale-supervisor failure mode: a
// long-running supervisor whose executable was removed (brew unlink, package
// upgrade) or whose build differs from the invoking gc keeps answering
// delegated commands such as `gc reload` with no visible hint.
func TestSupervisorBinaryCheckRun(t *testing.T) {
	const pid = 4242
	const hint = "gc supervisor stop --wait && gc supervisor install"

	cases := []struct {
		name         string
		running      bool
		info         SupervisorBinary
		wantStatus   CheckStatus
		wantContains []string
		wantHint     bool
	}{
		{
			name:         "supervisor not running",
			running:      false,
			info:         SupervisorBinary{ExePath: "/gone/gc", ExeDeleted: true},
			wantStatus:   StatusOK,
			wantContains: []string{"not running"},
		},
		{
			name:    "matching build and present executable",
			running: true,
			info: SupervisorBinary{
				ExePath: "/usr/local/bin/gc", BuildID: "abc1234", Version: "1.5.0",
				LocalBuildID: "abc1234", LocalVersion: "1.5.0", RestartHint: hint,
			},
			wantStatus:   StatusOK,
			wantContains: []string{"PID 4242", "/usr/local/bin/gc", "abc1234"},
		},
		{
			name:    "deleted executable",
			running: true,
			info: SupervisorBinary{
				ExePath: "/opt/homebrew/bin/gc", ExeDeleted: true, BuildID: "abc1234",
				LocalBuildID: "abc1234", RestartHint: hint,
			},
			wantStatus:   StatusWarning,
			wantContains: []string{"PID 4242", "/opt/homebrew/bin/gc", "no longer exists", "GC_BIN"},
			wantHint:     true,
		},
		{
			name:    "different build",
			running: true,
			info: SupervisorBinary{
				ExePath: "/opt/homebrew/bin/gc", BuildID: "old1234", Version: "1.4.2",
				LocalBuildID: "new5678", LocalVersion: "1.5.0", RestartHint: hint,
			},
			wantStatus:   StatusWarning,
			wantContains: []string{"PID 4242", "old1234", "1.4.2", "new5678", "1.5.0"},
			wantHint:     true,
		},
		{
			name:    "deleted executable and different build report both",
			running: true,
			info: SupervisorBinary{
				ExePath: "/opt/homebrew/bin/gc", ExeDeleted: true, BuildID: "old1234",
				LocalBuildID: "new5678", RestartHint: hint,
			},
			wantStatus:   StatusWarning,
			wantContains: []string{"no longer exists", "old1234", "new5678"},
			wantHint:     true,
		},
		{
			name:    "unknown local build is not drift",
			running: true,
			info: SupervisorBinary{
				ExePath: "/usr/local/bin/gc", BuildID: "abc1234", LocalBuildID: "unknown", RestartHint: hint,
			},
			wantStatus: StatusOK,
		},
		{
			name:    "unknown supervisor build is not drift",
			running: true,
			info: SupervisorBinary{
				ExePath: "/usr/local/bin/gc", LocalBuildID: "abc1234", RestartHint: hint,
			},
			wantStatus: StatusOK,
		},
		{
			name:    "probe errors are reported, not hidden",
			running: true,
			info: SupervisorBinary{
				ExeErr:       errors.New("ps: no such process"),
				StatusErr:    errors.New("connection refused"),
				LocalBuildID: "abc1234",
				RestartHint:  hint,
			},
			wantStatus:   StatusOK,
			wantContains: []string{"ps: no such process", "connection refused"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewSupervisorBinaryCheck(tc.running, pid, tc.info).Run(&CheckContext{})
			if r.Status != tc.wantStatus {
				t.Fatalf("Status = %v, want %v; message=%q", r.Status, tc.wantStatus, r.Message)
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(r.Message, want) {
					t.Errorf("Message = %q, want it to contain %q", r.Message, want)
				}
			}
			if tc.wantHint {
				if !strings.Contains(r.FixHint, hint) {
					t.Errorf("FixHint = %q, want it to contain %q", r.FixHint, hint)
				}
			} else if r.FixHint != "" {
				t.Errorf("FixHint = %q, want empty", r.FixHint)
			}
		})
	}
}
