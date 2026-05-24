package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestHQStoreChaosSoakScriptContract(t *testing.T) {
	repoRoot := repoRoot(t)
	script := filepath.Join(repoRoot, "scripts", "hqstore-chaos-soak")

	info, err := os.Stat(script)
	if err != nil {
		t.Fatalf("stat hqstore-chaos-soak: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("hqstore-chaos-soak mode = %o, want executable", info.Mode().Perm())
	}

	syntax := exec.Command("bash", "-n", script)
	if out, err := syntax.CombinedOutput(); err != nil {
		t.Fatalf("bash -n hqstore-chaos-soak failed: %v\n%s", err, out)
	}

	help := exec.Command(script, "--help")
	help.Dir = repoRoot
	out, err := help.CombinedOutput()
	if err != nil {
		t.Fatalf("hqstore-chaos-soak --help failed: %v\n%s", err, out)
	}
	text := string(out)
	for _, want := range []string{
		"--city",
		"--bin",
		"--api",
		"--cycles",
		"--min-interval",
		"--max-interval",
		"--restart-timeout",
		"--rss-budget-kb",
		"--goroutine-budget",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("help output missing %s:\n%s", want, text)
		}
	}
}
