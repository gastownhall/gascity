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
)

const canonicalTutorialRoot = "docs/tutorials"

var (
	goldenGCBinary string
	goldenBDPath   string
)

func TestMain(m *testing.M) {
	if !hasClaudeAuth() || (!useClaudeForCodex() && !hasCodexAuth()) {
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
	tmpDir, err := os.MkdirTemp(tmpRoot, "gctutorial-*")
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

	os.Exit(m.Run())
}

type tutorialEnv struct {
	Root       string
	Home       string
	RuntimeDir string
	Env        *helpers.Env

	supervisor     *exec.Cmd
	supervisorDone chan error
	supervisorLog  *os.File
}

func tutorialTmuxTmpDir(runtimeDir string) string {
	return filepath.Join(runtimeDir, "tmux")
}

func newTutorialBaseEnv(gcBinary, home, runtimeDir string) *helpers.Env {
	env := helpers.NewEnv(gcBinary, home, runtimeDir).
		Without("GC_SESSION").
		Without("GC_BEADS").
		Without("GC_DOLT").
		With("DOLT_ROOT_PATH", home)
	env.With("PATH", filepath.Join(home, ".local", "bin")+":"+env.Get("PATH"))
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
	if err := os.MkdirAll(filepath.Join(home, ".dolt"), 0o755); err != nil {
		t.Fatalf("creating dolt dir: %v", err)
	}
	doltCfg := `{"user.name":"gc-test","user.email":"gc-test@test.local"}`
	if err := os.WriteFile(filepath.Join(home, ".dolt", "config_global.json"), []byte(doltCfg), 0o644); err != nil {
		t.Fatalf("writing dolt config: %v", err)
	}
	if err := stageClaudeAuth(home); err != nil {
		t.Fatalf("staging Claude auth: %v", err)
	}
	if err := helpers.EnsureClaudeStateFile(home); err != nil {
		t.Fatalf("seeding Claude state: %v", err)
	}
	if err := stageCodexAuth(home); err != nil {
		t.Fatalf("staging Codex auth: %v", err)
	}
	if err := stageProviderBinaries(home); err != nil {
		t.Fatalf("staging provider binaries: %v", err)
	}
	if err := linkTutorialSessionRoots(hostHomeDir(), home); err != nil {
		t.Fatalf("linking session roots: %v", err)
	}

	env := newTutorialBaseEnv(goldenGCBinary, home, runtimeDir)

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
		Root:       root,
		Home:       home,
		RuntimeDir: runtimeDir,
		Env:        env,
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

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		out, err := runEnvCommandWithTimeout(env, env.Home, 2*time.Second, "gc", "supervisor", "status")
		if err == nil && strings.Contains(out, "Supervisor is running") {
			return nil
		}
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
	return fmt.Errorf("tutorial supervisor did not become ready:\n%s", string(logData))
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
    echo "Supervisor API listening on http://127.0.0.1:7777"
    echo "Supervisor started."
    trap 'exit 0' TERM INT
    while :; do sleep 1; done
    ;;
  "supervisor status")
    echo "Supervisor is running (PID 4242)"
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

	if err := startTutorialSupervisor(tutorial); err != nil {
		t.Fatalf("startTutorialSupervisor: %v", err)
	}
	defer func() {
		if tutorial.supervisor != nil && tutorial.supervisor.Process != nil {
			_ = tutorial.supervisor.Process.Kill()
		}
		if tutorial.supervisorDone != nil {
			<-tutorial.supervisorDone
		}
		if tutorial.supervisorLog != nil {
			_ = tutorial.supervisorLog.Close()
		}
	}()
}

func TestNewTutorialBaseEnvSetsIsolatedTmuxTmpDir(t *testing.T) {
	home := t.TempDir()
	runtimeDir := filepath.Join(home, "runtime")
	got := newTutorialBaseEnv("/tmp/fake-gc", home, runtimeDir)

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

func hasClaudeAuth() bool {
	if strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")) != "" || strings.TrimSpace(os.Getenv("ANTHROPIC_AUTH_TOKEN")) != "" {
		return true
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
	cmd := exec.Command("codex", "login", "status")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	return codexStatusOutputLoggedIn(out)
}

func stageClaudeAuth(_ string) error {
	// Tutorial acceptance uses wrapped provider binaries that delegate to the
	// authenticated host CLI, so there is no isolated Claude auth state to copy.
	return nil
}

func stageCodexAuth(_ string) error {
	// Tutorial acceptance uses wrapped provider binaries that delegate to the
	// authenticated host CLI, so there is no isolated Codex auth state to copy.
	return nil
}

func stageProviderBinaries(dstHome string) error {
	binDir := filepath.Join(dstHome, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}
	claudeShim, err := providerBinaryShim("claude")
	if err != nil {
		return err
	}
	if err := helpers.StageProviderBinary(binDir, "claude", claudeShim); err != nil {
		return err
	}
	if !useClaudeForCodex() {
		codexShim, err := providerBinaryShim("codex")
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

func providerBinaryShim(name string) (string, error) {
	switch name {
	case "claude":
		if strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")) != "" || strings.TrimSpace(os.Getenv("ANTHROPIC_AUTH_TOKEN")) != "" {
			return "", nil
		}
		return hostProviderShim(name, []string{"CLAUDE_CONFIG_DIR", "XDG_CONFIG_HOME", "XDG_STATE_HOME"})
	case "codex":
		if strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) != "" {
			return "", nil
		}
		return hostProviderShim(name, []string{"XDG_CONFIG_HOME", "XDG_STATE_HOME"})
	default:
		return "", nil
	}
}

func hostProviderShim(name string, unsetVars []string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}

	realHome := hostHomeDir()
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
