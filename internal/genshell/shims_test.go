package genshell

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/shellquote"
)

func TestAwkPrintsEachLineWithAFieldOnce(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
	}{
		{name: "repeats anywhere in the input", input: "b\na\nb\nc\na\n", want: "b\na\nc\n"},
		{name: "empty lines and lines of blanks", input: "\na\n \t\n\nb\n", want: "a\nb\n"},
		{name: "a line is its full text", input: "a\n a\na \n a\n", want: "a\n a\na \n"},
		{name: "a carriage return is text", input: "a\r\na\n", want: "a\r\na\n"},
		{name: "a last line without a newline", input: "a\nb", want: "a\nb\n"},
		{name: "a repeated last line without a newline", input: "a\na", want: "a\n"},
		{name: "no input", input: "", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			status := runAwk([]string{awkDedupeProgram}, strings.NewReader(tc.input), &out, &errOut)
			if status != 0 || out.String() != tc.want {
				t.Errorf("awk over %q = %q, status %d; want %q, status 0 (stderr %q)", tc.input, out.String(), status, tc.want, errOut.String())
			}
		})
	}
}

// TestAwkInAScriptPipeline pins that a script runs the one awk form in
// process, between the commands it pipes from and to, with awk's status.
func TestAwkInAScriptPipeline(t *testing.T) {
	script := `printf "b\na\n\nb\n" | awk 'NF && !seen[$0]++' | while read -r l; do printf "<%s>" "$l"; done; ` +
		`printf "a\n" | awk 'NF && !seen[$0]++' > /dev/null; printf "status:%s" "$?"`
	stdout, stderr, err := runScript(t, script, Options{})
	if err != nil {
		t.Fatalf("Run: %v (stderr %q)", err, stderr)
	}
	if want := "<b><a>status:0"; stdout != want {
		t.Errorf("stdout = %q, want %q (stderr %q)", stdout, want, stderr)
	}
}

func TestAwkRefusesEveryOtherProgram(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"{ print $1 }"},
		{awkDedupeProgram + " "},
		{"-F:", awkDedupeProgram},
		{awkDedupeProgram, "ids.txt"},
	} {
		var out, errOut bytes.Buffer
		status := runAwk(args, strings.NewReader("a\n"), &out, &errOut)
		if status != 127 || out.Len() != 0 || !strings.Contains(errOut.String(), "not run") {
			t.Errorf("awk %q = %q, status %d, stderr %q; want nothing printed, status 127 and the refusal", args, out.String(), status, errOut.String())
		}
	}
}

// xargsRun runs `xargs -rI{} sh -c nested _ {}` over input in a script, and
// returns its streams and the status the script saw.
func xargsRun(t *testing.T, input, nested string, opts Options) (stdout, stderr, status string) {
	t.Helper()
	opts.Stdin = strings.NewReader(input)
	stdout, stderr, err := runScript(t, `xargs -rI{} sh -c `+shellquote.Quote(nested)+` _ {}; printf "status:%s" "$?"`, opts)
	if err != nil {
		t.Fatalf("Run: %v (stdout %q, stderr %q)", err, stdout, stderr)
	}
	stdout, status, _ = strings.Cut(stdout, "status:")
	return stdout, stderr, status
}

func TestXargsRunsTheScriptOnceForEachLine(t *testing.T) {
	stdout, stderr, status := xargsRun(t, "a\n  b\n\n \t\nc d\ne", `printf "<%s|%s|%s>" "$0" "$1" "$#"`, Options{})
	if want := "<_|a|1><_|b|1><_|c d|1><_|e|1>"; stdout != want || status != "0" {
		t.Errorf("runs = %q, status %s; want %q, status 0 (stderr %q)", stdout, status, want, stderr)
	}
}

func TestXargsWithNoInputRunsNothing(t *testing.T) {
	for _, input := range []string{"", "\n", " \t\n\n"} {
		stdout, stderr, status := xargsRun(t, input, `printf ran`, Options{})
		if stdout != "" || status != "0" {
			t.Errorf("over %q: runs = %q, status %s; want none, status 0 (stderr %q)", input, stdout, status, stderr)
		}
	}
}

// TestXargsGivesEachRunNoStandardInput pins that a run cannot read the lines
// xargs has yet to read: GNU xargs gives each command /dev/null. The input is
// longer than xargs's read buffer, so lines are still waiting on its standard
// input when the first run starts.
func TestXargsGivesEachRunNoStandardInput(t *testing.T) {
	input := strings.Repeat("ga-0123456789\n", 1000)
	stdout, stderr, status := xargsRun(t, input, `if read -r line; then printf "[%s read %s]" "$1" "$line"; fi`, Options{})
	if stdout != "" || status != "0" {
		t.Errorf("a run read %q, status %s; want no run to read anything, status 0 (stderr %q)", stdout, status, stderr)
	}
}

func TestXargsRunsBdThroughExecWithTheExportedEnvironment(t *testing.T) {
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
			defer mu.Unlock()
			procs = append(procs, p)
			if p.Args[2] == "ga-2" {
				return &ExitError{Status: 1}
			}
			return nil
		},
	}
	opts.Stdin = strings.NewReader("ga-1\nga-2\n")
	stdout, stderr, err := runScript(t, `export GC_EXPORTED=1; local_only=2; `+
		`xargs -rI{} sh -c 'if ! bd update "$1" --status open; then printf "failed:%s;" "$1"; fi; printf "%s|%s;" "$GC_EXPORTED" "${local_only:-}"' _ {}; printf "status:%s" "$?"`, opts)
	if err != nil {
		t.Fatalf("Run: %v (stderr %q)", err, stderr)
	}
	if want := "1|;failed:ga-2;1|;status:0"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	if len(procs) != 2 {
		t.Fatalf("Exec ran %d processes, want one for each line", len(procs))
	}
	absDir, _ := filepath.Abs(dir)
	for i, id := range []string{"ga-1", "ga-2"} {
		p := procs[i]
		if want := []string{"bd", "update", id, "--status", "open"}; !slices.Equal(p.Args, want) {
			t.Errorf("process %d Args = %q, want %q", i, p.Args, want)
		}
		if p.Dir != absDir {
			t.Errorf("process %d Dir = %q, want %q", i, p.Dir, absDir)
		}
		if !hasEnv(p.Env, "GC_INHERITED", "yes") || !hasEnv(p.Env, "GC_EXPORTED", "1") || hasEnvName(p.Env, "local_only") {
			t.Errorf("process %d Env = %q, want the exported variables alone", i, p.Env)
		}
	}
}

func TestXargsReportsItsStatuses(t *testing.T) {
	t.Run("a run that fails leaves the rest to run, then 123", func(t *testing.T) {
		stdout, _, status := xargsRun(t, "a\nb\nc\n", `[ "$1" = b ] && exit 3; printf "%s" "$1"`, Options{})
		if stdout != "ac" || status != "123" {
			t.Errorf("runs = %q, status %s; want %q, status 123", stdout, status, "ac")
		}
	})

	t.Run("a run that exits 255 stops the rest, 124", func(t *testing.T) {
		stdout, stderr, status := xargsRun(t, "a\nb\nc\n", `[ "$1" = b ] && exit 255; printf "%s" "$1"`, Options{})
		if stdout != "a" || status != "124" || !strings.Contains(stderr, "exited with status 255; aborting") {
			t.Errorf("runs = %q, status %s, stderr %q; want %q, status 124 and the abort", stdout, status, stderr, "a")
		}
	})

	t.Run("a run that cannot start stops the rest, 126", func(t *testing.T) {
		gone := filepath.Join(t.TempDir(), "gone")
		if err := os.Mkdir(gone, 0o755); err != nil {
			t.Fatal(err)
		}
		var calls int
		opts := Options{
			Dir: gone,
			Exec: func(context.Context, Process) error {
				calls++
				return os.Remove(gone)
			},
		}
		stdout, stderr, status := xargsRun(t, "a\nb\nc\n", `bd remove-the-directory "$1"`, opts)
		if status != "126" || calls != 1 || !strings.Contains(stderr, "xargs: sh:") {
			t.Errorf("status %s after %d runs, stdout %q, stderr %q; want status 126 after the first run alone", status, calls, stdout, stderr)
		}
	})

	for name, line := range map[string]string{
		"a single quote":     "b'c",
		"a double quote":     `b"c`,
		"a backslash":        `b\c`,
		"a blank at the end": "b ",
	} {
		t.Run("a line with "+name+" stops the rest, 1", func(t *testing.T) {
			stdout, stderr, status := xargsRun(t, "a\n"+line+"\nc\n", `printf "%s" "$1"`, Options{})
			if stdout != "a" || status != "1" || !strings.Contains(stderr, "needs quote or blank handling") {
				t.Errorf("runs = %q, status %s, stderr %q; want %q, status 1 and the refusal", stdout, status, stderr, "a")
			}
		})
	}
}

func TestXargsStopsWhenTheContextIsDone(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var out bytes.Buffer
	var calls int
	opts := Options{
		Env:    []string{"PATH=" + t.TempDir()},
		Stdin:  strings.NewReader("a\nb\n"),
		Stdout: &out,
		Exec: func(ctx context.Context, _ Process) error {
			calls++
			<-ctx.Done()
			return ctx.Err()
		},
	}
	started := time.Now()
	err := Program{Script: `xargs -rI{} sh -c 'bd update "$1"' _ {}; printf "after"`}.Run(ctx, opts)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("Run returned after %s, want promptly after the deadline", elapsed)
	}
	if calls != 1 || out.Len() != 0 {
		t.Errorf("Exec ran %d times and stdout = %q; want one run and nothing after the deadline", calls, out.String())
	}
}

func TestXargsRefusesEveryOtherUse(t *testing.T) {
	var calls []string
	opts := Options{
		Exec: func(_ context.Context, p Process) error {
			calls = append(calls, strings.Join(p.Args, " "))
			return nil
		},
	}
	for _, command := range []string{
		`xargs bd update`,
		`xargs -I{} sh -c 'bd update "$1"' _ {}`,
		`xargs -r -I{} sh -c 'bd update "$1"' _ {}`,
		`xargs -rI% sh -c 'bd update "$1"' _ %`,
		`xargs -rI{} bash -c 'bd update "$1"' _ {}`,
		`xargs -rI{} sh -c 'bd update {}' _ {}`,
		`xargs -rI{} sh -c 'bd update "$1"' {} {}`,
		`xargs -rI{} sh -c 'bd update "$1"' _ {} extra`,
		`xargs -rI{} sh -c 'bd update "$1"' _`,
		`xargs -rI{} sh -c 'if bd update "$1"' _ {}`,
	} {
		t.Run(command, func(t *testing.T) {
			opts.Stdin = strings.NewReader("ga-1\n")
			stdout, stderr, err := runScript(t, command+`; printf "status:%s" "$?"`, opts)
			if err != nil || stdout != "status:127" || !strings.Contains(stderr, "not run") {
				t.Errorf("stdout %q, stderr %q, err %v; want status 127 and the refusal", stdout, stderr, err)
			}
		})
	}
	if len(calls) != 0 {
		t.Errorf("Exec ran %q, want nothing", calls)
	}
}
