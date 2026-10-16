package config

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/gastownhall/gascity/internal/fsys"
)

// TestOpsParses decodes every [ops] gate and keeps the raw spelling (the
// mode mapping lives in internal/rollout).
func TestOpsParses(t *testing.T) {
	cfg, err := Parse([]byte("[ops]\ncontent = \"auto\"\ncontrol = \"require\"\nlifecycle = \"off\"\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := OpsConfig{ContentMode: "auto", ControlMode: "require", LifecycleMode: "off"}
	if cfg.Ops != want {
		t.Fatalf("Ops = %+v, want %+v", cfg.Ops, want)
	}
	empty, err := Parse([]byte("[workspace]\nname = \"test\"\n"))
	if err != nil {
		t.Fatalf("Parse without [ops]: %v", err)
	}
	if empty.Ops != (OpsConfig{}) {
		t.Fatalf("Ops without [ops] = %+v, want the zero value", empty.Ops)
	}
}

// TestOpsRejectsOutOfEnum proves a typo or a blank value in any gate fails a
// single layer (Parse) and the composed root (a fragment supplying it), naming
// the gate. A blank value is not unset: the resolver reads any non-empty value
// as set, so letting one load would fail every gate's resolution at boot.
func TestOpsRejectsOutOfEnum(t *testing.T) {
	for _, key := range []string{"content", "control", "lifecycle"} {
		for _, raw := range []string{"requre", "shadow", "true", " "} {
			t.Run(key+"="+raw, func(t *testing.T) {
				layer := "[ops]\n" + key + " = \"" + raw + "\"\n"
				want := "ops." + key
				if _, err := Parse([]byte(layer)); err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("Parse: err = %v, want the %s error", err, want)
				}
				fs := fsys.NewFake()
				fs.Files["/city/city.toml"] = []byte("include = [\"fragment.toml\"]\n\n[workspace]\nname = \"test\"\n")
				fs.Files["/city/fragment.toml"] = []byte(layer)
				if _, _, err := LoadWithIncludes(fs, "/city/city.toml"); err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("LoadWithIncludes: err = %v, want the %s error from the fragment", err, want)
				}
			})
		}
	}
}

// TestOpsAcceptsModeSpellings proves the mode grammar's case and space
// folding loads cleanly.
func TestOpsAcceptsModeSpellings(t *testing.T) {
	for _, raw := range []string{"off", "AUTO", " require "} {
		if _, err := Parse([]byte("[ops]\ncontent = \"" + raw + "\"\n")); err != nil {
			t.Fatalf("Parse(%q): %v", raw, err)
		}
	}
}

// TestOpsMergesPerKey proves a fragment defining one [ops] gate keeps the
// root's others, and that the gate it sets wins.
func TestOpsMergesPerKey(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte("include = [\"fragment.toml\"]\n\n[workspace]\nname = \"test\"\n\n" +
		"[ops]\ncontent = \"auto\"\ncontrol = \"require\"\nlifecycle = \"auto\"\n")
	fs.Files["/city/fragment.toml"] = []byte("[ops]\ncontrol = \"off\"\n")
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	want := OpsConfig{ContentMode: "auto", ControlMode: "off", LifecycleMode: "auto"}
	if cfg.Ops != want {
		t.Fatalf("Ops = %+v, want %+v", cfg.Ops, want)
	}
}

// TestOpsUnknownKeySuggests proves a misspelled [ops] key gets a typo
// suggestion instead of silently reading as off.
func TestOpsUnknownKeySuggests(t *testing.T) {
	var cfg City
	md, err := toml.Decode("[ops]\nlifecycel = \"auto\"\n", &cfg)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	warnings := CheckUndecodedKeys(md, "city.toml")
	for _, w := range warnings {
		if strings.Contains(w, "lifecycel") {
			if !strings.Contains(w, `did you mean "lifecycle"`) {
				t.Fatalf("warning should suggest lifecycle, got: %s", w)
			}
			return
		}
	}
	t.Fatalf("no warning about lifecycel in: %v", warnings)
}
