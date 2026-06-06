package main

import (
	"os"
	"path/filepath"
	"strings"
)

// prependGCBinDirToPATH ensures that the directory containing the gc binary
// is the first entry in env["PATH"]. If env["PATH"] is unset, falls back to
// the calling process's PATH as the base.
//
// This protects spawned agents (which may write `gc` in shell prompts) from
// PATH collisions with unrelated binaries — notably Homebrew's `graphviz`
// package, which ships /opt/homebrew/bin/gc and breaks bare `gc` invocations
// for any agent whose PATH happens to put /opt/homebrew/bin first.
//
// gcBin is the absolute path to the gc binary (typically the value the caller
// also writes to env["GC_BIN"]). If empty or has no directory component, the
// function is a no-op.
func prependGCBinDirToPATH(env map[string]string, gcBin string) {
	if gcBin == "" {
		return
	}
	dir := filepath.Dir(gcBin)
	if dir == "" || dir == "." {
		return
	}
	sep := string(os.PathListSeparator)
	base, ok := env["PATH"]
	if !ok {
		base = os.Getenv("PATH")
	}
	if base == "" {
		env["PATH"] = dir
		return
	}

	parts := strings.Split(base, sep)
	entries := []string{dir}
	for _, p := range parts {
		if p == dir {
			continue
		}
		entries = append(entries, p)
	}
	env["PATH"] = strings.Join(entries, sep)
}
