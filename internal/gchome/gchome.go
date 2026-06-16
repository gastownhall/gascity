// Package gchome resolves machine-local Gas City state paths.
package gchome

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Default returns the Gas City machine-local state directory.
//
// Resolution order: GC_HOME, user home/.gc, process-unique temp fallback.
func Default() string {
	if v := strings.TrimSpace(os.Getenv("GC_HOME")); v != "" {
		return v
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".gc")
	}
	// Home unresolved. Never fall back to a fixed os.TempDir()/.gc: that path
	// is shared and world-writable, so concurrent processes clobber each
	// other's state and unrelated city scans pick it up as a real city
	// (#3506). Hand out a process-unique directory instead.
	if dir, err := os.MkdirTemp("", "gc-home-*"); err == nil {
		return dir
	}
	// MkdirTemp failed, so the temp directory itself is unusable. Return a
	// process-unique path under it rather than "" (which callers would join
	// into a CWD-relative path, silently writing state to the wrong place) or
	// the shared os.TempDir()/.gc that #3506 is about. The caller then fails
	// loudly when it cannot create or write this path.
	return filepath.Join(os.TempDir(), fmt.Sprintf("gc-home-%d", os.Getpid()))
}

// RegistriesPath returns the configured registry file path under home.
func RegistriesPath(home string) string {
	return filepath.Join(home, "registries.toml")
}

// RegistryCacheRoot returns the registry catalog cache directory under home.
func RegistryCacheRoot(home string) string {
	return filepath.Join(home, "registry-cache")
}
