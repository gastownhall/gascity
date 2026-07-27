package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

// newBenchCity writes a minimal bd-provider city, the shape a store open
// resolves its conditional-writes mode from.
func newBenchCity(b *testing.B) string {
	b.Helper()
	cityPath := b.TempDir()
	toml := "name = \"bench\"\nprefix = \"bc\"\n\n[beads]\nprovider = \"bd\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(toml), 0o644); err != nil {
		b.Fatalf("writing city.toml: %v", err)
	}
	return cityPath
}

// BenchmarkLoadCityConfig measures one crossing of the config-load boundary:
// pack expansion plus the builtin-cache readiness walk that reads every file
// of every cached pack.
//
// This is the unit of work the store open used to repeat. A `gc bd show <id>
// --json` crossed this boundary three times — once in the bd command and twice
// more inside the single store open that scope resolution performs to decide
// which store holds a bead ID — so two crossings per invocation were redundant.
func BenchmarkLoadCityConfig(b *testing.B) {
	b.Setenv("GC_HOME", b.TempDir())
	cityPath := newBenchCity(b)
	if _, err := loadCityConfig(cityPath, io.Discard); err != nil {
		b.Fatalf("warming: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := loadCityConfig(cityPath, io.Discard); err != nil {
			b.Fatalf("loadCityConfig: %v", err)
		}
	}
}
