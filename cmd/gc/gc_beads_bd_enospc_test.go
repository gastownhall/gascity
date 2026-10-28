package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// enospcDetectors returns the two copies of recovery_should_skip_due_to_enospc
// as shell source: the canonical helper (dolt-enospc.sh) and the inline
// fallback gc-beads-bd.sh defines when the helper is not beside it.
func enospcDetectors(t *testing.T) map[string]string {
	t.Helper()
	root := repoRootForLint(t)
	scripts := filepath.Join(root, "examples", "bd", "assets", "scripts")
	helper, err := os.ReadFile(filepath.Join(scripts, "dolt-enospc.sh"))
	if err != nil {
		t.Fatalf("read dolt-enospc.sh: %v", err)
	}
	script, err := os.ReadFile(filepath.Join(scripts, "gc-beads-bd.sh"))
	if err != nil {
		t.Fatalf("read gc-beads-bd.sh: %v", err)
	}
	// The inline copy sits inside the `else` branch, indented four spaces.
	m := regexp.MustCompile(`(?m)^else\n((?:    .*\n|\n)*?)fi\n`).FindAllSubmatch(script, -1)
	var inline string
	for _, sub := range m {
		if strings.Contains(string(sub[1]), "recovery_should_skip_due_to_enospc()") {
			inline = string(sub[1])
			break
		}
	}
	if inline == "" {
		t.Fatal("gc-beads-bd.sh has no inline recovery_should_skip_due_to_enospc fallback")
	}
	return map[string]string{"dolt-enospc.sh": string(helper), "gc-beads-bd.sh inline": inline}
}

// TestENOSPCSkipHoldsOnlyWhileTheDiskIsShort pins that ENOSPC evidence in the
// dolt log skips recovery while the data dir is still short of space, and
// stops skipping once space is back: a stopped server writes nothing to its
// log, so the log tail alone never ages an ENOSPC line out.
func TestENOSPCSkipHoldsOnlyWhileTheDiskIsShort(t *testing.T) {
	const gib = 1 << 20 // in the 1024-byte blocks df -Pk reports
	cases := []struct {
		name     string
		log      string
		availKB  string // fake df Available column; "" makes df fail
		floor    string // GC_DOLT_WARN_FREE_BYTES; "" leaves it unset
		wantSkip bool
	}{
		{name: "no ENOSPC in log", log: "Server ready\n", availKB: "1", wantSkip: false},
		{name: "ENOSPC and disk still short", log: "fatal: no space left on device\n", availKB: strconv.Itoa(gib), wantSkip: true},
		{name: "ENOSPC but space is back", log: "fatal: no space left on device\n", availKB: strconv.Itoa(3 * gib), wantSkip: false},
		{name: "ENOSPC at exactly the floor", log: "ENOSPC\n", availKB: strconv.Itoa(2 * gib), wantSkip: false},
		{name: "ENOSPC and df fails", log: "copy_file_range: no space\n", availKB: "", wantSkip: true},
		{name: "ENOSPC with a raised floor", log: "ENOSPC\n", availKB: strconv.Itoa(3 * gib), floor: "4294967296", wantSkip: true},
		{name: "ENOSPC with a lowered floor", log: "ENOSPC\n", availKB: strconv.Itoa(gib), floor: "536870912", wantSkip: false},
		// 0 turns gc's disk warning off; it must leave the skip on the default floor.
		{name: "ENOSPC with floor 0 and disk short", log: "ENOSPC\n", availKB: strconv.Itoa(gib), floor: "0", wantSkip: true},
		{name: "ENOSPC with floor 0 and space back", log: "ENOSPC\n", availKB: strconv.Itoa(3 * gib), floor: "0", wantSkip: false},
	}
	for label, detector := range enospcDetectors(t) {
		for _, tc := range cases {
			t.Run(label+"/"+tc.name, func(t *testing.T) {
				dir := t.TempDir()
				dataDir := filepath.Join(dir, "data")
				if err := os.MkdirAll(dataDir, 0o755); err != nil {
					t.Fatalf("mkdir data: %v", err)
				}
				logFile := filepath.Join(dir, "dolt.log")
				if err := os.WriteFile(logFile, []byte(tc.log), 0o644); err != nil {
					t.Fatalf("write log: %v", err)
				}
				out, skipped := runENOSPCDetector(t, dir, detector, logFile, dataDir, tc.availKB, tc.floor)
				if skipped != tc.wantSkip {
					t.Fatalf("skip = %v, want %v\n%s", skipped, tc.wantSkip, out)
				}
			})
		}
	}
}

// TestENOSPCSkipMeasuresTheDataDir pins which filesystem the free-space check
// reads: DATA_DIR when the caller set it, else the log's directory.
func TestENOSPCSkipMeasuresTheDataDir(t *testing.T) {
	for label, detector := range enospcDetectors(t) {
		for _, withDataDir := range []bool{true, false} {
			name := "log dir"
			if withDataDir {
				name = "data dir"
			}
			t.Run(label+"/"+name, func(t *testing.T) {
				dir := t.TempDir()
				logDir := filepath.Join(dir, "packs")
				dataDir := filepath.Join(dir, "data")
				for _, d := range []string{logDir, dataDir} {
					if err := os.MkdirAll(d, 0o755); err != nil {
						t.Fatalf("mkdir: %v", err)
					}
				}
				logFile := filepath.Join(logDir, "dolt.log")
				if err := os.WriteFile(logFile, []byte("ENOSPC\n"), 0o644); err != nil {
					t.Fatalf("write log: %v", err)
				}
				useDataDir := ""
				want := logDir
				if withDataDir {
					useDataDir = dataDir
					want = dataDir
				}
				out, _ := runENOSPCDetector(t, dir, detector, logFile, useDataDir, strconv.Itoa(3<<20), "")
				args, err := os.ReadFile(filepath.Join(dir, "df.args"))
				if err != nil {
					t.Fatalf("df was not called: %v\n%s", err, out)
				}
				if got := strings.TrimSpace(string(args)); got != "-Pk "+want {
					t.Fatalf("df args = %q, want %q", got, "-Pk "+want)
				}
			})
		}
	}
}

// runENOSPCDetector sources detector in /bin/sh with a fake df on PATH and
// reports whether recovery_should_skip_due_to_enospc returned true.
func runENOSPCDetector(t *testing.T, dir, detector, logFile, dataDir, availKB, floor string) (string, bool) {
	t.Helper()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	df := `#!/bin/sh
printf '%s\n' "$*" > "` + filepath.Join(dir, "df.args") + `"
`
	if availKB == "" {
		df += "echo 'df: probe failed' >&2\nexit 1\n"
	} else {
		df += `echo 'Filesystem 1024-blocks Used Available Capacity Mounted on'
echo "/dev/fake 999999999 1 ` + availKB + ` 1% /"
`
	}
	writeExecutable(t, filepath.Join(binDir, "df"), df)
	harness := filepath.Join(dir, "harness.sh")
	writeExecutable(t, harness, "#!/bin/sh\n"+detector+"\nif recovery_should_skip_due_to_enospc; then echo SKIP; else echo RUN; fi\n")
	cmd := exec.Command("/bin/sh", harness)
	env := os.Environ()
	for _, key := range []string{"PATH", "LOG_FILE", "DATA_DIR", "GC_DOLT_DATA_DIR", "GC_DOLT_WARN_FREE_BYTES"} {
		env = removeEnvKey(env, key)
	}
	env = append(env, "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"), "LOG_FILE="+logFile)
	if dataDir != "" {
		env = append(env, "DATA_DIR="+dataDir)
	}
	if floor != "" {
		env = append(env, "GC_DOLT_WARN_FREE_BYTES="+floor)
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run detector: %v\n%s", err, out)
	}
	return string(out), strings.Contains(string(out), "SKIP")
}
