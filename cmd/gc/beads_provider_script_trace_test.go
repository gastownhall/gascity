package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// providerScriptTraceRecord is the subset of gc's JSONL bd-trace record these
// assertions read.
type providerScriptTraceRecord struct {
	Source   string   `json:"source"`
	Args     []string `json:"args"`
	Dir      string   `json:"dir"`
	ExitCode int      `json:"exit_code"`
	Error    string   `json:"err"`
}

// providerScriptTraceRecords reads the JSONL trace and returns the
// provider-script records in it.
func providerScriptTraceRecords(t *testing.T, path string) []providerScriptTraceRecord {
	t.Helper()
	type record = providerScriptTraceRecord
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read trace: %v", err)
	}
	var out []record
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode trace line %q: %v", line, err)
		}
		if rec.Source == providerScriptTraceSource {
			out = append(out, rec)
		}
	}
	return out
}

// writeProviderScriptDouble writes a provider script that exits with the code
// its first argument names, so a trace assertion can drive both outcomes.
func writeProviderScriptDouble(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "gc-beads-double")
	body := "#!/bin/sh\ncase \"$1\" in\n  fail) exit 7 ;;\n  *) exit 0 ;;\nesac\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil { //nolint:gosec // the double must be executable
		t.Fatal(err)
	}
	return path
}

// TestProviderScriptTraceRecordsEveryScriptFork pins the records the three
// script exec sites now emit.
//
// Without them gc's bd trace is a partial census of its own subject: the calls
// gc makes in-process are recorded, and the ones it delegates to the provider
// script — a separate process that writes nothing — are not. Those delegated
// calls are exactly the ones the proxied topology added, so a fork measurement
// taken without them would be most wrong about the thing being measured.
func TestProviderScriptTraceRecordsEveryScriptFork(t *testing.T) {
	dir := t.TempDir()
	trace := filepath.Join(dir, "trace.jsonl")
	t.Setenv("GC_BD_TRACE_JSON", trace)
	script := writeProviderScriptDouble(t, dir)
	environ := []string{"GC_CITY_PATH=" + dir, "PATH=" + os.Getenv("PATH")}

	if err := runProviderOwnedOpStrict(t.Context(), 30*time.Second, script, environ, "ensure-ready"); err != nil {
		t.Fatalf("provider-owned ensure-ready: %v", err)
	}
	if err := runProviderOpWithEnv(script, environ, "health"); err != nil {
		t.Fatalf("exec beads health: %v", err)
	}
	if err := runProviderOwnedOpStrict(t.Context(), 30*time.Second, script, environ, "fail"); err == nil {
		t.Fatal("a provider op that exited 7 reported success")
	}

	records := providerScriptTraceRecords(t, trace)
	if len(records) != 3 {
		t.Fatalf("recorded %d provider-script call(s), want 3:\n%+v", len(records), records)
	}
	for i, want := range []string{"ensure-ready", "health", "fail"} {
		// argv[0] is the script, so the op is the second element — the same
		// shape the in-process records use, where argv[0] is "bd".
		if len(records[i].Args) < 2 || records[i].Args[1] != want {
			t.Errorf("record %d args = %v, want the %s op", i, records[i].Args, want)
		}
		if records[i].Args[0] != filepath.Base(script) {
			t.Errorf("record %d names %q, want the script %q", i, records[i].Args[0], filepath.Base(script))
		}
		if records[i].Dir != dir {
			t.Errorf("record %d dir = %q, want the city %q", i, records[i].Dir, dir)
		}
	}
	if records[0].ExitCode != 0 || records[1].ExitCode != 0 {
		t.Errorf("successful ops recorded exit codes %d and %d, want 0", records[0].ExitCode, records[1].ExitCode)
	}
	// The child's own status, not a generic failure marker: an op that exits 2
	// ("not needed") and one that exits 7 are different facts, and a trace that
	// flattened them would hide a provider that is refusing work.
	if records[2].ExitCode != 7 {
		t.Errorf("the failing op recorded exit code %d, want 7", records[2].ExitCode)
	}
	if records[2].Error == "" {
		t.Error("the failing op recorded no error")
	}
}

// TestProviderScriptTraceIsOffWithoutTheEnvVar pins that the records cost
// nothing on an ordinary run: an unset trace variable writes no file at all.
func TestProviderScriptTraceIsOffWithoutTheEnvVar(t *testing.T) {
	dir := t.TempDir()
	trace := filepath.Join(dir, "trace.jsonl")
	t.Setenv("GC_BD_TRACE_JSON", "")
	script := writeProviderScriptDouble(t, dir)

	if err := runProviderOpWithEnv(script, []string{"GC_CITY_PATH=" + dir}, "health"); err != nil {
		t.Fatalf("exec beads health: %v", err)
	}
	if _, err := os.Stat(trace); !os.IsNotExist(err) {
		t.Fatalf("a run with no trace variable wrote %s (%v)", trace, err)
	}
}

// TestProviderScriptTraceExitCodeForANonExitFailure pins the one case a bare
// ExitCode() cannot express. A context kill or a spawn failure produces no
// child status, and recording 0 for those would make a failed op read as a
// successful one in the very count it was added to inform.
func TestProviderScriptTraceExitCodeForANonExitFailure(t *testing.T) {
	dir := t.TempDir()
	trace := filepath.Join(dir, "trace.jsonl")
	t.Setenv("GC_BD_TRACE_JSON", trace)

	traceProviderScriptCall(filepath.Join(dir, "gc-beads-double"), dir, []string{"start"}, time.Now(), errors.New("signal: killed"))

	records := providerScriptTraceRecords(t, trace)
	if len(records) != 1 {
		t.Fatalf("recorded %d call(s), want 1", len(records))
	}
	if records[0].ExitCode != -1 {
		t.Fatalf("a failure with no child status recorded exit code %d, want -1", records[0].ExitCode)
	}
	if records[0].Error == "" {
		t.Error("a failure with no child status recorded no error either; -1 alone does not say what happened")
	}
	// The other side of this branch — an *exec.ExitError reporting the child's
	// own status — is asserted where a real child produces one: the exit-7 case
	// in TestProviderScriptTraceRecordsEveryScriptFork. It cannot be asserted
	// here, because an ExitError's status lives in an os.ProcessState no test can
	// fabricate, and an errors.As over a plain error tests nothing about the
	// branch it names.
}

// TestProviderScriptTraceDir pins where the record's dir comes from. The script
// runs with no working directory of its own, so the city has to be read back
// out of the environment gc built for it.
func TestProviderScriptTraceDir(t *testing.T) {
	cases := []struct {
		name    string
		environ []string
		want    string
	}{
		{name: "nil environment", want: ""},
		{name: "no city", environ: []string{"PATH=/usr/bin", "HOME=/home/u"}, want: ""},
		{name: "the city", environ: []string{"PATH=/usr/bin", "GC_CITY_PATH=/city", "HOME=/home/u"}, want: "/city"},
		{
			// A prefix match would take GC_CITY_PATH_OVERRIDE for the city.
			name:    "a longer key that starts the same way",
			environ: []string{"GC_CITY_PATH_EXTRA=/not-the-city", "GC_CITY_PATH=/city"},
			want:    "/city",
		},
		{name: "an empty city", environ: []string{"GC_CITY_PATH="}, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := providerScriptTraceDir(tc.environ); got != tc.want {
				t.Fatalf("providerScriptTraceDir = %q, want %q", got, tc.want)
			}
		})
	}
}
