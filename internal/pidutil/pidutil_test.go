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
