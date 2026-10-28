package genshell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/shellquote"
)

func TestParseSplitsAGeneratedCommand(t *testing.T) {
	script := `printf "%s" "$1"; printf 'it'\''s'`
	command := shellquote.Join([]string{"sh", "-c", script, "--", "rig1/worker", "rig1/workflow-control"})

	prog, err := Parse(command)
	if err != nil {
		t.Fatalf("Parse(%q): %v", command, err)
	}
	if prog.Script != script {
		t.Errorf("Script = %q, want %q", prog.Script, script)
	}
	if prog.Name != "--" {
		t.Errorf("Name = %q, want %q", prog.Name, "--")
	}
	if want := []string{"rig1/worker", "rig1/workflow-control"}; !slices.Equal(prog.Args, want) {
		t.Errorf("Args = %q, want %q", prog.Args, want)
	}
	if len(prog.Assigns) != 0 {
		t.Errorf("Assigns = %q, want none", prog.Assigns)
	}
}

func TestParseReadsPrefixAssignmentsAndQuoting(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		want    Program
	}{
		{
			name:    "assignments as prefixShellEnv writes them",
			command: "GC_DOLT_HOST=" + shellquote.Quote("127.0.0.1") + " GC_DOLT_PORT=" + shellquote.Quote("3307") + " " + shellquote.Join([]string{"sh", "-c", "printf x", "--", "a"}),
			want:    Program{Assigns: []string{"GC_DOLT_HOST=127.0.0.1", "GC_DOLT_PORT=3307"}, Script: "printf x", Name: "--", Args: []string{"a"}},
		},
		{
			name:    "no name",
			command: shellquote.Join([]string{"sh", "-c", `printf "[]"`}),
			want:    Program{Script: `printf "[]"`, Name: "sh"},
		},
		{
			name:    "name only",
			command: `sh -c 'printf x' name`,
			want:    Program{Script: "printf x", Name: "name", Args: []string{}},
		},
		{
			name:    "double quotes and escapes",
			command: `EMPTY= sh -c "printf \"%s\" \$1" -- "x y" a\ b ''`,
			want:    Program{Assigns: []string{"EMPTY="}, Script: `printf "%s" $1`, Name: "--", Args: []string{"x y", "a b", ""}},
		},
		{
			name:    "escaped glob characters",
			command: `sh -c x -- a\*b \~`,
			want:    Program{Script: "x", Name: "--", Args: []string{"a*b", "~"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.command)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.command, err)
			}
			if !slices.Equal(got.Assigns, tc.want.Assigns) || got.Script != tc.want.Script || got.Name != tc.want.Name || !slices.Equal(got.Args, tc.want.Args) {
				t.Errorf("Parse(%q) = %#v, want %#v", tc.command, got, tc.want)
			}
		})
	}
}

func TestParseRefusesAnythingElse(t *testing.T) {
	for _, command := range []string{
		``,
		`printf x`,
		`bash -c 'printf x'`,
		`/bin/sh -c 'printf x'`,
		`sh -e -c 'printf x'`,
		`sh -c`,
		`sh 'printf x'`,
		`sh -c 'printf x'; rm -rf city`,
		`sh -c 'printf x' && rm -rf city`,
		`sh -c 'printf x' | jq .`,
		`sh -c 'printf x' > out`,
		`sh -c 'printf x' 2>/dev/null`,
		`! sh -c 'printf x'`,
		`sh -c 'printf x' &`,
		`(sh -c 'printf x')`,
		`{ sh -c 'printf x'; }`,
		`sh -c "$SCRIPT"`,
		`sh -c $SCRIPT`,
		`sh -c "$(cat script)"`,
		"sh -c `cat script`",
		`sh -c 'printf x' -- "a$(id)"`,
		`sh -c 'printf x' -- *`,
		`sh -c 'printf x' -- rig?`,
		`sh -c 'printf x' -- [ab]`,
		`sh -c 'printf x' -- ~`,
		`sh -c 'printf x' -- ~/x`,
		`sh -c 'printf x' -- {a,b}`,
		`FOO=$BAR sh -c 'printf x'`,
		`FOO="$(id)" sh -c 'printf x'`,
		`FOO=~ sh -c 'printf x'`,
		`FOO+=x sh -c 'printf x'`,
	} {
		t.Run(command, func(t *testing.T) {
			prog, err := Parse(command)
			if !errors.Is(err, ErrUnsupported) {
				t.Fatalf("Parse(%q) = %#v, %v; want an error wrapping ErrUnsupported", command, prog, err)
			}
		})
	}
}

// runScript runs script with args through Run and returns its streams.
func runScript(t *testing.T, script string, opts Options, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	opts.Stdout = &out
	opts.Stderr = &errOut
	if opts.Env == nil {
		opts.Env = []string{"PATH=" + t.TempDir()}
	}
	err = Program{Script: script, Name: "--", Args: args}.Run(context.Background(), opts)
	return out.String(), errOut.String(), err
}

func TestRunInterpretsTheShellOfGeneratedPrograms(t *testing.T) {
	script := `probe() { target="$1"; [ -z "$target" ] && return 1; printf "probe:%s\n" "$target"; return 0; }; ` +
		`for id in "" "a" "b"; do [ -z "$id" ] && continue; case "$id" in a) printf "A\n" ;; *) printf "other:%s\n" "$id" ;; esac; done; ` +
		`legacy="x-control-dispatcher"; printf "%s\n" "${legacy%control-dispatcher}workflow-control"; ` +
		`[ -n "${never_set:-}" ] || { v=$(printf "%s" "sub"); printf "%s\n" "$v"; }; ` +
		`probe "" || printf "empty-target\n"; ` +
		`probe "$1"; ` +
		`if [ "$1" = "t1" ] && [ "$2" != "t1" ]; then printf "args-ok\n"; else printf "args-bad\n"; fi; ` +
		`r=$(probe "$2") && printf "captured:%s\n" "$r"; ` +
		`printf "%s\n%s\n" "one" "two"; ` +
		`exit 3`

	stdout, stderr, err := runScript(t, script, Options{}, "t1", "t2")

	want := "A\nother:b\nx-workflow-control\nsub\nempty-target\nprobe:t1\nargs-ok\ncaptured:probe:t2\none\ntwo\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q (stderr %q)", stdout, want, stderr)
	}
	var exit *ExitError
	if !errors.As(err, &exit) || exit.Status != 3 {
		t.Errorf("err = %v, want exit status 3", err)
	}
}

func TestRunGivesTheScriptItsNameAndArgs(t *testing.T) {
	stdout, stderr, err := runScript(t, `printf "%s|%s|%s|%s" "$0" "$1" "$2" "$#"`, Options{}, "a b", "c")
	if err != nil {
		t.Fatalf("Run: %v (stderr %q)", err, stderr)
	}
	if want := "--|a b|c|2"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

// TestRunSupportsTheDeclarationBuiltins pins the language the script is read
// in: read as POSIX, the interpreter refuses export and readonly, which dash
// and bash both run.
func TestRunSupportsTheDeclarationBuiltins(t *testing.T) {
	script := `export GC_A=1; readonly GC_B=2; f() { local x=3; printf "%s" "$x"; }; ` +
		`printf "%s %s " "$GC_A" "$GC_B"; f; printf " [%s]" "${x:-}"`
	stdout, stderr, err := runScript(t, script, Options{})
	if err != nil {
		t.Fatalf("Run: %v (stderr %q)", err, stderr)
	}
	if want := "1 2 3 []"; stdout != want {
		t.Errorf("stdout = %q, want %q (stderr %q)", stdout, want, stderr)
	}
}

func TestRunAddsTheAssignmentsToTheEnvironment(t *testing.T) {
	var out bytes.Buffer
	prog := Program{Assigns: []string{"GC_X=assigned"}, Script: `printf "%s %s" "$GC_X" "$GC_Y"`}
	err := prog.Run(context.Background(), Options{Env: []string{"GC_X=inherited", "GC_Y=kept"}, Stdout: &out})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if want := "assigned kept"; out.String() != want {
		t.Errorf("stdout = %q, want %q", out.String(), want)
	}
}

func TestRunReportsTheExitStatus(t *testing.T) {
	for _, tc := range []struct {
		script string
		want   int
	}{
		{script: `printf x`, want: 0},
		{script: `exit 0`, want: 0},
		{script: `false`, want: 1},
		{script: `exit 7`, want: 7},
		{script: `[ -n "" ]`, want: 1},
	} {
		t.Run(tc.script, func(t *testing.T) {
			_, _, err := runScript(t, tc.script, Options{})
			if tc.want == 0 {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			var exit *ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != tc.want {
				t.Fatalf("err = %v, want exit status %d", err, tc.want)
			}
		})
	}
}

// TestRunRefusesOtherPrograms pins that a script starts no program but bd and
// gc, and runs awk and xargs only in the forms gc generates (shims_test.go).
func TestRunRefusesOtherPrograms(t *testing.T) {
	// A stand-in for each refused program, first on PATH, leaves a marker if
	// it ever runs.
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	names := []string{"cat", "awk", "xargs", "sh", "rm", "grep"}
	for _, name := range names {
		writeMarkerStandIn(t, dir, name, marker)
	}
	// The control: each stand-in does leave the marker when run.
	control := exec.Command(standInPath(dir, "cat"))
	if out, err := control.CombinedOutput(); err != nil {
		t.Fatalf("control: run the cat stand-in: %v\n%s", err, out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("control: the cat stand-in left no marker: %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatalf("remove control marker: %v", err)
	}

	var calls []string
	opts := Options{
		Env: []string{"PATH=" + dir},
		Exec: func(_ context.Context, p Process) error {
			calls = append(calls, p.Args[0])
			return nil
		},
	}
	for _, script := range []string{
		`cat /dev/null`,
		`awk 'BEGIN { print 1 }'`,
		`printf 'x\n' | xargs cat`,
		`sh -c 'printf x'`,
		`rm -f anything`,
		`printf x | grep x`,
		`command cat /dev/null`,
		`eval cat /dev/null`,
		`exec cat /dev/null`,
	} {
		t.Run(script, func(t *testing.T) {
			stdout, stderr, err := runScript(t, script+`; printf "status:%s" "$?"`, opts)
			if strings.HasPrefix(script, "exec ") {
				// exec replaces the shell, so the status is the script's.
				var exit *ExitError
				if !errors.As(err, &exit) || exit.Status != 127 {
					t.Fatalf("err = %v, want exit status 127 (stdout %q, stderr %q)", err, stdout, stderr)
				}
			} else if stdout != "status:127" {
				t.Fatalf("stdout = %q, want %q (stderr %q, err %v)", stdout, "status:127", stderr, err)
			}
			if !strings.Contains(stderr, "not run") {
				t.Errorf("stderr = %q, want the refusal", stderr)
			}
		})
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("a refused program ran: marker stat err = %v", err)
	}
	if len(calls) != 0 {
		t.Errorf("Exec was asked to run %q, want nothing", calls)
	}
}

// writeMarkerStandIn writes an executable named name into dir that creates
// marker when run.
func writeMarkerStandIn(t *testing.T, dir, name, marker string) {
	t.Helper()
	path := standInPath(dir, name)
	body := "#!/bin/sh\n: > '" + marker + "'\n"
	if runtime.GOOS == "windows" {
		body = "@echo ran> \"" + marker + "\"\r\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write stand-in %s: %v", name, err)
	}
}

func standInPath(dir, name string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(dir, name+".cmd")
	}
	return filepath.Join(dir, name)
}

func TestRunSendsBdAndGcToExec(t *testing.T) {
	dir := t.TempDir()
	var (
		mu    sync.Mutex
		procs []Process
	)
	opts := Options{
		Dir: dir,
		Env: []string{"PATH=" + t.TempDir(), "GC_INHERITED=yes"},
		Exec: func(_ context.Context, p Process) error {
			mu.Lock()
			procs = append(procs, p)
			mu.Unlock()
			switch p.Args[0] + " " + p.Args[1] {
			case "bd list":
				fmt.Fprint(p.Stdout, `[{"id":"ga-1","title":"<one> & two"}]`)
			case "gc ready":
				fmt.Fprint(p.Stdout, `[]`)
			case "bd show":
				fmt.Fprint(p.Stderr, "bd show noise")
				fmt.Fprint(p.Stdout, `{"shown":true}`)
			}
			return nil
		},
	}
	script := `local_only=1; export GC_EXPORTED=2; ` +
		`bd list --json --limit=1 2>/dev/null | jq -c '.[0]'; ` +
		`r=$(gc ready --assignee="$1" --json); printf "gc:%s\n" "$r"; ` +
		`s=$(bd show "a b" 2>/dev/null); printf "show:%s\n" "$s"`

	stdout, stderr, err := runScript(t, script, opts, "rig1/worker")
	if err != nil {
		t.Fatalf("Run: %v (stderr %q)", err, stderr)
	}
	want := "{\"id\":\"ga-1\",\"title\":\"<one> & two\"}\ngc:[]\nshow:{\"shown\":true}\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want the 2>/dev/null to discard bd's", stderr)
	}

	if len(procs) != 3 {
		t.Fatalf("Exec ran %d processes, want 3", len(procs))
	}
	wantArgs := [][]string{
		{"bd", "list", "--json", "--limit=1"},
		{"gc", "ready", "--assignee=rig1/worker", "--json"},
		{"bd", "show", "a b"},
	}
	absDir, _ := filepath.Abs(dir)
	for i, p := range procs {
		if !slices.Equal(p.Args, wantArgs[i]) {
			t.Errorf("process %d Args = %q, want %q", i, p.Args, wantArgs[i])
		}
		if p.Dir != absDir {
			t.Errorf("process %d Dir = %q, want %q", i, p.Dir, absDir)
		}
		if !hasEnv(p.Env, "GC_INHERITED", "yes") || !hasEnv(p.Env, "GC_EXPORTED", "2") {
			t.Errorf("process %d Env = %q, want GC_INHERITED=yes and GC_EXPORTED=2", i, p.Env)
		}
		if hasEnvName(p.Env, "local_only") {
			t.Errorf("process %d Env = %q, want no unexported local_only", i, p.Env)
		}
	}
}

func hasEnv(env []string, name, value string) bool {
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if strings.EqualFold(k, name) && v == value {
			return true
		}
	}
	return false
}

func hasEnvName(env []string, name string) bool {
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if strings.EqualFold(k, name) {
			return true
		}
	}
	return false
}

func TestRunGivesTheScriptExecStatuses(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		wantStatus string
		wantStderr string
	}{
		{name: "success", err: nil, wantStatus: "0"},
		{name: "exit status", err: &ExitError{Status: 4}, wantStatus: "4"},
		{name: "exit status above 255", err: &ExitError{Status: 0xC000013A}, wantStatus: "255"},
		{name: "failure claiming 0", err: &ExitError{Status: 0}, wantStatus: "1"},
		{name: "did not run", err: errors.New("bd: executable file not found"), wantStatus: "127", wantStderr: "bd: executable file not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := Options{Exec: func(context.Context, Process) error { return tc.err }}
			stdout, stderr, err := runScript(t, `bd ready; printf "%s" "$?"`, opts)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if stdout != tc.wantStatus {
				t.Errorf("status = %q, want %q", stdout, tc.wantStatus)
			}
			if !strings.Contains(stderr, tc.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tc.wantStderr)
			}
		})
	}
}

func TestRunWithoutExecRefusesBdAndGc(t *testing.T) {
	stdout, stderr, err := runScript(t, `bd ready; printf "%s " "$?"; gc ready; printf "%s" "$?"`, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stdout != "127 127" {
		t.Errorf("statuses = %q, want %q (stderr %q)", stdout, "127 127", stderr)
	}
}

func TestRunRedirectsToDevNull(t *testing.T) {
	stdout, stderr, err := runScript(t, `printf out >/dev/null; printf err 2>/dev/null >&2; read line </dev/null; printf "ok:%s" "$?"`, Options{})
	if err != nil {
		t.Fatalf("Run: %v (stderr %q)", err, stderr)
	}
	if stdout != "ok:1" {
		t.Errorf("stdout = %q, want %q", stdout, "ok:1")
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want nothing", stderr)
	}
}

func TestRunStopsWhenTheContextIsDone(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var out bytes.Buffer
	opts := Options{
		Env:    []string{"PATH=" + t.TempDir()},
		Stdout: &out,
		Exec: func(ctx context.Context, _ Process) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}
	started := time.Now()
	err := Program{Script: `r=$(bd ready); printf "after"`}.Run(ctx, opts)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("Run returned after %s, want promptly after the deadline", elapsed)
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want nothing after the deadline", out.String())
	}
}

func TestLookPathSearchesTheProcessPath(t *testing.T) {
	dir := t.TempDir()
	name := "bd"
	if runtime.GOOS == "windows" {
		name = "bd.exe"
		// An extensionless file is not a Windows executable.
		if err := os.WriteFile(filepath.Join(dir, "gc"), nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	want := filepath.Join(dir, name)
	if err := os.WriteFile(want, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + t.TempDir() + string(os.PathListSeparator) + dir, "PATHEXT=.COM;.EXE"}

	got, err := LookPath(Process{Args: []string{"bd"}, Dir: t.TempDir(), Env: env})
	if err != nil || got != want {
		t.Errorf("LookPath(bd) = %q, %v; want %q", got, err, want)
	}
	if runtime.GOOS == "windows" {
		if got, err := LookPath(Process{Args: []string{"gc"}, Dir: t.TempDir(), Env: env}); err == nil {
			t.Errorf("LookPath(gc) = %q, want not found for an extensionless file", got)
		}
	}
	if got, err := LookPath(Process{Args: []string{"bd"}, Dir: dir, Env: []string{"PATH=" + t.TempDir()}}); err == nil {
		t.Errorf("LookPath(bd) off PATH = %q, want not found even from bd's own directory", got)
	}
}
