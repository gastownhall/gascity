package config

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/gastownhall/gascity/internal/fsys"
)

// TestParseCheckScriptEnvDecodesAndValidates pins the TOML shape of
// [check_script_env] and that Parse refuses a reserved key, so a city whose
// config would override the check environment's own variables never loads.
func TestParseCheckScriptEnvDecodesAndValidates(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(`
[workspace]
name = "test"

[check_script_env]
PYTHONUSERBASE = "$HOME/Library/Python/3.14"
GC_BUILD_SCHEMA_ROOTS = "/schemas"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// Values are stored verbatim: $VAR expansion is the controller's job at
	// the edge, against its own environment, never the loader's.
	if got := cfg.CheckScriptEnv["PYTHONUSERBASE"]; got != "$HOME/Library/Python/3.14" {
		t.Errorf("PYTHONUSERBASE = %q, want the unexpanded value", got)
	}
	// A GC_* name the check environment does not write stays allowed: the
	// stock artifact validator reads GC_BUILD_SCHEMA_ROOTS, for instance.
	if got := cfg.CheckScriptEnv["GC_BUILD_SCHEMA_ROOTS"]; got != "/schemas" {
		t.Errorf("GC_BUILD_SCHEMA_ROOTS = %q", got)
	}

	for _, key := range []string{"HOME", "PATH", "BEADS_DIR", "GC_BEAD_ID", "XDG_CONFIG_HOME", "BASH_ENV"} {
		_, err := Parse([]byte("[workspace]\nname = \"test\"\n\n[check_script_env]\n" + key + " = \"x\"\n"))
		if err == nil {
			t.Errorf("%s: Parse accepted a reserved check_script_env key", key)
			continue
		}
		if !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "check_script_env") {
			t.Errorf("%s: error %q must name the key and the table", key, err)
		}
	}
}

// TestMarshalOmitsEmptyCheckScriptEnv pins that the default city.toml gc
// writes gains no [check_script_env] table: the setting is opt-in and an
// absent table must round-trip as absent.
func TestMarshalOmitsEmptyCheckScriptEnv(t *testing.T) {
	t.Parallel()
	cfg := &City{Workspace: Workspace{Name: "x"}}
	out, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "check_script_env") {
		t.Fatalf("Marshal emitted an empty table:\n%s", out)
	}
	cfg.CheckScriptEnv = map[string]string{"PYTHONUSERBASE": "/opt/py"}
	out, err = cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "[check_script_env]\nPYTHONUSERBASE = \"/opt/py\"") {
		t.Fatalf("Marshal output:\n%s", out)
	}
}

// TestLoadWithIncludes_CheckScriptEnvFragment pins that a fragment adds to the
// table rather than replacing it (like provider env, with a warning on a
// redefinition), and that the composed root is validated even when the root
// city.toml had no table of its own to validate at Parse time.
func TestLoadWithIncludes_CheckScriptEnvFragment(t *testing.T) {
	t.Parallel()
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["env.toml"]

[workspace]
name = "test"

[check_script_env]
KEEP_ME = "root"
SHARED = "root"
`)
	fs.Files["/city/env.toml"] = []byte(`
[check_script_env]
PYTHONUSERBASE = "/opt/py"
SHARED = "fragment"
`)
	cfg, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	want := map[string]string{"KEEP_ME": "root", "SHARED": "fragment", "PYTHONUSERBASE": "/opt/py"}
	for k, v := range want {
		if cfg.CheckScriptEnv[k] != v {
			t.Errorf("%s = %q, want %q", k, cfg.CheckScriptEnv[k], v)
		}
	}
	if len(cfg.CheckScriptEnv) != len(want) {
		t.Errorf("table = %v, want exactly %v", cfg.CheckScriptEnv, want)
	}
	if joined := strings.Join(prov.Warnings, "\n"); !strings.Contains(joined, "check_script_env.SHARED redefined by") {
		t.Errorf("warnings = %q, want the redefinition of SHARED named", joined)
	}

	fs.Files["/city/env.toml"] = []byte(`
[check_script_env]
HOME = "/real/home"
`)
	if _, _, err := LoadWithIncludes(fs, "/city/city.toml"); err == nil || !strings.Contains(err.Error(), `"HOME"`) {
		t.Fatalf("LoadWithIncludes error = %v, want the fragment's reserved HOME refused on the composed root", err)
	}
}

// TestPackCannotSetCheckScriptEnv pins that the table is operator-only: a
// pack.toml carrying [check_script_env] is left undecoded by PackConfig and
// never reaches the composed city, so a pack cannot inject variables into the
// environment of the checks that grade its own workflows.
func TestPackCannotSetCheckScriptEnv(t *testing.T) {
	t.Parallel()
	var pack PackConfig
	md, err := toml.Decode(`
[pack]
name = "p"

[check_script_env]
PYTHONUSERBASE = "/injected"
`, &pack)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	var undecoded []string
	for _, key := range md.Undecoded() {
		undecoded = append(undecoded, key.String())
	}
	if len(undecoded) == 0 || !strings.HasPrefix(undecoded[0], "check_script_env") {
		t.Fatalf("undecoded = %v, want the check_script_env table left undecoded by PackConfig", undecoded)
	}
}
