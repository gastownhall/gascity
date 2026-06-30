package herdr

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestEffectiveWorkDir verifies the launch-cwd fallback: an existing WorkDir is
// used as-is, but an empty or not-yet-created WorkDir (e.g. an ephemeral pool
// wisp whose per-bead worktree hasn't been checked out at launch) falls back to
// the city root so herdr never lands the session in $HOME (where Claude Code
// re-prompts the trust dialog on every launch).
func TestEffectiveWorkDir(t *testing.T) {
	existing := t.TempDir()
	missing := filepath.Join(existing, "not-created-yet")
	root := "/some/city/root"

	tests := []struct {
		name string
		cfg  runtime.Config
		want string
	}{
		{
			name: "existing workdir used as-is",
			cfg:  runtime.Config{WorkDir: existing, Env: map[string]string{"GC_CITY_ROOT": root}},
			want: existing,
		},
		{
			name: "missing workdir falls back to city root",
			cfg:  runtime.Config{WorkDir: missing, Env: map[string]string{"GC_CITY_ROOT": root}},
			want: root,
		},
		{
			name: "empty workdir falls back to city root",
			cfg:  runtime.Config{WorkDir: "", Env: map[string]string{"GC_CITY_ROOT": root}},
			want: root,
		},
		{
			name: "missing workdir, no city root → empty (herdr uses its server cwd)",
			cfg:  runtime.Config{WorkDir: missing, Env: map[string]string{}},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := effectiveWorkDir(tt.cfg); got != tt.want {
				t.Errorf("effectiveWorkDir(%+v) = %q, want %q", tt.cfg, got, tt.want)
			}
		})
	}
}
