//go:build windows

package proctable

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestRootArgvReadsAChildCommandLine(t *testing.T) {
	helper := startHelper(t, uniqueSessionID(t, "argv"), t.TempDir())

	argv, err := rootArgv(helper.pid)
	if err != nil {
		t.Fatalf("rootArgv(%d): %v", helper.pid, err)
	}
	if len(argv) < 2 {
		t.Fatalf("rootArgv(%d) = %q, want the helper's own executable and test flags", helper.pid, argv)
	}
	if got, want := filepath.Base(argv[0]), filepath.Base(os.Args[0]); !strings.EqualFold(got, want) {
		t.Errorf("argv[0] basename = %q, want %q", got, want)
	}
	if !slices.Contains(argv, "-test.run=^TestProctableWindowsHelperProcess$") {
		t.Errorf("rootArgv(%d) = %q, want the -test.run flag the helper was started with", helper.pid, argv)
	}
}

// TestRootArgvDrivesTheCityInfrastructureFence proves the argv the PEB yields
// is shaped the way the fence reads it, without spawning a real watchdog.
func TestRootArgvDrivesTheCityInfrastructureFence(t *testing.T) {
	helper := startHelper(t, uniqueSessionID(t, "argv-fence"), t.TempDir())

	argv, err := rootArgv(helper.pid)
	if err != nil {
		t.Fatalf("rootArgv(%d): %v", helper.pid, err)
	}
	if IsCityInfrastructureArgv(argv) {
		t.Errorf("the helper %q was classified as city infrastructure", argv)
	}
	if !IsCityInfrastructureArgv([]string{argv[0], ManagedDoltScopeWatchdogVerb, "cfg", "log", "city"}) {
		t.Error("a watchdog argv built from a real argv[0] was not classified as city infrastructure")
	}
}

func TestRootStartIdentityMatchesProcessIdentity(t *testing.T) {
	helper := startHelper(t, uniqueSessionID(t, "argv-identity"), t.TempDir())

	if got := rootStartIdentity(helper.pid); got != helper.identity {
		t.Errorf("rootStartIdentity(%d) = %q, want %q", helper.pid, got, helper.identity)
	}
	if got := rootStartIdentity(impossiblePID); got != "" {
		t.Errorf("rootStartIdentity(%d) = %q, want an empty token for a process that cannot exist", impossiblePID, got)
	}
	if got := rootStartIdentity(0); got != "" {
		t.Errorf("rootStartIdentity(0) = %q, want an empty token", got)
	}
}

func TestSplitWindowsCommandLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		want []string
	}{
		{name: "empty", line: "   ", want: nil},
		{name: "plain", line: `gc.exe start --city C:\city`, want: []string{"gc.exe", "start", "--city", `C:\city`}},
		{name: "quoted path", line: `"C:\Program Files\gc\gc.exe" run`, want: []string{`C:\Program Files\gc\gc.exe`, "run"}},
		{name: "quoted argument", line: `gc.exe --note "hello world"`, want: []string{"gc.exe", "--note", "hello world"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := splitWindowsCommandLine(tc.line)
			if err != nil {
				t.Fatalf("splitWindowsCommandLine(%q): %v", tc.line, err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("splitWindowsCommandLine(%q) = %q, want %q", tc.line, got, tc.want)
			}
		})
	}
}

func TestRootArgvRejectsAPIDThatCannotExist(t *testing.T) {
	if _, err := rootArgv(impossiblePID); err == nil {
		t.Fatalf("rootArgv(%d) succeeded for a process that cannot exist", impossiblePID)
	}
}

// TestRootIsCityInfrastructureReadsTheProcessTheScanSaw classifies live argv
// mocks through the identity their scan would have recorded, and pins what a
// root that no longer carries that identity reads as.
func TestRootIsCityInfrastructureReadsTheProcessTheScanSaw(t *testing.T) {
	watchdog := startArgvMock(t, ManagedDoltScopeWatchdogVerb)
	helper := startArgvMock(t, "not-infrastructure")
	live := runtime.LiveRuntime{PID: watchdog.pid, StartIdentity: watchdog.identity}

	if infrastructure, err := RootIsCityInfrastructure(live); infrastructure || err != nil {
		t.Fatalf("RootIsCityInfrastructure read the live host without the test opting in: %v, %v", infrastructure, err)
	}
	defer allowLiveWindowsScanForTesting()()

	for _, tc := range []struct {
		name string
		live runtime.LiveRuntime
		want bool
	}{
		{name: "watchdog", live: live, want: true},
		{name: "helper", live: runtime.LiveRuntime{PID: helper.pid, StartIdentity: helper.identity}, want: false},
		// The scan saw an earlier process at this PID: the watchdog now holding
		// it is not the root, and KillRuntime will not touch it either.
		{name: "recycled", live: runtime.LiveRuntime{PID: watchdog.pid, StartIdentity: "1"}, want: false},
		{name: "gone", live: runtime.LiveRuntime{PID: impossiblePID, StartIdentity: "1"}, want: false},
	} {
		infrastructure, err := RootIsCityInfrastructure(tc.live)
		if err != nil || infrastructure != tc.want {
			t.Errorf("%s: RootIsCityInfrastructure(%+v) = %v, %v; want %v with no error", tc.name, tc.live, infrastructure, err, tc.want)
		}
	}
	if _, err := RootIsCityInfrastructure(runtime.LiveRuntime{PID: watchdog.pid}); err == nil {
		t.Error("RootIsCityInfrastructure classified a root without the start identity its scan observed")
	}
}

// TestRootIsCityInfrastructureRefusesARootItCannotRead is the regression test
// for an unreadable root reading as "not infrastructure", which the orphan kill
// then terminated. A watchdog that refuses PROCESS_VM_READ is still running and
// still a watchdog; what it is cannot be read, and that must be said.
func TestRootIsCityInfrastructureRefusesARootItCannotRead(t *testing.T) {
	watchdog := startArgvMock(t, ManagedDoltScopeWatchdogVerb)
	denyProcessRead(t, watchdog.pid)
	defer allowLiveWindowsScanForTesting()()

	infrastructure, err := RootIsCityInfrastructure(runtime.LiveRuntime{PID: watchdog.pid, StartIdentity: watchdog.identity})
	if err == nil {
		t.Fatalf("RootIsCityInfrastructure = %v with no error for a running root whose command line could not be read", infrastructure)
	}
	if !watchdog.running() {
		t.Fatalf("the watchdog mock, PID %d, exited during classification", watchdog.pid)
	}
}

// TestSplitWindowsCommandLineRefusesMoreArgumentsThanItCanRead pins that the
// argument bound is reported rather than silently applied. A real command line
// can carry more one-character arguments than the bound, and a truncated argv
// is a wrong one.
func TestSplitWindowsCommandLineRefusesMoreArgumentsThanItCanRead(t *testing.T) {
	atLimit := strings.TrimSpace(strings.Repeat("a ", maxCommandLineArguments))
	argv, err := splitWindowsCommandLine(atLimit)
	if err != nil || len(argv) != maxCommandLineArguments {
		t.Fatalf("splitWindowsCommandLine(%d arguments) = %d arguments, %v", maxCommandLineArguments, len(argv), err)
	}

	overLimit := atLimit + " a"
	if argv, err := splitWindowsCommandLine(overLimit); err == nil {
		t.Fatalf("splitWindowsCommandLine(%d arguments) returned %d and no error", maxCommandLineArguments+1, len(argv))
	}
}
