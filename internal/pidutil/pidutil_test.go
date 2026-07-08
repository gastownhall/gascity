package pidutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestAliveTreatsZombieAsDead(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("zombie detection uses /proc on linux")
	}

	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = cmd.Wait() }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !Alive(cmd.Process.Pid) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("Alive(%d) stayed true for exited child", cmd.Process.Pid)
}

func TestPSReportsZombieReturnsWhenPSHangs(t *testing.T) {
	binDir := t.TempDir()
	psPath := filepath.Join(binDir, "ps")
	if err := os.WriteFile(psPath, []byte("#!/bin/sh\nexec sleep 10\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(ps): %v", err)
	}
	t.Setenv("PATH", strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)))

	start := time.Now()
	if got := psReportsZombie(os.Getpid()); got {
		t.Fatalf("psReportsZombie() = true, want false when ps hangs")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("psReportsZombie took %s, want bounded timeout", elapsed)
	}
}

func TestStartTimeStableForLivePID(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("start-time reads /proc/<pid>/stat on linux")
	}
	first, err := StartTime(os.Getpid())
	if err != nil {
		t.Fatalf("StartTime(%d): %v", os.Getpid(), err)
	}
	if first == "" {
		t.Fatalf("StartTime(%d) = empty, want a starttime token", os.Getpid())
	}
	second, err := StartTime(os.Getpid())
	if err != nil {
		t.Fatalf("StartTime(%d) second call: %v", os.Getpid(), err)
	}
	if first != second {
		t.Fatalf("StartTime not stable across calls: %q vs %q", first, second)
	}
}

func TestStartTimeRejectsInvalidPID(t *testing.T) {
	if _, err := StartTime(0); err == nil {
		t.Fatal("StartTime(0) = nil error, want error")
	}
}

// TestAliveWithStartTimeDisambiguatesRecycledPID checks the three branches that
// close the PID-reuse hole: a matching start time reports alive, a mismatched
// one (the recycled-PID case) reports dead even though the PID is live, and an
// empty start time falls back to plain liveness.
func TestAliveWithStartTimeDisambiguatesRecycledPID(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("start-time identity uses /proc on linux")
	}
	self := os.Getpid()
	st, err := StartTime(self)
	if err != nil {
		t.Fatalf("StartTime(%d): %v", self, err)
	}

	if !AliveWithStartTime(self, st) {
		t.Fatalf("AliveWithStartTime(%d, matching) = false, want alive", self)
	}
	// A different start-time token models the PID having been reaped and reused
	// by an unrelated process: the original target must read as dead.
	if AliveWithStartTime(self, st+"0") {
		t.Fatalf("AliveWithStartTime(%d, mismatched) = true, want dead (recycled)", self)
	}
	// Empty start time disables the identity check (darwin / uncaptured).
	if !AliveWithStartTime(self, "") {
		t.Fatalf("AliveWithStartTime(%d, empty) = false, want fallback to Alive", self)
	}
}

func TestAliveWithStartTimeDeadPID(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("spawning test process: %v", err)
	}
	pid := cmd.ProcessState.Pid()
	if AliveWithStartTime(pid, "12345") {
		t.Fatalf("AliveWithStartTime(%d, ...) = true for exited process", pid)
	}
}

func TestAliveWithCmdlineRejectsUnrelatedLivePID(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("cmdline detection uses /proc on linux")
	}

	if AliveWithCmdline(os.Getpid(), func(_ []string) bool {
		return false
	}) {
		t.Fatalf("AliveWithCmdline(%d) = true for non-matching cmdline", os.Getpid())
	}
}

func TestAliveWithCmdlineAcceptsMatchingLivePID(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("cmdline detection uses /proc on linux")
	}

	if !AliveWithCmdline(os.Getpid(), func(argv []string) bool {
		return len(argv) > 0 && strings.Contains(filepath.Base(argv[0]), "pidutil")
	}) {
		t.Fatalf("AliveWithCmdline(%d) = false for matching cmdline", os.Getpid())
	}
}

func TestCmdlineReturnsOwnArgv(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("cmdline detection uses /proc on linux")
	}

	argv, err := Cmdline(os.Getpid())
	if err != nil {
		t.Fatalf("Cmdline(%d): %v", os.Getpid(), err)
	}
	if len(argv) == 0 || !strings.Contains(filepath.Base(argv[0]), "pidutil") {
		t.Fatalf("Cmdline(%d) = %v, want test binary argv", os.Getpid(), argv)
	}
}

func TestNormalizeArgv(t *testing.T) {
	got := NormalizeArgv([]string{"cut", "", "-d", " ", "\t ", "-f", "1"})
	want := []string{"cut", "-d", "-f", "1"}
	if len(got) != len(want) {
		t.Fatalf("NormalizeArgv = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("NormalizeArgv = %q, want %q", got, want)
		}
	}
	if out := NormalizeArgv(nil); len(out) != 0 {
		t.Fatalf("NormalizeArgv(nil) = %q, want empty", out)
	}
}

func TestArgvContainsSequence(t *testing.T) {
	argv := []string{"gc", "nudge", "poll", "--city", "/tmp/city"}
	cases := []struct {
		name string
		seq  []string
		want bool
	}{
		{name: "empty sequence", seq: nil, want: true},
		{name: "contiguous sequence", seq: []string{"nudge", "poll"}, want: true},
		{name: "non-contiguous sequence", seq: []string{"gc", "poll"}, want: false},
		{name: "argv shorter than sequence", seq: []string{"gc", "nudge", "poll", "--city", "/tmp/city", "extra"}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ArgvContainsSequence(argv, tc.seq...); got != tc.want {
				t.Fatalf("ArgvContainsSequence(%v, %v) = %v, want %v", argv, tc.seq, got, tc.want)
			}
		})
	}
}

func TestArgvHasFlagValue(t *testing.T) {
	argv := []string{"gc", "nudge", "poll", "--city", "/tmp/city-a", "--session=s-worker"}
	cases := []struct {
		name  string
		flag  string
		value string
		want  bool
	}{
		{name: "space form", flag: "--city", value: "/tmp/city-a", want: true},
		{name: "equals form", flag: "--session", value: "s-worker", want: true},
		{name: "wrong value", flag: "--city", value: "/tmp/city-b", want: false},
		{name: "empty flag", flag: "", value: "/tmp/city-a", want: false},
		{name: "empty value", flag: "--city", value: "", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ArgvHasFlagValue(argv, tc.flag, tc.value); got != tc.want {
				t.Fatalf("ArgvHasFlagValue(%v, %q, %q) = %v, want %v", argv, tc.flag, tc.value, got, tc.want)
			}
		})
	}
}
