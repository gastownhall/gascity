package tmux

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestNewSessionKeepsSecretEnvOutOfArgv is the unit guard: a value that is not
// on the argv allow list must never appear in the tmux command line.
func TestNewSessionKeepsSecretEnvOutOfArgv(t *testing.T) {
	const secret = "sk-test-not-a-real-credential"
	fake := &fakeExecutor{}
	tm := NewTmux()
	tm.exec = fake

	env := map[string]string{"ANTHROPIC_AUTH_TOKEN": secret, "GC_RIG": "rig-a", "LC_ALL": ""}
	if err := tm.NewSessionWithCommandAndEnv("gc-test-secret-env", "/work", "claude", env); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	if len(fake.calls) == 0 {
		t.Fatal("no tmux calls recorded")
	}
	for _, call := range fake.calls {
		for _, arg := range call {
			if strings.Contains(arg, secret) {
				t.Fatalf("secret value reached tmux argv: %v", call)
			}
		}
	}

	create := fake.calls[0]
	if len(create) < 4 || create[len(create)-4] != "start-server" || create[len(create)-3] != ";" || create[len(create)-2] != "source-file" {
		t.Fatalf("session was not created from a command file: %v", create)
	}
	// Even the inert vars ride the file once one secret forces it: the whole
	// command moves, so there is no partial argv to get the split wrong.
	if slices.Contains(create, "-e") {
		t.Errorf("no -e flag may survive on the argv path: %v", create)
	}
}

// TestNewSessionKeepsInertEnvOnArgv pins the other half of the contract: an
// environment that cannot authenticate anything needs no temp file, so a host
// with a read-only temp dir still starts those sessions.
func TestNewSessionKeepsInertEnvOnArgv(t *testing.T) {
	fake := &fakeExecutor{}
	tm := NewTmux()
	tm.exec = fake

	env := map[string]string{"GC_RIG": "rig-a", "LANG": "en_US.UTF-8"}
	if err := tm.NewSessionWithCommandAndEnv("gc-test-inert-env", "/work", "claude", env); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	joined := strings.Join(fake.calls[0], "\x00")
	if !strings.Contains(joined, "\x00-e\x00GC_RIG=rig-a\x00") {
		t.Errorf("inert env did not stay on the -e path: %v", fake.calls[0])
	}
	if strings.Contains(joined, "source-file") {
		t.Errorf("inert env must not stage a command file: %v", fake.calls[0])
	}
}

// TestNewSessionFailsClosedWhenFileUnwritable proves the fix cannot silently
// degrade into the leak it replaces.
func TestNewSessionFailsClosedWhenFileUnwritable(t *testing.T) {
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "does-not-exist"))
	fake := &fakeExecutor{}
	tm := NewTmux()
	tm.exec = fake

	env := map[string]string{"OPENAI_API_KEY": "sk-test-not-a-real-credential"}
	err := tm.NewSessionWithCommandAndEnv("gc-test-nofile", "/work", "claude", env)
	if err == nil {
		t.Fatal("session creation must fail when the command file cannot be staged")
	}
	if len(fake.calls) != 0 {
		t.Errorf("no tmux command may run once staging failed: %v", fake.calls)
	}
}

func TestStageTmuxCommandFileIsPrivateAndRemovable(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	path, cleanup, err := stageTmuxCommandFile("new-session -d")
	if err != nil {
		t.Fatalf("stageTmuxCommandFile: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat staged file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("staged file mode = %04o, want 0600", got)
	}
	// The file name is random, but the directory holding it must not be
	// traversable either — a world-executable temp dir is what lets another
	// user stat their way to it.
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat staged dir: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("staged dir mode = %04o, want 0700", got)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read staged file: %v", err)
	}
	if string(body) != "new-session -d\n" {
		t.Errorf("staged file body = %q", body)
	}
	cleanup()
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Errorf("staged dir survived cleanup: %v", err)
	}
}

// TestSweepStaleStagedDirs covers the crash-between-create-and-cleanup case: a
// process killed before its deferred remove runs leaves a secret-bearing file
// with nothing to collect it, so the next session create does.
func TestSweepStaleStagedDirs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	old := time.Now().Add(-2 * staleStagedDirAge)

	stale := filepath.Join(dir, stagedDirPrefix+"orphan")
	if err := os.Mkdir(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, stagedFileName), []byte("new-session -e K=secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	fresh := filepath.Join(dir, stagedDirPrefix+"inflight")
	if err := os.Mkdir(fresh, 0o700); err != nil {
		t.Fatal(err)
	}
	// Not ours: an unrelated temp directory of the same age must survive.
	bystander := filepath.Join(dir, "someone-elses-dir")
	if err := os.Mkdir(bystander, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(bystander, old, old); err != nil {
		t.Fatal(err)
	}

	sweepStaleStagedDirs()

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale staged dir survived the sweep: %v", err)
	}
	for _, keep := range []string{fresh, bystander} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("sweep removed %s, which it must not touch: %v", filepath.Base(keep), err)
		}
	}
}

// TestNewSessionSweepsStaleStagedDirs pins the sweep to the moment a session is
// created, which is the only moment the staging directory is known to be in
// use.
func TestNewSessionSweepsStaleStagedDirs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	stale := filepath.Join(dir, stagedDirPrefix+"orphan")
	if err := os.Mkdir(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, stagedFileName), []byte("new-session -e K=secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * staleStagedDirAge)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	tm := NewTmux()
	tm.exec = &fakeExecutor{}
	if err := tm.NewSessionWithCommandAndEnv("gc-test-sweep", "", "claude",
		map[string]string{"ANTHROPIC_AUTH_TOKEN": "sk-test-not-a-real-credential"}); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("session create did not sweep the orphaned staged dir: %v", err)
	}
}

func TestTmuxQuote(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"plain", "'plain'"},
		{"", "''"},
		{"a b", "'a b'"},
		{"it's", `'it'\''s'`},
		{"two\nlines", "'two\nlines'"},
		{`$HOME #{x} "q" \z;`, `'$HOME #{x} "q" \z;'`},
	} {
		if got := tmuxQuote(tc.in); got != tc.want {
			t.Errorf("tmuxQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestTmuxCommandLineQuotesEveryArgument(t *testing.T) {
	got := tmuxCommandLine([]string{"new-session", "-d", "-s", "s", "-e", `K=a'b$c#d`, "agent --x"})
	want := `'new-session' '-d' '-s' 's' '-e' 'K=a'\''b$c#d' 'agent --x'`
	if got != want {
		t.Errorf("tmuxCommandLine =\n%s\nwant\n%s", got, want)
	}
}
