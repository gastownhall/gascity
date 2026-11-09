package setupcmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/execgrace"
	"github.com/gastownhall/gascity/internal/runtime"
)

// sentinel stands in for a credential. It is synthetic; no real value belongs in
// a test.
const sentinel = "sk-test-NOT-A-REAL-CREDENTIAL-8f3a21"

// jwtSignature ends jwt: the part that leaks when the head is dropped.
const jwtSignature = ".SIGNATURE-NOT-A-REAL-CREDENTIAL"

// jwt has a service-account JWT's shape and order of magnitude (~1.5KB).
var jwt = "eyJhbGciOiJSUzI1NiJ9." + strings.Repeat("QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVph", 40) + jwtSignature

// fixed is the historical single-deadline budget: no ceiling, so Timeout is a
// wall-clock deadline.
func fixed(timeout time.Duration) Options { return Options{Timeout: timeout} }

// assertRecordedDir checks the directory a command recorded with `pwd > file`,
// resolving symlinks on both sides (t.TempDir can hand back a symlinked path,
// e.g. macOS /var -> /private/var, while pwd reports the resolved one) and
// requires it to be want.
func assertRecordedDir(t *testing.T, recorded, want string) {
	t.Helper()
	data, err := os.ReadFile(recorded)
	if err != nil {
		t.Fatalf("reading recorded cwd %q: %v", recorded, err)
	}
	got, err := filepath.EvalSymlinks(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", strings.TrimSpace(string(data)), err)
	}
	wantResolved, err := filepath.EvalSymlinks(want)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", want, err)
	}
	if got != wantResolved {
		t.Fatalf("working directory = %q, want %q", got, wantResolved)
	}
}

// TestOutputTail pins the bounded capture Run folds into failure messages: the
// last limit bytes, redacted, whitespace-trimmed, with no truncation marker.
func TestOutputTail(t *testing.T) {
	cases := []struct {
		name   string
		limit  int
		writes []string
		want   string
	}{
		{name: "no output", limit: 8, writes: nil, want: ""},
		{name: "whitespace only", limit: 8, writes: []string{" \n\t "}, want: ""},
		{name: "under limit", limit: 8, writes: []string{"abc\n"}, want: "abc"},
		{name: "exact limit", limit: 4, writes: []string{"abcd"}, want: "abcd"},
		{name: "oversized single write keeps tail", limit: 4, writes: []string{"abcdefgh"}, want: "efgh"},
		{name: "rollover across writes", limit: 4, writes: []string{"abc", "def"}, want: "cdef"},
		{name: "many small writes", limit: 3, writes: []string{"a", "b", "c", "d", "e"}, want: "cde"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tail := newOutputTail(tc.limit)
			for _, w := range tc.writes {
				n, err := tail.Write([]byte(w))
				if err != nil {
					t.Fatalf("Write(%q) error: %v", w, err)
				}
				if n != len(w) {
					t.Fatalf("Write(%q) = %d, want %d", w, n, len(w))
				}
			}
			if got := tail.render(nil); got != tc.want {
				t.Fatalf("render = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRunUsesGCDIRAsWorkingDirectory pins the cwd contract: the command runs
// in env["GC_DIR"] when it exists, so relative paths in a setup command
// resolve against the session directory.
func TestRunUsesGCDIRAsWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	fallback := t.TempDir()
	if err := Run(context.Background(), "pwd > out.txt", map[string]string{"GC_DIR": dir},
		Options{Timeout: 5 * time.Second, FallbackDir: fallback}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertRecordedDir(t, filepath.Join(dir, "out.txt"), dir)
}

// TestRunFallsBackWhenGCDIRMissing pins the not-yet-created-workDir fallback:
// a pre_start often creates the very directory GC_DIR names, so a missing
// GC_DIR must run the command in FallbackDir rather than fail on chdir.
func TestRunFallsBackWhenGCDIRMissing(t *testing.T) {
	fallback := t.TempDir()
	missing := filepath.Join(fallback, "does", "not", "exist")
	if err := Run(context.Background(), "pwd > cwd.txt", map[string]string{"GC_DIR": missing},
		Options{Timeout: 5 * time.Second, FallbackDir: fallback}); err != nil {
		t.Fatalf("Run with missing GC_DIR = %v, want nil (cwd fallback)", err)
	}
	assertRecordedDir(t, filepath.Join(fallback, "cwd.txt"), fallback)
}

// TestRunMissingGCDIRWithoutFallbackInheritsCwd pins the no-fallback edge: a
// missing GC_DIR with no FallbackDir runs in the caller's working directory
// instead of failing on chdir.
func TestRunMissingGCDIRWithoutFallbackInheritsCwd(t *testing.T) {
	out := filepath.Join(t.TempDir(), "cwd.txt")
	missing := filepath.Join(t.TempDir(), "does", "not", "exist")
	err := Run(context.Background(), `pwd > "$OUT"`, map[string]string{"GC_DIR": missing, "OUT": out}, fixed(5*time.Second))
	if err != nil {
		t.Fatalf("Run with missing GC_DIR and no fallback = %v, want nil", err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	assertRecordedDir(t, out, wd)
}

// TestRunAppendsEnvOverlay pins that env entries reach the command on top of
// the inherited process environment.
func TestRunAppendsEnvOverlay(t *testing.T) {
	t.Setenv("GC_TEST_KEY", "inherited")
	if err := Run(context.Background(), `[ "$GC_TEST_KEY" = v ] && [ -n "$PATH" ]`,
		map[string]string{"GC_TEST_KEY": "v"}, fixed(5*time.Second)); err != nil {
		t.Fatalf("env overlay not visible to command: %v", err)
	}
}

// TestRunRejectsNonPositiveTimeout pins that a missing budget is a caller
// error, not an instantly-expired deadline, and that the command never runs.
func TestRunRejectsNonPositiveTimeout(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	err := Run(context.Background(), `touch "$MARKER"`, map[string]string{"MARKER": marker}, Options{})
	if err == nil {
		t.Fatal("Run with zero timeout = nil, want error")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("error = %q, want it to name the timeout", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("command ran despite an invalid timeout")
	}
}

// TestRunFoldsCombinedOutputIntoFailure pins the failure shape: the exit error
// followed by the trimmed tail of the command's interleaved stdout and stderr.
func TestRunFoldsCombinedOutputIntoFailure(t *testing.T) {
	err := Run(context.Background(), "echo out; echo err >&2; exit 3", nil, fixed(5*time.Second))
	if err == nil {
		t.Fatal("expected error")
	}
	if got, want := err.Error(), "exit status 3: out\nerr"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

// TestRunFailureWithoutOutputIsBareError pins that a silent failure reports the
// exit error alone.
func TestRunFailureWithoutOutputIsBareError(t *testing.T) {
	err := Run(context.Background(), "exit 4", nil, fixed(5*time.Second))
	if err == nil {
		t.Fatal("expected error")
	}
	if got, want := err.Error(), "exit status 4"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

// TestRunBoundsOutputTail pins that a chatty failure reports only the last
// outputLimit bytes.
func TestRunBoundsOutputTail(t *testing.T) {
	err := Run(context.Background(), `head -c 20000 /dev/zero | tr '\0' a; printf END; exit 1`, nil, fixed(5*time.Second))
	if err == nil {
		t.Fatal("expected error")
	}
	tail, ok := strings.CutPrefix(err.Error(), "exit status 1: ")
	if !ok {
		t.Fatalf("error = %.80q..., want exit status prefix", err)
	}
	if len(tail) != outputLimit {
		t.Fatalf("tail length = %d, want %d", len(tail), outputLimit)
	}
	if !strings.HasSuffix(tail, "END") {
		t.Fatalf("tail does not end with the command's last output: ...%q", tail[len(tail)-16:])
	}
}

// TestRunRedactsSecretStraddlingTailBoundary pins redact-before-truncate: a
// credential whose head falls before the reported window must not survive as
// a decapitated suffix.
func TestRunRedactsSecretStraddlingTailBoundary(t *testing.T) {
	// The secret's last half lands inside the final outputLimit bytes.
	pad := outputLimit - len(sentinel)/2
	err := Run(context.Background(),
		`printf '%s' "$SOME_NEW_TOKEN"; head -c `+strconv.Itoa(pad)+` /dev/zero | tr '\0' x; exit 1`,
		map[string]string{"SOME_NEW_TOKEN": sentinel}, fixed(5*time.Second))
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), sentinel[len(sentinel)/2:]) {
		t.Fatalf("failure leaks the tail of a straddling credential: ...%q", err.Error()[len("exit status 1: "):][:64])
	}
}

// TestRunFixedTimeoutCauseIsDeadlineExceeded pins the fixed budget: with no
// ceiling, Timeout is a wall-clock deadline whose expiry callers can match
// with errors.Is(err, context.DeadlineExceeded).
func TestRunFixedTimeoutCauseIsDeadlineExceeded(t *testing.T) {
	err := Run(context.Background(), "echo tick; sleep 5", nil, fixed(200*time.Millisecond))
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %q, want errors.Is DeadlineExceeded", err)
	}
}

// TestRunIdleBudgetCauseIsErrIdle pins that a silent command under the
// activity-aware budget fails with a cause naming the idle budget.
func TestRunIdleBudgetCauseIsErrIdle(t *testing.T) {
	start := time.Now()
	err := Run(context.Background(), "exec sleep 30", nil,
		Options{Timeout: 200 * time.Millisecond, MaxTimeout: 30 * time.Second})
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, execgrace.ErrIdle) {
		t.Fatalf("error = %q, want errors.Is execgrace.ErrIdle", err)
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatalf("idle budget took %v to fire", elapsed)
	}
}

// TestRunCeilingCauseIsErrCeiling pins that a command streaming output forever
// still dies at MaxTimeout, with a cause naming the ceiling.
func TestRunCeilingCauseIsErrCeiling(t *testing.T) {
	err := Run(context.Background(), "while :; do echo tick; sleep 0.05; done", nil,
		Options{Timeout: 5 * time.Second, MaxTimeout: 300 * time.Millisecond})
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, execgrace.ErrCeiling) {
		t.Fatalf("error = %q, want errors.Is execgrace.ErrCeiling", err)
	}
}

// TestRunTimeoutSignalsProcessGroupWithTERM is the stranded-staged-state
// regression: on budget expiry the command's whole process group gets SIGTERM
// first, so the shell's rollback trap and a background child's trap both run
// before any forced kill.
func TestRunTimeoutSignalsProcessGroupWithTERM(t *testing.T) {
	dir := t.TempDir()
	parent, child := filepath.Join(dir, "parent"), filepath.Join(dir, "child")
	script := `sh -c 'trap "echo restored > \"$CHILD\"; exit 0" TERM; sleep 30 & wait' &
trap 'echo restored > "$PARENT"; exit 143' TERM
sleep 30 & wait`
	err := Run(context.Background(), script, map[string]string{"PARENT": parent, "CHILD": child}, fixed(500*time.Millisecond))
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %q, want errors.Is DeadlineExceeded", err)
	}
	for _, marker := range []string{parent, child} {
		if _, statErr := os.Stat(marker); statErr != nil {
			t.Errorf("TERM trap for %s never ran — rollback would be lost: %v", filepath.Base(marker), statErr)
		}
	}
}

// TestRunTimeoutEscalatesToKill pins the other half of TERM-then-KILL: a
// command that ignores SIGTERM is force-killed once the grace runs out rather
// than holding the start forever.
func TestRunTimeoutEscalatesToKill(t *testing.T) {
	start := time.Now()
	err := Run(context.Background(), "trap '' TERM; sleep 30", nil, fixed(200*time.Millisecond))
	if err == nil {
		t.Fatal("expected error")
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatalf("a TERM-ignoring command held Run for %v", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %q, want errors.Is DeadlineExceeded", err)
	}
}

// TestRunBackgroundChildSucceedsBounded is the regression for setup commands
// that daemonize a child inheriting stdio: without Cmd.WaitDelay the capture
// pipe never reaches EOF and Run blocks until the descendant exits, far past
// the timeout. The command itself exits 0, so it must be reported as success
// once the wait delay force-closes the pipe.
func TestRunBackgroundChildSucceedsBounded(t *testing.T) {
	start := time.Now()
	err := Run(context.Background(), "sleep 30 & exit 0", nil, fixed(30*time.Second))
	if elapsed := time.Since(start); elapsed >= 10*time.Second {
		t.Fatalf("Run blocked %v on a background child holding stdio", elapsed)
	}
	if err != nil {
		t.Fatalf("daemonizing setup command exiting 0 should succeed, got %v", err)
	}
}

// TestOutputTailRetainsEnoughToRedact pins redact-before-truncate through the
// bounded writer, where it is not free: the writer drops bytes as they stream,
// long before anyone knows what the secrets are. Retaining exactly the reported
// limit would put the head of a straddling credential beyond recovery.
func TestOutputTailRetainsEnoughToRedact(t *testing.T) {
	const limit = 4096
	// Place the cut inside the sentinel: the last limit bytes begin partway
	// through it, so only retention beyond limit keeps it whole.
	const lead = 10
	filler := strings.Repeat("f", limit+20-len(sentinel))
	// Earlier output the writer must drop as it streams.
	early := strings.Repeat("p", runtime.OutputTailRetention(limit))
	tail := newOutputTail(limit)
	for _, chunk := range []string{early, strings.Repeat("s", lead) + sentinel, filler} {
		if _, err := tail.Write([]byte(chunk)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	got := tail.render([]string{sentinel})
	if strings.Contains(got, "CREDENTIAL") {
		t.Errorf("a credential straddling the truncation boundary leaked: %q", got)
	}
	if !strings.Contains(got, runtime.RedactedValue) {
		t.Fatalf("the secret was not in the retained buffer at all: %q", got)
	}
	// The controls: both the retained buffer and the rendering stay bounded,
	// or this passes on a writer that simply kept everything.
	if len(tail.buf) > runtime.OutputTailRetention(limit) {
		t.Errorf("writer retained %d bytes, want at most %d", len(tail.buf), runtime.OutputTailRetention(limit))
	}
	if len(got) > limit {
		t.Errorf("render returned %d bytes, so the limit is not being applied", len(got))
	}
}

// TestOutputTailRetainsAJWTLengthCredential pins the slack against the
// population it actually has to cover, not against the tokens that inspired it.
//
// The retained window is the whole guarantee: a value whose head falls outside
// it is gone before redaction runs, and its tail then survives into the reported
// text matching nothing. So the slack is an upper bound on credential length,
// and every unrecognized env value is a credential here by construction —
// service-account JWTs and PEM keys included, which run kilobytes, not the sub-
// 100 bytes an API key does. A fixture inside the old 1024 would have pinned
// only the comfortable half of that range.
func TestOutputTailRetainsAJWTLengthCredential(t *testing.T) {
	const limit = 256
	if len(jwt) <= 1024 {
		t.Fatalf("fixture is %d bytes, which the previous slack already covered", len(jwt))
	}
	// Lead the secret with enough filler that its head lands outside a
	// limit+1024 window but inside a limit+8192 one, and trail it with a marker
	// that must survive.
	tail := newOutputTail(limit)
	if _, err := tail.Write([]byte(strings.Repeat("f", 4000) + jwt + "curl: (22) 401")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got := tail.render([]string{jwt})
	if strings.Contains(got, jwtSignature) {
		t.Errorf("the tail of a JWT-length credential survived the reported window: %q", got)
	}
	if !strings.Contains(got, runtime.RedactedValue) {
		t.Fatalf("the secret was not retained whole, so nothing was redacted: %q", got)
	}
	// The control: over-redaction that shredded the diagnostic would pass the
	// assertions above.
	if !strings.Contains(got, "curl: (22) 401") {
		t.Errorf("the surrounding diagnostic did not survive: %q", got)
	}
}

// echoedCredentials are outputs whose redaction shrinks them far enough to pull
// the writer's first retained byte into the reported window: one credential
// echoed over and over, as a `set -x` retry loop or `curl -v --retry` prints
// it, for long enough that the writer discards output and cuts into an
// occurrence.
var echoedCredentials = []struct {
	name    string
	secret  string
	prefix  string // printed before the credential on each line
	repeats int
	last    string // the command's final, credential-free output
}{
	{name: "short token", secret: sentinel, repeats: 500, last: "abcde"},
	{name: "JWT-length credential", secret: jwt, prefix: "> Authorization: Bearer ", repeats: 40, last: "curl: (22) 401"},
}

// TestOutputTailNeverRendersADecapitatedCredential is the regression for a
// bounded writer that redacts only what it kept. Each whole occurrence of an
// echoed credential shrinks to <redacted>, the reported window reaches back to
// the first retained byte, and that byte sits inside an occurrence whose head
// the writer discarded: the surviving suffix matches nothing, so redaction
// alone reports it verbatim.
func TestOutputTailNeverRendersADecapitatedCredential(t *testing.T) {
	for _, tc := range echoedCredentials {
		line := tc.prefix + tc.secret + "\n"
		output := strings.Repeat(line, tc.repeats) + tc.last
		// Both of Write's discarding paths: one oversized write, and the
		// rollover of many small ones.
		for _, writes := range []struct {
			name   string
			chunks []string
		}{
			{name: "one write", chunks: []string{output}},
			{name: "line by line", chunks: append(slices.Repeat([]string{line}, tc.repeats), tc.last)},
		} {
			t.Run(tc.name+"/"+writes.name, func(t *testing.T) {
				tail := newOutputTail(outputLimit)
				for _, chunk := range writes.chunks {
					if _, err := tail.Write([]byte(chunk)); err != nil {
						t.Fatalf("Write: %v", err)
					}
				}
				secrets := []string{tc.secret}

				// The counterfactual, asserted rather than described: redacting
				// the retained bytes alone reports the decapitated suffix.
				if leaked, _ := runtime.RedactSecretsTail(string(tail.buf), outputLimit, secrets); !strings.Contains(leaked, "CREDENTIAL") {
					t.Fatal("the retained bytes do not begin inside an occurrence, so nothing is tested here")
				}

				got := tail.render(secrets)
				if strings.Contains(got, "CREDENTIAL") {
					t.Errorf("a decapitated credential reached the rendered tail: %.120q", got)
				}
				// The controls: the rest was redacted rather than dropped, the
				// diagnostic after it survives, and both bounds still hold.
				if !strings.Contains(got, runtime.RedactedValue) {
					t.Errorf("nothing was redacted, so the assertion above proves nothing: %.120q", got)
				}
				if !strings.HasSuffix(got, tc.last) {
					t.Errorf("the command's last output did not survive: %.120q", got)
				}
				if len(tail.buf) > runtime.OutputTailRetention(outputLimit) {
					t.Errorf("writer retained %d bytes, want at most %d", len(tail.buf), runtime.OutputTailRetention(outputLimit))
				}
				if len(got) > outputLimit {
					t.Errorf("render returned %d bytes, so the limit is not being applied", len(got))
				}
			})
		}
	}
}

// TestRunFailureNeverReportsADecapitatedCredential is the same regression end to
// end: a failing setup command that echoes its credential over and over must
// not carry any part of it into the durable error.
func TestRunFailureNeverReportsADecapitatedCredential(t *testing.T) {
	for _, tc := range echoedCredentials {
		t.Run(tc.name, func(t *testing.T) {
			command := `i=0; while [ "$i" -lt ` + strconv.Itoa(tc.repeats) + ` ]; do printf '%s%s\n' '` + tc.prefix +
				`' "$SOME_NEW_TOKEN"; i=$((i+1)); done; printf '%s' '` + tc.last + `'; exit 1`
			err := Run(context.Background(), command, map[string]string{"SOME_NEW_TOKEN": tc.secret}, fixed(10*time.Second))
			if err == nil {
				t.Fatal("a setup command exiting 1 must fail")
			}
			if strings.Contains(err.Error(), "CREDENTIAL") {
				t.Errorf("a decapitated credential reached the failure: %.120q", err.Error())
			}
			// The controls: the command ran as scripted and its output was
			// redacted, not lost.
			if !strings.HasPrefix(err.Error(), "exit status 1: ") || !strings.HasSuffix(err.Error(), tc.last) {
				t.Fatalf("the failure does not carry the command's output: %.120q", err.Error())
			}
			if !strings.Contains(err.Error(), runtime.RedactedValue) {
				t.Errorf("nothing was redacted, so the assertion above proves nothing: %.120q", err.Error())
			}
		})
	}
}

// TestRunFailureOmitsCredentials is the end-to-end shape: the failure tail is
// scrubbed against both environments the command was given, the session env
// and this process's own.
func TestRunFailureOmitsCredentials(t *testing.T) {
	const inherited = "inherited-NOT-A-REAL-CREDENTIAL"
	t.Setenv("ANTHROPIC_AUTH_TOKEN", inherited)

	err := Run(context.Background(),
		`echo "session=$GH_TOKEN" >&2; echo "inherited=$ANTHROPIC_AUTH_TOKEN"; exit 3`,
		map[string]string{"GH_TOKEN": sentinel}, fixed(10*time.Second))
	if err == nil {
		t.Fatal("a setup command exiting 3 must fail")
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Errorf("the session env credential reached the failure: %v", err)
	}
	if strings.Contains(err.Error(), inherited) {
		t.Errorf("the inherited credential reached the failure: %v", err)
	}
	// The controls. Without these the assertions above pass on any error that
	// simply never captured the output — including one where the command
	// never ran.
	if !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("the command did not run as expected: %v", err)
	}
	for _, want := range []string{"session=" + runtime.RedactedValue, "inherited=" + runtime.RedactedValue} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("failure detail missing %q, so nothing here was scrubbed: %v", want, err)
		}
	}
}
