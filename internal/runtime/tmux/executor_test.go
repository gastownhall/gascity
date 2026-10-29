package tmux

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// fakeExecutor captures tmux command arguments for unit testing.
type fakeExecutor struct {
	calls [][]string // each call's full args
	out   string
	err   error
	outs  []string
	errs  []error
	idx   int
	// agentPane answers agent-pane lookups with fakeAgentPaneRow (recorded,
	// but consuming none of outs/errs), for tests scripting what follows.
	agentPane bool
}

func (f *fakeExecutor) execute(args []string) (string, error) {
	// Copy args to avoid aliasing with the caller's slice.
	cp := make([]string, len(args))
	copy(cp, args)
	f.calls = append(f.calls, cp)
	if f.agentPane {
		if row, ok := fakeAgentPaneRow(args); ok {
			return row, nil
		}
	}
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
	if row, ok := fakeAgentPaneRow(args); ok {
		return row, nil
	}
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
		outs: []string{"", "pane\t1"},
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
		{"-u", "-L", "x", "list-panes", "-s", "-t", "=runner:", "-F", agentPaneMarkerFormat + "\t#{pane_dead}"},
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
	want := []string{"-u", "-L", "x", "list-panes", "-s", "-t", "=runner:", "-F", "#{pane_dead}"}
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

	if len(fe.calls) < 2 {
		t.Fatal("expected the agent-pane lookup, then a capture-pane call")
	}
	got := fe.calls[1]
	want := []string{"-u", "capture-pane", "-p", "-t", fakeAgentPane("mayor"), "-S", "-120"}
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
	exec := &fakeExecutor{out: "agent\t@1.%0"}
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
	if len(exec.calls) != 5 {
		t.Fatalf("tmux calls = %d (%v), want 5 (set-environment, adopt check, pane lookup, respawn-pane, the input fence's environment read)", len(exec.calls), exec.calls)
	}

	setEnv := strings.Join(exec.calls[0], " ")
	if !strings.Contains(setEnv, "set-environment -t =gc-test-token-pin -r GC_CONTROLLER_TOKEN") {
		t.Errorf("first call = %q, want it to mark GC_CONTROLLER_TOKEN removed from the session env", setEnv)
	}
	if strings.Contains(setEnv, "-u GC_CONTROLLER_TOKEN") {
		t.Errorf("first call = %q uses -u; that unsets the session entry and re-exposes the server's global value", setEnv)
	}
	if respawn := strings.Join(exec.calls[3], " "); !strings.Contains(respawn, "respawn-pane") {
		t.Errorf("fourth call = %q, want respawn-pane", respawn)
	}
	// A non-empty key is a real value, not a withholding, and must not be
	// marked for removal.
	if strings.Contains(setEnv, "GC_CITY") {
		t.Errorf("first call = %q marked GC_CITY removed; only empty-valued keys are withheld", setEnv)
	}

	t.Run("hosted beads namespace", func(t *testing.T) {
		exec := &fakeExecutor{out: "agent\t@1.%0"}
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
		if len(exec.calls) != 6 {
			t.Fatalf("tmux calls = %d (%v), want two set-environment calls, the adopt check, the pane lookup, respawn-pane, then the input fence's environment read", len(exec.calls), exec.calls)
		}
		for i, key := range []string{"BEADS_DB", "BEADS_FUTURE_AUTHORITY"} {
			setEnv := strings.Join(exec.calls[i], " ")
			if !strings.Contains(setEnv, "set-environment -t =gc-test-beads-pin -r "+key) {
				t.Errorf("call %d = %q, want %s marked removed from the session env", i, setEnv, key)
			}
		}
		if respawn := strings.Join(exec.calls[4], " "); !strings.Contains(respawn, "respawn-pane") {
			t.Errorf("fifth call = %q, want respawn-pane", respawn)
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
	exec := &fakeExecutor{out: "agent\t@1.%0"}
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
	if len(exec.calls) != 4 {
		t.Fatalf("tmux calls = %d (%v), want 4 (adopt check, pane lookup, respawn-pane and the input fence's environment read only; nesting flags must not be marked)", len(exec.calls), exec.calls)
	}
}

// Relaunch respawns the agent pane, never the active one. "=name:" resolves to
// the current window's active pane, so a human who left another window or pane
// focused would have that pane killed and the old agent left running. The
// session name is resolved in one list-panes over all its windows to the pane
// carrying the agent-pane marker, else (a session from before the marker) the
// first pane listed, and respawned fenced to its session and window; a pane id
// passes through without the lookup.
func TestRespawnTargetsAgentPaneNotActivePane(t *testing.T) {
	lookup := []string{"list-panes", "-s", "-t", "=gc-test-agent-pane:", "-F", agentPaneMarkerFormat + "\t#{window_id}.#{pane_id}"}
	// respawnAgent first checks for a lone unmarked pane to adopt.
	adopt := []string{"list-panes", "-s", "-t", "=gc-test-agent-pane:", "-F", agentPaneMarkerFormat + "\t#{pane_id}"}
	cases := []struct {
		name string
		out  string // the lookup's rows
		run  func(*Tmux, *tmuxStartOps) error
		want [][]string // each call's argv after runCtx's "-u"
	}{
		{"respawnAgent, marked pane moved behind a human's", "orphan\t@3.%8\nagent\t@4.%7\norphan\t@4.%9", func(_ *Tmux, ops *tmuxStartOps) error {
			return ops.respawnAgent("gc-test-agent-pane", "/proj", "claude", nil)
		}, [][]string{adopt, lookup, {"respawn-pane", "-k", "-t", "=gc-test-agent-pane:@4.%7", "-c", "/proj", "claude"}, {"show-environment", "-t", "=gc-test-agent-pane"}}},
		{"unmarked session takes the first pane", "pane\t@3.%7\npane\t@3.%8", func(tm *Tmux, _ *tmuxStartOps) error {
			return tm.RespawnPaneWithWorkDir("gc-test-agent-pane", "", "claude")
		}, [][]string{lookup, {"respawn-pane", "-k", "-t", "=gc-test-agent-pane:@3.%7", "claude"}}},
		{"pane id", "", func(tm *Tmux, _ *tmuxStartOps) error {
			return tm.RespawnPaneWithWorkDir("%3", "", "claude")
		}, [][]string{{"respawn-pane", "-k", "-t", "%3", "claude"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exec := &fakeExecutor{out: tc.out}
			tm := NewTmux()
			tm.exec = exec
			if err := tc.run(tm, &tmuxStartOps{tm: tm}); err != nil {
				t.Fatalf("respawn: %v", err)
			}
			if len(exec.calls) != len(tc.want) {
				t.Fatalf("tmux calls = %q, want %q", exec.calls, tc.want)
			}
			for i, want := range tc.want {
				if got := exec.calls[i][1:]; !slices.Equal(got, want) {
					t.Errorf("call %d = %q, want %q", i, got, want)
				}
			}
		})
	}
}

// A failed agent-pane lookup fails the respawn closed. Falling back to the
// session target would respawn the active pane, and an empty id would make
// tmux respawn some other session's pane (respawn-pane -t "").
func TestRespawnFailsClosedWhenAgentPaneLookupFails(t *testing.T) {
	cases := []struct {
		name   string
		exec   *fakeExecutor
		wantIs error
	}{
		{"session gone", &fakeExecutor{errs: []error{ErrSessionNotFound}}, ErrSessionNotFound},
		{"no panes", &fakeExecutor{outs: []string{""}}, nil},
		// A human killed the agent pane and the session lives on in theirs.
		{"agent pane gone", &fakeExecutor{outs: []string{"orphan\t@1.%3\norphan\t@1.%4"}}, errAgentPaneGone},
		{"two agent panes", &fakeExecutor{outs: []string{"agent\t@1.%3\nagent\t@2.%4"}}, errAgentPaneAmbiguous},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tm := NewTmux()
			tm.exec = tc.exec
			err := tm.RespawnPaneWithWorkDir("gc-test-agent-pane", "/proj", "claude")
			if err == nil {
				t.Fatal("RespawnPaneWithWorkDir succeeded, want the lookup failure")
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Errorf("err = %v, want errors.Is %v", err, tc.wantIs)
			}
			if len(tc.exec.calls) != 1 || slices.Contains(tc.exec.calls[0], "respawn-pane") {
				t.Errorf("tmux calls = %q, want only the list-panes lookup", tc.exec.calls)
			}
		})
	}
}

// agentPaneID reads the id of name's agent pane the way every session-keyed
// read resolves it.
func agentPaneID(tm *Tmux, name string) (string, error) {
	return tm.readAgentPane(tm.run, name, "#{pane_id}")
}

// fakeAgentPaneRow answers an agent-pane list-panes lookup the way tmux does
// for a session whose one pane, %1 in window @1, is the marked agent pane
// running claude as pid 4242. ok is false for any other command, so a fake
// executor can serve the lookup and script everything else.
func fakeAgentPaneRow(args []string) (string, bool) {
	if tmuxSubcommand(args) != "list-panes" {
		return "", false
	}
	fields, ok := strings.CutPrefix(args[len(args)-1], agentPaneMarkerFormat+"\t")
	if !ok {
		return "", false
	}
	return "agent\t" + strings.NewReplacer(
		"#{window_id}", "@1", "#{pane_id}", "%1", "#{pane_current_command}", "claude",
		"#{pane_pid}", "4242", "#{pane_dead}", "0", "#{pane_current_path}", "/w",
	).Replace(fields), true
}

// fakeAgentPane is the target fakeAgentPaneRow resolves session to.
func fakeAgentPane(session string) string { return "=" + session + ":@1.%1" }

// markedAgentPaneExecutor serves a session whose first listed pane is a human's
// (%1 in @1, running vim as pid 111) and whose second is the marked agent pane
// (%5 in @2, running claude as pid 222). It answers capture-pane with capture
// and every other command with "".
type markedAgentPaneExecutor struct {
	calls   [][]string
	capture string
	lookErr error
	inMode  string // answers the #{pane_in_mode} copy-mode probe
}

func (e *markedAgentPaneExecutor) execute(args []string) (string, error) {
	e.calls = append(e.calls, slices.Clone(args))
	switch tmuxSubcommand(args) {
	case "list-panes":
		if e.lookErr != nil {
			return "", e.lookErr
		}
		fields, ok := strings.CutPrefix(args[len(args)-1], agentPaneMarkerFormat+"\t")
		if !ok {
			return "", nil
		}
		human := strings.NewReplacer("#{window_id}", "@1", "#{pane_id}", "%1", "#{pane_pid}", "111", "#{pane_current_command}", "vim", "#{pane_dead}", "0", "#{pane_height}", "30").Replace(fields)
		agent := strings.NewReplacer("#{window_id}", "@2", "#{pane_id}", "%5", "#{pane_pid}", "222", "#{pane_current_command}", "claude", "#{pane_dead}", "0", "#{pane_height}", "20").Replace(fields)
		return "orphan\t" + human + "\nagent\t" + agent, nil
	case "capture-pane":
		return e.capture, nil
	case "display-message":
		if slices.Contains(args, "#{pane_in_mode}") {
			return e.inMode, nil
		}
	}
	return "", nil
}

func (e *markedAgentPaneExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return e.execute(args)
}

// targetsOf returns the -t argument of every call to sub.
func targetsOf(calls [][]string, sub string) []string {
	var targets []string
	for _, call := range calls {
		if tmuxSubcommand(call) != sub {
			continue
		}
		if target, ok := tmuxTargetArg(call); ok {
			targets = append(targets, target)
		}
	}
	return targets
}

// Session-keyed reads answer for the marked agent pane, even when a human's
// pane is listed first.
func TestAgentPaneReadsPreferMarkedPane(t *testing.T) {
	tm := NewTmux()
	tm.exec = &markedAgentPaneExecutor{}
	if got, err := tm.GetPanePID("w"); err != nil || got != "222" {
		t.Errorf("GetPanePID = (%q, %v), want the agent's 222", got, err)
	}
	if got, err := tm.GetPaneCommand("w"); err != nil || got != "claude" {
		t.Errorf("GetPaneCommand = (%q, %v), want the agent's claude", got, err)
	}
	if got, err := agentPaneID(tm, "w"); err != nil || got != "%5" {
		t.Errorf("agent pane id = (%q, %v), want the agent's %%5", got, err)
	}
	if got, err := tm.paneProcessState("w"); err != nil || got != (paneProcessState{PID: 222}) {
		t.Errorf("paneProcessState = (%+v, %v), want the agent's pid 222", got, err)
	}
}

func TestPickAgentPaneRow(t *testing.T) {
	for _, tc := range []struct {
		name, out, want string
		wantErr         error
	}{
		{"marked row wins", "orphan\ta\nagent\tb\norphan\tc", "b", nil},
		{"unmarked takes first row", "pane\ta\npane\tb", "a", nil},
		{"trimmed empty fields", "pane", "", nil},
		{"empty output", "", "", nil},
		{"a marker-looking field is not a marker", "pane\tagent\npane\tb", "agent", nil},
		// A human killed the agent pane; the session lives on in theirs.
		{"marked session without its agent pane", "orphan\ta\norphan\tb", "", errAgentPaneGone},
		// Two panes claim the session: fail closed rather than guess.
		{"two agent rows", "agent\ta\nagent\tb", "", errAgentPaneAmbiguous},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pickAgentPaneRow(tc.out)
			if got != tc.want || !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Errorf("pickAgentPaneRow(%q) = (%q, %v), want (%q, %v)", tc.out, got, err, tc.want, tc.wantErr)
			}
		})
	}
}

// Every create path marks the pane new-session printed as the agent pane,
// valued with the session's id, and only then flags the session as marked.
func TestCreatePathsMarkAgentPane(t *testing.T) {
	mark := []string{"set-option", "-p", "-F", "-t", "%7", agentPaneOption, "#{session_id}"}
	flag := []string{"set-option", "-t", "=w:", agentSessionOption, "1"}
	for _, tc := range []struct {
		name string
		run  func(*Tmux) error
	}{
		{"NewSession", func(tm *Tmux) error { return tm.NewSession("w", "") }},
		{"NewSessionWithCommand", func(tm *Tmux) error { return tm.NewSessionWithCommand("w", "", "claude") }},
		{"NewSessionWithCommandAndEnv", func(tm *Tmux) error {
			return tm.NewSessionWithCommandAndEnv("w", "", "claude", map[string]string{"GC_CITY": "/c"})
		}},
		{"NewSessionWithCommandAndEnv staged", func(tm *Tmux) error {
			return tm.NewSessionWithCommandAndEnv("w", "", "claude", map[string]string{"GC_CONTROLLER_TOKEN": "s3cr3t-token-value"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			confineStagedFiles(t)
			exec := &fakeExecutor{out: "%7"}
			tm := NewTmux()
			tm.exec = exec
			if err := tc.run(tm); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			created := slices.IndexFunc(exec.calls, func(c []string) bool {
				return slices.Contains(c, "new-session") || slices.Contains(c, "source-file")
			})
			marked := slices.IndexFunc(exec.calls, func(c []string) bool { return slices.Equal(c[1:], mark) })
			flagged := slices.IndexFunc(exec.calls, func(c []string) bool { return slices.Equal(c[1:], flag) })
			if created < 0 || marked <= created || flagged <= marked {
				t.Fatalf("calls = %q, want the create, then %q, then %q", exec.calls, mark, flag)
			}
		})
	}
}

// A failed mark is logged, and the session is not flagged: a flagged session
// without a marked pane would fail closed for its whole life.
func TestMarkAgentPaneFailureIsLoggedAndUnflagged(t *testing.T) {
	var warned strings.Builder
	prev := agentPaneWarningOut
	agentPaneWarningOut = &warned
	t.Cleanup(func() { agentPaneWarningOut = prev })
	exec := &fakeExecutor{outs: []string{"%7"}, errs: []error{nil, errors.New("tmux set-option: boom")}}
	tm := NewTmux()
	tm.exec = exec
	if err := tm.NewSession("w", ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if !strings.Contains(warned.String(), "marking agent pane") || !strings.Contains(warned.String(), "boom") {
		t.Errorf("warning = %q, want the failed mark logged", warned.String())
	}
	for _, c := range exec.calls {
		if slices.Contains(c, agentSessionOption) {
			t.Errorf("session flagged after a failed mark: %q", c)
		}
	}
}

// respawnAgent marks a session that has exactly one, unmarked pane (a warm
// box from before the marker), and leaves any other session alone.
func TestRespawnAgentAdoptsLoneUnmarkedPane(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rows     string
		wantMark bool
	}{
		{"lone unmarked pane", "pane\t%3", true},
		{"two unmarked panes", "pane\t%3\npane\t%4", false},
		{"already marked", "agent\t%3", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exec := &scriptedTargetExecutor{}
			exec.listPanes = func(format string) string {
				if format == agentPaneMarkerFormat+"\t#{pane_id}" {
					return tc.rows
				}
				return "agent\t@1.%3"
			}
			tm := NewTmux()
			tm.exec = exec
			if err := (&tmuxStartOps{tm: tm}).respawnAgent("w", "", "claude", nil); err != nil {
				t.Fatalf("respawnAgent: %v", err)
			}
			marked := slices.ContainsFunc(exec.calls, func(c []string) bool { return slices.Contains(c, agentPaneOption) })
			if marked != tc.wantMark {
				t.Errorf("marked = %v, want %v; calls=%q", marked, tc.wantMark, exec.calls)
			}
		})
	}
}

// Interrupt, SendKeys, ClearHistory and the startup captures and dialog keys
// go to the agent pane, fenced to its session and window, never the active
// pane.
func TestAgentPaneInputOpsTargetMarkedPane(t *testing.T) {
	const fenced = "=w:@2.%5"
	cases := []struct {
		name    string
		capture string
		run     func(*Tmux) error
		sub     string
		n       int // calls to sub; -1 for any number
	}{
		{"Interrupt", "", func(tm *Tmux) error { return (&Provider{tm: tm}).Interrupt("w") }, "send-keys", 1},
		{"SendKeys", "", func(tm *Tmux) error { return (&Provider{tm: tm}).SendKeys("w", "Down", "Enter") }, "send-keys", 2},
		{"ClearHistory", "", func(tm *Tmux) error { return tm.ClearHistory("w") }, "clear-history", 1},
		{"startup capture", "", func(tm *Tmux) error {
			_, err := newTmuxStartOps(tm, "", 0, runtime.Config{}, true).capturePane("w", 10)
			return err
		}, "capture-pane", 1},
		{"startup readiness", "> ", func(tm *Tmux) error {
			rc := &RuntimeConfig{Tmux: &RuntimeTmuxConfig{ReadyPromptPrefix: "> "}}
			return newTmuxStartOps(tm, "", 0, runtime.Config{}, true).waitForReady(context.Background(), "w", rc, time.Second)
		}, "capture-pane", 1},
		{"SendKeysDebounced", "", func(tm *Tmux) error { return tm.SendKeysDebounced("w", "hi", 0) }, "send-keys", 2},
		{"SendKeysReplace", "", func(tm *Tmux) error { return tm.SendKeysReplace("w", "hi", 0) }, "send-keys", 3},
		{"Pending", "", func(tm *Tmux) error { _, err := tm.Pending("w"); return err }, "capture-pane", 1},
		{"Peek", "x", func(tm *Tmux) error { _, err := (&Provider{tm: tm}).Peek("w", 10); return err }, "capture-pane", 1},
		{"Peek all", "x", func(tm *Tmux) error { _, err := (&Provider{tm: tm}).Peek("w", 0); return err }, "capture-pane", 1},
		{"WaitForIdle", "❯ ", func(tm *Tmux) error { return tm.WaitForIdle(context.Background(), "w", 5*time.Second) }, "capture-pane", -1},
		{"SnapshotIdle", "❯ ", func(tm *Tmux) error { _, err := tm.SnapshotIdle("w"); return err }, "capture-pane", 1},
		{"NudgeSession keys", "❯ ", func(tm *Tmux) error { return tm.NudgeSession("w", "hi") }, "send-keys", -1},
		{"NudgeSession captures", "❯ ", func(tm *Tmux) error { return tm.NudgeSession("w", "hi") }, "capture-pane", -1},
		{"startup dialog capture", "", func(tm *Tmux) error {
			return tm.DismissKnownDialogs(context.Background(), "w", 50*time.Millisecond)
		}, "capture-pane", -1},
		{"startup dialog keys", "Do you trust the contents of this directory?", func(tm *Tmux) error {
			_ = tm.DismissKnownDialogs(context.Background(), "w", 3*time.Second)
			return nil
		}, "send-keys", -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exec := &markedAgentPaneExecutor{capture: tc.capture}
			tm := NewTmux()
			tm.exec = exec
			if err := tc.run(tm); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			targets := targetsOf(exec.calls, tc.sub)
			if len(targets) == 0 || (tc.n >= 0 && len(targets) != tc.n) {
				t.Fatalf("%s targets = %q, want %d to %s; calls=%q", tc.sub, targets, tc.n, fenced, exec.calls)
			}
			for _, target := range targets {
				if target != fenced {
					t.Errorf("%s -t %q, want %q", tc.sub, target, fenced)
				}
			}
		})
	}
}

// agentInputTarget, the nudge's resolver, classifies rows exactly as every
// agent-pane read does. It takes the marked pane even when a human pane listed
// before it runs the agent's CLI, and fails closed when the marked agent pane
// is gone or more than one pane claims it: a nudge must never land in a human
// pane. Only an unmarked (pre-upgrade) session falls back to process
// detection, then to its first pane.
func TestAgentInputTargetPrefersMarkerAndFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name      string
		outs      []string
		want      string
		wantErr   error
		wantCalls int
	}{
		{"marked pane behind a human claude", []string{"orphan\t@1.%1\tclaude\t111\nagent\t@2.%5\tsh\t222"}, "=w:@2.%5", nil, 1},
		{"marked agent pane gone", []string{"orphan\t@1.%1\tclaude\t111\norphan\t@1.%2\tsh\t222"}, "", errAgentPaneGone, 1},
		{"two marked agent panes", []string{"agent\t@1.%1\tclaude\t111\nagent\t@2.%5\tsh\t222"}, "", errAgentPaneAmbiguous, 1},
		{"unmarked falls back to process names", []string{"pane\t@1.%1\tvim\t111\npane\t@1.%2\tclaude\t222", "GT_PROCESS_NAMES=claude"}, "=w:@1.%2", nil, 2},
		{"unmarked single pane", []string{"pane\t@1.%1\tclaude\t111"}, "=w:@1.%1", nil, 1},
		{"unmarked with no match takes the first pane", []string{"pane\t@1.%1\tvim\t111\npane\t@1.%2\tvim\t222", "GT_PROCESS_NAMES=claude"}, "=w:@1.%1", nil, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exec := &fakeExecutor{outs: tc.outs}
			tm := NewTmux()
			tm.exec = exec
			got, err := tm.agentInputTarget("w")
			if got != tc.want || !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Fatalf("agentInputTarget = (%q, %v), want (%q, %v)", got, err, tc.want, tc.wantErr)
			}
			if len(exec.calls) != tc.wantCalls {
				t.Errorf("tmux calls = %q, want %d", exec.calls, tc.wantCalls)
			}
		})
	}
}

// The resolved agent pane is cached for one operation only. Each operation
// resolves once, however many tmux calls it makes (WaitForIdle polls several
// times; Respond captures, probes copy-mode, keys and re-captures), and a
// second operation resolves again, so a pane replaced in between (respawn,
// relaunch) is never answered from a stale resolution.
func TestAgentPaneResolvedOncePerOperation(t *testing.T) {
	prev := respondVerifyInterval
	respondVerifyInterval = time.Millisecond
	t.Cleanup(func() { respondVerifyInterval = prev })
	approval := readApprovalFixture(t, "bash-manual-80x24.txt")
	cases := []struct {
		name    string
		capture string
		run     func(*Tmux) error
	}{
		{"SendKeysDebounced", "", func(tm *Tmux) error { return tm.SendKeysDebounced("w", "hi", 0) }},
		{"SendKeysReplace", "", func(tm *Tmux) error { return tm.SendKeysReplace("w", "hi", 0) }},
		{"WaitForIdle", "❯ ", func(tm *Tmux) error { return tm.WaitForIdle(context.Background(), "w", 5*time.Second) }},
		{"Respond", approval, func(tm *Tmux) error {
			err := tm.Respond("w", runtime.InteractionResponse{Action: "approve"})
			if err != nil && strings.Contains(err.Error(), "did not clear") {
				return nil // the fake never clears the prompt; the calls are what count
			}
			return err
		}},
		{"Peek", "x", func(tm *Tmux) error { _, err := (&Provider{tm: tm}).Peek("w", 10); return err }},
		{"NudgeSession", "❯ ", func(tm *Tmux) error { return tm.NudgeSession("w", "hi") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "Respond" {
				prevTimeout := respondVerifyTimeout
				respondVerifyTimeout = 20 * time.Millisecond
				t.Cleanup(func() { respondVerifyTimeout = prevTimeout })
			}
			exec := &markedAgentPaneExecutor{capture: tc.capture}
			tm := NewTmux()
			tm.exec = exec
			for op := 1; op <= 2; op++ {
				if err := tc.run(tm); err != nil {
					t.Fatalf("%s #%d: %v", tc.name, op, err)
				}
				if got := len(targetsOf(exec.calls, "list-panes")); got != op {
					t.Fatalf("after %d operations: %d agent-pane lookups, want %d; calls=%q", op, got, op, exec.calls)
				}
			}
		})
	}
}

// A pane a human scrolled into copy-mode is probed and released on the agent's
// pane: probing the active pane would miss a parked agent pane, and a cancel
// sent there would knock a human's pane out of the mode they chose.
func TestCopyModeProbeAndCancelTargetResolvedAgentPane(t *testing.T) {
	const fenced = "=w:@2.%5"
	approval := readApprovalFixture(t, "bash-manual-80x24.txt")
	prevTimeout := respondVerifyTimeout
	respondVerifyTimeout = 20 * time.Millisecond
	t.Cleanup(func() { respondVerifyTimeout = prevTimeout })
	for _, tc := range []struct {
		name    string
		capture string
		run     func(*Tmux) error
	}{
		{"SendKeysDebounced", "", func(tm *Tmux) error { return tm.SendKeysDebounced("w", "hi", 0) }},
		{"NudgeSession", "❯ ", func(tm *Tmux) error { return tm.NudgeSession("w", "hi") }},
		{"Respond", approval, func(tm *Tmux) error {
			err := tm.Respond("w", runtime.InteractionResponse{Action: "approve"})
			if err != nil && strings.Contains(err.Error(), "did not clear") {
				return nil
			}
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exec := &markedAgentPaneExecutor{capture: tc.capture, inMode: "1"}
			tm := NewTmux()
			tm.exec = exec
			if err := tc.run(tm); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			probe, cancel := "", ""
			for _, c := range exec.calls {
				target, _ := tmuxTargetArg(c)
				switch {
				case slices.Contains(c, "#{pane_in_mode}") && probe == "":
					probe = target
				case slices.Contains(c, "-X") && slices.Contains(c, "cancel") && cancel == "":
					cancel = target
				}
			}
			if probe != fenced || cancel != fenced {
				t.Errorf("copy-mode probe -t %q, cancel -t %q, want both %q; calls=%q", probe, cancel, fenced, exec.calls)
			}
		})
	}
}

// A failed agent-pane lookup sends nothing anywhere: no key reaches the
// active pane in its place.
func TestAgentPaneInputOpsFailClosed(t *testing.T) {
	boom := errors.New("tmux list-panes: boom")
	for _, tc := range []struct {
		name string
		run  func(*Tmux) error
	}{
		{"Interrupt", func(tm *Tmux) error { return (&Provider{tm: tm}).Interrupt("w") }},
		{"SendKeys", func(tm *Tmux) error { return (&Provider{tm: tm}).SendKeys("w", "Enter") }},
		{"ClearHistory", func(tm *Tmux) error { return tm.ClearHistory("w") }},
		{"startup dialogs", func(tm *Tmux) error {
			return tm.DismissKnownDialogs(context.Background(), "w", 50*time.Millisecond)
		}},
		{"NudgeSession", func(tm *Tmux) error { return tm.NudgeSession("w", "hi") }},
		// The startup nudge takes no prompt look, so nothing behind the
		// lookup would stop keys going to the active pane.
		{"startup nudge", func(tm *Tmux) error { return tm.nudgeStartupSession("w", "hi") }},
		{"SendKeysDebounced", func(tm *Tmux) error { return tm.SendKeysDebounced("w", "hi", 0) }},
		{"SendKeysReplace", func(tm *Tmux) error { return tm.SendKeysReplace("w", "hi", 0) }},
		{"Respond", func(tm *Tmux) error { return tm.Respond("w", runtime.InteractionResponse{Action: "approve"}) }},
		{"WaitForIdle", func(tm *Tmux) error { return tm.WaitForIdle(context.Background(), "w", time.Second) }},
		{"ClearInput", func(tm *Tmux) error { return tm.ClearInput(context.Background(), "w", 0) }},
		{"startup paneBusy", func(tm *Tmux) error {
			_, err := newTmuxStartOps(tm, "", 0, runtime.Config{}, true).paneBusy("w")
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exec := &markedAgentPaneExecutor{lookErr: boom}
			tm := NewTmux()
			tm.exec = exec
			if err := tc.run(tm); !errors.Is(err, boom) {
				t.Errorf("%s = %v, want the lookup error", tc.name, err)
			}
			for _, sub := range []string{"send-keys", "paste-buffer", "clear-history", "capture-pane"} {
				if got := targetsOf(exec.calls, sub); len(got) != 0 {
					t.Errorf("%s ran %s %q after a failed lookup", tc.name, sub, got)
				}
			}
		})
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

// Every session env pins the demand-spawn keys (processenv.DemandSpawnEnvKeys)
// empty, and that withholding has to survive into respawn-pane the way a
// credential's does: a warm box an older gc provisioned holds its own
// `-e GC_TRIGGER_BEAD_ID`, and a tmux server started from an agent's pane holds
// that pane's ids in its global env (gastownhall/gascity#5738). A stamped
// seat's GC_SPAWN_ORIGIN=demand is a value, not a withholding, so it is never
// marked; an unstamped seat pins it empty and has it marked too.
func TestDemandSpawnKeysMarkedRemovedOnCreateAndRespawn(t *testing.T) {
	triggerKeys := []string{
		"GC_TRIGGER_BEAD_ID",
		"GC_TRIGGER_BEAD_STORE_REF",
		"GC_TRIGGER_WORK_BEAD_ID",
		"GC_TRIGGER_WORK_STORE_REF",
	}
	seats := []struct {
		name   string
		origin string // GC_SPAWN_ORIGIN in the launch env
		marked []string
	}{
		{"stamped", "demand", triggerKeys},
		{"unstamped", "", append([]string{"GC_SPAWN_ORIGIN"}, triggerKeys...)},
	}
	paths := []struct {
		name string
		run  func(*Tmux, map[string]string) error
	}{
		{"create", func(tm *Tmux, env map[string]string) error {
			return tm.NewSessionWithCommandAndEnv("gc-test-demand-pin", "", "claude", env)
		}},
		{"respawn", func(tm *Tmux, env map[string]string) error {
			return (&tmuxStartOps{tm: tm}).respawnAgent("gc-test-demand-pin", "/proj", "claude", env)
		}},
	}
	for _, path := range paths {
		for _, seat := range seats {
			t.Run(path.name+"/"+seat.name, func(t *testing.T) {
				exec := &fakeExecutor{out: "agent\t@1.%0"}
				tm := NewTmux()
				tm.exec = exec

				env := map[string]string{"GC_CITY": "/tmp/city", "GC_SPAWN_ORIGIN": seat.origin}
				for _, key := range triggerKeys {
					env[key] = ""
				}
				if err := path.run(tm, env); err != nil {
					t.Fatalf("%s: %v", path.name, err)
				}
				var marked []string
				for _, call := range exec.calls {
					if i := slices.Index(call, "set-environment"); i >= 0 && slices.Contains(call[i:], "-r") {
						marked = append(marked, call[len(call)-1])
					}
				}
				if !slices.Equal(marked, seat.marked) {
					t.Errorf("keys marked removed from the session env = %v, want %v", marked, seat.marked)
				}
			})
		}
	}
}

// TestNewTmuxCommandBoundsPipeWait pins the WaitDelay on every real tmux
// subprocess. The hang it bounds needs a stopped tmux server holding the
// client's stdio (passed over SCM_RIGHTS), which the executor seam cannot
// model: it replaces the exec.Cmd entirely. So this checks the construction
// both real executor paths share, without starting a process.
func TestNewTmuxCommandBoundsPipeWait(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newTmuxCommand(context.Background(), []string{"-u", "list-panes", "-a"}, &stdout, &stderr)
	if cmd.WaitDelay != tmuxWaitDelay || tmuxWaitDelay <= 0 {
		t.Fatalf("WaitDelay = %v, want tmuxWaitDelay (%v) > 0", cmd.WaitDelay, tmuxWaitDelay)
	}
	if tmuxWaitDelay > fetchTimeout {
		t.Fatalf("tmuxWaitDelay = %v, want <= fetchTimeout (%v)", tmuxWaitDelay, fetchTimeout)
	}
	if cmd.Stdout != &stdout || cmd.Stderr != &stderr {
		t.Fatal("tmux command does not capture stdout and stderr into the given buffers")
	}
	if !slices.Equal(cmd.Args[1:], []string{"-u", "list-panes", "-a"}) {
		t.Fatalf("args = %q, want the tmux argv unchanged", cmd.Args)
	}
}

// TestSnapshotIdleSeparatesBusyFromIndeterminate pins the readings of the pane
// scan behind SnapshotIdle. Only a visible busy indicator is (false, nil). A
// capture showing neither it nor a ready prompt is
// runtime.ErrIdleIndeterminate: an idle TUI does not repaint a prompt that a
// broadcast message or an alternate screen displaced, so callers that hold on
// busy must not read that pane as busy.
func TestSnapshotIdleSeparatesBusyFromIndeterminate(t *testing.T) {
	errCapture := errors.New("capture-pane failed")
	for _, tc := range []struct {
		name              string
		pane              string
		captureErr        error
		wantIdle          bool
		wantIndeterminate bool
	}{
		{name: "ready prompt", pane: "● Done.\n\n❯ \n  ? for shortcuts", wantIdle: true},
		{name: "busy indicator over a visible prompt", pane: "❯ \n  esc to interrupt"},
		{name: "busy spinner", pane: "✻ Thinking… (12s · ↓ 1.2k tokens)"},
		{
			name:              "prompt displaced by a broadcast",
			pane:              "Broadcast message from root@host (somewhere) (Wed Oct  8 18:00:01 2026):\n\nDISK ALERT: /data at 91% used",
			wantIndeterminate: true,
		},
		{name: "empty capture", wantIndeterminate: true},
		{name: "capture failure", captureErr: errCapture},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fe := &fakeExecutor{
				agentPane: true,
				// show-environment finds no configured prefix, so the scan
				// looks for DefaultReadyPromptPrefix.
				outs: []string{"", tc.pane},
				errs: []error{errors.New("unknown variable: GC_READY_PROMPT_PREFIX"), tc.captureErr},
			}
			tm := &Tmux{cfg: Config{SocketName: "x"}, exec: fe}

			idle, err := tm.SnapshotIdle("sess")

			if len(fe.calls) != 3 || !slices.Contains(fe.calls[1], "list-panes") || !slices.Contains(fe.calls[2], "capture-pane") {
				t.Fatalf("tmux calls = %q, want show-environment, the agent-pane lookup, then one capture-pane", fe.calls)
			}
			if idle != tc.wantIdle {
				t.Errorf("idle = %t, want %t", idle, tc.wantIdle)
			}
			if got := errors.Is(err, runtime.ErrIdleIndeterminate); got != tc.wantIndeterminate {
				t.Errorf("errors.Is(err, ErrIdleIndeterminate) = %t, want %t (err = %v)", got, tc.wantIndeterminate, err)
			}
			switch {
			case tc.captureErr != nil:
				if !errors.Is(err, tc.captureErr) {
					t.Errorf("err = %v, want the capture error %v", err, tc.captureErr)
				}
			case !tc.wantIndeterminate && err != nil:
				t.Errorf("err = %v, want nil", err)
			}
		})
	}
}
