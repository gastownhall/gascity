package dashboardbff

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pathEntry returns the index of dir in a PATH entry list, or -1.
func pathEntry(dirs []string, dir string) int {
	for i, d := range dirs {
		if d == dir {
			return i
		}
	}
	return -1
}

// TestCleanEnvPath pins the deterministic default order the dashboard gives
// its subprocesses when ADMIN_PATH is unset (#7979): the home bin dirs and
// the fixed system base keep their relative order, and the platform's
// package-manager directories (Homebrew on darwin, snap/linuxbrew on linux)
// join only behind them, so a binary resolving today still wins while
// Homebrew's dolt becomes reachable. cleanEnvPath is pure — every row runs
// on every host.
func TestCleanEnvPath(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	// An existing user-managed toolchain dir must join the default order
	// (existence-checked, unlike the home bin dirs, which always lead).
	if err := os.MkdirAll(filepath.Join(home, "go", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name        string
		home        string
		goos        string
		want        []string // entries that must appear
		unwant      []string // entries that must not appear
		order       []string // entries that must appear in this relative order
		prefix      string   // exact expected prefix, when set
		allAbsolute bool     // every entry must be absolute and outside /tmp
	}{
		{
			// Reproduces #7979: Homebrew lives outside the fixed system base.
			name:  "darwin default adds homebrew behind the system base",
			home:  home,
			goos:  "darwin",
			want:  []string{"/opt/homebrew/bin", "/opt/homebrew/sbin"},
			order: []string{filepath.Join(home, ".local", "bin"), "/usr/local/bin", "/usr/bin", "/bin", "/opt/homebrew/bin"},
		},
		{
			name:   "linux default adds snap and linuxbrew, never homebrew",
			home:   home,
			goos:   "linux",
			want:   []string{"/snap/bin", "/home/linuxbrew/.linuxbrew/bin"},
			unwant: []string{"/opt/homebrew/bin"},
			order:  []string{filepath.Join(home, ".local", "bin"), "/usr/local/bin", "/usr/bin", "/bin", "/snap/bin"},
		},
		{
			// HOME unset: no home-derived entries at all, so nothing under the
			// /tmp HOME fallback cleanEnv uses for HOME= reaches the PATH.
			name:        "empty home drops home-derived entries",
			home:        "",
			goos:        "darwin",
			prefix:      "/usr/local/bin:/usr/bin:/bin",
			unwant:      []string{"/tmp/.local/bin", "/tmp/bin"},
			allAbsolute: true,
		},
		{
			name:  "existing go bin joins after the system base",
			home:  home,
			goos:  "linux",
			want:  []string{filepath.Join(home, "go", "bin")},
			order: []string{"/bin", filepath.Join(home, "go", "bin")},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := cleanEnvPath("", tt.home, tt.goos)
			entries := filepath.SplitList(got)
			for _, w := range tt.want {
				if pathEntry(entries, w) < 0 {
					t.Errorf("cleanEnvPath(\"\", %q, %q) = %q, want entry %q", tt.home, tt.goos, got, w)
				}
			}
			for _, w := range tt.unwant {
				if pathEntry(entries, w) >= 0 {
					t.Errorf("cleanEnvPath(\"\", %q, %q) = %q, unwanted entry %q", tt.home, tt.goos, got, w)
				}
			}
			if tt.prefix != "" && !strings.HasPrefix(got, tt.prefix) {
				t.Errorf("cleanEnvPath(\"\", %q, %q) = %q, want prefix %q", tt.home, tt.goos, got, tt.prefix)
			}
			if tt.allAbsolute {
				for _, e := range entries {
					if !strings.HasPrefix(e, "/") || strings.HasPrefix(e, "/tmp/") {
						t.Errorf("cleanEnvPath(\"\", %q, %q) = %q, entry %q must be absolute and outside /tmp", tt.home, tt.goos, got, e)
					}
				}
			}
			prev := -1
			for _, w := range tt.order {
				i := pathEntry(entries, w)
				if i < 0 {
					t.Errorf("cleanEnvPath(\"\", %q, %q) = %q, want ordered entry %q", tt.home, tt.goos, got, w)
					break
				}
				if i <= prev {
					t.Errorf("cleanEnvPath(\"\", %q, %q) = %q, entry %q (index %d) must come after %q (index %d)", tt.home, tt.goos, got, w, i, tt.order[prev], prev)
					break
				}
				prev = i
			}
		})
	}
}
