package herdr

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestEffectiveWorkDir verifies the launch-cwd resolution Start performs after
// staging and pre_start have run.
//
// A configured WorkDir must exist by then — pre_start is what materializes
// per-bead worktrees — so set-but-absent is a loud error rather than a silent
// substitute: herdr's own missing---cwd fallback is $HOME (where Claude Code
// re-prompts the trust dialog and the altered boot state swallows the startup
// nudge), and the provider's old city-root substitution masked never-created
// worktrees entirely, leaving agents running in the wrong repo.
//
// An EMPTY WorkDir keeps the legitimate fallback chain: a non-empty
// GC_CITY_ROOT env, then the provider's cityRoot, then "" (city-less; defers
// to the server cwd, itself pinned to the city root in startServer).
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
		wantErr  bool
	}{
		{
			name:     "existing workdir used as-is",
			cfg:      runtime.Config{WorkDir: existing, Env: map[string]string{"GC_CITY_ROOT": envRoot}},
			cityRoot: provRoot,
			want:     existing,
		},
		{
			name:     "set-but-absent workdir is an error (staging/pre_start should have created it)",
			cfg:      runtime.Config{WorkDir: missing, Env: map[string]string{}},
			cityRoot: provRoot,
			wantErr:  true,
		},
		{
			name:     "set-but-absent workdir errors even with GC_CITY_ROOT set (no silent substitute)",
			cfg:      runtime.Config{WorkDir: missing, Env: map[string]string{"GC_CITY_ROOT": envRoot}},
			cityRoot: provRoot,
			wantErr:  true,
		},
		{
			name:     "empty workdir prefers a set GC_CITY_ROOT env",
			cfg:      runtime.Config{WorkDir: "", Env: map[string]string{"GC_CITY_ROOT": envRoot}},
			cityRoot: provRoot,
			want:     envRoot,
		},
		{
			name:     "empty workdir, empty env falls back to provider cityRoot (the pool-spawn-in-$HOME fix)",
			cfg:      runtime.Config{WorkDir: "", Env: map[string]string{}},
			cityRoot: provRoot,
			want:     provRoot,
		},
		{
			name:     "no workdir, no env, no cityRoot returns empty (city-less; defers to server cwd)",
			cfg:      runtime.Config{WorkDir: "", Env: map[string]string{}},
			cityRoot: "",
			want:     "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := effectiveWorkDir(tt.cfg, tt.cityRoot)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("effectiveWorkDir(%+v, %q) = %q, nil; want error", tt.cfg, tt.cityRoot, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("effectiveWorkDir(%+v, %q) error: %v", tt.cfg, tt.cityRoot, err)
			}
			if got != tt.want {
				t.Errorf("effectiveWorkDir(%+v, %q) = %q, want %q", tt.cfg, tt.cityRoot, got, tt.want)
			}
		})
	}
}
