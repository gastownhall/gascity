//go:build acceptance_c

package tutorialgoldens

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

const (
	canonicalTutorialCommit = "140d5ac39"
	canonicalTutorialRoot   = "test/acceptance/tutorial_goldens/testdata/140d5ac39/docs/tutorials"
)

var (
	goldenGCBinary string
	goldenBDPath   string
)

func TestMain(m *testing.M) {
	if !hasClaudeAuth() || !hasCodexAuth() {
		fmt.Fprintln(os.Stderr, "tutorial-goldens: skipping package (requires both Claude and Codex auth)")
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
}

func newTutorialEnv(t *testing.T) *tutorialEnv {
	t.Helper()

	root := t.TempDir()
	home := filepath.Join(root, "home")
	runtimeDir := filepath.Join(root, "runtime")
	for _, dir := range []string{home, runtimeDir} {
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
	env.With("PATH", filepath.Join(home, ".local", "bin")+":"+env.Get("PATH"))

	if apiKey := strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")); apiKey != "" {
		env.With("ANTHROPIC_API_KEY", apiKey)
	}
	if apiKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY")); apiKey != "" {
		env.With("OPENAI_API_KEY", apiKey)
	}

	return &tutorialEnv{
		Root:       root,
		Home:       home,
		RuntimeDir: runtimeDir,
		Env:        env,
	}
}

func hostHomeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		panic("tutorial-goldens: resolving home dir: " + err.Error())
	}
	return home
}

func hasClaudeAuth() bool {
	if strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")) != "" {
		return true
	}
	home := hostHomeDir()
	for _, candidate := range []string{
		filepath.Join(home, ".claude", ".credentials.json"),
		filepath.Join(home, ".claude", "credentials.json"),
	} {
		if _, err := os.Stat(candidate); err == nil {
			return true
		}
	}
	return false
}

func hasCodexAuth() bool {
	if strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) != "" {
		return true
	}
	home := hostHomeDir()
	_, err := os.Stat(filepath.Join(home, ".codex", "auth.json"))
	return err == nil
}

func stageClaudeAuth(dstHome string) error {
	realHome := hostHomeDir()
	srcClaudeDir := filepath.Join(realHome, ".claude")
	dstClaudeDir := filepath.Join(dstHome, ".claude")
	if err := os.MkdirAll(dstClaudeDir, 0o755); err != nil {
		return err
	}
	for _, name := range []string{".credentials.json", "credentials.json", "settings.json"} {
		if err := copyFileIfExists(filepath.Join(srcClaudeDir, name), filepath.Join(dstClaudeDir, name), 0o600); err != nil {
			return err
		}
	}
	return copyFileIfExists(filepath.Join(realHome, ".claude.json"), filepath.Join(dstHome, ".claude.json"), 0o600)
}

func stageCodexAuth(dstHome string) error {
	dstCodexDir := filepath.Join(dstHome, ".codex")
	if err := os.MkdirAll(dstCodexDir, 0o755); err != nil {
		return err
	}
	return copyFileIfExists(filepath.Join(hostHomeDir(), ".codex", "auth.json"), filepath.Join(dstCodexDir, "auth.json"), 0o600)
}

func stageProviderBinaries(dstHome string) error {
	binDir := filepath.Join(dstHome, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}
	for _, name := range []string{"claude", "codex"} {
		if err := helpers.StageProviderBinary(binDir, name, ""); err != nil {
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
