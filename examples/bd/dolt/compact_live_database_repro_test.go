package dolt_test

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCompactScriptAbortsWhenDatabaseLeavesDataDirAfterGC asserts that a
// compaction pass whose full GC leaves the database out of the data dir is
// not reported as a success: compact must exit non-zero and quarantine the
// database instead of moving on.
func TestCompactScriptAbortsWhenDatabaseLeavesDataDirAfterGC(t *testing.T) {
	fixture := newCompactScriptFixture(t)

	realDolt := filepath.Join(fixture.binDir, "dolt.real")
	if err := os.Rename(filepath.Join(fixture.binDir, "dolt"), realDolt); err != nil {
		t.Fatalf("move fake dolt aside: %v", err)
	}
	shim := `#!/bin/sh
"` + realDolt + `" "$@"
rc=$?
case "$*" in
  *DOLT_GC*)
    if [ "$rc" -eq 0 ]; then
      rm -rf -- "$GC_DOLT_DATA_DIR/beads"
    fi
    ;;
esac
exit "$rc"
`
	if err := os.WriteFile(filepath.Join(fixture.binDir, "dolt"), []byte(shim), 0o755); err != nil {
		t.Fatalf("write dolt shim: %v", err)
	}

	out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if _, statErr := os.Stat(filepath.Join(fixture.dataDir, "beads")); !os.IsNotExist(statErr) {
		t.Fatalf("shim did not drop beads after DOLT_GC (stat err=%v):\n%s", statErr, out)
	}
	if err == nil {
		t.Fatalf("compact succeeded although beads left the data dir after its full GC:\n%s", out)
	}
	marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf("compact must quarantine a database that left the data dir (stat err=%v):\n%s", statErr, out)
	}
}
