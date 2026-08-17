package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestExpandCityPacks_PackRuntimesResolved(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/alpha/pack.toml", `
[pack]
name = "alpha"
schema = 1

[runtimes.cloudflare]
command = "scripts/gc-runtime-cloudflare"
protocol = 0

[runtimes.bridge]
command = "gc-runtime-bridge"
`)

	cfg := &City{Workspace: Workspace{Includes: []string{"packs/alpha"}}}
	if _, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir); err != nil {
		t.Fatalf("ExpandCityPacks: %v", err)
	}

	packDir := filepath.Join(dir, "packs/alpha")
	cf, ok := cfg.Runtimes["cloudflare"]
	if !ok {
		t.Fatalf("runtime %q not registered; got %v", "cloudflare", cfg.Runtimes)
	}
	if want := filepath.Join(packDir, "scripts/gc-runtime-cloudflare"); cf.Command != want {
		t.Errorf("pack-relative command = %q, want %q", cf.Command, want)
	}
	if cf.Protocol != 0 {
		t.Errorf("protocol = %d, want 0", cf.Protocol)
	}
	if cf.PackName != "alpha" {
		t.Errorf("PackName = %q, want alpha", cf.PackName)
	}
	if cf.PackDir != packDir {
		t.Errorf("PackDir = %q, want %q", cf.PackDir, packDir)
	}
	if cf.Name != "cloudflare" {
		t.Errorf("Name = %q, want cloudflare", cf.Name)
	}

	// Bare names (no path separator) stay as-is for PATH lookup.
	br, ok := cfg.Runtimes["bridge"]
	if !ok {
		t.Fatal("runtime bridge not registered")
	}
	if br.Command != "gc-runtime-bridge" {
		t.Errorf("bare command = %q, want gc-runtime-bridge (PATH name)", br.Command)
	}
}

func TestExpandCityPacks_PackRuntimeFromNestedInclude(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/inner/pack.toml", `
[pack]
name = "inner"
schema = 1

[runtimes.nested]
command = "run/provider.sh"
`)
	writeFile(t, dir, "packs/outer/pack.toml", `
[pack]
name = "outer"
schema = 1
includes = ["../inner"]
`)

	cfg := &City{Workspace: Workspace{Includes: []string{"packs/outer"}}}
	if _, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir); err != nil {
		t.Fatalf("ExpandCityPacks: %v", err)
	}

	rt, ok := cfg.Runtimes["nested"]
	if !ok {
		t.Fatalf("nested include runtime not registered; got %v", cfg.Runtimes)
	}
	if want := filepath.Join(dir, "packs/inner/run/provider.sh"); rt.Command != want {
		t.Errorf("command = %q, want %q (resolved against the declaring pack)", rt.Command, want)
	}
	if rt.PackName != "inner" {
		t.Errorf("PackName = %q, want inner", rt.PackName)
	}
}

func TestExpandCityPacks_ImportedPackRuntimeKeepsBareName(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/alpha/pack.toml", `
[pack]
name = "alpha"
schema = 1

[runtimes.cloudflare]
command = "scripts/run.sh"
`)

	cfg := &City{Imports: map[string]Import{
		"cf": {Source: "packs/alpha"},
	}}
	if _, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir); err != nil {
		t.Fatalf("ExpandCityPacks: %v", err)
	}

	// Runtime selection names are global, never binding-qualified:
	// session provider strings in city.toml reference the declared name.
	if _, ok := cfg.Runtimes["cloudflare"]; !ok {
		t.Fatalf("imported runtime should keep bare name; got %v", cfg.Runtimes)
	}
	if _, ok := cfg.Runtimes["cf.cloudflare"]; ok {
		t.Error("imported runtime must not be binding-qualified")
	}
}

func TestExpandCityPacks_NonTransitiveImportFiltersNestedRuntimes(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/dep/pack.toml", `
[pack]
name = "dep"
schema = 1

[runtimes.hidden]
command = "scripts/hidden.sh"
`)
	writeFile(t, dir, "packs/direct/pack.toml", `
[pack]
name = "direct"
schema = 1

[imports.dep]
source = "../dep"

[runtimes.visible]
command = "scripts/visible.sh"
`)

	transitive := false
	cfg := &City{Imports: map[string]Import{
		"d": {Source: "packs/direct", Transitive: &transitive},
	}}
	if _, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir); err != nil {
		t.Fatalf("ExpandCityPacks: %v", err)
	}

	if _, ok := cfg.Runtimes["visible"]; !ok {
		t.Errorf("directly declared runtime should register; got %v", cfg.Runtimes)
	}
	if _, ok := cfg.Runtimes["hidden"]; ok {
		t.Error("non-transitive import must not surface nested pack runtimes")
	}
}

func TestExpandCityPacks_PackRuntimeConflictErrors(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/alpha/pack.toml", `
[pack]
name = "alpha"
schema = 1

[runtimes.same]
command = "scripts/alpha.sh"
`)
	writeFile(t, dir, "packs/beta/pack.toml", `
[pack]
name = "beta"
schema = 1

[runtimes.same]
command = "scripts/beta.sh"
`)

	cfg := &City{Workspace: Workspace{Includes: []string{"packs/alpha", "packs/beta"}}}
	_, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir)
	if err == nil {
		t.Fatal("conflicting runtime declarations must error, not shadow")
	}
	for _, want := range []string{"same", "alpha", "beta"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestExpandCityPacks_PackRuntimeIdenticalDeclarationsDedupe(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/shared/pack.toml", `
[pack]
name = "shared"
schema = 1

[runtimes.common]
command = "scripts/common.sh"
`)
	writeFile(t, dir, "packs/left/pack.toml", `
[pack]
name = "left"
schema = 1
includes = ["../shared"]
`)
	writeFile(t, dir, "packs/right/pack.toml", `
[pack]
name = "right"
schema = 1
includes = ["../shared"]
`)

	cfg := &City{Workspace: Workspace{Includes: []string{"packs/left", "packs/right"}}}
	if _, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir); err != nil {
		t.Fatalf("identical declaration via diamond DAG must dedupe, got: %v", err)
	}
	if _, ok := cfg.Runtimes["common"]; !ok {
		t.Fatalf("diamond runtime should register once; got %v", cfg.Runtimes)
	}
}

func TestMergeCityRuntimes_SamePackNameDifferentDirConflicts(t *testing.T) {
	// Two DIFFERENT pack directories sharing a pack.name declare the same runtime
	// name with the same bare (PATH-resolved) command. They are distinct packs and
	// MUST conflict — a shared name+command must not collapse two declaring
	// directories into one runtime row.
	cfg := &City{}
	a := DiscoveredRuntime{Name: "same", Command: "gc-runtime", Protocol: 0, PackName: "shared", PackDir: filepath.Join(t.TempDir(), "alpha")}
	b := DiscoveredRuntime{Name: "same", Command: "gc-runtime", Protocol: 0, PackName: "shared", PackDir: filepath.Join(t.TempDir(), "beta")}

	if err := mergeCityRuntimes(cfg, []DiscoveredRuntime{a}); err != nil {
		t.Fatalf("first declaration must register: %v", err)
	}
	err := mergeCityRuntimes(cfg, []DiscoveredRuntime{b})
	if err == nil {
		t.Fatal("same pack.name+command from a different pack directory must conflict, not dedupe")
	}
	if !strings.Contains(err.Error(), "same") {
		t.Errorf("error %q should name the runtime", err)
	}
}

func TestMergeCityRuntimes_SameDirReDeclarationDedupes(t *testing.T) {
	// The diamond-import case: the SAME resolved pack directory is reached twice.
	// Identical re-declarations dedupe (the absolute-path comparison tolerates a
	// relative vs. absolute spelling of the same directory).
	cfg := &City{}
	dir := t.TempDir()
	abs := DiscoveredRuntime{Name: "common", Command: "gc-runtime", Protocol: 0, PackName: "shared", PackDir: dir}
	rel := DiscoveredRuntime{Name: "common", Command: "gc-runtime", Protocol: 0, PackName: "shared", PackDir: filepath.Join(dir, ".")}

	if err := mergeCityRuntimes(cfg, []DiscoveredRuntime{abs}); err != nil {
		t.Fatalf("first declaration must register: %v", err)
	}
	if err := mergeCityRuntimes(cfg, []DiscoveredRuntime{rel}); err != nil {
		t.Fatalf("identical re-declaration from the same dir must dedupe, got: %v", err)
	}
	if _, ok := cfg.Runtimes["common"]; !ok {
		t.Fatalf("runtime should register once; got %v", cfg.Runtimes)
	}
}

func TestExpandCityPacks_PackRuntimeValidation(t *testing.T) {
	cases := []struct {
		name    string
		toml    string
		wantErr string
	}{
		{
			name: "missing command",
			toml: `
[runtimes.foo]
protocol = 0
`,
			wantErr: "command is required",
		},
		{
			name: "blank command",
			toml: `
[runtimes.foo]
command = "  "
`,
			wantErr: "command is required",
		},
		{
			name: "unsupported protocol",
			toml: `
[runtimes.foo]
command = "run.sh"
protocol = 1
`,
			wantErr: "protocol 1 not supported",
		},
		{
			name: "negative protocol",
			toml: `
[runtimes.foo]
command = "run.sh"
protocol = -1
`,
			wantErr: "protocol -1 not supported",
		},
		{
			name: "colon in name",
			toml: `
[runtimes."exec:foo"]
command = "run.sh"
`,
			wantErr: "must not contain",
		},
		{
			name: "slash in name",
			toml: `
[runtimes."a/b"]
command = "run.sh"
`,
			wantErr: "must not contain",
		},
		{
			name: "whitespace in name",
			toml: `
[runtimes."a b"]
command = "run.sh"
`,
			wantErr: "must not contain",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, "packs/bad/pack.toml", `
[pack]
name = "bad"
schema = 1
`+tc.toml)
			cfg := &City{Workspace: Workspace{Includes: []string{"packs/bad"}}}
			_, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir)
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want substring %q", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), "bad") {
				t.Errorf("error %q should name the declaring pack", err)
			}
		})
	}
}

func TestExpandPacks_RigImportRuntimeRegistersCityWide(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/rigpack/pack.toml", `
[pack]
name = "rigpack"
schema = 1

[runtimes.rigrt]
command = "scripts/rigrt.sh"
`)

	cfg := &City{
		Rigs: []Rig{{
			Name:    "r1",
			Imports: map[string]Import{"rp": {Source: "packs/rigpack"}},
		}},
	}
	if err := ExpandPacks(cfg, fsys.OSFS{}, dir, map[string][]string{}); err != nil {
		t.Fatalf("ExpandPacks: %v", err)
	}

	// Session provider selection is city-wide; rig-imported runtime packs
	// register into the same namespace as city-level ones.
	rt, ok := cfg.Runtimes["rigrt"]
	if !ok {
		t.Fatalf("rig-imported runtime should register city-wide; got %v", cfg.Runtimes)
	}
	if want := filepath.Join(dir, "packs/rigpack/scripts/rigrt.sh"); rt.Command != want {
		t.Errorf("command = %q, want %q", rt.Command, want)
	}
}

func TestExpandCityPacks_PackRuntimeCrossPackIdenticalCommandErrors(t *testing.T) {
	// Dedupe is only for the same pack reached through a diamond import
	// DAG. Two unrelated packs declaring the same runtime name collide
	// even when the commands are identical: the runtime is attributed to
	// its declaring pack (doctor diagnostics, `gc runtime check` notes),
	// so silent first-merge-wins re-attribution is a composition error.
	dir := t.TempDir()
	writeFile(t, dir, "packs/alpha/pack.toml", `
[pack]
name = "alpha"
schema = 1

[runtimes.same]
command = "gc-runtime-shared"
`)
	writeFile(t, dir, "packs/beta/pack.toml", `
[pack]
name = "beta"
schema = 1

[runtimes.same]
command = "gc-runtime-shared"
`)

	cfg := &City{Workspace: Workspace{Includes: []string{"packs/alpha", "packs/beta"}}}
	_, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir)
	if err == nil {
		t.Fatal("cross-pack declarations of the same runtime name must error even with identical commands")
	}
	for _, want := range []string{"same", "alpha", "beta"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}
