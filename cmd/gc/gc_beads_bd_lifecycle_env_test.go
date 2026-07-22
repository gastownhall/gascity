package main

import (
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGCBeadsBDQuarantinesLifecycleDelegationUntilStoreBridge(t *testing.T) {
	repoRoot := repoRootForLint(t)
	provider := filepath.Join(repoRoot, "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	cityDir := t.TempDir()
	packDir := filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt")
	captureFile := filepath.Join(t.TempDir(), "gc-invocations.txt")
	fakeGC := filepath.Join(t.TempDir(), "gc")
	fakeScript := `#!/bin/sh
printf '%s|scope=%s|token=%s\n' "$*" "${GC_LIFECYCLE_MUTATION_SCOPE:-}" "${GC_LIFECYCLE_MUTATION_TOKEN:-}" >> "` + captureFile + `"
case "$1 $2" in
  "dolt-state runtime-layout")
    printf 'GC_PACK_STATE_DIR\t%s\n' "` + packDir + `"
    printf 'GC_DOLT_DATA_DIR\t%s\n' "` + filepath.Join(cityDir, ".beads", "dolt") + `"
    printf 'GC_DOLT_LOG_FILE\t%s\n' "` + filepath.Join(packDir, "dolt.log") + `"
    printf 'GC_DOLT_STATE_FILE\t%s\n' "` + filepath.Join(packDir, "state.json") + `"
    printf 'GC_DOLT_PID_FILE\t%s\n' "` + filepath.Join(packDir, "dolt.pid") + `"
    printf 'GC_DOLT_LOCK_FILE\t%s\n' "` + filepath.Join(packDir, "dolt.lock") + `"
    printf 'GC_DOLT_CONFIG_FILE\t%s\n' "` + filepath.Join(packDir, "config.yaml") + `"
    ;;
  "dolt-state allocate-port")
    printf '3307\n'
    ;;
  "bd-store-bridge --dir")
    cat >/dev/null
    ;;
  *)
    exit 65
    ;;
esac
`
	if err := os.WriteFile(fakeGC, []byte(fakeScript), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := osexec.Command(provider, "set-metadata", "root", "close_reason")
	cmd.Stdin = strings.NewReader("done")
	cmd.Env = append(os.Environ(),
		"GC_BIN="+fakeGC,
		"GC_CITY_PATH="+cityDir,
		"GC_STORE_ROOT="+cityDir,
		"GC_DOLT_HOST=db.example.test",
		"GC_DOLT_PORT=3307",
		"GC_LIFECYCLE_MUTATION_SCOPE=scope-marker",
		"GC_LIFECYCLE_MUTATION_TOKEN=token-marker",
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("gc-beads-bd set-metadata: %v\n%s", err, output)
	}

	captured, err := os.ReadFile(captureFile)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(captured)), "\n")
	if len(lines) != 3 {
		t.Fatalf("gc invocations = %q, want runtime-layout, allocate-port, and bd-store-bridge", captured)
	}
	for _, line := range lines[:2] {
		if !strings.HasSuffix(line, "|scope=|token=") {
			t.Fatalf("setup helper inherited lifecycle delegation: %q", line)
		}
	}
	if got := lines[2]; !strings.HasPrefix(got, "bd-store-bridge ") || !strings.HasSuffix(got, "|scope=scope-marker|token=token-marker") {
		t.Fatalf("bridge lifecycle delegation = %q, want original markers", got)
	}
}
