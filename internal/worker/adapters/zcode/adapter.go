// Package zcode ships the ZCode worker adapter: a persistent, tmux-drivable
// REPL front end for the ZCode CLI, which has no public TUI of its own.
//
// The adapter is a shell script rather than Go because it must live inside the
// worker's tmux pane, own the pane's tty (non-canonical mode for prompts over
// the 4095-byte canonical limit) and absorb the pane's signals. It is embedded
// here so the engine is the single source of truth for it, and installed onto
// PATH by whoever provisions the worker host — provider commands are resolved
// off PATH, not from a materialized workdir overlay.
package zcode

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

// ExecutableName is the adapter's name on PATH. It is the Command of the
// zcode builtin provider spec.
const ExecutableName = "zcode-repl"

//go:embed zcode-repl
var script []byte

// Script returns the adapter source.
func Script() []byte {
	out := make([]byte, len(script))
	copy(out, script)
	return out
}

// Install writes the adapter into binDir as an executable and returns its
// path. Existing content is replaced.
func Install(binDir string) (string, error) {
	if binDir == "" {
		return "", fmt.Errorf("zcode adapter: empty install dir")
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(binDir, ExecutableName)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, script, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	// WriteFile honors umask; the adapter must be executable regardless.
	if err := os.Chmod(dst, 0o755); err != nil {
		return "", err
	}
	return dst, nil
}
