//go:build windows

package fsys

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// IsExecutable reports whether info names a file Windows runs as a program.
//
// Windows has no execute permission bits: os.Stat reports 0o111 only on
// directories, so the Unix test refuses every program. An existing .com or .exe
// file runs when addressed by its explicit path regardless of PATHEXT. For
// everything else, Windows decides by extension, and the extensions that make a
// file runnable by name are the ones PATHEXT lists — the list cmd.exe and
// os/exec consult — or .com, .exe, .bat and .cmd when PATHEXT is unset, as
// os/exec assumes. The match ignores case. Whether info is a regular file is
// the caller's to check, as on Unix.
func IsExecutable(info fs.FileInfo) bool {
	ext := filepath.Ext(info.Name())
	if ext == "" {
		return false
	}
	if strings.EqualFold(ext, ".com") || strings.EqualFold(ext, ".exe") {
		return true
	}
	for _, runnable := range executableExtensions() {
		if strings.EqualFold(ext, runnable) {
			return true
		}
	}
	return false
}

// executableExtensions parses PATHEXT the way os/exec does: entries are
// separated by semicolons, empty entries are skipped, and an entry without a
// leading dot gains one.
func executableExtensions() []string {
	value := os.Getenv("PATHEXT")
	if value == "" {
		return []string{".com", ".exe", ".bat", ".cmd"}
	}
	var exts []string
	for _, ext := range strings.Split(strings.ToLower(value), ";") {
		if ext == "" {
			continue
		}
		if ext[0] != '.' {
			ext = "." + ext
		}
		exts = append(exts, ext)
	}
	return exts
}
