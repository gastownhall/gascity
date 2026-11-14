package main

import (
	"bytes"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// TestWatchConfigDirs_RecursiveRootIgnoresRuntimeDirs asserts that the
// recursive config-watch walk skips the ecosystem/runtime directories every
// other pack-tree walk in this repo already skips.
//
// addPath's recursive WalkDir filters directories through
// shouldIgnoreConfigWatchEvent, which excludes only .gc and .beads. A
// recursive WatchTarget — a local-git-repo import, or a rig whose import
// source is the repo root — therefore registers a watch on every directory
// under .git/, node_modules/ and __pycache__/, and every write inside them
// (a commit, a fetch, an index.lock) reaches the event loop, marks the
// config dirty, and drives a full config reload plus a watcher restart.
//
// isIgnoredPackRuntimePath (internal/config/pack.go) skips .beads, .cache,
// .gc, .git, state, tmp, and node_modules/__pycache__ at any depth; the
// lint walk in cmd/gc/cmd_lint.go skips .git, .gc, .beads, node_modules.
// The watch walk is the one that does not, so the exclusion added for
// gastownhall/gascity#2954 (pack content hashing, #3063) never reached it.
func TestWatchConfigDirs_RecursiveRootIgnoresRuntimeDirs(t *testing.T) {
	old := debounceDelay
	debounceDelay = 5 * time.Millisecond
	t.Cleanup(func() { debounceDelay = old })

	root := t.TempDir()
	packFile := filepath.Join(root, "pack.toml")
	if err := os.WriteFile(packFile, []byte("[pack]\nname = \"p\"\nschema = 1\n"), 0o644); err != nil {
		t.Fatalf("seed pack.toml: %v", err)
	}

	// Runtime/ecosystem subtrees that a real local-repo import carries.
	runtimeDirs := map[string]string{
		"git":         filepath.Join(root, ".git", "objects", "ab"),
		"node":        filepath.Join(root, "node_modules", "left-pad", "lib"),
		"pycache":     filepath.Join(root, "__pycache__"),
		"nestedCache": filepath.Join(root, "skills", "s", "node_modules", "dep"),
	}
	for name, dir := range runtimeDirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "seed"), []byte("seed\n"), 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	// Unit-level: every runtime path must be classified as ignorable, the way
	// .gc/.beads already are.
	for name, dir := range runtimeDirs {
		probe := filepath.Join(dir, "seed")
		if !shouldIgnoreConfigWatchEvent(probe) {
			t.Errorf("shouldIgnoreConfigWatchEvent(%q) = false, want true (%s)", probe, name)
		}
	}
	if shouldIgnoreConfigWatchEvent(packFile) {
		t.Fatalf("shouldIgnoreConfigWatchEvent(%q) = true, want false", packFile)
	}

	var dirty atomic.Bool
	pokeCh := make(chan struct{}, 1)
	var stderr bytes.Buffer
	cleanup := watchConfigTargets([]config.WatchTarget{{Path: root, Recursive: true}}, &dirty, pokeCh, &stderr)
	defer cleanup()

	// Behavioral: writes inside the runtime subtrees must not poke the
	// controller or mark the config dirty.
	for name, dir := range runtimeDirs {
		select {
		case <-pokeCh:
		default:
		}
		dirty.Store(false)

		if err := os.WriteFile(filepath.Join(dir, "seed"), []byte("changed\n"), 0o644); err != nil {
			t.Fatalf("rewrite %s: %v", name, err)
		}
		// Negative-assertion window: asserts no watcher poke arrives (ga-57b2dk exclusion).
		select {
		case <-pokeCh:
			t.Errorf("unexpected watcher poke after write under %s; stderr=%q", name, stderr.String())
		case <-time.After(250 * time.Millisecond):
		}
		if dirty.Load() {
			t.Errorf("dirty flag set after write under %s; stderr=%q", name, stderr.String())
		}
	}

	// Control: the watcher is live — a real pack edit still pokes. Without
	// this the negative assertions above would pass on a dead watcher.
	select {
	case <-pokeCh:
	default:
	}
	dirty.Store(false)
	if err := os.WriteFile(packFile, []byte("[pack]\nname = \"p2\"\nschema = 1\n"), 0o644); err != nil {
		t.Fatalf("rewrite pack.toml: %v", err)
	}
	awaitClose(t, pokeCh, "watcher poke after pack.toml edit")
	if !dirty.Load() {
		t.Fatalf("dirty flag not set after pack.toml edit; stderr=%q", stderr.String())
	}
}
