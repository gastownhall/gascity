//go:build windows

package fsys

import (
	"os"
	"path/filepath"
	"testing"
)

func statNewFile(t *testing.T, name string) os.FileInfo {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("MZ"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func TestIsExecutableRecognisesWindowsProgramsByExtension(t *testing.T) {
	t.Setenv("PATHEXT", ".COM;.EXE;.BAT;.CMD;.VBS")
	for name, want := range map[string]bool{
		"gc.exe":     true,
		"GC.EXE":     true,
		"gc.Exe":     true,
		"tool.com":   true,
		"tool.bat":   true,
		"tool.cmd":   true,
		"tool.vbs":   true,
		"gc.txt.exe": true,
		"gc":         false,
		"gc.txt":     false,
		"gc.exe.txt": false,
		"gc.ps1":     false,
	} {
		t.Run(name, func(t *testing.T) {
			info := statNewFile(t, name)
			if info.Mode().Perm()&0o111 != 0 {
				t.Fatalf("premise: os.Stat reported execute bits %v for a Windows file", info.Mode())
			}
			if got := IsExecutable(info); got != want {
				t.Fatalf("IsExecutable(%s) = %t, want %t", name, got, want)
			}
		})
	}
}

func TestIsExecutableFollowsPATHEXT(t *testing.T) {
	t.Setenv("PATHEXT", ".PS1;cmd;;")
	for name, want := range map[string]bool{
		"script.ps1": true,
		"tool.cmd":   true,
		"tool.bat":   false,
		"tool.com":   true,
		"gc.exe":     true,
	} {
		t.Run(name, func(t *testing.T) {
			if got := IsExecutable(statNewFile(t, name)); got != want {
				t.Fatalf("IsExecutable(%s) with PATHEXT=.PS1;cmd;; = %t, want %t", name, got, want)
			}
		})
	}
}

func TestIsExecutableHandlesOddPATHEXT(t *testing.T) {
	t.Setenv("PATHEXT", ";PS1;.WeIrD;;")
	for name, want := range map[string]bool{
		"script.ps1": true,
		"tool.weird": true,
		"tool.cmd":   false,
		"tool.com":   true,
		"gc.exe":     true,
	} {
		t.Run(name, func(t *testing.T) {
			if got := IsExecutable(statNewFile(t, name)); got != want {
				t.Fatalf("IsExecutable(%s) with odd PATHEXT = %t, want %t", name, got, want)
			}
		})
	}
}

func TestIsExecutableDefaultsWithoutPATHEXT(t *testing.T) {
	defaults := map[string]bool{
		"gc.exe":     true,
		"tool.com":   true,
		"tool.bat":   true,
		"tool.cmd":   true,
		"tool.vbs":   false,
		"script.ps1": false,
	}
	t.Run("empty", func(t *testing.T) {
		t.Setenv("PATHEXT", "")
		for name, want := range defaults {
			if got := IsExecutable(statNewFile(t, name)); got != want {
				t.Errorf("IsExecutable(%s) with PATHEXT empty = %t, want %t", name, got, want)
			}
		}
	})
	t.Run("unset", func(t *testing.T) {
		t.Setenv("PATHEXT", "")
		if err := os.Unsetenv("PATHEXT"); err != nil {
			t.Fatal(err)
		}
		for name, want := range defaults {
			if got := IsExecutable(statNewFile(t, name)); got != want {
				t.Errorf("IsExecutable(%s) with PATHEXT unset = %t, want %t", name, got, want)
			}
		}
	})
}
