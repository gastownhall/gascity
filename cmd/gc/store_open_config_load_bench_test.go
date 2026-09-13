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
// This is the unit of work the store open used to repeat. `gc bd close`
// crossed this boundary three times and `gc bd update` twice — once in the bd
// command, then again inside each store open on the write path — so those
// extra crossings were redundant. Reads already crossed exactly once.
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
