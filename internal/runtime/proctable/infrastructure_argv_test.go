package proctable

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
	"github.com/gastownhall/gascity/internal/runtime/programstatus"
)

func TestIsProgramStatusRelayArgv(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		want bool
	}{
		{name: "relay", argv: []string{"/opt/gc/bin/gc-osc7501", "relay", "--", "/bin/sh", "-c", "claude"}, want: true},
		{name: "bare name", argv: []string{"gc-osc7501", "relay"}, want: true},
		{name: "tap", argv: []string{"gc-osc7501", "tap", "-t", "%1", "-n", "r"}, want: false},
		{name: "other binary", argv: []string{"/bin/relay", "relay"}, want: false},
		{name: "verb only as a later argument", argv: []string{"sh", "-c", "gc-osc7501 relay"}, want: false},
		{name: "bare argv0", argv: []string{"gc-osc7501"}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsProgramStatusRelayArgv(tc.argv); got != tc.want {
				t.Fatalf("IsProgramStatusRelayArgv(%q) = %v, want %v", tc.argv, got, tc.want)
			}
		})
	}
}

func TestIsProgramStatusTapArgv(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		want bool
	}{
		{name: "tap", argv: []string{"/opt/gc/bin/gc-osc7501", "tap", "-t", "%3", "-n", "r", "-x", "/usr/bin/tmux", "-S", "/tmp/s"}, want: true},
		{name: "bare name", argv: []string{"gc-osc7501", "tap"}, want: true},
		{name: "relay", argv: []string{"gc-osc7501", "relay", "--", "/bin/sh", "-c", "claude"}, want: false},
		{name: "other binary", argv: []string{"/usr/bin/tap", "tap"}, want: false},
		{name: "verb only as a later argument", argv: []string{"sh", "-c", "exec gc-osc7501 tap -t %3"}, want: false},
		{name: "bare argv0", argv: []string{"gc-osc7501"}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsProgramStatusTapArgv(tc.argv); got != tc.want {
				t.Fatalf("IsProgramStatusTapArgv(%q) = %v, want %v", tc.argv, got, tc.want)
			}
		})
	}
}

// TestProgramStatusCompanionNameMatchesProgramStatus pins the duplicated
// companion name to the one gc arms with.
func TestProgramStatusCompanionNameMatchesProgramStatus(t *testing.T) {
	if ProgramStatusCompanionName != programstatus.CompanionName {
		t.Fatalf("ProgramStatusCompanionName = %q, want programstatus.CompanionName %q", ProgramStatusCompanionName, programstatus.CompanionName)
	}
}

func TestIsCityInfrastructureArgv(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		want bool
	}{
		{name: "scope watchdog", argv: []string{"/usr/local/bin/gc", ManagedDoltScopeWatchdogVerb, "/city/.beads/dolt-config.yaml", "/city/dolt.log", "/city"}, want: true},
		{name: "bd proxy child", argv: []string{"/opt/beads/bd-1.3.0", BDProxyChildVerb, "--root", "/city/.beads/proxy"}, want: true},
		{name: "agent runtime", argv: []string{"claude", "--resume"}, want: false},
		{name: "dolt server", argv: []string{"dolt", "sql-server", "--config", "/city/.beads/dolt-config.yaml"}, want: false},
		{name: "verb only as a later argument", argv: []string{"sh", "-c", ManagedDoltScopeWatchdogVerb}, want: false},
		{name: "bare argv0", argv: []string{BDProxyChildVerb}, want: false},
		{name: "empty", argv: nil, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsCityInfrastructureArgv(tc.argv); got != tc.want {
				t.Fatalf("IsCityInfrastructureArgv(%q) = %v, want %v", tc.argv, got, tc.want)
			}
		})
	}
}

// TestBDProxyChildVerbMatchesProxyEndpoint pins the duplicated verb to the one
// gc's proxy-ownership code (and bd) actually uses.
func TestBDProxyChildVerbMatchesProxyEndpoint(t *testing.T) {
	if BDProxyChildVerb != proxyendpoint.ChildVerb {
		t.Fatalf("BDProxyChildVerb = %q, want proxyendpoint.ChildVerb %q", BDProxyChildVerb, proxyendpoint.ChildVerb)
	}
}

func TestIsCityInfrastructureRootReadsInjectedProcfs(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("reads argv from a procfs-shaped tree")
	}
	root := t.TempDir()
	write := func(pid int, argv ...string) {
		t.Helper()
		dir := filepath.Join(root, strconv.Itoa(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.Join(argv, "\x00")+"\x00"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(100, "gc", ManagedDoltScopeWatchdogVerb, "cfg", "log", "/city")
	write(101, "bd", BDProxyChildVerb, "--root", "/city/.beads/proxy")
	write(102, "claude")

	// Without an injected root the fence never reads the host's live /proc.
	if IsCityInfrastructureRoot(100) {
		t.Fatal("IsCityInfrastructureRoot read the live /proc under go test")
	}
	SetScanRootForTesting(t, root)
	for pid, want := range map[int]bool{100: true, 101: true, 102: false, 103: false, 0: false} {
		if got := IsCityInfrastructureRoot(pid); got != want {
			t.Errorf("IsCityInfrastructureRoot(%d) = %v, want %v", pid, got, want)
		}
	}
}
