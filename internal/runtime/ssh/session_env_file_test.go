package ssh

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestShellEnvFileExportsSortedEntries(t *testing.T) {
	got := shellEnvFile(map[string]string{"B": "two", "A": `a'b`})
	want := "A='a'\\''b'\nexport A\nB='two'\nexport B\n"
	if got != want {
		t.Errorf("shellEnvFile =\n%q\nwant\n%q", got, want)
	}
}

// TestStagingScriptHeredocSurvivesHostileValues covers the one way a staged
// file could be corrupted (or worse, terminated early so the rest of the value
// executes as shell): a value containing what looks like a heredoc terminator.
func TestStagingScriptHeredocSurvivesHostileValues(t *testing.T) {
	body := "K='GC_EOF_DEADBEEF\nnot the end'\nexport K\n"
	script, err := stagingScript(body, "")
	if err != nil {
		t.Fatalf("stagingScript: %v", err)
	}
	tag := heredocTagOf(t, script)
	if strings.Contains(body, tag) {
		t.Fatalf("delimiter %q occurs inside the staged body", tag)
	}
	// Exactly two occurrences: the <<'TAG' opener and the closing line.
	if n := strings.Count(script, tag); n != 2 {
		t.Errorf("delimiter appears %d times, want 2:\n%s", n, script)
	}
	if !strings.Contains(script, "\n"+tag+"\n") {
		t.Error("heredoc is not closed on its own line")
	}
}

func TestStagingScriptOmitsTmuxFileWhenNoSessionIsCreated(t *testing.T) {
	script, err := stagingScript("K='v'\nexport K\n", "")
	if err != nil {
		t.Fatalf("stagingScript: %v", err)
	}
	if strings.Contains(script, "session.tmux") {
		t.Error("relaunch stages no tmux command file")
	}
	if !strings.Contains(script, "env.sh") {
		t.Error("relaunch still stages the sh env file")
	}
	if !strings.Contains(script, `mktemp -d "$t/`+stagedDirPrefix+`XXXXXX"`) {
		t.Error("staging script does not create a private directory")
	}
}

// TestStagingScriptVerifiesEveryWrite pins the fix for the silent-truncation
// hole: an unchecked heredoc on a full remote disk yields a short env.sh that
// sources cleanly and starts a session missing exactly the credentials it
// needed.
func TestStagingScriptVerifiesEveryWrite(t *testing.T) {
	envBody := "K='v'\nexport K\n"
	tmuxBody := "'new-session' '-d'\n"
	script, err := stagingScript(envBody, tmuxBody)
	if err != nil {
		t.Fatalf("stagingScript: %v", err)
	}
	for _, want := range []string{
		`cat > "$d/env.sh" <<'`,
		`cat > "$d/session.tmux" <<'`,
		fmt.Sprintf(`[ "$(wc -c < "$d/env.sh")" -eq %d ] || fail`, len(envBody)),
		fmt.Sprintf(`[ "$(wc -c < "$d/session.tmux")" -eq %d ] || fail`, len(tmuxBody)),
		`fail() { rm -rf "$d"; exit 1; }`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("staging script missing %q:\n%s", want, script)
		}
	}
	// Both heredocs carry the exit-status check on the cat itself.
	if n := strings.Count(script, `<<'`); n != strings.Count(script, `' || fail`) {
		t.Errorf("not every heredoc checks cat's exit status:\n%s", script)
	}
}

// TestStagingScriptFailsClosedOnAShortWrite drives the real shell down the
// truncation path — the one an unchecked heredoc would have let through — and
// pins both halves of the contract: non-zero exit, and no half-written
// credential left on the box.
func TestStagingScriptFailsClosedOnAShortWrite(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	envBody := "K='v'\nexport K\n"
	script, err := stagingScript(envBody, "")
	if err != nil {
		t.Fatalf("stagingScript: %v", err)
	}
	// Corrupt the expected byte count the way a short write would: the file on
	// disk no longer matches what we asked the box to store.
	short := strings.Replace(script, fmt.Sprintf("-eq %d ]", len(envBody)), "-eq 999 ]", 1)
	if short == script {
		t.Fatalf("test needs updating: no byte-count check found in\n%s", script)
	}

	tmp := t.TempDir()
	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader(short)
	cmd.Env = append(os.Environ(), "TMPDIR="+tmp)
	out, err := cmd.Output()
	if err == nil {
		t.Fatal("staging script must exit non-zero when a staged file is short")
	}
	if dir := lastLine(string(out)); strings.HasPrefix(dir, "/") {
		t.Errorf("staging script reported a directory %q despite failing", dir)
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("failed staging left files behind: %v", entries)
	}
}

// TestStagingScriptRunsAgainstARealShell executes the generated script for real
// and checks the artifacts it is supposed to leave behind: a 0700 directory
// holding 0600 files whose bytes match what we asked for.
func TestStagingScriptRunsAgainstARealShell(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	envBody := shellEnvFile(map[string]string{"K": "a'b\"c$d#e\nf"})
	tmuxBody := tmuxCommandLine([]string{"new-session", "-d", "-e", "K=a'b"}) + "\n"
	script, err := stagingScript(envBody, tmuxBody)
	if err != nil {
		t.Fatalf("stagingScript: %v", err)
	}

	tmp := t.TempDir()
	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = append(os.Environ(), "TMPDIR="+tmp)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run staging script: %v", err)
	}
	dir := lastLine(string(out))
	if !strings.HasPrefix(dir, tmp) {
		t.Fatalf("staging script reported %q, want a directory under %q", dir, tmp)
	}
	if info, err := os.Stat(dir); err != nil {
		t.Fatalf("stat staged dir: %v", err)
	} else if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("staged dir mode = %04o, want 0700", got)
	}
	for name, want := range map[string]string{envFileName: envBody, tmuxFileName: tmuxBody} {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %04o, want 0600", name, got)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(body) != want {
			t.Errorf("%s =\n%q\nwant\n%q", name, body, want)
		}
	}

	// Sourcing the env file must reproduce the value byte for byte.
	got, err := exec.Command("sh", "-c", ". "+shellQuote([]string{filepath.Join(dir, envFileName)})+`; printf %s "$K"`).Output()
	if err != nil {
		t.Fatalf("source staged env file: %v", err)
	}
	if string(got) != "a'b\"c$d#e\nf" {
		t.Errorf("sourced value = %q", got)
	}
}

// TestStagingScriptSweepsStaleDirectories covers the crash case: a gc killed
// between mktemp and its cleanup leaves a secret-bearing directory on the box
// with nothing to remove it, so the next staging run collects it.
func TestStagingScriptSweepsStaleDirectories(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	if _, err := exec.LookPath("find"); err != nil {
		t.Skip("no find")
	}
	tmp := t.TempDir()
	stale := filepath.Join(tmp, stagedDirPrefix+"orphan")
	if err := os.Mkdir(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, envFileName), []byte("K='secret'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * staleStagedDirMinutes * time.Minute)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	// A same-aged directory that is not ours must survive.
	bystander := filepath.Join(tmp, "someone-elses-dir")
	if err := os.Mkdir(bystander, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(bystander, old, old); err != nil {
		t.Fatal(err)
	}

	script, err := stagingScript("K='v'\nexport K\n", "")
	if err != nil {
		t.Fatalf("stagingScript: %v", err)
	}
	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = append(os.Environ(), "TMPDIR="+tmp)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run staging script: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale staged directory survived the sweep: %v", err)
	}
	if _, err := os.Stat(bystander); err != nil {
		t.Errorf("sweep removed an unrelated directory: %v", err)
	}
	// The sweep must not take the run's own fresh directory with it.
	if dir := lastLine(string(out)); dir == "" {
		t.Error("staging script reported no directory")
	} else if _, err := os.Stat(dir); err != nil {
		t.Errorf("sweep removed the directory this run just created: %v", err)
	}
}

// heredocTagOf pulls the generated delimiter back out of the script.
func heredocTagOf(t *testing.T, script string) string {
	t.Helper()
	_, rest, ok := strings.Cut(script, "<<'")
	if !ok {
		t.Fatalf("no heredoc in script:\n%s", script)
	}
	tag, _, ok := strings.Cut(rest, "'")
	if !ok {
		t.Fatalf("unterminated heredoc opener:\n%s", script)
	}
	return tag
}

func TestStageSecretEnvReadsTheLastLineOfTheScriptOutput(t *testing.T) {
	// A chatty remote profile can write to stdout ahead of mktemp's answer.
	f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
		if len(argv) == 1 && argv[0] == "sh" {
			return []byte("Welcome to the box\n/tmp/gc-session-abc123\n"), 0, nil
		}
		return nil, 0, nil
	}}
	p := &Provider{conn: &Conn{ep: Endpoint{Host: "box"}, run: f}}
	staged, err := p.stageSecretEnv(context.Background(), map[string]string{"K": "v"}, nil)
	if err != nil {
		t.Fatalf("stageSecretEnv: %v", err)
	}
	if staged.dir != "/tmp/gc-session-abc123" {
		t.Errorf("dir = %q, want the last line of the script output", staged.dir)
	}
	if staged.tmuxPath != "" {
		t.Errorf("no tmux command file is staged when no session is created: %q", staged.tmuxPath)
	}
}

func TestStageSecretEnvRejectsOutputThatIsNotAStagedDir(t *testing.T) {
	// The reported path reaches `rm -rf` on cleanup, so anything that is not
	// recognizably one of ours has to be refused rather than acted on.
	for name, out := range map[string]string{
		"mktemp failed":     "mktemp: failed\n",
		"relative path":     "gc-session-abc\n",
		"unrelated absdir":  "/etc\n",
		"trailing chatter":  "/tmp/gc-session-abc\nhave a nice day\n",
		"empty script exit": "",
	} {
		f := &fakeRunner{respond: func([]string) ([]byte, int, error) { return []byte(out), 0, nil }}
		p := &Provider{conn: &Conn{ep: Endpoint{Host: "box"}, run: f}}
		if _, err := p.stageSecretEnv(context.Background(), map[string]string{"K": "v"}, nil); err == nil {
			t.Errorf("%s: staging must fail when the script does not report a staged directory", name)
		}
	}
}

func TestStageSecretEnvIsANoOpWithoutSecrets(t *testing.T) {
	f := &fakeRunner{}
	p := &Provider{conn: &Conn{ep: Endpoint{Host: "box"}, run: f}}
	staged, err := p.stageSecretEnv(context.Background(), nil, []string{"new-session"})
	if err != nil {
		t.Fatalf("stageSecretEnv: %v", err)
	}
	if staged.staged() {
		t.Errorf("nothing may be written to the box for an all-inert env: %+v", staged)
	}
	if len(f.calls) != 0 {
		t.Errorf("no remote call may be made: %v", f.calls)
	}
}

func TestTmuxCommandLineQuotesEveryArgument(t *testing.T) {
	got := tmuxCommandLine([]string{"new-session", "-d", "-s", "s", "-e", `K=a'b$c#d`, "agent --x"})
	want := `'new-session' '-d' '-s' 's' '-e' 'K=a'\''b$c#d' 'agent --x'`
	if got != want {
		t.Errorf("tmuxCommandLine =\n%s\nwant\n%s", got, want)
	}
}
