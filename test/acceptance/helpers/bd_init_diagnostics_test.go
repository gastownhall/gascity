package acceptancehelpers

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// These tests pin the bd tooling wrapper's failed-init diagnostics. gc rig add
// deletes the rig's new .beads when bd init fails, so the logs that explain the
// failure only exist while bd init is still returning. The wrapper that
// InstallBeadsTooling puts on PATH therefore prints them itself, before control
// goes back to gc, without disturbing anything else about the run.
//
// The tests build their commands with ToolCommand and execute them with
// CombinedOutput/Run, so no new os/exec construction lands in a test file.

const portConflictLine = "Port 43599 already in use."

// fakeInitBD describes the "real" bd a wrapper test installs the tooling
// around.
type fakeInitBD struct {
	status    int
	stdout    string
	stderr    string
	serverLog *string
	proxyLog  *string
}

func strPtr(s string) *string { return &s }

// install writes the fake real bd and returns it with the directory it keeps
// its fixtures and argv record in.
func (f fakeInitBD) install(t *testing.T) (bdPath, stateDir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stubs are POSIX-only")
	}
	stateDir = t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(stateDir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	stdoutSrc := write("stdout.txt", f.stdout)
	stderrSrc := write("stderr.txt", f.stderr)
	var copies strings.Builder
	if f.serverLog != nil {
		fmt.Fprintf(&copies, "cp %s \"$root/dolt/server.log\"\n", shellQuote(write("server.src", *f.serverLog)))
	}
	if f.proxyLog != nil {
		fmt.Fprintf(&copies, "cp %s \"$root/dolt/proxy.log\"\n", shellQuote(write("proxy.src", *f.proxyLog)))
	}
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$@" > %s
if [ "$1" = init ]; then
root=${BEADS_DIR:-$PWD/.beads}
mkdir -p "$root/dolt"
%sfi
cat %s
cat %s >&2
exit %d
`, shellQuote(filepath.Join(stateDir, "argv.txt")), copies.String(), shellQuote(stdoutSrc), shellQuote(stderrSrc), f.status)
	bdPath = filepath.Join(stateDir, "bd")
	if err := os.WriteFile(bdPath, []byte(script), 0o755); err != nil { //nolint:gosec // test stub must be executable
		t.Fatal(err)
	}
	return bdPath, stateDir
}

// wrapperResult is what the wrapper did, measured from outside.
type wrapperResult struct {
	stdout string
	stderr string
	status int
}

// runWrapper installs the tooling around bd and runs the wrapper it wrote.
func runWrapper(t *testing.T, bd, workdir string, extraEnv []string, args ...string) wrapperResult {
	t.Helper()
	wrapper, err := InstallBeadsTooling(newCanaryEnv(t), filepath.Join(t.TempDir(), "bin"), bd, "")
	if err != nil {
		t.Fatalf("InstallBeadsTooling: %v", err)
	}
	cmd := ToolCommand(t, wrapper, args...)
	if workdir != "" {
		cmd.Dir = workdir
	}
	cmd.Env = append(cmd.Env, extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	res := wrapperResult{stdout: stdout.String(), stderr: stderr.String()}
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
	case errors.As(runErr, &exitErr):
		res.status = exitErr.ExitCode()
	default:
		t.Fatalf("run wrapper: %v", runErr)
	}
	return res
}

func readArgv(t *testing.T, stateDir string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(stateDir, "argv.txt"))
	if err != nil {
		t.Fatalf("the real bd never ran: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// A failed init prints the rig's own server and proxy logs before it returns,
// so they are in gc's captured output when it deletes the scope.
func TestBDInitWrapperEmitsLogsBeforeControlReturns(t *testing.T) {
	scope := filepath.Join(t.TempDir(), "rig", ".beads")
	bd, _ := fakeInitBD{
		status:    1,
		stderr:    "Error: failed to open uow provider: uow: ping db: connection refused\n",
		serverLog: strPtr("Starting server\n" + portConflictLine + "\n"),
		proxyLog:  strPtr("proxy published\nbackend exited\n"),
	}.install(t)

	res := runWrapper(t, bd, "", []string{"BEADS_DIR=" + scope}, "init", "--server")
	// The caller (gc rig add) removes the scope only now; everything below was
	// already written by the time the wrapper exited.
	if err := os.RemoveAll(scope); err != nil {
		t.Fatal(err)
	}

	sections := ParseBDInitDiagnostics(res.stderr)
	if len(sections) != 2 {
		t.Fatalf("want a server.log and a proxy.log section, got %d:\n%s", len(sections), res.stderr)
	}
	wantPaths := []string{
		filepath.Join(scope, "dolt", "server.log"),
		filepath.Join(scope, "dolt", "proxy.log"),
	}
	for i, want := range wantPaths {
		if sections[i].Path != want {
			t.Errorf("section %d path = %q, want %q", i, sections[i].Path, want)
		}
	}
	if !strings.Contains(sections[0].Body, portConflictLine) {
		t.Errorf("server.log section lost the conflict line:\n%s", sections[0].Body)
	}
	if !strings.Contains(sections[1].Body, "backend exited") {
		t.Errorf("proxy.log section lost its content:\n%s", sections[1].Body)
	}
	if !strings.HasPrefix(res.stderr, "Error: failed to open uow provider") {
		t.Errorf("the real failure must come first and unchanged, got:\n%s", res.stderr)
	}
}

// The wrapper is transparent about the failure itself: same exit status, same
// stdout, same stderr text, same argv.
func TestBDInitWrapperPreservesStatusStreamsAndArgv(t *testing.T) {
	bd, state := fakeInitBD{
		status:    7,
		stdout:    "partial stdout\n",
		stderr:    "real stderr\n",
		serverLog: strPtr(portConflictLine + "\n"),
	}.install(t)
	scope := filepath.Join(t.TempDir(), ".beads")

	res := runWrapper(t, bd, "", []string{"BEADS_DIR=" + scope}, "init", "--server", "--prefix", "my rig", "--skip-hooks", "")

	if res.status != 7 {
		t.Errorf("exit status = %d, want the real bd's 7", res.status)
	}
	if res.stdout != "partial stdout\n" {
		t.Errorf("stdout = %q, want it unchanged", res.stdout)
	}
	if !strings.HasPrefix(res.stderr, "real stderr\n") {
		t.Errorf("stderr must start with the real stderr, got %q", res.stderr)
	}
	want := []string{"init", "--server", "--prefix", "my rig", "--skip-hooks", ""}
	if got := readArgv(t, state); strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("real bd argv = %q, want %q", got, want)
	}
}

// A successful init, and every other verb failing or not, is byte-for-byte what
// the plain tool-home wrapper produced.
func TestBDInitWrapperLeavesSuccessAndOtherVerbsUntouched(t *testing.T) {
	logs := strPtr(portConflictLine + "\n")
	for _, tc := range []struct {
		name       string
		status     int
		args       []string
		wantStatus int
	}{
		{"successful init", 0, []string{"init", "--server"}, 0},
		{"failing list", 3, []string{"list", "--json"}, 3},
		{"failing create", 4, []string{"create", "title"}, 4},
		{"failing init-like verb", 5, []string{"initialize"}, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bd, state := fakeInitBD{
				status: tc.status, stdout: "out\n", stderr: "err\n", serverLog: logs, proxyLog: logs,
			}.install(t)
			scope := filepath.Join(t.TempDir(), ".beads")
			// Present for non-init verbs too, so a wrapper that looked at the
			// logs regardless of the verb would be caught.
			if err := os.MkdirAll(filepath.Join(scope, "dolt"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(scope, "dolt", "server.log"), []byte(portConflictLine+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			res := runWrapper(t, bd, "", []string{"BEADS_DIR=" + scope}, tc.args...)

			if res.status != tc.wantStatus {
				t.Errorf("status = %d, want %d", res.status, tc.wantStatus)
			}
			if res.stdout != "out\n" || res.stderr != "err\n" {
				t.Errorf("output changed: stdout=%q stderr=%q", res.stdout, res.stderr)
			}
			if got := readArgv(t, state); strings.Join(got, " ") != strings.Join(tc.args, " ") {
				t.Errorf("argv = %q, want %q", got, tc.args)
			}
		})
	}
}

// A log that is not there is reported, and never replaces the failure bd had.
func TestBDInitWrapperReportsUnavailableLogsAndKeepsTheOriginalError(t *testing.T) {
	bd, _ := fakeInitBD{status: 9, stderr: "Error: original failure\n"}.install(t)
	scope := filepath.Join(t.TempDir(), ".beads")

	res := runWrapper(t, bd, "", []string{"BEADS_DIR=" + scope}, "init")

	if res.status != 9 {
		t.Errorf("status = %d, want 9", res.status)
	}
	if !strings.HasPrefix(res.stderr, "Error: original failure\n") {
		t.Errorf("original error not first:\n%s", res.stderr)
	}
	sections := ParseBDInitDiagnostics(res.stderr)
	if len(sections) != 2 {
		t.Fatalf("want one section per log, even unavailable ones, got %d:\n%s", len(sections), res.stderr)
	}
	for _, s := range sections {
		if !strings.Contains(s.Note, "unavailable") {
			t.Errorf("section %s note = %q, want it to say unavailable", s.Path, s.Note)
		}
		if strings.TrimSpace(s.Body) != "" {
			t.Errorf("unavailable section %s has a body: %q", s.Path, s.Body)
		}
	}
	if strings.Contains(res.stderr, "Port ") {
		t.Errorf("invented proof for a missing log:\n%s", res.stderr)
	}
}

// Oversized logs are cut to a head and a tail, and the startup conflict line
// survives wherever it sits.
func TestBDInitWrapperBoundsOversizedLogsAndKeepsTheConflictLine(t *testing.T) {
	filler := strings.Repeat("dolt noise line that is not interesting at all\n", 4000) // ~188 KiB
	cases := map[string]string{
		"conflict in the head":   portConflictLine + "\n" + filler + "usage trailer\n",
		"conflict in the middle": filler + portConflictLine + "\n" + filler + "usage trailer\n",
		"conflict then a long error trailer": "startup\n" + portConflictLine + "\n" +
			strings.Repeat("usage: bd init [flags] and a long wall of text\n", 4000),
	}
	for name, log := range cases {
		t.Run(name, func(t *testing.T) {
			bd, _ := fakeInitBD{status: 1, serverLog: strPtr(log), proxyLog: strPtr(log)}.install(t)
			scope := filepath.Join(t.TempDir(), ".beads")

			res := runWrapper(t, bd, "", []string{"BEADS_DIR=" + scope}, "init")

			sections := ParseBDInitDiagnostics(res.stderr)
			if len(sections) != 2 {
				t.Fatalf("want 2 sections, got %d", len(sections))
			}
			if !strings.Contains(sections[0].Body, "\n"+portConflictLine+"\n") && !strings.HasPrefix(sections[0].Body, portConflictLine+"\n") {
				t.Errorf("server.log section lost the exact conflict line (len %d)", len(sections[0].Body))
			}
			// Per log: head + tail + the retained match lines + the markers.
			bound := BDInitDiagnosticsHeadBytes + BDInitDiagnosticsTailBytes + 2048
			for _, s := range sections {
				if len(s.Body) > bound {
					t.Errorf("section %s is %d bytes, over its %d byte bound", s.Path, len(s.Body), bound)
				}
			}
			if len(res.stderr) > 2*(bound+1024) {
				t.Errorf("total diagnostics are %d bytes; the bound did not hold", len(res.stderr))
			}
		})
	}
}

// With BEADS_DIR unset bd init works in $PWD/.beads, and so do the diagnostics.
func TestBDInitWrapperFallsBackToTheWorkingDirectoryScope(t *testing.T) {
	bd, _ := fakeInitBD{status: 1, serverLog: strPtr(portConflictLine + "\n")}.install(t)
	work := t.TempDir()

	res := runWrapper(t, bd, work, nil, "init")

	sections := ParseBDInitDiagnostics(res.stderr)
	if len(sections) == 0 {
		t.Fatalf("no diagnostics without BEADS_DIR:\n%s", res.stderr)
	}
	// The wrapper's sh derives $PWD from getcwd, which returns the physical
	// path. Resolve the expectation (never the input) so a symlinked temp
	// root, such as macOS /var -> /private/var, names the same file.
	physicalWork, err := filepath.EvalSymlinks(work)
	if err != nil {
		t.Fatalf("resolving %s: %v", work, err)
	}
	if want := filepath.Join(physicalWork, ".beads", "dolt", "server.log"); sections[0].Path != want {
		t.Errorf("path = %q, want %q", sections[0].Path, want)
	}
}

// The extra layer must not weaken the HOME/XDG isolation: the real bd still
// sees only the tool home, and a host canary is untouched.
func TestBDInitWrapperKeepsToolHomeIsolation(t *testing.T) {
	canary, untouched := hostCanary(t)
	bd, logDir := envRecordingBD(t)
	env := newCanaryEnv(t)
	wrapper, err := InstallBeadsTooling(env, filepath.Join(t.TempDir(), "bin"), bd, "")
	if err != nil {
		t.Fatal(err)
	}
	scope := filepath.Join(t.TempDir(), ".beads")
	cmd := ToolCommand(t, wrapper, "init", "--server")
	cmd.Env = append(env.List(), "BEADS_DIR="+scope)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("init via wrapper: %v\n%s", err, out)
	}
	assertBdSawNoHostHome(t, canary, recordedEnvs(t, logDir))
	untouched()
}

// ParseBDInitDiagnostics reads back what the wrapper wrote, even when gc has
// wrapped it in its own error text.
func TestParseBDInitDiagnostics(t *testing.T) {
	out := "gc rig add: provider-owned beads init: boom\n" +
		"--- bd init diagnostics: /r/.beads/dolt/server.log (120 bytes) ---\n" +
		"a\n" + portConflictLine + "\n" +
		"--- end bd init diagnostics: /r/.beads/dolt/server.log ---\n" +
		"--- bd init diagnostics: /r/.beads/dolt/proxy.log (unavailable: not a readable file) ---\n" +
		"--- end bd init diagnostics: /r/.beads/dolt/proxy.log ---\n" +
		"trailing gc text\n"

	got := ParseBDInitDiagnostics(out)

	if len(got) != 2 {
		t.Fatalf("got %d sections, want 2: %+v", len(got), got)
	}
	if got[0].Path != "/r/.beads/dolt/server.log" || got[0].Body != "a\n"+portConflictLine+"\n" {
		t.Errorf("section 0 = %+v", got[0])
	}
	if got[1].Path != "/r/.beads/dolt/proxy.log" || !strings.Contains(got[1].Note, "unavailable") || got[1].Body != "" {
		t.Errorf("section 1 = %+v", got[1])
	}
	if ParseBDInitDiagnostics("nothing to see\n") != nil {
		t.Error("output with no markers must parse to no sections")
	}
}
