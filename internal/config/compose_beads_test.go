package config

import (
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// TestLoadWithIncludesDefaultsConditionalWrites: omitted → default "off".
func TestLoadWithIncludesDefaultsConditionalWrites(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "test"
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if got := cfg.Beads.NormalizedConditionalWrites(); got != "off" {
		t.Fatalf("NormalizedConditionalWrites = %q, want off when omitted", got)
	}
}

// TestLoadWithIncludesPreservesExplicitConditionalWrites: explicit value with no
// fragment survives.
func TestLoadWithIncludesPreservesExplicitConditionalWrites(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "test"

[beads]
conditional_writes = "require"
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if got := cfg.Beads.NormalizedConditionalWrites(); got != "require" {
		t.Fatalf("NormalizedConditionalWrites = %q, want require", got)
	}
}

// TestLoadWithIncludesPreservesConditionalWritesAcrossBeadsFragment is the
// load-bearing regression: an included fragment that defines ONLY an unrelated
// [beads] sibling key must NOT reset the root's explicit conditional_writes.
// Without the per-field IsDefined preservation branch this is a silent
// require→off downgrade through routine config layering.
func TestLoadWithIncludesPreservesConditionalWritesAcrossBeadsFragment(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["fragment.toml"]

[workspace]
name = "test"

[beads]
conditional_writes = "require"
`)
	fs.Files["/city/fragment.toml"] = []byte(`
[beads]
bd_compatibility = "bd-1.0.5"
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if got := cfg.Beads.NormalizedConditionalWrites(); got != "require" {
		t.Fatalf("NormalizedConditionalWrites = %q, want root require to survive a [beads] fragment", got)
	}
	if cfg.Beads.NormalizedBDCompatibility() != "bd-1.0.5" {
		t.Fatalf("BDCompatibility = %q, want the fragment's bd-1.0.5", cfg.Beads.NormalizedBDCompatibility())
	}
}

// TestLoadWithIncludesFragmentOverridesConditionalWrites is the companion to the
// preservation test: a fragment that DOES set conditional_writes must win (LWW),
// so the preservation branch can't drift into "base value always wins."
func TestLoadWithIncludesFragmentOverridesConditionalWrites(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["fragment.toml"]

[workspace]
name = "test"

[beads]
conditional_writes = "off"
`)
	fs.Files["/city/fragment.toml"] = []byte(`
[beads]
conditional_writes = "auto"
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if got := cfg.Beads.NormalizedConditionalWrites(); got != "auto" {
		t.Fatalf("NormalizedConditionalWrites = %q, want the fragment's auto to win", got)
	}
}

// TestConditionalWritesParseAndDefault covers decode and the accessor default.
func TestConditionalWritesParseAndDefault(t *testing.T) {
	// zero value / omitted → default "off".
	if (BeadsConfig{}).NormalizedConditionalWrites() != "off" {
		t.Fatalf("zero-value accessor = %q, want off", (BeadsConfig{}).NormalizedConditionalWrites())
	}
	// an explicit value decodes.
	out, err := Parse([]byte("[beads]\nconditional_writes = \"auto\"\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if out.Beads.ConditionalWrites != "auto" {
		t.Fatalf("decoded conditional_writes = %q, want auto", out.Beads.ConditionalWrites)
	}
	// a [beads] section without the key leaves it empty (→ default via accessor).
	out2, err := Parse([]byte("[beads]\nprovider = \"bd\"\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if out2.Beads.ConditionalWrites != "" || out2.Beads.NormalizedConditionalWrites() != "off" {
		t.Fatalf("unset conditional_writes = %q (norm %q), want empty→off", out2.Beads.ConditionalWrites, out2.Beads.NormalizedConditionalWrites())
	}
}
