//go:build !windows

package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// BenchmarkGcBdPassthroughPreExec measures a `gc bd show` from a city-agent
// shape (GC_CITY_PATH, cwd at the city root) as a fresh process would run it,
// on the loaders gc bd used before the trims and on the trimmed ones. Each
// iteration forgets the per-process readiness state, so the legacy arm pays
// the cold readiness pass, the warm revalidation and the import scan that the
// trimmed arm skips. Both arms exec the same fake bd, so the difference is
// gc's own pre-exec work. Read it against BenchmarkBuiltinReadinessPass.
func BenchmarkGcBdPassthroughPreExec(b *testing.B) {
	for _, arm := range []struct {
		name   string
		legacy bool
	}{{"legacy", true}, {"trimmed", false}} {
		b.Run(arm.name, func(b *testing.B) {
			cityDir := newBdPassthroughBenchCity(b)
			if arm.legacy {
				defer useLegacyBdPassthroughSeams()()
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				resetBuiltinRuntimeReadyCacheFor(cityDir)
				resetHostedCredentialProbeCache()
				builtinImportWarningCache.Delete(normalizePathForCompare(cityDir))
				b.StartTimer()
				if code := doBd([]string{"show", "demo-abc"}, io.Discard, io.Discard); code != 0 {
					b.Fatalf("doBd(show) = %d", code)
				}
			}
		})
	}
}

// newBdPassthroughBenchCity is newBdTrimsTestCity for a benchmark.
func newBdPassthroughBenchCity(b *testing.B) string {
	b.Helper()
	root := b.TempDir()
	b.Setenv("GC_HOME", filepath.Join(root, "gc-home"))
	b.Setenv("GC_DOLT", "skip")
	b.Setenv("GC_RIG", "")
	b.Setenv("GC_DIR", "")
	origCityFlag, origRigFlag := cityFlag, rigFlag
	b.Cleanup(func() { cityFlag, rigFlag = origCityFlag, origRigFlag })
	cityFlag, rigFlag = "", ""

	cityDir := filepath.Join(root, "city")
	binDir := filepath.Join(root, "bin")
	for _, dir := range []string{filepath.Join(cityDir, ".beads"), filepath.Join(cityDir, ".gc"), filepath.Join(cityDir, "frontend", ".beads"), binDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			b.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join(cityDir, "city.toml"):        "[[rigs]]\nname = \"frontend\"\nprefix = \"fe\"\n",
		filepath.Join(cityDir, ".gc", "site.toml"): fmt.Sprintf("workspace_name = \"demo\"\nworkspace_prefix = \"demo\"\n\n[[rig]]\nname = \"frontend\"\npath = %s\n", strconv.Quote(filepath.Join(cityDir, "frontend"))),
	}
	for path, body := range files {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		b.Fatal(err)
	}
	writeBuiltinImportsFixture(b, cityDir, "core", "bd")
	b.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	b.Setenv("GC_CITY_PATH", cityDir)
	b.Chdir(cityDir)
	if err := EnsureBuiltinRuntimeAssets(cityDir, io.Discard); err != nil {
		b.Fatalf("warming the builtin caches: %v", err)
	}
	return cityDir
}
