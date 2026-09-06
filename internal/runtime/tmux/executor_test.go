package tmux

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeExecutor captures tmux command arguments for unit testing.
type fakeExecutor struct {
	calls [][]string // each call's full args
	out   string
	err   error
	outs  []string
	errs  []error
	idx   int
}

func (f *fakeExecutor) execute(args []string) (string, error) {
	// Copy args to avoid aliasing with the caller's slice.
	cp := make([]string, len(args))
	copy(cp, args)
	f.calls = append(f.calls, cp)
	if f.idx < len(f.outs) || f.idx < len(f.errs) {
		var out string
		var err error
		if f.idx < len(f.outs) {
			out = f.outs[f.idx]
		}
		if f.idx < len(f.errs) {
			err = f.errs[f.idx]
		}
		f.idx++
		return out, err
	}
	return f.out, f.err
}

func (f *fakeExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return f.execute(args)
}

func TestNewSessionWithCommandAndEnvClearsEmptyVars(t *testing.T) {
	exec := &fakeExecutor{}
	tm := NewTmux()
	tm.exec = exec

	env := map[string]string{
		"LANG":     "en_US.UTF-8",
		"LC_ALL":   "",
		"LC_CTYPE": "",
	}
	if err := tm.NewSessionWithCommandAndEnv("gc-test-locale-clear", "", "claude", env); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	if len(exec.calls) == 0 {
		t.Fatal("no tmux calls recorded")
	}

	args := exec.calls[0]
	joined := strings.Join(args, "\x00")
	if !strings.Contains(joined, "\x00-e\x00LANG=en_US.UTF-8\x00") {
		t.Fatalf("new-session args missing LANG -e flag: %v", args)
	}
	if got := args[len(args)-1]; got != "env -u LC_ALL -u LC_CTYPE claude" {
		t.Fatalf("command = %q, want env -u LC_ALL -u LC_CTYPE claude", got)
	}
}

// The controller token is withheld from agent panes by an EMPTY value, not by
// dropping the key (convergence.ScrubTokenEnv, processenv.ControllerOnlyEnvKeys).
// This pins the adapter half of that contract at the argv boundary: an empty
// value must become an `env -u` prefix on the pane command, never a `-e KEY=`
// flag, because -e alone would leave the tmux server's global copy visible to
// the shell. A key the caller dropped emits neither, which is why dropping
// withholds nothing.
func TestNewSessionWithCommandAndEnvUnsetsControllerToken(t *testing.T) {
	exec := &fakeExecutor{}
	tm := NewTmux()
	tm.exec = exec

	env := map[string]string{
		"GC_CITY":             "/tmp/city",
		"GC_CONTROLLER_TOKEN": "",
	}
	if err := tm.NewSessionWithCommandAndEnv("gc-test-token-pin", "", "claude", env); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	if len(exec.calls) == 0 {
		t.Fatal("no tmux calls recorded")
	}

	args := exec.calls[0]
	joined := strings.Join(args, "\x00")
	if strings.Contains(joined, "\x00-e\x00GC_CONTROLLER_TOKEN=") {
		t.Fatalf("new-session exported GC_CONTROLLER_TOKEN with -e instead of unsetting it: %v", args)
	}
	if got := args[len(args)-1]; got != "env -u GC_CONTROLLER_TOKEN claude" {
		t.Fatalf("command = %q, want %q", got, "env -u GC_CONTROLLER_TOKEN claude")
	}
}

func TestNewSessionWithCommandAndEnvRejectsUnsafeUnsetKey(t *testing.T) {
	for _, key := range []string{
		"BEADS_BAD KEY",
		"BEADS_BAD;touch /tmp/pwned",
		"BEADS_BAD$(touch /tmp/pwned)",
		"BEADS_BAD=other",
		"-u",
	} {
		t.Run(key, func(t *testing.T) {
			exec := &fakeExecutor{}
			tm := NewTmux()
			tm.exec = exec

			err := tm.NewSessionWithCommandAndEnv("gc-test-invalid-env-key", "", "claude", map[string]string{key: ""})
			if err == nil {
				t.Fatalf("NewSessionWithCommandAndEnv accepted unsafe unset key %q", key)
			}
			if !strings.Contains(err.Error(), "invalid environment variable name") {
				t.Fatalf("error = %v, want an environment-name validation error", err)
			}
			for _, call := range exec.calls {
				if slices.Contains(call, "new-session") {
					t.Fatalf("invalid unset key reached tmux new-session: %v", call)
				}
			}
		})
	}
}

func TestRespawnAgentRejectsUnsafeWithheldBeadsKey(t *testing.T) {
	exec := &fakeExecutor{}
	tm := NewTmux()
	tm.exec = exec
	ops := &tmuxStartOps{tm: tm}

	err := ops.respawnAgent("gc-test-invalid-env-key", "", "claude", map[string]string{
		"BEADS_BAD;touch /tmp/pwned": "",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid environment variable name") {
		t.Fatalf("respawnAgent error = %v, want an environment-name validation error", err)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("invalid withheld key reached tmux: %v", exec.calls)
	}
}

type promptFooterExecutor struct {
	calls [][]string
}

func (p *promptFooterExecutor) execute(args []string) (string, error) {
	cp := make([]string, len(args))
	copy(cp, args)
	p.calls = append(p.calls, cp)
	if len(args) == 0 {
		return "", nil
	}
	for i := 0; i < len(args)-1; i++ {
		if args[i] != "-S" {
			continue
		}
		lines, err := strconv.Atoi(strings.TrimPrefix(args[i+1], "-"))
		if err != nil {
			return "", nil
		}
		if lines >= promptObservationLines {
			return strings.Join([]string{
				"Claude Code v2.1.112",
				"status line",
				"❯\u00a0",
				"",
				"",
				"",
				"",
				"",
				"",
				"",
				"",
				"",
				"",
				"",
			}, "\n"), nil
		}
		return strings.Repeat("\n", 20), nil
	}
	return "", nil
}

func (p *promptFooterExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return p.execute(args)
}

// ctxBlockingExecutor blocks executeCtx until ctx is canceled. Used to
// verify that callers honor a wall-clock deadline on the subprocess.
type ctxBlockingExecutor struct {
	calls [][]string
}

func (b *ctxBlockingExecutor) execute(args []string) (string, error) {
	cp := make([]string, len(args))
	copy(cp, args)
	b.calls = append(b.calls, cp)
	return "", nil
}

func (b *ctxBlockingExecutor) executeCtx(ctx context.Context, args []string) (string, error) {
	cp := make([]string, len(args))
	copy(cp, args)
	b.calls = append(b.calls, cp)
	<-ctx.Done()
	return "", ctx.Err()
}

// TestRunBoundsByTmuxSubprocessTimeout verifies that Tmux.run applies a
// wall-clock cap to subprocess invocations. A wedged tmux subprocess must
// not be able to hang the shutdown path indefinitely.
func TestRunBoundsByTmuxSubprocessTimeout(t *testing.T) {
	orig := tmuxSubprocessTimeout
	tmuxSubprocessTimeout = 50 * time.Millisecond
	t.Cleanup(func() { tmuxSubprocessTimeout = orig })

	bx := &ctxBlockingExecutor{}
	tm := &Tmux{cfg: DefaultConfig(), exec: bx}

	type result struct {
		err error
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		_, err := tm.run("list-sessions")
		done <- result{err: err}
	}()

	select {
	case r := <-done:
		elapsed := time.Since(start)
		if r.err == nil {
			t.Fatalf("err = nil after %s, want context.DeadlineExceeded", elapsed)
		}
		if !errors.Is(r.err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want context.DeadlineExceeded chain", r.err)
		}
		if elapsed > 500*time.Millisecond {
			t.Fatalf("elapsed = %s, want < 500ms", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("tm.run did not return within 2s — tmuxSubprocessTimeout not applied")
	}
}

func TestRunInjectsSocketFlag(t *testing.T) {
	fe := &fakeExecutor{}
	tm := &Tmux{cfg: Config{SocketName: "bright-lights"}, exec: fe}
	_, _ = tm.run("list-sessions")

	if len(fe.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(fe.calls))
	}
	got := fe.calls[0]
	want := []string{"-u", "-L", "bright-lights", "list-sessions"}
	if len(got) != len(want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRunNoSocketFlagWhenEmpty(t *testing.T) {
	fe := &fakeExecutor{}
	tm := &Tmux{cfg: DefaultConfig(), exec: fe}
	_, _ = tm.run("list-sessions")

	if len(fe.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(fe.calls))
	}
	got := fe.calls[0]
	want := []string{"-u", "list-sessions"}
	if len(got) != len(want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestHiddenAttachedKeyBytesSupportsArrowNavigation(t *testing.T) {
	tests := map[string]string{
		"Up":    "\x1b[A",
		"Down":  "\x1b[B",
		"Right": "\x1b[C",
		"Left":  "\x1b[D",
	}
	for key, want := range tests {
		got, ok := hiddenAttachedKeyBytes(key)
		if !ok {
			t.Fatalf("hiddenAttachedKeyBytes(%q) not supported", key)
		}
		if string(got) != want {
			t.Fatalf("hiddenAttachedKeyBytes(%q) = %q, want %q", key, string(got), want)
		}
	}
}

func TestRunAlwaysPrependsUTF8Flag(t *testing.T) {
	fe := &fakeExecutor{}
	tm := &Tmux{cfg: Config{SocketName: "x"}, exec: fe}
	_, _ = tm.run("new-session", "-s", "test")

	if len(fe.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(fe.calls))
	}
	got := fe.calls[0]
	if got[0] != "-u" {
		t.Errorf("args[0] = %q, want %q", got[0], "-u")
	}
	// Verify full arg list: -u -L x new-session -s test
	want := []string{"-u", "-L", "x", "new-session", "-s", "test"}
	if len(got) != len(want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLatestActivityTimestamp(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    int64
		wantErr bool
	}{
		{name: "single timestamp", input: "123", want: 123},
		{name: "multiple timestamps", input: "123\n456\n234", want: 456},
		{name: "blank lines ignored", input: "\n123\n\n456\n", want: 456},
		{name: "invalid timestamp", input: "123\nnope", wantErr: true},
		{name: "no timestamps", input: "\n\n", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := latestActivityTimestamp(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("latestActivityTimestamp(%q) error = nil, want error", tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("latestActivityTimestamp(%q) error = %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("latestActivityTimestamp(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestIsSessionRunningFalseWhenPaneDead(t *testing.T) {
	fe := &fakeExecutor{
		outs: []string{"", "1"},
	}
	tm := &Tmux{cfg: Config{SocketName: "x"}, exec: fe}

	if tm.IsSessionRunning("runner") {
		t.Fatal("IsSessionRunning = true, want false for dead pane")
	}

	if len(fe.calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(fe.calls))
	}
	want := [][]string{
		{"-u", "-L", "x", "has-session", "-t", "=runner"},
		{"-u", "-L", "x", "display-message", "-t", "runner:^.0", "-p", "#{pane_dead}"},
	}
	for i := range want {
		if len(fe.calls[i]) != len(want[i]) {
			t.Fatalf("call %d = %v, want %v", i, fe.calls[i], want[i])
		}
		for j := range want[i] {
			if fe.calls[i][j] != want[i][j] {
				t.Errorf("call %d arg %d = %q, want %q", i, j, fe.calls[i][j], want[i][j])
			}
		}
	}
}

func TestIsSessionRunningFallsBackToSessionExistsOnPaneQueryError(t *testing.T) {
	fe := &fakeExecutor{
		outs: []string{""},
		errs: []error{nil, ErrNoServer},
	}
	tm := &Tmux{cfg: Config{SocketName: "x"}, exec: fe}

	if !tm.IsSessionRunning("runner") {
		t.Fatal("IsSessionRunning = false, want true when pane query fails after session exists")
	}
}

func TestProviderIsDeadRuntimeSessionRequiresEveryPaneDead(t *testing.T) {
	fe := &fakeExecutor{
		out: "1\n0",
	}
	tm := &Tmux{cfg: Config{SocketName: "x"}, exec: fe}
	p := &Provider{tm: tm}

	dead, err := p.IsDeadRuntimeSession("runner")
	if err != nil {
		t.Fatalf("IsDeadRuntimeSession: %v", err)
	}
	if dead {
		t.Fatal("IsDeadRuntimeSession = true, want false when any pane is live")
	}

	if len(fe.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(fe.calls))
	}
	want := []string{"-u", "-L", "x", "list-panes", "-s", "-t", "=runner", "-F", "#{pane_dead}"}
	if len(fe.calls[0]) != len(want) {
		t.Fatalf("call = %v, want %v", fe.calls[0], want)
	}
	for i := range want {
		if fe.calls[0][i] != want[i] {
			t.Fatalf("call arg %d = %q, want %q; call=%v", i, fe.calls[0][i], want[i], fe.calls[0])
		}
	}
}

func TestProviderIsDeadRuntimeSessionTrueWhenAllPanesDead(t *testing.T) {
	fe := &fakeExecutor{
		out: "1\n1",
	}
	tm := &Tmux{cfg: Config{SocketName: "x"}, exec: fe}
	p := &Provider{tm: tm}

	dead, err := p.IsDeadRuntimeSession("runner")
	if err != nil {
		t.Fatalf("IsDeadRuntimeSession: %v", err)
	}
	if !dead {
		t.Fatal("IsDeadRuntimeSession = false, want true when all panes are dead")
	}
}

func TestProviderIsDeadRuntimeSessionTreatsAbsentSessionAsNotDead(t *testing.T) {
	fe := &fakeExecutor{
		err: ErrSessionNotFound,
	}
	tm := &Tmux{cfg: Config{SocketName: "x"}, exec: fe}
	p := &Provider{tm: tm}

	dead, err := p.IsDeadRuntimeSession("missing")
	if err != nil {
		t.Fatalf("IsDeadRuntimeSession: %v", err)
	}
	if dead {
		t.Fatal("IsDeadRuntimeSession = true, want false for absent session")
	}
}

func TestWaitForRuntimeReadyCapturesPromptAboveBlankFooter(t *testing.T) {
	fe := &promptFooterExecutor{}
	tm := &Tmux{cfg: DefaultConfig(), exec: fe}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := tm.WaitForRuntimeReady(ctx, "mayor", &RuntimeConfig{
		Tmux: &RuntimeTmuxConfig{ReadyPromptPrefix: "❯ "},
	}, time.Second)
	if err != nil {
		t.Fatalf("WaitForRuntimeReady() error = %v, want nil", err)
	}

	if len(fe.calls) == 0 {
		t.Fatal("expected capture-pane call")
	}
	got := fe.calls[0]
	want := []string{"-u", "capture-pane", "-p", "-t", "mayor", "-S", "-120"}
	if len(got) != len(want) {
		t.Fatalf("first call = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("first call arg %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// The respawn twin of TestNewSessionWithCommandAndEnvUnsetsControllerToken, and
// the guard the original fix was missing: respawn-pane takes no env argument, so
// the create path's `env -u` prefix — a property of one command string — does
// not reach the relaunched agent. The withholding has to be in the SESSION
// environment before the respawn, marked with `set-environment -r`.
//
// `-r` and not `-u`: -u deletes the session entry and lets the server's global
// value show through again, which is the leak rather than the fix.
func TestRespawnAgentMarksControllerTokenRemovedFromSessionEnv(t *testing.T) {
	exec := &fakeExecutor{}
	tm := NewTmux()
	tm.exec = exec
	ops := &tmuxStartOps{tm: tm}

	env := map[string]string{
		"GC_CITY":             "/tmp/city",
		"GC_CONTROLLER_TOKEN": "",
	}
	if err := ops.respawnAgent("gc-test-token-pin", "/proj", "claude", env); err != nil {
		t.Fatalf("respawnAgent: %v", err)
	}
	if len(exec.calls) != 2 {
		t.Fatalf("tmux calls = %d (%v), want 2 (set-environment then respawn-pane)", len(exec.calls), exec.calls)
	}

	setEnv := strings.Join(exec.calls[0], " ")
	if !strings.Contains(setEnv, "set-environment -t gc-test-token-pin -r GC_CONTROLLER_TOKEN") {
		t.Errorf("first call = %q, want it to mark GC_CONTROLLER_TOKEN removed from the session env", setEnv)
	}
	if strings.Contains(setEnv, "-u GC_CONTROLLER_TOKEN") {
		t.Errorf("first call = %q uses -u; that unsets the session entry and re-exposes the server's global value", setEnv)
	}
	if respawn := strings.Join(exec.calls[1], " "); !strings.Contains(respawn, "respawn-pane") {
		t.Errorf("second call = %q, want respawn-pane", respawn)
	}
	// A non-empty key is a real value, not a withholding, and must not be
	// marked for removal.
	if strings.Contains(setEnv, "GC_CITY") {
		t.Errorf("first call = %q marked GC_CITY removed; only empty-valued keys are withheld", setEnv)
	}

	t.Run("hosted beads namespace", func(t *testing.T) {
		exec := &fakeExecutor{}
		tm := NewTmux()
		tm.exec = exec
		ops := &tmuxStartOps{tm: tm}

		env := map[string]string{
			"BEADS_DB":               "",
			"BEADS_DIR":              "/selected/.beads",
			"BEADS_FUTURE_AUTHORITY": "",
			"GC_CITY":                "/tmp/city",
		}
		if err := ops.respawnAgent("gc-test-beads-pin", "/proj", "claude", env); err != nil {
			t.Fatalf("respawnAgent: %v", err)
		}
		if len(exec.calls) != 3 {
			t.Fatalf("tmux calls = %d (%v), want two set-environment calls then respawn-pane", len(exec.calls), exec.calls)
		}
		for i, key := range []string{"BEADS_DB", "BEADS_FUTURE_AUTHORITY"} {
			setEnv := strings.Join(exec.calls[i], " ")
			if !strings.Contains(setEnv, "set-environment -t gc-test-beads-pin -r "+key) {
				t.Errorf("call %d = %q, want %s marked removed from the session env", i, setEnv, key)
			}
		}
		if respawn := strings.Join(exec.calls[2], " "); !strings.Contains(respawn, "respawn-pane") {
			t.Errorf("third call = %q, want respawn-pane", respawn)
		}
	})
}

// No withheld CREDENTIAL means no extra tmux round-trip. The nesting-detection
// flags every session env pins empty are deliberately included here: they are
// not secrets, the `env -u` prefix already does what they need, and marking them
// would put an extra tmux call on the hot path of every session in the repo for
// no gain. Scope is a correctness property, not just a cost one — a marker
// failure is fatal, so every key marked is a key that can fail a launch.
func TestRespawnAgentSkipsSessionEnvMarkingWhenNoCredentialWithheld(t *testing.T) {
	exec := &fakeExecutor{}
	tm := NewTmux()
	tm.exec = exec
	ops := &tmuxStartOps{tm: tm}

	env := map[string]string{
		"GC_CITY":                "/tmp/city",
		"CLAUDECODE":             "",
		"CLAUDE_CODE_ENTRYPOINT": "",
		"CODEX_THREAD_ID":        "",
		"CODEX_CI":               "",
	}
	if err := ops.respawnAgent("gc-test-no-pins", "/proj", "claude", env); err != nil {
		t.Fatalf("respawnAgent: %v", err)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("tmux calls = %d (%v), want 1 (respawn-pane only; nesting flags must not be marked)", len(exec.calls), exec.calls)
	}
}

// vanishingSessionExecutor fails set-environment the way tmux 3.4 does when the
// pane command has already exited and taken the session with it, and answers
// has-session the way tmux does for a session that is gone.
type vanishingSessionExecutor struct {
	calls      [][]string
	sessionOut string
	sessionErr error
}

func (v *vanishingSessionExecutor) execute(args []string) (string, error) {
	cp := make([]string, len(args))
	copy(cp, args)
	v.calls = append(v.calls, cp)
	// runCtx prepends "-u" (and -L when socketed), so the subcommand is not args[0].
	switch {
	case slices.Contains(args, "set-environment"):
		return "", wrapError(errors.New("exit status 1"), "no such session: gone", args)
	case slices.Contains(args, "has-session"):
		return v.sessionOut, v.sessionErr
	}
	return "", nil
}

func (v *vanishingSessionExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return v.execute(args)
}

// A short-lived pane command can exit and take its session down while the marker
// call is in flight. That is not a failure to apply the control: a session that
// is gone has no pane to leak into and no warm box to respawn. Turning it into
// an error made every normal creation of a fast-exiting session fail — racily,
// and reported as "creating session", which named the wrong cause entirely.
func TestMarkSessionEnvRemovedToleratesSessionThatAlreadyExited(t *testing.T) {
	exec := &vanishingSessionExecutor{sessionErr: wrapError(errors.New("exit status 1"), "can't find session: gone", []string{"has-session"})}
	tm := NewTmux()
	tm.exec = exec

	if err := tm.markSessionEnvRemoved("gone", []string{"GC_CONTROLLER_TOKEN"}); err != nil {
		t.Fatalf("markSessionEnvRemoved on an exited session = %v, want nil", err)
	}
	if len(exec.calls) != 2 {
		t.Fatalf("tmux calls = %d (%v), want 2 (set-environment then the has-session confirmation)", len(exec.calls), exec.calls)
	}
}

// The converse, and the reason the tolerance is scoped to one condition rather
// than made best-effort: on a session that is still alive, a marker failure is a
// real failure to apply a security control and must not be swallowed.
func TestMarkSessionEnvRemovedFailsClosedWhenSessionIsAlive(t *testing.T) {
	exec := &vanishingSessionExecutor{sessionOut: ""}
	tm := NewTmux()
	tm.exec = exec

	err := tm.markSessionEnvRemoved("alive", []string{"GC_CONTROLLER_TOKEN"})
	if err == nil {
		t.Fatal("markSessionEnvRemoved on a live session = nil, want the marker failure surfaced")
	}
	if !strings.Contains(err.Error(), "GC_CONTROLLER_TOKEN") {
		t.Errorf("error = %v, want it to name the key that could not be withheld", err)
	}
}

// The create path must ALSO plant the durable marker, not only the one-shot
// prefix — otherwise the very first relaunch of a freshly provisioned box leaks.
func TestNewSessionWithCommandAndEnvMarksUnsetKeysRemovedFromSessionEnv(t *testing.T) {
	exec := &fakeExecutor{}
	tm := NewTmux()
	tm.exec = exec

	env := map[string]string{
		"GC_CITY":             "/tmp/city",
		"CLAUDECODE":          "",
		"GC_CONTROLLER_TOKEN": "",
	}
	if err := tm.NewSessionWithCommandAndEnv("gc-test-token-pin", "", "claude", env); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}

	var marked bool
	for _, call := range exec.calls {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, "set-environment") && strings.Contains(joined, "-r GC_CONTROLLER_TOKEN") {
			marked = true
		}
		if strings.Contains(joined, "set-environment") && strings.Contains(joined, "CLAUDECODE") {
			t.Errorf("new-session marked the nesting flag CLAUDECODE in the session env: %v", call)
		}
	}
	if !marked {
		t.Errorf("new-session never marked GC_CONTROLLER_TOKEN removed from the session env; the first respawn would leak it: %v", exec.calls)
	}
}
