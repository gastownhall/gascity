package herdr

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestEffectiveWorkDir verifies the launch-cwd resolution: an existing WorkDir is
// used as-is; an empty or not-yet-created WorkDir (e.g. an ephemeral pool wisp
// whose per-bead worktree hasn't been checked out at launch) falls back to a
// non-empty GC_CITY_ROOT env, then to the provider's cityRoot — so herdr never
// lands the session in $HOME (where Claude Code re-prompts the trust dialog and
// the altered boot state swallows the startup nudge, leaving the spawn idle).
func TestEffectiveWorkDir(t *testing.T) {
	existing := t.TempDir()
	missing := filepath.Join(existing, "not-created-yet")
	envRoot := "/some/env/city/root"
	provRoot := "/some/provider/city/root"

	tests := []struct {
		name     string
		cfg      runtime.Config
		cityRoot string
		want     string
	}{
		{
			name:     "existing workdir used as-is",
			cfg:      runtime.Config{WorkDir: existing, Env: map[string]string{"GC_CITY_ROOT": envRoot}},
			cityRoot: provRoot,
			want:     existing,
		},
		{
			name:     "missing workdir prefers a set GC_CITY_ROOT env",
			cfg:      runtime.Config{WorkDir: missing, Env: map[string]string{"GC_CITY_ROOT": envRoot}},
			cityRoot: provRoot,
			want:     envRoot,
		},
		{
			name:     "missing workdir, empty env falls back to provider cityRoot (the pool-spawn-in-$HOME fix)",
			cfg:      runtime.Config{WorkDir: missing, Env: map[string]string{}},
			cityRoot: provRoot,
			want:     provRoot,
		},
		{
			name:     "empty workdir, empty env falls back to provider cityRoot",
			cfg:      runtime.Config{WorkDir: "", Env: map[string]string{}},
			cityRoot: provRoot,
			want:     provRoot,
		},
		{
			name:     "no workdir, no env, no cityRoot returns empty (city-less; defers to server cwd)",
			cfg:      runtime.Config{WorkDir: missing, Env: map[string]string{}},
			cityRoot: "",
			want:     "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := effectiveWorkDir(tt.cfg, tt.cityRoot); got != tt.want {
				t.Errorf("effectiveWorkDir(%+v, %q) = %q, want %q", tt.cfg, tt.cityRoot, got, tt.want)
			}
		})
	}
}
