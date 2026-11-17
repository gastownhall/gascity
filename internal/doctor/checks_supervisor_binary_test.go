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
	// The caller derives the remediation for its deployment; the check shows
	// it verbatim, including every line of a multi-line hint.
	const hint = "Fix, either:\n  - run cities with this gc\n  - or keep the running supervisor"

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
			info:         SupervisorBinary{ExePath: "/gone/gc", ExeDeleted: true, BuildDiffers: true},
			wantStatus:   StatusOK,
			wantContains: []string{"not running"},
		},
		{
			name:    "matching build and present executable",
			running: true,
			info: SupervisorBinary{
				ExePath: "/usr/local/bin/gc", BuildID: "abc1234", Version: "1.5.0",
				LocalBuildID: "abc1234", LocalVersion: "1.5.0", FixHint: hint,
			},
			wantStatus:   StatusOK,
			wantContains: []string{"PID 4242", "/usr/local/bin/gc", "build abc1234 (version 1.5.0)"},
		},
		{
			// The caller compares identities (an abbreviated hash matches its
			// full form); the check acts on its verdict, not the raw strings.
			name:    "raw build strings differ but the caller found the same build",
			running: true,
			info: SupervisorBinary{
				ExePath: "/usr/local/bin/gc", BuildID: "abc1234", LocalBuildID: "ABC1234DEF0", FixHint: hint,
			},
			wantStatus:   StatusOK,
			wantContains: []string{"/usr/local/bin/gc, build abc1234"},
		},
		{
			name:    "deleted executable",
			running: true,
			info: SupervisorBinary{
				ExePath: "/opt/homebrew/bin/gc", ExeDeleted: true, BuildID: "abc1234",
				LocalBuildID: "abc1234", FixHint: hint,
			},
			wantStatus:   StatusWarning,
			wantContains: []string{"PID 4242", "/opt/homebrew/bin/gc", "removed or replaced", "GC_BIN"},
			wantHint:     true,
		},
		{
			name:    "different build",
			running: true,
			info: SupervisorBinary{
				ExePath: "/opt/homebrew/bin/gc", BuildID: "old1234", Version: "1.4.2",
				LocalBuildID: "new5678", LocalVersion: "1.5.0", BuildDiffers: true, FixHint: hint,
			},
			wantStatus:   StatusWarning,
			wantContains: []string{"PID 4242", "it serves build old1234 (version 1.4.2) from /opt/homebrew/bin/gc but this gc is build new5678 (version 1.5.0)"},
			wantHint:     true,
		},
		{
			// An in-place homebrew-core upgrade: neither side carries a build
			// ID and the executable path still exists, so only the release
			// versions show the drift.
			name:    "different version with unknown builds",
			running: true,
			info: SupervisorBinary{
				ExePath: "/opt/homebrew/bin/gc", Version: "1.5.0", LocalVersion: "1.5.1",
				BuildDiffers: true, FixHint: hint,
			},
			wantStatus:   StatusWarning,
			wantContains: []string{"it serves version 1.5.0 from /opt/homebrew/bin/gc but this gc is version 1.5.1"},
			wantHint:     true,
		},
		{
			// A restart relaunches what the service names, which an operator
			// needs when it is not the file the supervisor runs.
			name:    "different build whose service launches another binary",
			running: true,
			info: SupervisorBinary{
				ExePath: "/opt/homebrew/Cellar/gc/1.4.2/bin/gc", ServiceBinary: "/opt/homebrew/bin/gc",
				BuildID: "old1234", LocalBuildID: "new5678", BuildDiffers: true, FixHint: hint,
			},
			wantStatus: StatusWarning,
			wantContains: []string{
				"it serves build old1234 from /opt/homebrew/Cellar/gc/1.4.2/bin/gc (its service launches /opt/homebrew/bin/gc) but this gc is build new5678",
			},
			wantHint: true,
		},
		{
			name:    "different build from an unknown executable",
			running: true,
			info: SupervisorBinary{
				ExeErr:  errors.New("ps: no such process"),
				BuildID: "old1234", LocalBuildID: "new5678", BuildDiffers: true, FixHint: hint,
			},
			wantStatus:   StatusWarning,
			wantContains: []string{"it serves build old1234 but this gc is build new5678"},
			wantHint:     true,
		},
		{
			name:    "deleted executable and different build report both",
			running: true,
			info: SupervisorBinary{
				ExePath: "/opt/homebrew/bin/gc", ExeDeleted: true, BuildID: "old1234",
				LocalBuildID: "new5678", BuildDiffers: true, FixHint: hint,
			},
			wantStatus:   StatusWarning,
			wantContains: []string{"removed or replaced", "it serves build old1234 from /opt/homebrew/bin/gc but this gc is build new5678"},
			wantHint:     true,
		},
		{
			name:    "unknown supervisor identity is not drift",
			running: true,
			info: SupervisorBinary{
				ExePath: "/usr/local/bin/gc", LocalBuildID: "abc1234", FixHint: hint,
			},
			wantStatus:   StatusOK,
			wantContains: []string{"/usr/local/bin/gc, unknown build"},
		},
		{
			name:    "probe errors are reported, not hidden",
			running: true,
			info: SupervisorBinary{
				ExeErr:       errors.New("ps: no such process"),
				StatusErr:    errors.New("connection refused"),
				LocalBuildID: "abc1234",
				FixHint:      hint,
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
				if r.FixHint != hint {
					t.Errorf("FixHint = %q, want %q", r.FixHint, hint)
				}
			} else if r.FixHint != "" {
				t.Errorf("FixHint = %q, want empty", r.FixHint)
			}
		})
	}
}
