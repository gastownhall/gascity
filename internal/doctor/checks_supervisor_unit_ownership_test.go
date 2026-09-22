package doctor

import (
	"fmt"
	"strings"
	"testing"
)

func TestSupervisorUnitOwnershipCheck_Name(t *testing.T) {
	c := NewSupervisorUnitOwnershipCheck(false, 0, SupervisorUnitOwnership{})
	if c.Name() != "supervisor-unit-ownership" {
		t.Errorf("Name() = %q, want %q", c.Name(), "supervisor-unit-ownership")
	}
}

func TestSupervisorUnitOwnershipCheck_CanFix(t *testing.T) {
	c := NewSupervisorUnitOwnershipCheck(false, 0, SupervisorUnitOwnership{})
	if c.CanFix() {
		t.Error("CanFix() = true, want false")
	}
}

func TestSupervisorUnitOwnershipCheck_WarmupEligible(t *testing.T) {
	c := NewSupervisorUnitOwnershipCheck(false, 0, SupervisorUnitOwnership{})
	if c.WarmupEligible() {
		t.Error("WarmupEligible() = true, want false")
	}
}

// TestSupervisorUnitOwnershipCheckRun exercises
// SupervisorUnitOwnershipCheck.Run, the doctor-facing half of ga-9pjtoy: it
// must flag as an error a live supervisor that is not owned by gc's own
// installed systemd unit (unit inactive, or active with a mismatched
// MainPID), while treating "no supervisor running" and "no unit installed"
// as OK.
func TestSupervisorUnitOwnershipCheckRun(t *testing.T) {
	const livePID = 4242

	cases := []struct {
		name              string
		supervisorRunning bool
		ownership         SupervisorUnitOwnership
		wantStatus        CheckStatus
	}{
		{
			name:              "supervisor not running",
			supervisorRunning: false,
			ownership:         SupervisorUnitOwnership{Status: "no_unit"},
			wantStatus:        StatusOK,
		},
		{
			name:              "no unit installed",
			supervisorRunning: true,
			ownership:         SupervisorUnitOwnership{Status: "no_unit"},
			wantStatus:        StatusOK,
		},
		{
			name:              "owned by unit",
			supervisorRunning: true,
			ownership: SupervisorUnitOwnership{
				Status:     "owned",
				Unit:       "gascity-supervisor.service",
				UnitActive: true,
				UnitPID:    livePID,
			},
			wantStatus: StatusOK,
		},
		{
			name:              "unit installed but inactive",
			supervisorRunning: true,
			ownership: SupervisorUnitOwnership{
				Status:     "outside_unit",
				Unit:       "gascity-supervisor.service",
				UnitActive: false,
				UnitPID:    0,
			},
			wantStatus: StatusError,
		},
		{
			name:              "unit active but MainPID mismatch",
			supervisorRunning: true,
			ownership: SupervisorUnitOwnership{
				Status:     "outside_unit",
				Unit:       "gascity-supervisor.service",
				UnitActive: true,
				UnitPID:    9999,
			},
			wantStatus: StatusError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			check := NewSupervisorUnitOwnershipCheck(tc.supervisorRunning, livePID, tc.ownership)
			result := check.Run(&CheckContext{})

			if result.Status != tc.wantStatus {
				t.Fatalf("Status = %v, want %v (result: %+v)", result.Status, tc.wantStatus, result)
			}
			if tc.wantStatus != StatusError {
				return
			}
			if result.FixHint == "" || !strings.Contains(result.FixHint, "reset-failed") {
				t.Errorf("FixHint = %q, want a remedy mentioning systemctl --user reset-failed", result.FixHint)
			}
			for _, wantPID := range []int{livePID, tc.ownership.UnitPID} {
				if !strings.Contains(result.Message, fmt.Sprintf("%d", wantPID)) {
					t.Errorf("Message = %q, want it to name PID %d", result.Message, wantPID)
				}
			}
		})
	}
}
