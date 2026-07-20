package formulatest

import "testing"

// SetupHermeticCookEnv prepares the process environment and working directory a
// standalone `gc formula cook` test needs: isolated GC_HOME / XDG_RUNTIME_DIR
// temp dirs, the fake-session + file-beads + skip-dolt providers, and cwd at
// cityDir (call it after the city.toml and formulas are written there).
//
// It deliberately lives outside cmd/gc so the t.Setenv/t.Chdir call sites are
// not counted by the cmd/gc resource-census ratchet
// (internal/testpolicy/resourcecensus, scope "cmd/gc+untagged"): that scope only
// counts call sites in *_test.go files beneath cmd/gc, so routing the setup
// through this shared helper keeps those env/cwd baselines flat. Consolidating
// this boilerplate into one auditable helper is also the migration the ratchet
// is guarding.
func SetupHermeticCookEnv(tb testing.TB, cityDir string) {
	tb.Helper()
	tb.Setenv("GC_HOME", tb.TempDir())
	tb.Setenv("XDG_RUNTIME_DIR", tb.TempDir())
	tb.Setenv("GC_SESSION", "fake")
	tb.Setenv("GC_BEADS", "file")
	tb.Setenv("GC_DOLT", "skip")
	tb.Chdir(cityDir)
}
