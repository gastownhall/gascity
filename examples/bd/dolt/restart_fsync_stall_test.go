package dolt_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// stallFixture wires a fake gc (answering `dolt-state fsync-probe`) and the
// fake gc-beads-bd + helpers from restart_test.go into one city.
type stallFixture struct {
	cityPath string
	bdLog    string
	probeLog string
	env      []string
}

func newStallFixture(t *testing.T, probeMode string, probeMS int) *stallFixture {
	t.Helper()
	cityPath := t.TempDir()
	bdLog := writeFakeBeadsBDForRestart(t, cityPath, map[string]int{"stop": 0, "start": 0})

	// The fake city's .gc/scripts carries only the ENOSPC helper, so restart
	// must find dolt-fsync-stall.sh the way it finds a missing ENOSPC helper:
	// beside the real bd pack (GC_PACK_DIR/../assets/scripts).
	dataDir := filepath.Join(cityPath, "dolt-data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	probeLog := filepath.Join(cityPath, "probe.log")
	fakeGC := filepath.Join(cityPath, "fake-gc")
	body := fmt.Sprintf(`#!/bin/sh
if [ "$1 $2" = "dolt-state fsync-probe" ]; then
  shift 2
  printf '%%s\n' "$*" >> %q
  case %q in
    unfinished) printf 'status\tunfinished\nfsync_ms\t1000\n' ;;
    finished)   printf 'status\tfinished\nfsync_ms\t%d\n' ;;
    error)      printf 'status\terror\nerror\tboom\n'; exit 1 ;;
    exit1)      exit 1 ;;
    garbage)    printf 'not tab separated\n' ;;
  esac
  exit 0
fi
exit 1
`, probeLog, probeMode, probeMS)
	if err := os.WriteFile(fakeGC, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return &stallFixture{
		cityPath: cityPath,
		bdLog:    bdLog,
		probeLog: probeLog,
		env:      []string{"GC_BIN=" + fakeGC, "GC_DOLT_DATA_DIR=" + dataDir},
	}
}

func (f *stallFixture) opsRun(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(f.bdLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return strings.Join(strings.Fields(string(data)), " ")
}

func TestRestartRefusesFsyncStallUnlessForced(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode string
		ms   int
	}{
		{"unfinished probe", "unfinished", 0},
		{"finished at the warn threshold", "finished", 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := repoRoot(t)
			f := newStallFixture(t, tc.mode, tc.ms)

			out, err := runRestartWithEnv(t, f.cityPath, root, f.env)
			if err == nil {
				t.Fatalf("gc dolt restart unexpectedly ignored an fsync stall:\n%s", out)
			}
			for _, want := range []string{
				"gc dolt restart: Dolt's data directory fsync is stalled; refusing restart because it adds recovery fsync load to storage that is already stalled",
				"run gc dolt restart --force only if a restart is still required",
			} {
				if !strings.Contains(string(out), want) {
					t.Fatalf("refusal output missing %q:\n%s", want, out)
				}
			}
			if ops := f.opsRun(t); ops != "" {
				t.Fatalf("restart invoked gc-beads-bd (%q) despite the stall refusal", ops)
			}

			out, err = runRestartWithEnv(t, f.cityPath, root, f.env, "--force")
			if err != nil {
				t.Fatalf("gc dolt restart --force failed: %v\n%s", err, out)
			}
			if !strings.Contains(string(out), "--force set") {
				t.Fatalf("--force did not say it overrode the stall; output:\n%s", out)
			}
			if ops := f.opsRun(t); ops != "stop start" {
				t.Fatalf("forced restart ops = %q, want %q", ops, "stop start")
			}
		})
	}
}

func TestRestartProceedsWhenFsyncFast(t *testing.T) {
	root := repoRoot(t)
	f := newStallFixture(t, "finished", 12)
	out, err := runRestartWithEnv(t, f.cityPath, root, f.env)
	if err != nil {
		t.Fatalf("gc dolt restart refused a fast fsync: %v\n%s", err, out)
	}
	if ops := f.opsRun(t); ops != "stop start" {
		t.Fatalf("ops = %q, want %q", ops, "stop start")
	}
	probe, _ := os.ReadFile(f.probeLog)
	if !strings.Contains(string(probe), "--dir "+strings.TrimPrefix(f.env[1], "GC_DOLT_DATA_DIR=")) || !strings.Contains(string(probe), "--timeout-ms 1000") {
		t.Fatalf("probe not called with the data dir and the 1000ms default bound: %q", probe)
	}
}

func TestRestartFsyncGuardFailsOpenWithNote(t *testing.T) {
	for _, mode := range []string{"error", "exit1", "garbage"} {
		t.Run(mode, func(t *testing.T) {
			root := repoRoot(t)
			f := newStallFixture(t, mode, 0)
			out, err := runRestartWithEnv(t, f.cityPath, root, f.env)
			if err != nil {
				t.Fatalf("gc dolt restart refused although the probe was inconclusive: %v\n%s", err, out)
			}
			if n := strings.Count(string(out), "fsync-stall guard skipped"); n != 1 {
				t.Fatalf("fail-open lines = %d, want 1; output:\n%s", n, out)
			}
			if ops := f.opsRun(t); ops != "stop start" {
				t.Fatalf("ops = %q, want %q", ops, "stop start")
			}
		})
	}
}

func TestRestartFsyncGuardFailsOpenWhenGCBinUnset(t *testing.T) {
	root := repoRoot(t)
	f := newStallFixture(t, "unfinished", 0)
	env := []string{f.env[1]} // data dir only; no GC_BIN
	out, err := runRestartWithEnv(t, f.cityPath, root, env)
	if err != nil {
		t.Fatalf("gc dolt restart refused without a gc binary to probe with: %v\n%s", err, out)
	}
	if n := strings.Count(string(out), "fsync-stall guard skipped"); n != 1 {
		t.Fatalf("fail-open lines = %d, want 1; output:\n%s", n, out)
	}
}

func TestRestartENOSPCRefusalStillWinsOverFsyncStall(t *testing.T) {
	root := repoRoot(t)
	f := newStallFixture(t, "unfinished", 0)
	logPath := filepath.Join(f.cityPath, ".gc", "runtime", "packs", "dolt", "dolt.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("fatal: no space left on device\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runRestartWithEnv(t, f.cityPath, root, f.env)
	if err == nil {
		t.Fatalf("restart succeeded despite ENOSPC:\n%s", out)
	}
	if !strings.Contains(string(out), "recent Dolt log shows ENOSPC") || strings.Contains(string(out), "fsync is stalled") {
		t.Fatalf("ENOSPC refusal must come first and alone; output:\n%s", out)
	}
}

// volatileHealthFields strips the health JSON lines that differ run to run
// (clock, ephemeral listener port, latency) so two runs can be compared.
var volatileHealthFields = regexp.MustCompile(`(?m)^\s*"(timestamp|port|latency_ms)":.*$`)

// TestDotfileProbeFileIsInvisibleToDataDirListings pins AC9 for the scripts:
// every enumeration of the data directory in the dolt pack uses a
// directory-only glob, so the root-level probe dotfile is never treated as a
// database.
func TestDotfileProbeFileIsInvisibleToDataDirListings(t *testing.T) {
	root := repoRoot(t)
	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "dolt-data")
	if err := os.MkdirAll(filepath.Join(dataDir, "hq", ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func() string {
		env := append(reachableServerEnv(t, root, cityPath), "GC_DOLT_DATA_DIR="+dataDir)
		out, err := newHealthScriptCmd(root, env, "--json").Output()
		if err != nil {
			t.Fatalf("health.sh --json failed: %v\n%s", err, out)
		}
		return volatileHealthFields.ReplaceAllString(string(out), "")
	}
	before := run()
	if err := os.WriteFile(filepath.Join(dataDir, ".gc-fsync-probe-123"), make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	after := run()
	if before != after {
		t.Fatalf("health.sh output changed when a probe dotfile appeared in the data dir:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// TestRestartFsyncGuardFailsOpenWhenHelperDirMissing covers a dolt pack whose
// sibling bd assets directory is absent: the fallback helper lookup must not
// abort restart under set -e, and the guard fails open with one note.
func TestRestartFsyncGuardFailsOpenWhenHelperDirMissing(t *testing.T) {
	for _, args := range [][]string{nil, {"--force"}} {
		t.Run(fmt.Sprintf("args %v", args), func(t *testing.T) {
			root := repoRoot(t)
			f := newStallFixture(t, "unfinished", 0)
			// GC_PACK_DIR/../assets/scripts does not exist under this pack dir.
			packDir := filepath.Join(t.TempDir(), "dolt")
			if err := os.MkdirAll(packDir, 0o755); err != nil {
				t.Fatal(err)
			}
			env := append(append([]string{}, f.env...), "GC_PACK_DIR="+packDir)
			out, err := runRestartWithEnv(t, f.cityPath, root, env, args...)
			if err != nil {
				t.Fatalf("gc dolt restart failed without the fsync-stall helper: %v\n%s", err, out)
			}
			if n := strings.Count(string(out), "fsync-stall guard skipped"); n != 1 {
				t.Fatalf("fail-open lines = %d, want 1; output:\n%s", n, out)
			}
			if ops := f.opsRun(t); ops != "stop start" {
				t.Fatalf("ops = %q, want %q", ops, "stop start")
			}
		})
	}
}
