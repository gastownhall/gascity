package scripts_test

import (
	"os"
	"path/filepath"
	"testing"
)

const localRuntimeLib = ".githooks/lib/local-runtime.sh"

// installLocalRuntimeProbe copies the production library when present, then
// overrides only its probe boundary. Before the library exists this remains an
// executable fixture: the old hooks ignore it and select the wrong engine.
// No production environment knob is needed to fake a probe result.
func installLocalRuntimeProbe(t *testing.T, root, repo, result, record string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, localRuntimeLib))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	override := "\nlocal_runtime_probe() {\n"
	if record != "" {
		override += "  printf 'probe\\n' >> " + shellQuote(record) + "\n"
	}
	override += "  printf '%s\\n' " + shellQuote(result) + "\n}\n"
	path := filepath.Join(repo, localRuntimeLib)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, path, string(body)+override)
}
