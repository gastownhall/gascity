package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bdConfigFixture reads one of the real bd-written config.yaml fixtures shared
// with internal/beads/contract (see files_bd_nested_config_test.go there for
// how they were generated). bd >= 1.3.1 writes a dotted key nested
// (`dolt:` / `    host: ...`); gc writes it flat (`dolt.host: ...`).
func bdConfigFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRootForLint(t), "internal", "beads", "contract", "testdata", "bdconfig", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(data)
}

// gcBeadsBdConfigHarness extracts the config readers from gc-beads-bd.sh and
// appends body, which prints one `name=result` line per probe.
func gcBeadsBdConfigHarness(t *testing.T, body string) string {
	t.Helper()
	scriptBytes, err := os.ReadFile(filepath.Join(repoRootForLint(t), "examples", "bd", "assets", "scripts", "gc-beads-bd.sh"))
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	script := string(scriptBytes)
	var harness strings.Builder
	harness.WriteString("#!/bin/sh\nset -u\n")
	for _, fn := range []string{
		"trim_space",
		"normalize_dolt_mode",
		"read_metadata_string_field",
		"scope_backend_is_dolt",
		"beads_config_value",
		"scope_is_proxied",
		"provider_owned_scope_is_local",
	} {
		harness.WriteString(extractShellFunction(t, script, fn))
		harness.WriteString("\n")
	}
	harness.WriteString(body)
	path := filepath.Join(t.TempDir(), "harness.sh")
	if err := os.WriteFile(path, []byte(harness.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeBdConfigScope(t *testing.T, root, name, config, metadata string) string {
	t.Helper()
	scope := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(scope, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scope, ".beads", "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scope, ".beads", "metadata.json"), []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}
	return scope
}

func TestGcBeadsBdConfigValueReadsFlatAndNestedSpellings(t *testing.T) {
	root := t.TempDir()
	var body strings.Builder
	fixtures := []string{
		"bd-1.3.0-set-over-gc-canonical.yaml",
		"bd-1.3.1-set-over-gc-canonical.yaml",
		"bd-1.3.0-set-over-init-template.yaml",
		"bd-1.3.1-set-over-init-template.yaml",
	}
	for _, name := range fixtures {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(bdConfigFixture(t, name)), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"dolt.host", "dolt.port", "dolt.auto-start"} {
			body.WriteString("printf '%s\\n' \"" + name + " " + key + "=$(beads_config_value " + shellSingleQuote(path) + " " + key + ")\"\n")
		}
	}
	// Flat wins over nested; quotes and trailing comments are stripped; a
	// deeper mapping reusing the field name is not the key.
	both := filepath.Join(root, "both.yaml")
	if err := os.WriteFile(both, []byte("dolt:\n  sub:\n    mode: deeper\n  mode: nested\ndolt.mode: \"flat\" # note\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	deeper := filepath.Join(root, "deeper.yaml")
	if err := os.WriteFile(deeper, []byte("dolt:\n  sub:\n    mode: deeper\n  mode: 'nested' # note\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	commented := filepath.Join(root, "commented.yaml")
	if err := os.WriteFile(commented, []byte("dolt: # bd section\n  mode: nested\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// An empty flat value is the answer (viper returns it), not a reason to
	// read the nested one.
	emptyFlat := filepath.Join(root, "empty-flat.yaml")
	if err := os.WriteFile(emptyFlat, []byte("dolt.mode: ''\ndolt:\n  mode: nested\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body.WriteString("printf '%s\\n' \"commented=$(beads_config_value " + shellSingleQuote(commented) + " dolt.mode)\"\n")
	body.WriteString("printf '%s\\n' \"emptyflat=$(beads_config_value " + shellSingleQuote(emptyFlat) + " dolt.mode)\"\n")
	body.WriteString("printf '%s\\n' \"both=$(beads_config_value " + shellSingleQuote(both) + " dolt.mode)\"\n")
	body.WriteString("printf '%s\\n' \"deeper=$(beads_config_value " + shellSingleQuote(deeper) + " dolt.mode)\"\n")
	body.WriteString("printf '%s\\n' \"missing=$(beads_config_value " + shellSingleQuote(filepath.Join(root, "nope.yaml")) + " dolt.mode)\"\n")

	out := string(runShHarness(t, gcBeadsBdConfigHarness(t, body.String()), "beads_config_value", os.Environ()))
	want := []string{"both=flat", "deeper=nested", "missing=", "commented=nested", "emptyflat="}
	for _, name := range fixtures {
		want = append(want,
			name+" dolt.host=127.0.0.1",
			name+" dolt.port=3310",
			name+" dolt.auto-start=false")
	}
	for _, line := range want {
		if !strings.Contains(out, line+"\n") {
			t.Errorf("missing %q in output:\n%s", line, out)
		}
	}
}

func TestGcBeadsBdScopeTopologyReadsBdNestedConfig(t *testing.T) {
	root := t.TempDir()
	unmarkedMeta := `{"database":"dolt","backend":"dolt"}`
	directMeta := `{"database":"dolt","backend":"dolt","dolt_mode":"server"}`
	// bd 1.3.1 `bd config set dolt.host/dolt.port/dolt.mode` over the init
	// template: every key is nested. The scope names an external server, so
	// it is neither proxied (config mode server outranks the ambient proxy
	// selector) nor local.
	nested := writeBdConfigScope(t, root, "nested", bdConfigFixture(t, "bd-1.3.1-set-over-init-template.yaml"), unmarkedMeta)
	// gc's explicit-endpoint config after bd 1.3.1 re-set the endpoint.
	explicit := writeBdConfigScope(t, root, "explicit", bdConfigFixture(t, "bd-1.3.1-set-over-gc-canonical.yaml"), directMeta)
	// A city_canonical scope whose auto-start bd 1.3.1 turned on (nested).
	canonical := writeBdConfigScope(t, root, "canonical",
		"gc.endpoint_origin: city_canonical\ndolt:\n    disable-event-flush: true\n    auto-start: true\n", directMeta)
	// A scope that never named an endpoint stays local.
	bare := writeBdConfigScope(t, root, "bare", "issue_prefix: ex\n", unmarkedMeta)
	// The same auto-start answer, off, makes a city_canonical scope external.
	canonicalOff := writeBdConfigScope(t, root, "canonicalOff",
		"gc.endpoint_origin: city_canonical\ndolt:\n    auto-start: false\n", directMeta)

	var body strings.Builder
	for name, scope := range map[string]string{"nested": nested, "explicit": explicit, "canonical": canonical, "canonicalOff": canonicalOff, "bare": bare} {
		q := shellSingleQuote(scope)
		body.WriteString("if scope_is_proxied " + q + "; then echo " + name + ".proxied=yes; else echo " + name + ".proxied=no; fi\n")
		body.WriteString("if provider_owned_scope_is_local " + q + "; then echo " + name + ".local=yes; else echo " + name + ".local=no; fi\n")
	}
	env := append(os.Environ(), "BEADS_DOLT_PROXIED_SERVER=1", "GC_BEADS_TARGET=", "GC_BEADS_BACKEND=dolt")
	out := string(runShHarness(t, gcBeadsBdConfigHarness(t, body.String()), "scope topology", env))
	for _, line := range []string{
		"nested.proxied=no", "nested.local=no",
		"explicit.local=no",
		"canonical.local=yes",
		"canonicalOff.local=no",
		"bare.proxied=yes", "bare.local=yes",
	} {
		if !strings.Contains(out, line+"\n") {
			t.Errorf("missing %q in output:\n%s", line, out)
		}
	}
}

// The bd pack, the dolt pack and the wisps-composite-index schema script each
// carry a copy of the config reader, because they ship independently. The
// copies must not drift: compared with indentation and the function name
// normalized, the bodies are identical.
func TestBeadsConfigValueCopiesStayInSync(t *testing.T) {
	root := repoRootForLint(t)
	normalized := func(rel, name string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		body := extractShellFunction(t, string(data), name)
		body = strings.Replace(body, name+"()", "beads_config_value()", 1)
		lines := strings.Split(body, "\n")
		for i, line := range lines {
			lines[i] = strings.TrimLeft(line, " \t")
		}
		return strings.Join(lines, "\n")
	}
	want := normalized(filepath.Join("examples", "bd", "assets", "scripts", "gc-beads-bd.sh"), "beads_config_value")
	for rel, name := range map[string]string{
		filepath.Join("examples", "bd", "dolt", "assets", "scripts", "runtime.sh"): "beads_config_value",
		filepath.Join("schemas", "wisps-composite-index", "common.sh"):             "config_value",
	} {
		if got := normalized(rel, name); got != want {
			t.Errorf("%s %s() drifted from gc-beads-bd.sh beads_config_value():\n--- got ---\n%s\n--- want ---\n%s", rel, name, got, want)
		}
	}
}
