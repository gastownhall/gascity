package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/packman"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestPackRegistryJSONOutputMatchesSchemas(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GC_HOME", home)
	catalogDir := writeRegistryCatalog(t, packRegistryTestCatalog)

	runPackRegistryJSONAndValidate(t, []string{"pack", "registry", "add", "main", catalogDir, "--json"})
	runPackRegistryJSONAndValidate(t, []string{"pack", "registry", "list", "--json"})
	runPackRegistryJSONAndValidate(t, []string{"pack", "registry", "refresh", "--json"})
	runPackRegistryJSONAndValidate(t, []string{"pack", "registry", "search", "light", "--json"})
	runPackRegistryJSONAndValidate(t, []string{"pack", "registry", "show", "lighthouse", "--json"})
	runPackRegistryJSONAndValidate(t, []string{"pack", "registry", "remove", "main", "--json"})
}

func TestPackRegistryJSONOutputMatchesSchemasForFlagCombinations(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GC_HOME", home)
	goodDir := writeRegistryCatalog(t, packRegistryTestCatalog)
	otherDir := writeRegistryCatalog(t, packRegistryOtherCatalog)

	runPackRegistryJSONAndValidate(t, []string{"pack", "registry", "add", "good", goodDir, "--json"})
	runPackRegistryJSONAndValidate(t, []string{"pack", "registry", "add", "other", otherDir, "--no-validate", "--json"})
	runPackRegistryJSONAndValidate(t, []string{"pack", "registry", "refresh", "good", "--json"})
	runPackRegistryJSONAndValidate(t, []string{"pack", "registry", "search", "light", "--registry", "good", "--json"})
	runPackRegistryJSONAndValidate(t, []string{"pack", "registry", "search", "light", "--limit", "1", "--json"})
	runPackRegistryJSONAndValidate(t, []string{"pack", "registry", "search", "light", "--all", "--json"})

	missing := filepath.Join(t.TempDir(), "missing")
	runPackRegistryJSONAndValidate(t, []string{"pack", "registry", "add", "bad", missing, "--no-validate", "--json"})
	runPackRegistryJSONAndValidate(t, []string{"pack", "registry", "search", "light", "--refresh", "--json"})
}

func TestPackDependencyJSONOutputMatchesSchemas(t *testing.T) {
	clearGCEnv(t)
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_CITY", city)
	writeCityToml(t, city, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, city, `[pack]
name = "demo"
schema = 1

[imports.tools]
source = "https://example.com/tools.git"
version = "^1.0"
transitive = false
`)
	if err := packman.WriteLockfile(fsys.OSFS{}, city, &packman.Lockfile{
		Schema: packman.LockfileSchema,
		Packs: map[string]packman.LockedPack{
			"https://example.com/tools.git": {
				Version: "1.0.0",
				Commit:  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			},
		},
	}); err != nil {
		t.Fatalf("WriteLockfile: %v", err)
	}

	runPackRegistryJSONAndValidate(t, []string{"pack", "list", "--json"})
	runPackRegistryJSONAndValidate(t, []string{"pack", "show", "tools", "--json"})
	runPackRegistryJSONAndValidate(t, []string{"pack", "why", "tools", "--json"})

	prevResolve := resolveImportVersion
	t.Cleanup(func() { resolveImportVersion = prevResolve })
	resolveImportVersion = func(source, constraint string) (packman.ResolvedVersion, error) {
		switch constraint {
		case "^1.0":
			return packman.ResolvedVersion{Version: "1.1.0", Commit: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}, nil
		case "":
			return packman.ResolvedVersion{Version: "2.0.0", Commit: "cccccccccccccccccccccccccccccccccccccccc"}, nil
		default:
			t.Fatalf("unexpected resolve %q %q", source, constraint)
			return packman.ResolvedVersion{}, nil
		}
	}
	runPackRegistryJSONAndValidate(t, []string{"pack", "outdated", "--json"})
}

func TestPackRegistryJSONFailureMatchesFailureSchema(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GC_HOME", home)
	catalogDir := writeRegistryCatalog(t, packRegistryTestCatalog)
	otherDir := writeRegistryCatalog(t, packRegistryOtherCatalog)

	runPackRegistryJSONAndValidate(t, []string{"pack", "registry", "add", "main", catalogDir, "--json"})
	runPackRegistryJSONAndValidate(t, []string{"pack", "registry", "add", "other", otherDir, "--json"})
	runPackRegistryJSONAndValidate(t, []string{"pack", "registry", "add", "bad", filepath.Join(t.TempDir(), "missing"), "--no-validate", "--json"})

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"show missing", []string{"pack", "registry", "show", "missing", "--json"}},
		{"add duplicate", []string{"pack", "registry", "add", "main", catalogDir, "--json"}},
		{"remove missing", []string{"pack", "registry", "remove", "missing", "--json"}},
		{"search unknown registry", []string{"pack", "registry", "search", "--registry", "missing", "--json"}},
		{"search unavailable caches", []string{"pack", "registry", "search", "--registry", "bad", "--json"}},
		{"show ambiguous", []string{"pack", "registry", "show", "lighthouse", "--json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runPackRegistryJSONFailureAndValidate(t, tc.args)
		})
	}
}

func TestPackDependencyJSONFailureMatchesFailureSchema(t *testing.T) {
	clearGCEnv(t)
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_CITY", city)
	writeCityToml(t, city, `[workspace]
name = "demo"

[packs.legacy]
source = "https://example.com/legacy.git"
`)

	runPackRegistryJSONFailureAndValidate(t, []string{"pack", "list", "--legacy", "--json"})
}

func TestPackRegistryRefreshJSONAllFailuresUsesResultSchemaWithNonzeroExit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GC_HOME", home)
	runPackRegistryJSONAndValidate(t, []string{"pack", "registry", "add", "bad", filepath.Join(t.TempDir(), "missing"), "--no-validate", "--json"})
	runPackRegistryJSONAndValidateExitCode(t, []string{"pack", "registry", "refresh", "--json"}, 1)
}

func runPackRegistryJSONFailureAndValidate(t *testing.T, args []string) {
	t.Helper()
	command := commandFromArgs(args)
	var schemaStdout, schemaStderr bytes.Buffer
	schemaArgs := append(append([]string{}, command...), "--json-schema=failure")
	code := run(schemaArgs, &schemaStdout, &schemaStderr)
	if code != 0 {
		t.Fatalf("%s failure schema code=%d stderr=%q stdout=%q", strings.Join(command, " "), code, schemaStderr.String(), schemaStdout.String())
	}
	if schemaStderr.Len() != 0 {
		t.Fatalf("%s failure schema stderr=%q, want empty", strings.Join(command, " "), schemaStderr.String())
	}
	schema := compileJSONSchema(t, "gc://schemas/"+strings.Join(command, "/")+"/failure.schema.json", schemaStdout.Bytes())

	var stdout, stderr bytes.Buffer
	code = run(args, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("%s code=0, want nonzero stdout=%q stderr=%q", strings.Join(args, " "), stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "gc pack registry") {
		t.Fatalf("diagnostic leaked into JSON stdout: %q", stdout.String())
	}
	var payload any
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("%s failure stdout is not JSON: %v\n%s", strings.Join(args, " "), err, stdout.String())
	}
	if err := schema.Validate(payload); err != nil {
		t.Fatalf("%s failure stdout does not match schema: %v\nschema=%s\npayload=%s", strings.Join(args, " "), err, schemaStdout.String(), stdout.String())
	}
	if stderr.Len() == 0 {
		t.Fatalf("stderr empty, want human diagnostics")
	}
}

func TestPackRegistrySchemasHaveDescriptions(t *testing.T) {
	for _, command := range [][]string{
		{"pack", "list"},
		{"pack", "show"},
		{"pack", "outdated"},
		{"pack", "why"},
		{"pack", "registry", "add"},
		{"pack", "registry", "list"},
		{"pack", "registry", "refresh"},
		{"pack", "registry", "remove"},
		{"pack", "registry", "search"},
		{"pack", "registry", "show"},
	} {
		var stdout, stderr bytes.Buffer
		args := append(append([]string{}, command...), "--json-schema=result")
		code := run(args, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("%s schema code=%d stderr=%q stdout=%q", strings.Join(command, " "), code, stderr.String(), stdout.String())
		}
		var schema map[string]any
		if err := json.Unmarshal(stdout.Bytes(), &schema); err != nil {
			t.Fatalf("%s schema not JSON: %v\n%s", strings.Join(command, " "), err, stdout.String())
		}
		assertSchemaDescriptions(t, strings.Join(command, " "), schema)
	}
}

func TestPackRegistryJSONUnsupportedPathUsesPlatformFailure(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"pack", "fetch", "--json"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("pack fetch --json code=0, want nonzero")
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr=%q, want empty for platform unsupported failure", stderr.String())
	}
	var payload struct {
		OK    bool `json:"ok"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("unsupported payload is not JSON: %v\n%s", err, stdout.String())
	}
	if payload.OK || payload.Error.Code != "json_unsupported" {
		t.Fatalf("payload=%+v, want json_unsupported failure", payload)
	}
}

func runPackRegistryJSONAndValidate(t *testing.T, args []string) {
	t.Helper()
	runPackRegistryJSONAndValidateExitCode(t, args, 0)
}

func runPackRegistryJSONAndValidateExitCode(t *testing.T, args []string, wantCode int) {
	t.Helper()
	command := commandFromArgs(args)
	var schemaStdout, schemaStderr bytes.Buffer
	schemaArgs := append(append([]string{}, command...), "--json-schema=result")
	code := run(schemaArgs, &schemaStdout, &schemaStderr)
	if code != 0 {
		t.Fatalf("%s schema code=%d stderr=%q stdout=%q", strings.Join(command, " "), code, schemaStderr.String(), schemaStdout.String())
	}
	if schemaStderr.Len() != 0 {
		t.Fatalf("%s schema stderr=%q, want empty", strings.Join(command, " "), schemaStderr.String())
	}
	schema := compileJSONSchema(t, "gc://schemas/"+strings.Join(command, "/")+"/result.schema.json", schemaStdout.Bytes())

	var stdout, stderr bytes.Buffer
	code = run(args, &stdout, &stderr)
	if code != wantCode {
		t.Fatalf("%s code=%d, want %d stderr=%q stdout=%q", strings.Join(args, " "), code, wantCode, stderr.String(), stdout.String())
	}
	if strings.Contains(stdout.String(), "warning:") || strings.Contains(stdout.String(), "gc pack registry") {
		t.Fatalf("%s stdout contains diagnostics: %q", strings.Join(args, " "), stdout.String())
	}
	var payload any
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("%s stdout is not JSON: %v\n%s", strings.Join(args, " "), err, stdout.String())
	}
	if err := schema.Validate(payload); err != nil {
		t.Fatalf("%s stdout does not match schema: %v\nschema=%s\npayload=%s", strings.Join(args, " "), err, schemaStdout.String(), stdout.String())
	}
}

func compileJSONSchema(t *testing.T, uri string, data []byte) *jsonschema.Schema {
	t.Helper()
	var schemaDoc any
	if err := json.Unmarshal(data, &schemaDoc); err != nil {
		t.Fatalf("schema is not JSON: %v\n%s", err, data)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(uri, schemaDoc); err != nil {
		t.Fatalf("add schema resource: %v", err)
	}
	schema, err := compiler.Compile(uri)
	if err != nil {
		t.Fatalf("compile schema: %v\n%s", err, data)
	}
	return schema
}

func commandFromArgs(args []string) []string {
	command := []string{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--json":
			continue
		case strings.HasPrefix(arg, "--"):
			if !strings.Contains(arg, "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
			}
		default:
			command = append(command, arg)
			if len(command) == 2 && command[0] == "pack" && command[1] != "registry" {
				return command
			}
			if len(command) == 3 {
				return command
			}
		}
	}
	return command
}

func assertSchemaDescriptions(t *testing.T, path string, schema map[string]any) {
	t.Helper()
	props, _ := schema["properties"].(map[string]any)
	for name, raw := range props {
		prop, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		desc, _ := prop["description"].(string)
		if strings.TrimSpace(desc) == "" {
			t.Fatalf("%s property %s missing description", path, name)
		}
		if items, ok := prop["items"].(map[string]any); ok {
			assertSchemaDescriptions(t, path+"."+name+"[]", items)
		}
	}
}

func TestPackRegistryJSONCommandUsesGCHomeIsolation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GC_HOME", home)
	catalogDir := writeRegistryCatalog(t, packRegistryTestCatalog)
	runPackRegistryJSONAndValidate(t, []string{"pack", "registry", "add", "isolated", catalogDir, "--json"})
	if _, err := os.Stat(filepath.Join(home, "registries.toml")); err != nil {
		t.Fatalf("registries.toml not written under GC_HOME: %v", err)
	}
}
