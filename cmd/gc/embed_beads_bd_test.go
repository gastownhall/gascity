package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestMaterializeBeadsBdScript(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	path, err := MaterializeBeadsBdScript(dir)
	if err != nil {
		t.Fatalf("MaterializeBeadsBdScript() error: %v", err)
	}

	// Check file exists.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}

	// Check it's executable.
	if info.Mode()&0o111 == 0 {
		t.Errorf("script is not executable: mode %v", info.Mode())
	}

	// Check content is non-empty and starts with shebang.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 100 {
		t.Errorf("script too small: %d bytes", len(data))
	}
	if string(data[:2]) != "#!" {
		t.Error("script doesn't start with shebang")
	}
}

// TestBeadsBdScript_K8sDoltEnvInheritance verifies that gc-beads-bd inherits
// GC_K8S_DOLT_HOST/PORT into GC_DOLT_HOST/PORT when the standard vars are
// unset. This is critical for K8s pods where buildPodEnv strips GC_DOLT_HOST
// and only injects GC_K8S_DOLT_HOST.
func TestBeadsBdScript_K8sDoltEnvInheritance(t *testing.T) {
	dir := t.TempDir()
	scriptPath, err := MaterializeBeadsBdScript(dir)
	if err != nil {
		t.Fatal(err)
	}

	// The "start" operation exits 2 immediately when is_remote() is true
	// (remote server — nothing to start locally). Without the K8s env var
	// inheritance fix, is_remote() returns false and the script tries to
	// start a local dolt server, exiting 1 (missing dolt/flock).
	cmd := exec.Command(scriptPath, "start")
	cmd.Env = []string{
		"GC_CITY_PATH=" + dir,
		"GC_K8S_DOLT_HOST=dolt.example.com",
		"GC_K8S_DOLT_PORT=3307",
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
	}
	// GC_DOLT_HOST intentionally NOT set — simulates K8s pod env.
	out, err := cmd.CombinedOutput()
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if exitCode != 2 {
		t.Errorf("gc-beads-bd start with GC_K8S_DOLT_HOST: exit %d, want 2 (remote detected)\noutput: %s", exitCode, out)
	}
}

func TestMaterializeBeadsBdScript_idempotent(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	path1, err := MaterializeBeadsBdScript(dir)
	if err != nil {
		t.Fatal(err)
	}
	path2, err := MaterializeBeadsBdScript(dir)
	if err != nil {
		t.Fatal(err)
	}
	if path1 != path2 {
		t.Errorf("paths differ: %s != %s", path1, path2)
	}
}
