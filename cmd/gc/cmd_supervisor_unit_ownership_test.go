package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSupervisorDetermineUnitOwnership exercises
// supervisorDetermineUnitOwnership, which compares a live supervisor PID
// against gc's own systemd user unit for `gc supervisor status` and the
// doctor check (ga-9pjtoy). "outside_unit" covers both ways the unit can
// fail to own the live process: the unit is inactive, or it is active but
// tracking a different MainPID.
func TestSupervisorDetermineUnitOwnership(t *testing.T) {
	const livePID = 4242

	cases := []struct {
		name       string
		writeUnit  bool
		unitActive bool
		mainPID    int
		mainPIDOK  bool
		wantStatus string
	}{
		{
			name:       "no unit installed",
			writeUnit:  false,
			wantStatus: "no_unit",
		},
		{
			name:       "unit installed, active, MainPID matches live supervisor",
			writeUnit:  true,
			unitActive: true,
			mainPID:    livePID,
			mainPIDOK:  true,
			wantStatus: "owned",
		},
		{
			name:       "unit installed but inactive",
			writeUnit:  true,
			unitActive: false,
			mainPID:    0,
			mainPIDOK:  false,
			wantStatus: "outside_unit",
		},
		{
			name:       "unit installed and active but MainPID mismatch",
			writeUnit:  true,
			unitActive: true,
			mainPID:    9999,
			mainPIDOK:  true,
			wantStatus: "outside_unit",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			homeDir := t.TempDir()
			t.Setenv("HOME", homeDir)

			unitPath := supervisorSystemdServicePath()
			if tc.writeUnit {
				if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(unitPath, []byte("[Unit]\nDescription=test\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			oldActive := supervisorSystemctlActive
			oldMainPID := supervisorSystemctlMainPID
			supervisorSystemctlActive = func(_ string) bool { return tc.unitActive }
			supervisorSystemctlMainPID = func(_ string) (int, bool) { return tc.mainPID, tc.mainPIDOK }
			t.Cleanup(func() {
				supervisorSystemctlActive = oldActive
				supervisorSystemctlMainPID = oldMainPID
			})

			got := supervisorDetermineUnitOwnership(livePID)
			if got.Status != tc.wantStatus {
				t.Fatalf("supervisorDetermineUnitOwnership(%d) = %+v, want Status %q", livePID, got, tc.wantStatus)
			}
			if tc.writeUnit && got.Unit == "" {
				t.Fatalf("supervisorDetermineUnitOwnership(%d) = %+v, want non-empty Unit when a unit is installed", livePID, got)
			}
		})
	}
}
