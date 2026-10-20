package config

import (
	"reflect"
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
	if !reflect.DeepEqual(cfg.Ops, want) {
		t.Fatalf("Ops = %+v, want %+v", cfg.Ops, want)
	}
	empty, err := Parse([]byte("[workspace]\nname = \"test\"\n"))
	if err != nil {
		t.Fatalf("Parse without [ops]: %v", err)
	}
	if !reflect.DeepEqual(empty.Ops, OpsConfig{}) {
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
	if !reflect.DeepEqual(cfg.Ops, want) {
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

// TestOpsAuthoritativeParses proves [ops] authoritative defaults to nil, and
// accepts message, the one kind this build's conversation lane executes.
func TestOpsAuthoritativeParses(t *testing.T) {
	empty, err := Parse([]byte("[ops]\ncontent = \"auto\"\n"))
	if err != nil {
		t.Fatalf("Parse without authoritative: %v", err)
	}
	if empty.Ops.Authoritative != nil {
		t.Fatalf("Authoritative = %#v, want nil by default", empty.Ops.Authoritative)
	}
	if empty.Ops.LaneExecutes("message") {
		t.Fatal("LaneExecutes(message) = true with the list empty")
	}
	cfg, err := Parse([]byte("[ops]\nauthoritative = [\"message\"]\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !reflect.DeepEqual(cfg.Ops.Authoritative, []string{"message"}) {
		t.Fatalf("Authoritative = %#v, want [message]", cfg.Ops.Authoritative)
	}
	if !cfg.Ops.LaneExecutes("message") {
		t.Fatal("LaneExecutes(message) = false with message listed")
	}
	if cfg.Ops.LaneExecutes("interrupt") || cfg.Ops.LaneExecutes("") {
		t.Fatal("LaneExecutes is true for a kind that is not listed")
	}
}

// TestOpsAuthoritativeRejects proves an empty entry, a duplicate, an unknown
// kind and a kind the lane cannot execute each fail a single layer and the
// composed root, naming the key and the accepted kinds.
func TestOpsAuthoritativeRejects(t *testing.T) {
	for _, tc := range []struct {
		name, list, want string
	}{
		{"empty", `[""]`, "empty entry; accepted kinds: message"},
		{"blank", `[" "]`, "empty entry; accepted kinds: message"},
		{"duplicate", `["message", "message"]`, `"message" is listed twice`},
		{"unknown", `["mesage"]`, `"mesage" is not an op kind; accepted kinds: message`},
		{"interrupt", `["interrupt"]`, `cannot execute "interrupt" in this build; accepted kinds: message`},
		{"respond", `["respond"]`, `cannot execute "respond"`},
		{"lifecycle", `["kill"]`, `cannot execute "kill"`},
		{"cased", `["Message"]`, `"Message" is not an op kind`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			layer := "[ops]\nauthoritative = " + tc.list + "\n"
			if _, err := Parse([]byte(layer)); err == nil || !strings.Contains(err.Error(), "ops.authoritative") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse: err = %v, want ops.authoritative ... %s", err, tc.want)
			}
			fs := fsys.NewFake()
			fs.Files["/city/city.toml"] = []byte("include = [\"fragment.toml\"]\n\n[workspace]\nname = \"test\"\n")
			fs.Files["/city/fragment.toml"] = []byte(layer)
			if _, _, err := LoadWithIncludes(fs, "/city/city.toml"); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadWithIncludes: err = %v, want %s from the fragment", err, tc.want)
			}
		})
	}
}

// TestOpsAuthoritativeFragmentReplaces proves a fragment defining
// ops.authoritative replaces the root's list, and one that does not keeps it
// while still setting a gate.
func TestOpsAuthoritativeFragmentReplaces(t *testing.T) {
	for _, tc := range []struct {
		name, fragment string
		want           []string
	}{
		{"replaced with empty", "[ops]\nauthoritative = []\n", []string{}},
		{"kept", "[ops]\ncontrol = \"auto\"\n", []string{"message"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := fsys.NewFake()
			fs.Files["/city/city.toml"] = []byte("include = [\"fragment.toml\"]\n\n[workspace]\nname = \"test\"\n\n" +
				"[ops]\nauthoritative = [\"message\"]\n")
			fs.Files["/city/fragment.toml"] = []byte(tc.fragment)
			cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
			if err != nil {
				t.Fatalf("LoadWithIncludes: %v", err)
			}
			if !reflect.DeepEqual(cfg.Ops.Authoritative, tc.want) {
				t.Fatalf("Authoritative = %#v, want %#v", cfg.Ops.Authoritative, tc.want)
			}
		})
	}
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte("include = [\"fragment.toml\"]\n\n[workspace]\nname = \"test\"\n")
	fs.Files["/city/fragment.toml"] = []byte("[ops]\nauthoritative = [\"message\"]\n")
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if !cfg.Ops.LaneExecutes("message") {
		t.Fatalf("a fragment's list did not reach the root: %#v", cfg.Ops.Authoritative)
	}
}

// TestOpsAuthoritativeSchemaEnumMatchesLoad pins the schema's enum on [ops]
// authoritative to the kinds config load accepts, so an editor validates
// exactly what load does.
func TestOpsAuthoritativeSchemaEnumMatchesLoad(t *testing.T) {
	field, ok := reflect.TypeOf(OpsConfig{}).FieldByName("Authoritative")
	if !ok {
		t.Fatal("OpsConfig has no Authoritative field")
	}
	var enum []string
	for _, opt := range strings.Split(field.Tag.Get("jsonschema"), ",") {
		if v, found := strings.CutPrefix(opt, "enum="); found {
			enum = append(enum, v)
		}
	}
	var want []string
	for _, k := range laneAuthoritativeKinds {
		want = append(want, string(k))
	}
	if !reflect.DeepEqual(enum, want) {
		t.Fatalf("schema enum = %v, want the accepted kinds %v", enum, want)
	}
}
