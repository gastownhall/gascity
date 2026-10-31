//go:build windows

package orders

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/processgroup/processgrouptest"
)

// TestCheckConditionRunsThroughPowerShell pins condition checks on Windows:
// the check runs through PowerShell and its exit status decides, and a check
// whose program is a POSIX shell or shell script is refused. Git's bash is on
// PATH under the test runner, so a refused check that ran anyway would pass.
func TestCheckConditionRunsThroughPowerShell(t *testing.T) {
	cases := []struct {
		name, check string
		due         bool
		reason      string
	}{
		{name: "passes", check: "if ($true) { exit 0 }", due: true, reason: "condition: check passed (exit 0)"},
		{name: "not met", check: "cmd.exe /c exit 3", reason: "condition: not met (exit 3)"},
		{name: "shell script", check: "$PACK_DIR/checks/ready.sh", reason: "check command refused: its program $PACK_DIR/checks/ready.sh is a POSIX shell script"},
		{name: "shell", check: "bash -c 'exit 0'", reason: "check command refused: its program bash is a POSIX shell"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := checkCondition(Order{Name: "probe", Trigger: "condition", Check: tc.check}, TriggerOptions{})
			if got.Due != tc.due || !strings.HasPrefix(got.Reason, tc.reason) {
				t.Fatalf("checkCondition(%q) = due %v, %q; want due %v, %q", tc.check, got.Due, got.Reason, tc.due, tc.reason)
			}
		})
	}
}

// TestCheckConditionTimeoutReapsTheTreeOnWindows pins that a condition check
// that times out takes every process it started with it, because it runs in a
// Job Object that owns its tree.
func TestCheckConditionTimeoutReapsTheTreeOnWindows(t *testing.T) {
	record := filepath.Join(t.TempDir(), "descendant")
	a := Order{Name: "check", Trigger: "condition", Check: processgrouptest.DescendantCommand(record)}
	now := time.Date(2026, 2, 27, 12, 0, 0, 0, time.UTC)
	result := CheckTriggerWithOptions(a, now, neverRan, nil, nil, TriggerOptions{ConditionTimeout: 6 * time.Second})
	if result.Due || !strings.Contains(result.Reason, ConditionCheckTimedOutMarker) {
		t.Fatalf("result = %+v, want a timed-out check", result)
	}
	if d := processgrouptest.ReadDescendant(t, record); !d.WaitGone(10 * time.Second) {
		t.Fatal("the check's descendant survived its timeout")
	}
}

// TestCheckConditionWaitDelayReapsTheTreeOnWindows is the Windows twin of
// TestCheckTriggerConditionKillsProcessGroupAfterWaitDelay: a check that exits
// while a descendant still holds its output is cleaned up once the wait delay
// runs out, and the cleanup reaps the descendant.
func TestCheckConditionWaitDelayReapsTheTreeOnWindows(t *testing.T) {
	oldWaitDelay := conditionCheckPostCancelWaitDelay
	conditionCheckPostCancelWaitDelay = 500 * time.Millisecond
	t.Cleanup(func() { conditionCheckPostCancelWaitDelay = oldWaitDelay })
	record := filepath.Join(t.TempDir(), "descendant")
	a := Order{Name: "check", Trigger: "condition", Check: processgrouptest.LeaveDescendantCommand(record)}
	now := time.Date(2026, 2, 27, 12, 0, 0, 0, time.UTC)
	result := CheckTriggerWithOptions(a, now, neverRan, nil, nil, TriggerOptions{ConditionTimeout: 30 * time.Second})
	if result.Due || !strings.Contains(result.Reason, "post-cancel wait delay") {
		t.Fatalf("result = %+v, want the cleanup after the post-cancel wait delay", result)
	}
	if d := processgrouptest.ReadDescendant(t, record); !d.WaitGone(10 * time.Second) {
		t.Fatal("the check's descendant survived the cleanup after its wait delay")
	}
}

// TestFinishedCheckLeavesItsDescendantRunningOnWindows pins the other half of
// the Unix contract: a check that finishes, holding nothing open, leaves what
// it started running. On Windows that is the difference between releasing the
// Job Object and killing it on release.
func TestFinishedCheckLeavesItsDescendantRunningOnWindows(t *testing.T) {
	record := filepath.Join(t.TempDir(), "descendant")
	a := Order{Name: "check", Trigger: "condition", Check: processgrouptest.LeaveDetachedDescendantCommand(record)}
	now := time.Date(2026, 2, 27, 12, 0, 0, 0, time.UTC)
	result := CheckTriggerWithOptions(a, now, neverRan, nil, nil, TriggerOptions{ConditionTimeout: 30 * time.Second})
	if !result.Due {
		t.Fatalf("result = %+v, want a check that passed", result)
	}
	if d := processgrouptest.ReadDescendant(t, record); d.WaitGone(2 * time.Second) {
		t.Fatal("the descendant died with the check that finished; releasing its tree must not kill it")
	}
}
