package config

import (
	"strings"
	"testing"
)

func TestDiagnosticLocatorLineForRigPathKeepsHashInsideQuotedName(t *testing.T) {
	locator := NewDiagnosticLocator([]byte(`
[workspace]
name = "city"

[[rigs]]
name = "rig#one"
path = "../rig-one"
`))

	if got := locator.LineForRigPath("rig#one"); got != 7 {
		t.Fatalf("LineForRigPath = %d, want 7", got)
	}
}

func TestLegacyV1SurfaceErrorsIncludeSourceCoordinates(t *testing.T) {
	data := []byte(`[workspace]
name = "city"
includes = ["legacy"]
default_rig_includes = ["legacy"]

[packs.legacy]
source = "../packs/legacy"

[[agent]]
name = "mayor"
`)
	var cfg City
	_, err := tomlDecode(string(data), &cfg)
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}

	got := strings.Join(LegacyV1SurfaceErrors(&cfg, "pack.toml", data), "\n")
	for _, want := range []string{
		"pack.toml:9: unsupported PackV1 [[agent]] tables",
		"pack.toml:6: unsupported PackV1 [packs] entries",
		"pack.toml:3: unsupported PackV1 workspace.includes",
		"pack.toml:4: unsupported PackV1 workspace.default_rig_includes",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("LegacyV1SurfaceErrors missing %q:\n%s", want, got)
		}
	}
}
