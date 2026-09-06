package scripts_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestWorkerInferenceMakeTargetForwardsCursorAuthThroughSanitizedEnvironment(t *testing.T) {
	makefile, err := os.ReadFile(filepath.Join(repoRoot(t), "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}

	content := string(makefile)
	recipe := regexp.MustCompile(`(?m)^test-worker-inference:\n((?:\t[^\n]+\n?)+)`).FindStringSubmatch(content)
	if len(recipe) != 2 {
		t.Fatal("Makefile has no test-worker-inference recipe")
	}
	for _, name := range []string{
		"GC_ACCEPTANCE_BD_BIN",
		"GC_WORKER_INFERENCE_CURSOR_API_KEY",
		"GC_WORKER_INFERENCE_CURSOR_API_KEY_FILE",
		"CURSOR_API_KEY",
	} {
		forwarding := name + `="$${` + name + `-}"`
		if !strings.Contains(recipe[1], forwarding) {
			t.Errorf("test-worker-inference recipe does not forward %s through TEST_ENV:\n%s", name, recipe[1])
		}
	}
}
