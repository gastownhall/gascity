package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"pgregory.net/rapid"
)

// TestSupervisorDetermineUnitOwnership exercises
// supervisorDetermineUnitOwnership, which compares a live supervisor PID
// against gc's own systemd user unit for `gc supervisor status` and the
// doctor check (ga-9pjtoy). "outside_unit" covers both ways the unit can
// fail to own the live process: the unit is inactive, or it is active but
// tracking a different MainPID and the live PID is not in the unit cgroup.
func TestSupervisorDetermineUnitOwnership(t *testing.T) {
	const livePID = 4242

	cases := []struct {
		name          string
		writeUnit     bool
		unitActive    bool
		mainPID       int
		mainPIDOK     bool
		cgroupFixture string
		cgroupError   error
		wantStatus    string
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
			name:          "unit installed, active, live supervisor in unit cgroup",
			writeUnit:     true,
			unitActive:    true,
			mainPID:       9999,
			mainPIDOK:     true,
			cgroupFixture: "unit",
			wantStatus:    "owned",
		},
		{
			name:          "unit installed but inactive",
			writeUnit:     true,
			unitActive:    false,
			mainPID:       9999,
			mainPIDOK:     true,
			cgroupFixture: "unit",
			wantStatus:    "outside_unit",
		},
		{
			name:          "unit installed and active but live supervisor in another cgroup",
			writeUnit:     true,
			unitActive:    true,
			mainPID:       9999,
			mainPIDOK:     true,
			cgroupFixture: "other",
			wantStatus:    "outside_unit",
		},
		{
			name:        "unit installed and active but cgroup unreadable",
			writeUnit:   true,
			unitActive:  true,
			mainPID:     9999,
			mainPIDOK:   true,
			cgroupError: errors.New("permission denied"),
			wantStatus:  "outside_unit",
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
			oldReadProcessCgroup := supervisorReadProcessCgroup
			supervisorSystemctlActive = func(_ string) bool { return tc.unitActive }
			supervisorSystemctlMainPID = func(_ string) (int, bool) { return tc.mainPID, tc.mainPIDOK }
			supervisorReadProcessCgroup = func(pid int) ([]byte, error) {
				if pid != livePID {
					t.Fatalf("supervisorReadProcessCgroup(%d), want PID %d", pid, livePID)
				}
				switch tc.cgroupFixture {
				case "unit":
					return []byte("0::/user.slice/" + supervisorSystemdServiceName() + "\n"), tc.cgroupError
				case "other":
					return []byte("0::/user.slice/other.service\n"), tc.cgroupError
				default:
					return nil, tc.cgroupError
				}
			}
			t.Cleanup(func() {
				supervisorSystemctlActive = oldActive
				supervisorSystemctlMainPID = oldMainPID
				supervisorReadProcessCgroup = oldReadProcessCgroup
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

func TestSupervisorUnitOwnershipDecision(t *testing.T) {
	const unit = "gascity-supervisor.service"

	cases := []struct {
		name       string
		unitActive bool
		livePID    int
		mainPID    int
		cgroup     string
		want       bool
	}{
		{name: "MainPID match", unitActive: true, livePID: 42, mainPID: 42, want: true},
		{name: "unit cgroup match", unitActive: true, livePID: 42, mainPID: 7, cgroup: "0::/user.slice/" + unit + "\n", want: true},
		{name: "different cgroup", unitActive: true, livePID: 42, mainPID: 7, cgroup: "0::/user.slice/other.service\n", want: false},
		{name: "inactive unit", unitActive: false, livePID: 42, mainPID: 42, cgroup: "0::/user.slice/" + unit + "\n", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := supervisorUnitOwnsProcess(tc.unitActive, tc.livePID, tc.mainPID, []byte(tc.cgroup), unit)
			if got != tc.want {
				t.Fatalf("supervisorUnitOwnsProcess(%v, %d, %d, %q, %q) = %v, want %v", tc.unitActive, tc.livePID, tc.mainPID, tc.cgroup, unit, got, tc.want)
			}
		})
	}
}

func TestSupervisorUnitOwnershipRecognizesExactCgroupComponent(t *testing.T) {
	const unit = "gascity-supervisor.service"

	rapid.Check(t, func(rt *rapid.T) {
		prefix := rapid.StringMatching(`[a-z]{1,12}`).Draw(rt, "prefix")
		suffix := rapid.StringMatching(`[a-z]{1,12}`).Draw(rt, "suffix")
		livePID := rapid.IntRange(1, 1_000_000).Draw(rt, "livePID")
		mainPID := livePID + 1
		matching := []byte("0::/" + prefix + "/" + unit + "/" + suffix + "\n")
		nonmatching := []byte("0::/" + prefix + "/not-" + unit + "/" + suffix + "\n")

		if !supervisorUnitOwnsProcess(true, livePID, mainPID, matching, unit) {
			rt.Fatalf("active process in cgroup %q was not owned", matching)
		}
		if supervisorUnitOwnsProcess(true, livePID, mainPID, nonmatching, unit) {
			rt.Fatalf("active process in cgroup %q was owned by substring match", nonmatching)
		}
	})
}
