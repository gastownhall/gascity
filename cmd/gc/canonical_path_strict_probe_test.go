package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBdBinaryIdentityStillRejectsUnresolvablePaths(t *testing.T) {
	for _, state := range []string{"missing", "broken symlink", "symlink loop", "non-directory ancestor"} {
		t.Run(state, func(t *testing.T) {
			root := t.TempDir()
			binary := filepath.Join(root, "bd")
			switch state {
			case "broken symlink":
				if err := os.Symlink(filepath.Join(root, "gone"), binary); err != nil {
					t.Skipf("symlinks unsupported on this filesystem: %v", err)
				}
			case "symlink loop":
				if err := os.Symlink(binary, binary); err != nil {
					t.Skipf("symlinks unsupported on this filesystem: %v", err)
				}
			case "non-directory ancestor":
				if err := os.WriteFile(binary, []byte("not a directory"), 0o644); err != nil {
					t.Fatal(err)
				}
				binary = filepath.Join(binary, "child")
			}
			if _, err := statBdBinary(binary); err == nil {
				t.Fatalf("unresolvable bd path %q received a trusted file identity", binary)
			}
		})
	}
}

func TestCityConfigPathProbesStillRequireResolvableFile(t *testing.T) {
	for _, probe := range []struct {
		name    string
		resolve func(string) (string, error)
	}{
		{"snapshot", cityTOMLFile},
		{"reverse migration", reverseCityTOMLPath},
	} {
		t.Run(probe.name, func(t *testing.T) {
			for _, state := range []string{"missing", "broken symlink"} {
				t.Run(state, func(t *testing.T) {
					city := t.TempDir()
					if state == "broken symlink" {
						if err := os.Symlink(filepath.Join(city, "gone.toml"), filepath.Join(city, "city.toml")); err != nil {
							t.Skipf("symlinks unsupported on this filesystem: %v", err)
						}
					}
					if _, err := probe.resolve(city); err == nil {
						t.Fatalf("%s city.toml became a successful physical-file probe", state)
					}
				})
			}
		})
	}
}

func TestCityConfigPathProbesKeepPhysicalSymlinkTarget(t *testing.T) {
	for _, probe := range []struct {
		name    string
		resolve func(string) (string, error)
	}{
		{"snapshot", cityTOMLFile},
		{"reverse migration", reverseCityTOMLPath},
	} {
		t.Run(probe.name, func(t *testing.T) {
			city := t.TempDir()
			physical := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(physical, []byte("# physical config\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			want, err := filepath.EvalSymlinks(physical)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(physical, filepath.Join(city, "city.toml")); err != nil {
				t.Skipf("symlinks unsupported on this filesystem: %v", err)
			}
			got, err := probe.resolve(city)
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("resolved config = %q, want physical target %q", got, want)
			}
		})
	}
}
