//go:build acceptance_c

package tutorialgoldens

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
	"github.com/gastownhall/gascity/test/dolttest"
)

const canonicalTutorialRoot = "docs/tutorials"

var (
	goldenGCBinary string
	goldenBDPath   string
)

func TestMain(m *testing.M) {
	if !hasClaudeAuth() || (!useClaudeForCodex() && !hasCodexAuth()) {
		if !hostProviderMode() {
			fmt.Fprintf(os.Stderr, "tutorial-goldens: skipping package: on a developer box the provider CLIs run under an isolated HOME holding a copy of their credentials, and none was found to copy (~/.claude/.credentials.json, ~/.codex/auth.json, or ANTHROPIC_API_KEY/OPENAI_API_KEY). Set %s=1 to run them against your real home instead, which writes test trust entries into your ~/.claude.json\n", helpers.EnvAllowHostClaude)
			os.Exit(0)
		}
		if useClaudeForCodex() {
			fmt.Fprintln(os.Stderr, "tutorial-goldens: skipping package (requires Claude auth)")
		} else {
			fmt.Fprintln(os.Stderr, "tutorial-goldens: skipping package (requires both Claude and Codex auth)")
		}
		os.Exit(0)
	}

	tmpRoot, err := acceptanceTempRoot()
	if err != nil {
		panic("tutorial-goldens: preparing temp root: " + err.Error())
	}
	if err := os.Setenv("TMPDIR", tmpRoot); err != nil {
		panic("tutorial-goldens: setting TMPDIR: " + err.Error())
	}
	tmpDir, err := os.MkdirTemp(tmpRoot, fmt.Sprintf("gctutorial-%d-*", os.Getpid()))
	if err != nil {
		panic("tutorial-goldens: creating temp dir: " + err.Error())
	}
	if os.Getenv("GC_ACCEPTANCE_KEEP") != "1" {
		defer os.RemoveAll(tmpDir)
	}

	goldenGCBinary = helpers.BuildGC(tmpDir)
	if _, err := exec.LookPath("tmux"); err != nil {
		panic("tutorial-goldens: tmux not found")
	}
	if path, err := exec.LookPath("bd"); err == nil {
		goldenBDPath = path
	} else {
		panic("tutorial-goldens: bd not found")
	}

	// Reap dolt orphans left by prior crashed runs, then guard this run so an
	// interrupt / timeout / OOM does not leak a dolt sql-server (issue #3640).
	dolttest.SweepStale(tmpRoot, "gctutorial-")
	stopGuard := dolttest.Guard(tmpDir)

	code := m.Run()
	stopGuard()
	os.Exit(code)
}

type tutorialEnv struct {
	Root string
	Home string
	// ProviderHome is the HOME the wrapped provider CLIs run under: the
	// operator's real home in host mode, the isolated gc HOME otherwise.
	ProviderHome string
	RuntimeDir   string
	Env          *helpers.Env

	supervisor     *exec.Cmd
	supervisorDone chan error
	supervisorLog  *os.File
}

func tutorialTmuxTmpDir(runtimeDir string) string {
	return filepath.Join(runtimeDir, "tmux")
}

// newTutorialBaseEnv builds the tutorial Env. A non-empty bdPath is staged in
// the tutorial bin dir behind the tool-home wrapper: gc and the tutorial's own
// `bd` commands run with the real HOME, and bd must never resolve the
// operator's ~/.beads (or a user-level dolt.shared-server: true) through it.
func newTutorialBaseEnv(gcBinary, home, runtimeDir, bdPath string) *helpers.Env {
	env := helpers.NewEnv(gcBinary, home, runtimeDir)
	if hostProviderMode() {
		// Host mode (CI's throwaway home, or an explicit opt-in): the wrapped
		// provider binaries delegate to the operator's authenticated CLIs,
		// which resolve their state through the host HOME and read trust from
		// the real ~/.claude.json (the shim drops CLAUDE_CONFIG_DIR).
		env.WithHostHome().WithHostClaudeState()
	}
	// Otherwise gc keeps NewEnv's isolated HOME, which is also where the
	// provider CLIs run with a copy of their credentials (providerHome).
	env.Without("GC_SESSION").
		Without("GC_BEADS").
		Without("GC_DOLT").
		With("DOLT_ROOT_PATH", home)
	binDir := filepath.Join(home, ".local", "bin")
	if bdPath != "" {
		if _, err := helpers.InstallBeadsTooling(env, binDir, bdPath, ""); err != nil {
			panic("tutorial-goldens: " + err.Error())
		}
	}
	env.With("PATH", binDir+":"+env.Get("PATH"))
	// Tutorial cities all use the same workspace name (`my-city`), so without an
	// isolated tmux socket root they can adopt stale sessions from earlier runs.
	// That lets `peek` hit an old pane while `session logs` resolves the current
	// run's bead/work_dir and finds no transcript at all.
	env.With("TMUX_TMPDIR", tutorialTmuxTmpDir(runtimeDir))
	return env
}

func linkTutorialSessionRoots(hostHome, tutorialHome string) error {
	type sessionRoot struct {
		host string
		dst  string
	}
	roots := []sessionRoot{
		{
			host: filepath.Join(hostHome, ".claude", "projects"),
			dst:  filepath.Join(tutorialHome, ".claude", "projects"),
		},
		{
			host: filepath.Join(hostHome, ".codex", "sessions"),
			dst:  filepath.Join(tutorialHome, ".codex", "sessions"),
		},
		{
			host: filepath.Join(hostHome, ".gemini", "tmp"),
			dst:  filepath.Join(tutorialHome, ".gemini", "tmp"),
		},
	}
	for _, root := range roots {
		if err := os.MkdirAll(filepath.Dir(root.dst), 0o755); err != nil {
			return err
		}
		if info, err := os.Lstat(root.dst); err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				target, readErr := os.Readlink(root.dst)
				if readErr == nil && target == root.host {
					continue
				}
			}
			if removeErr := os.RemoveAll(root.dst); removeErr != nil {
				return removeErr
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := os.Symlink(root.host, root.dst); err != nil {
			return err
		}
	}
	return nil
}

func newTutorialEnv(t *testing.T) *tutorialEnv {
	t.Helper()

	tmpRoot, err := acceptanceTempRoot()
	if err != nil {
		t.Fatalf("preparing tutorial temp root: %v", err)
	}
	cleanupStaleTutorialProcesses(t, tmpRoot)
	root, err := os.MkdirTemp(tmpRoot, "gctutenv-*")
	if err != nil {
		t.Fatalf("creating tutorial temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	home := filepath.Join(root, "home")
	runtimeDir := filepath.Join(root, "runtime")
	tmuxTmpDir := tutorialTmuxTmpDir(runtimeDir)
	for _, dir := range []string{home, runtimeDir, tmuxTmpDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("creating %s: %v", dir, err)
		}
	}
	if err := helpers.WriteSupervisorConfig(home); err != nil {
		t.Fatalf("writing supervisor config: %v", err)
	}
	provider := providerHome(home)
	if err := stageClaudeAuth(provider); err != nil {
		t.Fatalf("staging Claude auth: %v", err)
	}
	if err := helpers.EnsureClaudeStateFile(home); err != nil {
		t.Fatalf("seeding Claude state: %v", err)
	}
	if err := stageCodexAuth(provider); err != nil {
		t.Fatalf("staging Codex auth: %v", err)
	}
	if err := stageProviderBinaries(home, provider); err != nil {
		t.Fatalf("staging provider binaries: %v", err)
	}
	if hostProviderMode() {
		// The CLIs write their transcripts under the real home; bridge them
		// in. In isolated mode they already land under the provider home.
		if err := linkTutorialSessionRoots(hostHomeDir(), home); err != nil {
			t.Fatalf("linking session roots: %v", err)
		}
	}

	env := newTutorialBaseEnv(goldenGCBinary, home, runtimeDir, goldenBDPath)

	for _, key := range []string{
		"ANTHROPIC_AUTH_TOKEN",
		"ANTHROPIC_API_KEY",
		"ANTHROPIC_BASE_URL",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL",
		"ANTHROPIC_DEFAULT_OPUS_MODEL",
		"ANTHROPIC_DEFAULT_SONNET_MODEL",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC",
		"CLAUDE_CODE_EFFORT_LEVEL",
		"CLAUDE_CODE_SUBAGENT_MODEL",
		"OPENAI_API_KEY",
	} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			env.With(key, value)
		}
	}

	tutorial := &tutorialEnv{
		Root:         root,
		Home:         home,
		ProviderHome: provider,
		RuntimeDir:   runtimeDir,
		Env:          env,
	}
	if err := startTutorialSupervisor(tutorial); err != nil {
		stopTutorialSupervisor(tutorial)
		t.Fatalf("starting tutorial supervisor: %v", err)
	}
	t.Cleanup(func() {
		stopTutorialSupervisor(tutorial)
	})
	return tutorial
}

func cleanupStaleTutorialProcesses(t *testing.T, tmpRoot string) {
	t.Helper()

	out, err := exec.Command("ps", "-ax", "-o", "pid=,command=").Output()
	if err != nil {
		t.Fatalf("listing stale tutorial processes: %v", err)
	}

	prefixes := []string{
		filepath.Join(tmpRoot, "gctutenv-"),
		filepath.Join("/private", strings.TrimPrefix(tmpRoot, "/"), "gctutenv-"),
	}
	selfPID := os.Getpid()
	var victims []int
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 1 || pid == selfPID {
			continue
		}
		cmd := strings.Join(fields[1:], " ")
		matched := false
		for _, prefix := range prefixes {
			if strings.Contains(cmd, prefix) {
				matched = true
				break
			}
		}
		if matched {
			victims = append(victims, pid)
		}
	}
	if len(victims) == 0 {
		return
	}

	for _, pid := range victims {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	time.Sleep(500 * time.Millisecond)
	for _, pid := range victims {
		if err := syscall.Kill(pid, 0); err == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

// tutorialSupervisorReadyTimeout bounds startTutorialSupervisor's wait for
// the control socket. It matches gc's own supervisorReadyTimeout
// (cmd/gc/cmd_supervisor_lifecycle.go), the budget `gc supervisor start`
// gives the same readiness condition.
const tutorialSupervisorReadyTimeout = 15 * time.Second

func startTutorialSupervisor(env *tutorialEnv) error {
	if env == nil || env.Env == nil {
		return fmt.Errorf("tutorial env is not initialized")
	}

	gcPath, err := helpers.ResolveGCPath(env.Env)
	if err != nil {
		return err
	}

	logPath := filepath.Join(env.Home, "supervisor.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return err
	}

	cmd := exec.Command(gcPath, "supervisor", "run")
	cmd.Dir = env.Home
	cmd.Env = env.Env.List()
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return err
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	env.supervisor = cmd
	env.supervisorDone = done
	env.supervisorLog = logFile

	deadline := time.Now().Add(tutorialSupervisorReadyTimeout)
	var lastOut string
	var lastErr error
	for time.Now().Before(deadline) {
		// Ready means the control socket answers with this supervisor's PID:
		// the only liveness signal `gc init`/`gc start` trust, and the last
		// thing the supervisor binds at startup, after its lock and its API.
		out, err := runEnvCommandWithTimeout(env, env.Home, 2*time.Second, "gc", "supervisor", "status")
		if err == nil && helpers.SupervisorStatusConfirmsPID(out, cmd.Process.Pid) {
			return nil
		}
		lastOut, lastErr = out, err
		select {
		case err := <-done:
			env.supervisor = nil
			env.supervisorDone = nil
			_ = logFile.Close()
			env.supervisorLog = nil
			logData, _ := os.ReadFile(logPath)
			if err == nil {
				return fmt.Errorf("tutorial supervisor exited early:\n%s", string(logData))
			}
			return fmt.Errorf("tutorial supervisor exited early: %w\n%s", err, string(logData))
		default:
		}
		time.Sleep(100 * time.Millisecond)
	}

	logData, _ := os.ReadFile(logPath)
	return fmt.Errorf("tutorial supervisor did not become ready within %s: want %q from gc supervisor status; last probe (err: %v):\n%s\nsupervisor log:\n%s",
		tutorialSupervisorReadyTimeout, helpers.SupervisorStatusPIDLine(cmd.Process.Pid), lastErr, lastOut, string(logData))
}

func TestStartTutorialSupervisorUsesAcceptanceBinaryForStatus(t *testing.T) {
	home := t.TempDir()
	runtimeDir := filepath.Join(home, "runtime")
	mustMkdirAll(t, runtimeDir)

	fakeBinDir := filepath.Join(home, "bin")
	mustMkdirAll(t, fakeBinDir)
	fakeGC := filepath.Join(fakeBinDir, "gc")
	writeFile(t, fakeGC, `#!/bin/sh
set -eu
case "$1 $2" in
  "supervisor run")
    echo $$ > "$GC_HOME/fake-supervisor.pid"
    echo "Supervisor API listening on http://127.0.0.1:7777"
    echo "Supervisor started."
    trap 'exit 0' TERM INT
    while :; do /bin/sleep 1; done
    ;;
  "supervisor status")
    # The first probes see what status reports while the supervisor holds
    # its lock but has not bound its control socket: liveness from a
    # fallback, with no PID.
    n=0
    [ -s "$GC_HOME/status-calls" ] && read -r n < "$GC_HOME/status-calls"
    n=$((n + 1))
    echo "$n" > "$GC_HOME/status-calls"
    if [ "$n" -lt 3 ] || [ ! -s "$GC_HOME/fake-supervisor.pid" ]; then
      echo "Supervisor is running (pid unavailable: control socket unreachable; liveness confirmed via api)"
    else
      read -r pid < "$GC_HOME/fake-supervisor.pid"
      echo "Supervisor is running (PID $pid)"
    fi
    ;;
  *)
    echo "unexpected args: $*" >&2
    exit 1
    ;;
esac
`, 0o755)

	tutorial := &tutorialEnv{
		Home:       home,
		RuntimeDir: runtimeDir,
		Env:        helpers.NewEnv(fakeGC, home, runtimeDir).With("PATH", "/does/not/exist"),
	}

	// Registered before the start so every failure exit below still kills
	// the fake supervisor; its loop otherwise outlives the test.
	t.Cleanup(func() {
		if tutorial.supervisor != nil && tutorial.supervisor.Process != nil {
			_ = tutorial.supervisor.Process.Kill()
		}
		if tutorial.supervisorDone != nil {
			<-tutorial.supervisorDone
		}
		if tutorial.supervisorLog != nil {
			_ = tutorial.supervisorLog.Close()
		}
	})
	if err := startTutorialSupervisor(tutorial); err != nil {
		t.Fatalf("startTutorialSupervisor: %v", err)
	}
	calls, err := os.ReadFile(filepath.Join(home, "status-calls"))
	if err != nil {
		t.Fatalf("reading status call count: %v", err)
	}
	if n, _ := strconv.Atoi(strings.TrimSpace(string(calls))); n < 3 {
		t.Fatalf("startTutorialSupervisor returned after %d status probe(s), want it to wait for the control socket to report the supervisor's PID", n)
	}
}

func TestNewTutorialBaseEnvSetsIsolatedTmuxTmpDir(t *testing.T) {
	home := t.TempDir()
	runtimeDir := filepath.Join(home, "runtime")
	got := newTutorialBaseEnv("/tmp/fake-gc", home, runtimeDir, "")

	wantTmux := filepath.Join(runtimeDir, "tmux")
	if got.Get("TMUX_TMPDIR") != wantTmux {
		t.Fatalf("TMUX_TMPDIR = %q, want %q", got.Get("TMUX_TMPDIR"), wantTmux)
	}
	if got.Get("DOLT_ROOT_PATH") != home {
		t.Fatalf("DOLT_ROOT_PATH = %q, want %q", got.Get("DOLT_ROOT_PATH"), home)
	}
	if !strings.HasPrefix(got.Get("PATH"), filepath.Join(home, ".local", "bin")+":") {
		t.Fatalf("PATH = %q, want tutorial bin dir prefix", got.Get("PATH"))
	}
}

func TestLinkTutorialSessionRootsCreatesSymlinkBridge(t *testing.T) {
	hostHome := t.TempDir()
	tutorialHome := t.TempDir()

	want := filepath.Join(hostHome, ".claude", "projects")
	if err := os.MkdirAll(want, 0o755); err != nil {
		t.Fatalf("MkdirAll(host projects): %v", err)
	}
	if err := linkTutorialSessionRoots(hostHome, tutorialHome); err != nil {
		t.Fatalf("linkTutorialSessionRoots: %v", err)
	}

	got := filepath.Join(tutorialHome, ".claude", "projects")
	target, err := os.Readlink(got)
	if err != nil {
		t.Fatalf("Readlink(%s): %v", got, err)
	}
	if target != want {
		t.Fatalf("projects symlink = %q, want %q", target, want)
	}
}

func stopTutorialSupervisor(env *tutorialEnv) {
	if env == nil {
		return
	}
	if env.Env != nil && env.Home != "" {
		_, _ = runEnvCommandWithTimeout(env, env.Home, 15*time.Second, "gc", "supervisor", "stop", "--wait")
	}
	if env.supervisorDone != nil {
		select {
		case <-env.supervisorDone:
		case <-time.After(10 * time.Second):
			if env.supervisor != nil && env.supervisor.Process != nil {
				_ = env.supervisor.Process.Kill()
			}
			<-env.supervisorDone
		}
	}
	if env.supervisorLog != nil {
		_ = env.supervisorLog.Close()
	}
	env.supervisor = nil
	env.supervisorDone = nil
	env.supervisorLog = nil
}

func hostHomeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		panic("tutorial-goldens: resolving home dir: " + err.Error())
	}
	return home
}

// hostProviderMode reports whether the provider CLIs run against the real
// home: only on a CI runner or with the explicit GC_TEST_ALLOW_HOST_CLAUDE=1,
// the same gate that lets the harness write the real ~/.claude.json.
func hostProviderMode() bool {
	return helpers.HostClaudeStateAllowed()
}

// providerHome is the HOME the wrapped provider CLIs run under for a tutorial
// whose gc home is home.
func providerHome(home string) string {
	if hostProviderMode() {
		return hostHomeDir()
	}
	return helpers.IsolatedHome(home)
}

func hasClaudeAuth() bool {
	if strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")) != "" || strings.TrimSpace(os.Getenv("ANTHROPIC_AUTH_TOKEN")) != "" {
		return true
	}
	if !hostProviderMode() {
		// Never run the CLI against the real home to ask: a staged copy of
		// its credentials is what isolated mode runs on.
		return fileExists(filepath.Join(hostHomeDir(), ".claude", ".credentials.json"))
	}
	cmd := exec.Command("claude", "auth", "status")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return claudeStatusOutputLoggedIn(out)
}

func hasCodexAuth() bool {
	if strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) != "" {
		return true
	}
	if !hostProviderMode() {
		return fileExists(filepath.Join(hostHomeDir(), ".codex", "auth.json"))
	}
	cmd := exec.Command("codex", "login", "status")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	return codexStatusOutputLoggedIn(out)
}

// stageClaudeAuth copies the Claude CLI's credentials into the provider home
// in isolated mode. In host mode the CLI already runs on the real home.
func stageClaudeAuth(providerHome string) error {
	if hostProviderMode() {
		return nil
	}
	_, err := helpers.StageClaudeAuthHome(hostHomeDir(), providerHome)
	return err
}

// codexAuthFiles are the files the Codex CLI logs in from.
var codexAuthFiles = []string{"auth.json", "config.toml"}

// stageCodexAuth copies the Codex CLI's credentials into the provider home in
// isolated mode, reading the real home only.
func stageCodexAuth(providerHome string) error {
	if hostProviderMode() {
		return nil
	}
	for _, name := range codexAuthFiles {
		data, err := os.ReadFile(filepath.Join(hostHomeDir(), ".codex", name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		dst := filepath.Join(providerHome, ".codex", name)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func stageProviderBinaries(dstHome, providerHome string) error {
	binDir := filepath.Join(dstHome, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}
	claudeShim, err := providerBinaryShim("claude", providerHome)
	if err != nil {
		return err
	}
	if err := helpers.StageProviderBinary(binDir, "claude", claudeShim); err != nil {
		return err
	}
	if !useClaudeForCodex() {
		codexShim, err := providerBinaryShim("codex", providerHome)
		if err != nil {
			return err
		}
		if err := helpers.StageProviderBinary(binDir, "codex", codexShim); err != nil {
			return err
		}
	}
	if path, err := exec.LookPath("python3"); err == nil {
		dst := filepath.Join(binDir, "python")
		_ = os.Remove(dst)
		if err := os.Symlink(path, dst); err != nil {
			return err
		}
	}
	return nil
}

func providerBinaryShim(name, providerHome string) (string, error) {
	switch name {
	case "claude":
		if strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")) != "" || strings.TrimSpace(os.Getenv("ANTHROPIC_AUTH_TOKEN")) != "" {
			return "", nil
		}
		return hostProviderShim(name, providerHome, []string{"CLAUDE_CONFIG_DIR", "XDG_CONFIG_HOME", "XDG_STATE_HOME"})
	case "codex":
		if strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) != "" {
			return "", nil
		}
		return hostProviderShim(name, providerHome, []string{"XDG_CONFIG_HOME", "XDG_STATE_HOME"})
	default:
		return "", nil
	}
}

// hostProviderShim runs the host's provider CLI with HOME=home: the real home
// in host mode, the isolated provider home otherwise.
func hostProviderShim(name, home string, unsetVars []string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}

	realHome := home
	userName := strings.TrimSpace(os.Getenv("USER"))
	login := strings.TrimSpace(os.Getenv("LOGNAME"))
	if current, err := user.Current(); err == nil {
		if userName == "" {
			userName = strings.TrimSpace(current.Username)
		}
		if login == "" {
			login = strings.TrimSpace(current.Username)
		}
	}
	if login == "" {
		login = filepath.Base(realHome)
	}
	if userName == "" {
		userName = login
	}

	parts := []string{"env"}
	for _, key := range unsetVars {
		parts = append(parts, "-u", key)
	}
	parts = append(parts,
		"HOME="+shellQuote(realHome),
		"USER="+shellQuote(userName),
		"LOGNAME="+shellQuote(login),
		shellQuote(path),
	)
	return strings.Join(parts, " "), nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func acceptanceTempRoot() (string, error) {
	root := strings.TrimSpace(os.Getenv("GC_ACCEPTANCE_TMPDIR"))
	if root == "" {
		root = filepath.Join("/tmp", "gcac")
		if err := os.MkdirAll(root, 0o755); err != nil {
			root = filepath.Join(os.TempDir(), "gcac")
		}
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	return root, nil
}

func useClaudeForCodex() bool {
	return strings.TrimSpace(os.Getenv("GC_TUTORIAL_GOLDENS_USE_CLAUDE_FOR_CODEX")) == "1"
}

func tutorialReviewerProvider() string {
	if useClaudeForCodex() {
		return "claude"
	}
	return "codex"
}

// registerTutorialReviewerProvider mirrors the tutorial-02 page step that
// registers the reviewer's provider in city.toml's explicit provider catalog
// ([providers.<name>] base = "builtin:<name>"). Without it, any agent
// referencing an unregistered provider fails config load with "provider
// catalog is missing referenced providers". Skipped when the reviewer rides
// the init-registered claude provider, which is already in the catalog.
func registerTutorialReviewerProvider(t *testing.T, cityPath string) {
	t.Helper()
	provider := tutorialReviewerProvider()
	if provider == "claude" {
		return
	}
	appendFile(t, filepath.Join(cityPath, "city.toml"),
		"\n[providers."+provider+"]\nbase = \"builtin:"+provider+"\"\n")
}

func claudeStatusOutputLoggedIn(out []byte) bool {
	var status struct {
		LoggedIn bool `json:"loggedIn"`
	}
	if err := json.Unmarshal(out, &status); err != nil {
		return false
	}
	return status.LoggedIn
}

func codexStatusOutputLoggedIn(out []byte) bool {
	return strings.HasPrefix(strings.TrimSpace(strings.ToLower(string(out))), "logged in")
}

// devBoxProviderMode clears both host-mode opt-ins for the test.
func devBoxProviderMode(t *testing.T) {
	t.Helper()
	t.Setenv("GITHUB_ACTIONS", "")
	t.Setenv(helpers.EnvAllowHostClaude, "")
}

// On a developer box gc and the provider CLIs share an isolated HOME; neither
// is handed the real one.
func TestTutorialProvidersRunIsolatedOnADevBox(t *testing.T) {
	devBoxProviderMode(t)
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	home := t.TempDir()
	env := newTutorialBaseEnv("/tmp/fake-gc", home, filepath.Join(home, "runtime"), "")
	isolated := helpers.IsolatedHome(home)
	if got := env.Get("HOME"); got != isolated {
		t.Fatalf("gc HOME = %q on a dev box, want the isolated %q", got, isolated)
	}
	if got := providerHome(home); got != isolated {
		t.Fatalf("providerHome = %q on a dev box, want %q", got, isolated)
	}
	if _, err := exec.LookPath("claude"); err == nil {
		shim, err := providerBinaryShim("claude", providerHome(home))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(shim, "HOME="+shellQuote(isolated)) || strings.Contains(shim, "HOME="+shellQuote(hostHomeDir())) {
			t.Fatalf("claude shim does not run under the isolated home %s:\n%s", isolated, shim)
		}
	}
}

// On a CI runner, or with GC_TEST_ALLOW_HOST_CLAUDE=1, the providers run on the
// real (throwaway) home, as before.
func TestTutorialProvidersUseTheHostHomeOnCI(t *testing.T) {
	for _, tc := range []struct{ key, val string }{
		{"GITHUB_ACTIONS", "true"},
		{helpers.EnvAllowHostClaude, "1"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			devBoxProviderMode(t)
			t.Setenv(tc.key, tc.val)
			home := t.TempDir()
			env := newTutorialBaseEnv("/tmp/fake-gc", home, filepath.Join(home, "runtime"), "")
			if got := env.Get("HOME"); got != hostHomeDir() {
				t.Fatalf("gc HOME = %q, want the host home %q", got, hostHomeDir())
			}
			if got := providerHome(home); got != hostHomeDir() {
				t.Fatalf("providerHome = %q, want the host home %q", got, hostHomeDir())
			}
		})
	}
}

// Isolated mode copies the CLIs' credentials out of the real home and never
// writes back to it.
func TestTutorialStagesProviderCredentialsReadOnly(t *testing.T) {
	devBoxProviderMode(t)
	hostHome := t.TempDir()
	t.Setenv("HOME", hostHome) // hostHomeDir's source; the guard still keys off the passwd home
	files := map[string]string{
		filepath.Join(hostHome, ".claude", ".credentials.json"): `{"claudeAiOauth":{"accessToken":"a"}}`,
		filepath.Join(hostHome, ".claude.json"):                 `{"oauthAccount":{"emailAddress":"dev@example.com"},"projects":{"/x":{}}}`,
		filepath.Join(hostHome, ".codex", "auth.json"):          `{"tokens":{"access_token":"c"}}`,
	}
	for path, body := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	provider := filepath.Join(t.TempDir(), "home")
	if err := stageClaudeAuth(provider); err != nil {
		t.Fatalf("stageClaudeAuth: %v", err)
	}
	if err := stageCodexAuth(provider); err != nil {
		t.Fatalf("stageCodexAuth: %v", err)
	}
	for _, rel := range []string{".claude/.credentials.json", ".claude.json", ".codex/auth.json"} {
		if !fileExists(filepath.Join(provider, rel)) {
			t.Errorf("isolated provider home lacks %s", rel)
		}
	}
	for path, body := range files {
		if got, _ := os.ReadFile(path); string(got) != body {
			t.Errorf("the real %s changed: %s", path, got)
		}
	}
	if !hasClaudeAuth() || !hasCodexAuth() {
		t.Errorf("auth probes do not see the staged-from credentials (claude=%v codex=%v)", hasClaudeAuth(), hasCodexAuth())
	}
}
