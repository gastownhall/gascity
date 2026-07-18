package herdr

import (
	"context"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// The tests below drive Provider.start against a fake `herdr` CLI (a shell
// script that logs every invocation and answers with canned JSON envelopes),
// pinning the host-side workdir-preparation contract added for parity with
// tmux/subprocess: staging runs, pre_start runs BEFORE the agent launches and
// its failure aborts the Start, the prepared workdir is what the agent gets
// (--cwd), and session_setup runs host-side after launch, non-fatally.
// start (not Start) is the entry point so no real session-server socket is
// needed — mirroring how the tmux package tests doStartSession, not Start.

// fakeHerdr is a stand-in herdr CLI. Each invocation appends its argv (minus
// the --session pair) to log; `agent start` additionally records whether
// probe existed at launch time, which is how tests observe pre_start-vs-launch
// ordering on disk.
type fakeHerdr struct {
	bin   string
	log   string
	probe string
}

func newFakeHerdr(t *testing.T, probe string) *fakeHerdr {
	t.Helper()
	if goruntime.GOOS == "windows" {
		t.Skip("fake herdr CLI is a POSIX shell script")
	}
	dir := t.TempDir()
	f := &fakeHerdr{
		bin:   filepath.Join(dir, "herdr"),
		log:   filepath.Join(dir, "invocations.log"),
		probe: probe,
	}
	script := `#!/bin/sh
shift 2 # drop --session <name>
printf '%s\n' "$*" >> '` + f.log + `'
case "$1 $2" in
"agent list") printf '{"result":{"agents":[]}}' ;;
"agent start")
	if [ -e '` + f.probe + `' ]; then
		printf 'probe-at-agent-start: present\n' >> '` + f.log + `'
	else
		printf 'probe-at-agent-start: absent\n' >> '` + f.log + `'
	fi
	printf '{"result":{"agent":{"name":"%s","pane_id":"p1"}}}' "$3" ;;
"agent get") printf '{"result":{"agent":{"name":"%s","pane_id":"p1","agent_status":"idle"}}}' "$3" ;;
"workspace list") printf '{"result":{"workspaces":[]}}' ;;
"workspace create") printf '{"result":{"workspace":{"workspace_id":"w1"},"tab":{"tab_id":"t1"},"root_pane":{"pane_id":"stray"}}}' ;;
"tab list") printf '{"result":{"tabs":[]}}' ;;
"tab create") printf '{"result":{"tab":{"tab_id":"t1"},"root_pane":{"pane_id":"stray"}}}' ;;
"agent read") printf '{"result":{"read":{"text":"⚠ 8 hooks need review before they can run.\\nPress t to trust all; enter to review hooks; esc to skip"}}}' ;;
*) printf '{"result":{}}' ;;
esac
`
	if err := os.WriteFile(f.bin, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake herdr: %v", err)
	}
	return f
}

// logLines returns the fake CLI's invocation log, one line per entry.
func (f *fakeHerdr) logLines(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(f.log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading fake herdr log: %v", err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

// logIndex returns the index of the first log line containing substr, or -1.
func logIndex(lines []string, substr string) int {
	for i, l := range lines {
		if strings.Contains(l, substr) {
			return i
		}
	}
	return -1
}

// newFakeStartProvider builds a Provider whose client shells out to the fake
// herdr CLI instead of a real one.
func newFakeStartProvider(t *testing.T, f *fakeHerdr) *Provider {
	t.Helper()
	p := New("teststart", t.TempDir(), "/city/root", 0)
	p.c.bin = f.bin
	return p
}

// sq single-quotes a path for safe embedding in a sh command string.
func sq(s string) string { return "'" + s + "'" }

// TestStartRunsPreStartBeforeAgentLaunch pins the core of the fix: pre_start
// commands (directory/worktree preparation) execute host-side BEFORE the agent
// is spawned, and the directory they create is the --cwd the agent launches
// in — no silent city-root fallback.
func TestStartRunsPreStartBeforeAgentLaunch(t *testing.T) {
	work := filepath.Join(t.TempDir(), "per-bead-worktree") // does not exist yet
	marker := filepath.Join(t.TempDir(), "prestart-ran")
	f := newFakeHerdr(t, marker)
	p := newFakeStartProvider(t, f)

	cfg := runtime.Config{
		WorkDir: work,
		PreStart: []string{
			"mkdir -p " + sq(work), // the worktree-setup role
			"touch " + sq(marker),
		},
	}
	if err := p.start(context.Background(), "gastown__worker", cfg); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := os.Stat(work); err != nil {
		t.Fatalf("pre_start did not create the workdir: %v", err)
	}

	lines := f.logLines(t)
	startIdx := logIndex(lines, "agent start")
	if startIdx < 0 {
		t.Fatalf("agent was never started; log:\n%s", strings.Join(lines, "\n"))
	}
	// The probe (created by pre_start) must already exist when `agent start`
	// runs: pre_start strictly precedes the launch.
	if probeIdx := logIndex(lines, "probe-at-agent-start: present"); probeIdx != startIdx+1 {
		t.Errorf("pre_start effects not visible at agent launch; log:\n%s", strings.Join(lines, "\n"))
	}
	// The prepared workdir — not the city root — is the launch cwd.
	if !strings.Contains(lines[startIdx], "--cwd "+work) {
		t.Errorf("agent start line missing --cwd %s: %q", work, lines[startIdx])
	}
}

// TestStartFailsWhenPreStartFails pins the fatal error semantics shared with
// tmux: a pre_start failure aborts the Start loudly (wrapped "running
// pre_start", indexed, with the command's stderr folded in) and the agent is
// never launched.
func TestStartFailsWhenPreStartFails(t *testing.T) {
	work := t.TempDir()
	f := newFakeHerdr(t, filepath.Join(work, "unused-probe"))
	p := newFakeStartProvider(t, f)

	cfg := runtime.Config{
		WorkDir:  work,
		PreStart: []string{"printf 'worktree add exploded' >&2; exit 7"},
	}
	err := p.start(context.Background(), "gastown__worker", cfg)
	if err == nil {
		t.Fatal("expected error from failing pre_start")
	}
	for _, want := range []string{"running pre_start", "pre_start[0]", "exit status 7", "worktree add exploded"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
	if idx := logIndex(f.logLines(t), "agent start"); idx >= 0 {
		t.Errorf("agent must not launch after a pre_start failure; log:\n%s", strings.Join(f.logLines(t), "\n"))
	}
}

// TestStartStagesWorkDirBeforePreStart pins that CopyFiles staging runs and
// that it precedes pre_start — the same order as tmux (stageStartFiles, then
// doStartSession Step 0): the pre_start command here only succeeds if the
// staged file is already in place.
func TestStartStagesWorkDirBeforePreStart(t *testing.T) {
	work := t.TempDir()
	src := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(work, "notes.txt")
	f := newFakeHerdr(t, staged)
	p := newFakeStartProvider(t, f)

	cfg := runtime.Config{
		WorkDir:   work,
		CopyFiles: []runtime.CopyEntry{{Src: src}},
		PreStart:  []string{"test -f " + sq(staged)},
	}
	if err := p.start(context.Background(), "gastown__worker", cfg); err != nil {
		t.Fatalf("start: %v (staging must run before pre_start)", err)
	}
	b, err := os.ReadFile(staged)
	if err != nil || string(b) != "payload" {
		t.Fatalf("staged file = %q, %v; want payload", b, err)
	}
	lines := f.logLines(t)
	startIdx := logIndex(lines, "agent start")
	if startIdx < 0 {
		t.Fatal("agent was never started")
	}
	if !strings.Contains(lines[startIdx], "--cwd "+work) {
		t.Errorf("agent start line missing --cwd %s: %q", work, lines[startIdx])
	}
}

// TestStartFailsOnAbsentWorkDir pins effectiveWorkDir's fail-loudly path at
// the Start level: a configured WorkDir that still doesn't exist after
// staging/pre_start is an error, not a silent city-root launch.
func TestStartFailsOnAbsentWorkDir(t *testing.T) {
	work := filepath.Join(t.TempDir(), "never-created")
	f := newFakeHerdr(t, work)
	p := newFakeStartProvider(t, f)

	err := p.start(context.Background(), "gastown__worker", runtime.Config{WorkDir: work})
	if err == nil {
		t.Fatal("expected error for a set-but-absent workdir")
	}
	if !strings.Contains(err.Error(), "unavailable after staging/pre_start") {
		t.Errorf("error = %q, want workdir-unavailable detail", err)
	}
	if idx := logIndex(f.logLines(t), "agent start"); idx >= 0 {
		t.Errorf("agent must not launch in a fallback dir; log:\n%s", strings.Join(f.logLines(t), "\n"))
	}
}

// TestStartRunsSessionSetupAfterLaunch pins the session_setup wiring: commands
// and the script run host-side AFTER the agent launches (and after the
// readiness wait, mirroring tmux's Step 5.5), receive GC_SESSION, and their
// failures are non-fatal.
func TestStartRunsSessionSetupAfterLaunch(t *testing.T) {
	work := t.TempDir()
	f := newFakeHerdr(t, filepath.Join(work, "unused-probe"))
	p := newFakeStartProvider(t, f)
	sessionEnvOut := filepath.Join(t.TempDir(), "gc-session-value")

	cfg := runtime.Config{
		WorkDir: work,
		SessionSetup: []string{
			`printf 'session_setup-ran\n' >> ` + sq(f.log) + `; printf '%s' "$GC_SESSION" > ` + sq(sessionEnvOut),
			"exit 1", // non-fatal: Start must still succeed
		},
		SessionSetupScript: `printf 'session_setup_script-ran\n' >> ` + sq(f.log),
	}
	if err := p.start(context.Background(), "gastown__worker", cfg); err != nil {
		t.Fatalf("start: %v (session_setup failures must be non-fatal)", err)
	}

	if b, err := os.ReadFile(sessionEnvOut); err != nil || string(b) != "gastown__worker" {
		t.Errorf("GC_SESSION seen by session_setup = %q, %v; want gastown__worker", b, err)
	}
	lines := f.logLines(t)
	startIdx := logIndex(lines, "agent start")
	waitIdx := logIndex(lines, "agent wait")
	setupIdx := logIndex(lines, "session_setup-ran")
	scriptIdx := logIndex(lines, "session_setup_script-ran")
	if startIdx < 0 || setupIdx < 0 || scriptIdx < 0 {
		t.Fatalf("missing expected log entries; log:\n%s", strings.Join(lines, "\n"))
	}
	if startIdx >= setupIdx || setupIdx >= scriptIdx {
		t.Errorf("session_setup must run after launch, script after commands; log:\n%s", strings.Join(lines, "\n"))
	}
	// The readiness wait precedes setup, mirroring tmux's ready→setup→nudge.
	if waitIdx < 0 || waitIdx >= setupIdx {
		t.Errorf("expected an idle wait before session_setup; log:\n%s", strings.Join(lines, "\n"))
	}
}

func TestStartAcceptsStartupDialogsBeforeIdleWait(t *testing.T) {
	work := t.TempDir()
	f := newFakeHerdr(t, filepath.Join(work, "unused-probe"))
	p := newFakeStartProvider(t, f)

	if err := p.start(context.Background(), "gastown__worker", runtime.Config{
		WorkDir:      work,
		ProcessNames: []string{"codex"},
	}); err != nil {
		t.Fatalf("start: %v", err)
	}

	lines := f.logLines(t)
	readIdx := logIndex(lines, "agent read")
	downIdx := logIndex(lines, "pane send-keys p1 Down")
	enterIdx := logIndex(lines, "pane send-keys p1 Enter")
	if readIdx < 0 || downIdx <= readIdx || enterIdx <= downIdx {
		t.Fatalf("startup dialog was not accepted after read; log:\n%s", strings.Join(lines, "\n"))
	}
}

func TestShouldAcceptStartupDialogs(t *testing.T) {
	enabled := true
	disabled := false
	for name, tc := range map[string]struct {
		cfg  runtime.Config
		want bool
	}{
		"explicit enabled":   {cfg: runtime.Config{AcceptStartupDialogs: &enabled}, want: true},
		"explicit disabled":  {cfg: runtime.Config{AcceptStartupDialogs: &disabled, ProcessNames: []string{"codex"}}, want: false},
		"process names":      {cfg: runtime.Config{ProcessNames: []string{"codex"}}, want: true},
		"permission warning": {cfg: runtime.Config{EmitsPermissionWarning: true}, want: true},
		"default disabled":   {cfg: runtime.Config{}, want: false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := runtime.ShouldAcceptStartupDialogs(tc.cfg); got != tc.want {
				t.Fatalf("runtime.ShouldAcceptStartupDialogs() = %t, want %t", got, tc.want)
			}
		})
	}
}
