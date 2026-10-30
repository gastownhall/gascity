package dashboardbff

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestCleanEnvPath pins the subprocess PATH default (#7979): with ADMIN_PATH
// unset, the platform package-manager dirs follow the fixed system base, so a
// Homebrew-installed dolt is found without displacing binaries that resolve
// today. The goos argument covers every platform on any host.
func TestCleanEnvPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		goos string
		// noHome passes an empty home, as when HOME is unset.
		noHome bool
		// goBin creates $HOME/go/bin before building the PATH.
		goBin bool
		check func(t *testing.T, home string, dirs []string)
	}{
		{
			name: "darwin adds Homebrew after the system base",
			goos: "darwin",
			check: func(t *testing.T, home string, dirs []string) {
				requireOrder(t, dirs,
					filepath.Join(home, ".local", "bin"),
					"/usr/local/bin",
					"/usr/bin",
					"/bin",
					"/opt/homebrew/bin",
				)
				requireContains(t, dirs, "/opt/homebrew/sbin")
			},
		},
		{
			name: "linux adds linuxbrew and snap, not Homebrew",
			goos: "linux",
			check: func(t *testing.T, home string, dirs []string) {
				requireOrder(t, dirs,
					filepath.Join(home, ".local", "bin"),
					"/usr/local/bin",
					"/usr/bin",
					"/bin",
					"/home/linuxbrew/.linuxbrew/bin",
				)
				requireContains(t, dirs, "/snap/bin")
				if slices.Contains(dirs, "/opt/homebrew/bin") {
					t.Errorf("linux PATH %v contains /opt/homebrew/bin", dirs)
				}
			},
		},
		{
			name:   "unset home adds no home-derived dirs",
			goos:   "darwin",
			noHome: true,
			check: func(t *testing.T, _ string, dirs []string) {
				if got := strings.Join(dirs[:3], ":"); got != cleanEnvBasePath {
					t.Errorf("PATH begins %q, want %q", got, cleanEnvBasePath)
				}
				for _, dir := range dirs {
					if !filepath.IsAbs(dir) || dir == "/tmp" || strings.HasPrefix(dir, "/tmp/") {
						t.Errorf("PATH %v contains %q; want only absolute non-/tmp dirs", dirs, dir)
					}
				}
				requireContains(t, dirs, "/opt/homebrew/bin")
			},
		},
		{
			name:  "existing user-managed dir follows the system base",
			goos:  "linux",
			goBin: true,
			check: func(t *testing.T, home string, dirs []string) {
				requireOrder(t, dirs, "/bin", filepath.Join(home, "go", "bin"))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			if tt.goBin {
				if err := os.MkdirAll(filepath.Join(home, "go", "bin"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			arg := home
			if tt.noHome {
				arg = ""
			}
			path := cleanEnvPath("", arg, tt.goos)
			tt.check(t, home, filepath.SplitList(path))
		})
	}
}

// requireContains fails unless dirs holds want.
func requireContains(t *testing.T, dirs []string, want string) {
	t.Helper()
	if !slices.Contains(dirs, want) {
		t.Errorf("PATH %v does not contain %q", dirs, want)
	}
}

// requireOrder fails unless every entry of want is in dirs, in that relative
// order.
func requireOrder(t *testing.T, dirs []string, want ...string) {
	t.Helper()
	prev := -1
	for _, w := range want {
		i := slices.Index(dirs, w)
		if i < 0 {
			t.Errorf("PATH %v does not contain %q", dirs, w)
			return
		}
		if i <= prev {
			t.Errorf("PATH %v has %q out of order; want relative order %v", dirs, w, want)
			return
		}
		prev = i
	}
}
