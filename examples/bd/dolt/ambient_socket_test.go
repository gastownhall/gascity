package dolt_test

import (
	"os"
	"strings"
	"testing"
)

// TestBeadsHelpersClearAmbientSocket proves each helper establishes its
// topology before invoking bd. An inherited socket must not override the
// explicit managed/proxied or DoltLite selection.
func TestBeadsHelpersClearAmbientSocket(t *testing.T) {
	root := repoRoot(t)
	script, err := os.ReadFile(root + "/../assets/scripts/gc-beads-bd.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	for _, name := range []string{"run_bd_init_proxied", "run_bd_doltlite"} {
		start := strings.Index(text, name+"() {")
		if start < 0 {
			t.Fatalf("helper %s not found", name)
		}
		body := text[start:]
		if end := strings.Index(body[1:], "\n}\n"); end >= 0 {
			body = body[:end+2]
		}
		if !strings.Contains(body, "BEADS_DOLT_SERVER_SOCKET") {
			t.Errorf("%s does not clear BEADS_DOLT_SERVER_SOCKET", name)
		}
		unsetAt := strings.Index(body, "BEADS_DOLT_SERVER_SOCKET")
		invokeAt := strings.Index(body, `"${BD_BIN:-bd}"`)
		if invokeAt < 0 {
			invokeAt = strings.Index(body, `"$bd_bin"`)
		}
		if invokeAt >= 0 && unsetAt > invokeAt {
			t.Errorf("%s clears ambient socket after invoking bd", name)
		}
	}
}
