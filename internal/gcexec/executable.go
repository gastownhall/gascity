package gcexec

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Current resolves the gc binary child helper processes should exec.
// GC_BIN is set by t3code's runtime installer and points at the active
// installed binary, while os.Executable can preserve stale development builds.
func Current() (string, error) {
	if candidate := strings.TrimSpace(os.Getenv("GC_BIN")); candidate != "" {
		if err := validate(candidate); err == nil {
			return candidate, nil
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if err := validate(exe); err != nil {
		return "", err
	}
	return exe, nil
}

func IsGoTestExecutable(path string) bool {
	return strings.HasSuffix(filepath.Base(path), ".test")
}

func validate(path string) error {
	if IsGoTestExecutable(path) {
		return fmt.Errorf("refusing to use Go test binary %q", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("gc executable path is a directory: %q", path)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("gc executable path is not executable: %q", path)
	}
	return nil
}
