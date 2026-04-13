//go:build acceptance_c

package tutorialgoldens

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
	"github.com/joho/godotenv"
)

const canonicalTutorialRoot = "docs/tutorials"

var (
	goldenGCBinary string
	goldenBDPath   string
)

func TestMain(m *testing.M) {
	if err := loadTutorialEnvFile(); err != nil {
		panic("tutorial-goldens: loading .env: " + err.Error())
	}
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

func newTutorialEnv(t *testing.T) *tutorialEnv {
	t.Helper()

	tmpRoot, err := acceptanceTempRoot()
	if err != nil {
		t.Fatalf("preparing tutorial temp root: %v", err)
	}
	root, err := os.MkdirTemp(tmpRoot, "gctutenv-*")
	if err != nil {
		t.Fatalf("creating tutorial temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	home := filepath.Join(root, "home")
	runtimeDir := filepath.Join(root, "runtime")
	tmuxDir := filepath.Join(runtimeDir, "tmux")
	for _, dir := range []string{home, runtimeDir, tmuxDir} {
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
	claudeConfigDir := filepath.Join(home, ".claude")
	if err := helpers.EnsureClaudeStateFile(home, claudeConfigDir); err != nil {
		t.Fatalf("seeding Claude state: %v", err)
	}
	if err := stageCodexAuth(home); err != nil {
		t.Fatalf("staging Codex auth: %v", err)
	}
	if err := stageProviderBinaries(home); err != nil {
		t.Fatalf("staging provider binaries: %v", err)
	}

	env := helpers.NewEnv(goldenGCBinary, home, runtimeDir).
		Without("GC_SESSION").
		Without("GC_BEADS").
		Without("GC_DOLT").
		With("DOLT_ROOT_PATH", home)
	ensureTutorialUserEnv(env)
	env.With("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	env.With("TMUX_TMPDIR", tmuxDir)
	env.With("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	env.With("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	env.With("PATH", filepath.Join(home, ".local", "bin")+":"+env.Get("PATH"))

	for _, key := range []string{
		"ANTHROPIC_AUTH_TOKEN",
		"ANTHROPIC_API_KEY",
		"ANTHROPIC_BASE_URL",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL",
		"ANTHROPIC_DEFAULT_OPUS_MODEL",
		"ANTHROPIC_DEFAULT_SONNET_MODEL",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC",
		"CLAUDE_CODE_EFFORT_LEVEL",
		"CLAUDE_CODE_OAUTH_TOKEN",
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

func stopTutorialSupervisor(env *tutorialEnv) {
	if env == nil {
		return
	}
	if env.Env != nil && env.Home != "" {
		_, _ = runEnvCommandWithTimeout(env, env.Home, 5*time.Second, "gc", "supervisor", "stop")
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
	// Only accept auth forms that are portable to the isolated temp-home
	// environment. Host-level `claude auth status` logins are NOT portable
	// because the harness no longer stages host HOME into the isolated env.
	if strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")) != "" || strings.TrimSpace(os.Getenv("ANTHROPIC_AUTH_TOKEN")) != "" {
		return true
	}
	return hasValidClaudeOAuthToken()
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

func stageCodexAuth(dstHome string) error {
	if strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) != "" {
		return nil
	}
	realHome := hostHomeDir()
	srcCodexDir := filepath.Join(realHome, ".codex")
	dstCodexDir := filepath.Join(dstHome, ".codex")
	if err := os.MkdirAll(dstCodexDir, 0o755); err != nil {
		return err
	}
	for _, name := range []string{"auth.json", "config.json", "config.toml"} {
		if err := copyFileIfExists(filepath.Join(srcCodexDir, name), filepath.Join(dstCodexDir, name), 0o600); err != nil {
			return err
		}
	}
	return nil
}

func stageProviderBinaries(dstHome string) error {
	binDir := filepath.Join(dstHome, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}
	if err := helpers.StageProviderBinary(binDir, "claude", ""); err != nil {
		return err
	}
	if !useClaudeForCodex() {
		if err := helpers.StageProviderBinary(binDir, "codex", ""); err != nil {
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

func loadTutorialEnvFile() error {
	return loadEnvFile(filepath.Join(helpers.FindModuleRoot(), ".env"))
}

func loadEnvFile(path string) error {
	values, err := godotenv.Read(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for key, value := range values {
		if os.Getenv(key) != "" {
			continue
		}
		_ = os.Setenv(key, value)
	}
	return nil
}

func hasValidClaudeOAuthToken() bool {
	token := strings.TrimSpace(os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"))
	if token == "" {
		return false
	}
	tmpHome, err := os.MkdirTemp("", "claude-oauth-check-*")
	if err != nil {
		return false
	}
	defer os.RemoveAll(tmpHome)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "claude", "--print", "ok")
	cmd.Env = []string{
		"HOME=" + tmpHome,
		"PATH=" + os.Getenv("PATH"),
		"CLAUDE_CODE_OAUTH_TOKEN=" + token,
	}
	appendNonEmptyEnv := func(key, value string) {
		if strings.TrimSpace(value) == "" {
			return
		}
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	userName, login := resolveTutorialUserIdentity(os.Getenv("USER"), os.Getenv("LOGNAME"))
	appendNonEmptyEnv("USER", userName)
	appendNonEmptyEnv("LOGNAME", login)
	appendNonEmptyEnv("SHELL", os.Getenv("SHELL"))
	appendNonEmptyEnv("LANG", os.Getenv("LANG"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(out)), "ok")
}

func ensureTutorialUserEnv(env *helpers.Env) {
	if env == nil {
		return
	}
	userName, login := resolveTutorialUserIdentity(env.Get("USER"), env.Get("LOGNAME"))
	if userName != "" {
		env.With("USER", userName)
	}
	if login != "" {
		env.With("LOGNAME", login)
	}
}

func resolveTutorialUserIdentity(userName, login string) (string, string) {
	userName = strings.TrimSpace(userName)
	login = strings.TrimSpace(login)
	if current, err := user.Current(); err == nil {
		if userName == "" {
			userName = strings.TrimSpace(current.Username)
		}
		if login == "" {
			login = strings.TrimSpace(current.Username)
		}
	}
	if userName == "" {
		userName = login
	}
	if login == "" {
		login = userName
	}
	return userName, login
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

func copyFileIfExists(src, dst string, perm os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return os.WriteFile(dst, data, perm)
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
