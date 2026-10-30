package dashboardbff

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
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

// TestCleanEnvPathAdminOverride pins the operator escape hatch: a set
// ADMIN_PATH is passed through verbatim, with nothing added, on every
// platform — the deterministic expansion applies only when it is unset.
func TestCleanEnvPathAdminOverride(t *testing.T) {
	t.Parallel()
	const admin = "/x/bin:/y/bin"
	for _, goos := range []string{"darwin", "linux"} {
		if got := cleanEnvPath(admin, t.TempDir(), goos); got != admin {
			t.Errorf("cleanEnvPath(%q, home, %q) = %q, want %q verbatim", admin, goos, got, admin)
		}
	}
	// An unset HOME must not turn the override into an expansion either.
	if got := cleanEnvPath(admin, "", "darwin"); got != admin {
		t.Errorf("cleanEnvPath(%q, \"\", \"darwin\") = %q, want %q verbatim", admin, got, admin)
	}
}

// TestExecBdPingChildGetsDeterministicSearchPath pins #7979 end to end: the
// environment bd receives must carry the deterministic searchpath order
// (existing user-managed dirs such as $HOME/go/bin plus the platform's
// package-manager dirs), so probes can find a Homebrew-installed dolt, and
// must not inherit the supervisor's own PATH — the only place the directory
// holding this fake bd exists.
func TestExecBdPingChildGetsDeterministicSearchPath(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "go", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	beadsPath := filepath.Join(root, ".beads")
	if err := os.Mkdir(beadsPath, 0o755); err != nil {
		t.Fatal(err)
	}
	pathFile := filepath.Join(root, "child-path")
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$PATH\" > \"" + pathFile + "\"\nprintf '%s' '{\"status\":\"ok\"}'\n"
	if err := os.WriteFile(filepath.Join(bin, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("ADMIN_PATH", "")
	// exec.Command resolves bd against the parent PATH before the runner
	// applies its scrubbed child environment.
	t.Setenv("PATH", bin)

	res, err := newExecRunner().execBdPing(context.Background(), beadsPath)
	if err != nil {
		t.Fatalf("execBdPing() error = %v", err)
	}
	if res.exitCode != 0 {
		t.Fatalf("execBdPing() exit = %d stdout=%q stderr=%q", res.exitCode, res.stdout, res.stderr)
	}
	recorded, err := os.ReadFile(pathFile)
	if err != nil {
		t.Fatal(err)
	}
	dirs := filepath.SplitList(strings.TrimSpace(string(recorded)))
	contains := func(dir string) bool {
		for _, d := range dirs {
			if d == dir {
				return true
			}
		}
		return false
	}
	if want := filepath.Join(home, "go", "bin"); !contains(want) {
		t.Errorf("bd child PATH = %q, want it to contain %q", recorded, want)
	}
	if want := map[string]string{
		"darwin": "/opt/homebrew/bin",
		"linux":  "/home/linuxbrew/.linuxbrew/bin",
	}[runtime.GOOS]; want != "" && !contains(want) {
		t.Errorf("bd child PATH = %q, want it to contain the %s package-manager dir %q", recorded, runtime.GOOS, want)
	}
	if contains(bin) {
		t.Errorf("bd child PATH = %q, must not inherit the supervisor's own PATH entry %q", recorded, bin)
	}
}
