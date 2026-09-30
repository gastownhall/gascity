package scripts_test

import (
	"os"
	"path/filepath"
	"testing"
)

// Symlinked homes (and macOS /var) must not turn canonical store comparisons
// into failures. The gate must also retain its ambient-runtime env isolation.
func TestGoTestShardCanonicalizesTMPDIRAndScrubsRuntimeEnv(t *testing.T) {
	f := newGoTestShardFixtureWithExit(t, 0)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(f.tmpDir, alias); err != nil {
		t.Fatal(err)
	}
	cmd := f.command("TMPDIR="+alias, "GC_CITY_PATH=/live/city", "GC_DOLT_HOST=live-host", "GC_DOLT_PORT=3306", "BEADS_DIR=/live/beads", "BEADS_DOLT_SERVER_PORT=3306")
	if code, out := runShardCommand(t, cmd); code != 0 {
		t.Fatalf("shard exit %d: %s", code, out)
	}
	env := fixtureEnvironment(t, readFixtureFile(t, f.productEnvFile))
	want, err := filepath.EvalSymlinks(f.tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	if env["TMPDIR"] != want {
		t.Errorf("TMPDIR = %q, want canonical %q", env["TMPDIR"], want)
	}
	for _, name := range []string{"GC_CITY_PATH", "GC_DOLT_HOST", "GC_DOLT_PORT", "BEADS_DIR", "BEADS_DOLT_SERVER_PORT"} {
		if value, ok := env[name]; ok {
			t.Errorf("ambient %s leaked into shard: %q", name, value)
		}
	}
}
