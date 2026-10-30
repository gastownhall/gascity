package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

const supervisorProcReaderSelfCheckEnv = "GC_TEST_SUPERVISOR_PROC_READER_SELF_CHECK"

// maybeRunSupervisorProcReaderSelfCheck runs before TestMain creates any
// fixtures. Its parent gives this test binary a matching kernel cmdline so
// self-exclusion cannot pass just because the caller is not a supervisor.
func maybeRunSupervisorProcReaderSelfCheck() {
	if os.Getenv(supervisorProcReaderSelfCheckEnv) != "1" {
		return
	}
	if p, ok := readLeakedSupervisorProc(os.Getpid()); !ok || p.Argv[0] != os.Args[0] {
		fmt.Fprintln(os.Stderr, "helper's kernel cmdline did not match supervisor run")
		os.Exit(2)
	}
	procs, err := discoverLeakedSupervisorProcs()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	for _, p := range procs {
		if p.PID == os.Getpid() {
			fmt.Fprintln(os.Stderr, "discovery included its matching caller")
			os.Exit(1)
		}
	}
	if _, err := fmt.Fprintln(os.Stdout, "matching caller excluded"); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestProcGCHomeStateClassifiesOnlyDefinitivePaths(t *testing.T) {
	for _, name := range []string{
		"existing absolute home is live",
		"removed absolute home is deleted",
		"unset home is unknown",
		"empty home is unknown",
		"relative home is unknown",
		"lookalike key is unknown",
		"other environment entries preserve home",
		"stat error is unknown",
		"missing environ is unknown",
		"unreadable environ is unknown",
	} {
		t.Run(name, func(t *testing.T) {
			pidDir := t.TempDir()
			home := t.TempDir()
			environ := "GC_HOME=" + home + "\x00"
			want := procPathStateUnknown
			switch name {
			case "existing absolute home is live":
				want = procPathStateLive
			case "removed absolute home is deleted":
				if err := os.Remove(home); err != nil {
					t.Fatal(err)
				}
				want = procPathStateDeleted
			case "unset home is unknown":
				environ = "PATH=/bin\x00"
			case "empty home is unknown":
				environ = "GC_HOME=\x00"
			case "relative home is unknown":
				environ = "GC_HOME=removed/home\x00"
			case "lookalike key is unknown":
				environ = "OTHER_GC_HOME=" + filepath.Join(home, "missing") + "\x00"
			case "other environment entries preserve home":
				environ = "OTHER_GC_HOME=/missing\x00GC_HOME=" + home + "\x00PATH=/bin\x00"
				want = procPathStateLive
			case "stat error is unknown":
				loop := filepath.Join(home, "loop")
				if err := os.Symlink("loop", loop); err != nil {
					t.Fatal(err)
				}
				environ = "GC_HOME=" + loop + "\x00"
			}
			path := filepath.Join(pidDir, "environ")
			switch name {
			case "missing environ is unknown":
			case "unreadable environ is unknown":
				// Reading a directory fails even when the test runs as root.
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.WriteFile(path, []byte(environ), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if got := procGCHomeState(pidDir); got != want {
				t.Fatalf("GC_HOME state = %q, want %q", got, want)
			}
		})
	}
}

func TestReadLeakedSupervisorProcRejectsMissingPID(t *testing.T) {
	if p, ok := readLeakedSupervisorProc(0); ok {
		t.Fatalf("PID 0 returned a supervisor: %+v", p)
	}
}

func TestDiscoverLeakedSupervisorProcsWithoutProcReturnsNone(t *testing.T) {
	if _, err := os.Stat("/proc"); !os.IsNotExist(err) {
		t.Skip("requires a host without /proc")
	}
	procs, err := discoverLeakedSupervisorProcs()
	if err != nil {
		t.Fatalf("discovery without /proc: %v", err)
	}
	if len(procs) != 0 {
		t.Fatalf("discovered %v without /proc", procs)
	}
}
