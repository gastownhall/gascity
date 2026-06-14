package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/config"
)

// materializeBuiltinPacksForTest is the test replacement for the retired
// per-city pack materialization: it hydrates the bundled-pack cache under
// the process GC_HOME (set by TestMain) and writes the stable gc-beads-bd
// shim for bd-provider cities.
func materializeBuiltinPacksForTest(t testing.TB, cityPath string) {
	t.Helper()
	if err := EnsureBuiltinRuntimeAssets(cityPath, io.Discard); err != nil {
		t.Fatalf("EnsureBuiltinRuntimeAssets: %v", err)
	}
}

// writeBuiltinImportsFixture gives a fixture city the canonical builtin
// pack composition: pinned [imports.<name>] entries in pack.toml (created
// with a minimal [pack] header when absent) plus a matching packs.lock.
// This is the shape gc init writes; the lock collection and the
// packv2-import-state doctor check only recognize pack.toml imports.
func writeBuiltinImportsFixture(t testing.TB, cityDir string, names ...string) {
	t.Helper()
	packPath := filepath.Join(cityDir, "pack.toml")
	data, err := os.ReadFile(packPath)
	if os.IsNotExist(err) {
		data = []byte(fmt.Sprintf("[pack]\nname = %q\nschema = 2\n", filepath.Base(cityDir)))
	} else if err != nil {
		t.Fatalf("reading pack.toml: %v", err)
	}
	if err := os.WriteFile(packPath, append(data, []byte(builtinImportsTOML(names...))...), 0o644); err != nil {
		t.Fatalf("writing pack.toml: %v", err)
	}
	writeBuiltinImportsLock(t, cityDir, names...)
}

// builtinImportsTOML returns [imports.<name>] manifest blocks for bundled
// builtin packs, usable inside city.toml or pack.toml fixture literals.
// Pair with writeBuiltinImportsLock so the sources resolve offline.
func builtinImportsTOML(names ...string) string {
	var b strings.Builder
	for _, name := range names {
		source, ok := builtinpacks.Source(name)
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "\n[imports.%s]\nsource = %q\nversion = %q\n", name, source, config.BundledSourcePinnedVersion(source))
	}
	return b.String()
}

// writeBuiltinImportsLock writes a packs.lock pinning the bundled builtin
// sources so fixture cities resolve them from the embedded synthetic cache
// without network access.
func writeBuiltinImportsLock(t testing.TB, cityDir string, names ...string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("schema = 1\n\n[packs]\n")
	for _, name := range names {
		source, ok := builtinpacks.Source(name)
		if !ok {
			t.Fatalf("unknown builtin pack %q", name)
		}
		version := config.BundledSourcePinnedVersion(source)
		fmt.Fprintf(&b, "[packs.%q]\nversion = %q\ncommit = %q\n\n", source, version, strings.TrimPrefix(version, "sha:"))
	}
	if err := os.WriteFile(filepath.Join(cityDir, "packs.lock"), []byte(b.String()), 0o644); err != nil {
		t.Fatalf("writing packs.lock: %v", err)
	}
}

// bundledGcBeadsBdScriptForTest returns the cache-resolved bundled bd
// lifecycle script (the stable per-city shim's exec target) for tests that
// assert on the script's content.
func bundledGcBeadsBdScriptForTest(t testing.TB) string {
	t.Helper()
	target, err := bundledGcBeadsBdScriptTarget()
	if err != nil {
		t.Fatalf("bundledGcBeadsBdScriptTarget: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("bundled gc-beads-bd script not cached: %v", err)
	}
	return target
}
