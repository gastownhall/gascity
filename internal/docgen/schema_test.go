package docgen

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/invopop/jsonschema"
)

// defProperties extracts the properties map for a named $defs entry.
func defProperties(t *testing.T, raw map[string]interface{}, defName string) map[string]interface{} {
	t.Helper()
	defs, ok := raw["$defs"].(map[string]interface{})
	if !ok {
		t.Fatal("no $defs")
	}
	def, ok := defs[defName].(map[string]interface{})
	if !ok {
		t.Fatalf("no %s definition in $defs", defName)
	}
	props, ok := def["properties"].(map[string]interface{})
	if !ok {
		t.Fatalf("%s has no properties", defName)
	}
	return props
}

func TestGenerateCitySchema(t *testing.T) {
	s, err := GenerateCitySchema()
	if err != nil {
		t.Fatalf("GenerateCitySchema: %v", err)
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("empty schema output")
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// City properties are in $defs.City (schema uses $ref at top level).
	props := defProperties(t, raw, "City")
	for _, expected := range []string{"workspace", "providers", "agent", "rigs"} {
		if _, ok := props[expected]; !ok {
			t.Errorf("missing City property %q", expected)
		}
	}
	// Should NOT have Go-style names.
	for _, bad := range []string{"Workspace", "Providers", "Agents"} {
		if _, ok := props[bad]; ok {
			t.Errorf("found Go-style property %q, expected TOML name", bad)
		}
	}
}

func TestCitySchemaDescriptions(t *testing.T) {
	s, err := GenerateCitySchema()
	if err != nil {
		t.Fatalf("GenerateCitySchema: %v", err)
	}

	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Check that Agent fields have description from doc comments.
	agentProps := defProperties(t, raw, "Agent")
	nameField, ok := agentProps["name"].(map[string]interface{})
	if !ok {
		t.Fatal("Agent name property not a map")
	}
	desc, ok := nameField["description"].(string)
	if !ok || desc == "" {
		t.Error("Agent.name has no description — AddGoComments may not be extracting comments")
	}
}

func TestCitySchemaCommandTemplateDescriptions(t *testing.T) {
	s, err := GenerateCitySchema()
	if err != nil {
		t.Fatalf("GenerateCitySchema: %v", err)
	}

	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	agentProps := defProperties(t, raw, "Agent")
	for field, want := range map[string]string{
		"scale_check": "Go template placeholders",
		"on_boot":     "Go template placeholders",
		"on_death":    "Go template placeholders",
		"work_query":  "Go template placeholders",
		"sling_query": "Go template placeholders",
	} {
		prop, ok := agentProps[field].(map[string]interface{})
		if !ok {
			t.Fatalf("Agent.%s property not a map", field)
		}
		desc, _ := prop["description"].(string)
		normalized := strings.Join(strings.Fields(desc), " ")
		if !strings.Contains(normalized, want) {
			t.Fatalf("Agent.%s description = %q, want substring %q", field, desc, want)
		}
		if !strings.Contains(normalized, "AgentBase") {
			t.Fatalf("Agent.%s description = %q, want PathContext fields surfaced", field, desc)
		}
	}
}

func TestCitySchemaAttachmentListFieldsRemainTombstones(t *testing.T) {
	s, err := GenerateCitySchema()
	if err != nil {
		t.Fatalf("GenerateCitySchema: %v", err)
	}

	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	check := func(defName string, fields ...string) {
		t.Helper()
		props := defProperties(t, raw, defName)
		for _, field := range fields {
			prop, ok := props[field].(map[string]interface{})
			if !ok {
				t.Fatalf("%s.%s property not a map", defName, field)
			}
			desc, _ := prop["description"].(string)
			if !strings.Contains(desc, "accepted but ignored") {
				t.Fatalf("%s.%s description = %q, want tombstone wording", defName, field, desc)
			}
		}
	}

	check("Agent", "skills", "mcp")
	check("AgentDefaults", "skills", "mcp")
	check("AgentOverride", "skills", "mcp", "skills_append", "mcp_append")
}

func TestCitySchemaOrderOverrideIncludesLegacyGateAlias(t *testing.T) {
	s, err := GenerateCitySchema()
	if err != nil {
		t.Fatalf("GenerateCitySchema: %v", err)
	}

	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	props := defProperties(t, raw, "OrderOverride")
	gateField, ok := props["gate"].(map[string]interface{})
	if !ok {
		t.Fatal("OrderOverride.gate property missing from schema")
	}
	if deprecated, ok := gateField["deprecated"].(bool); !ok || !deprecated {
		t.Fatalf("OrderOverride.gate deprecated = %v, want true", gateField["deprecated"])
	}
}

// TestCitySchemaCityAgentNotRequired guards against the regression where
// City.Agents was reflected as a required property because its TOML tag
// lacked omitempty. Real cities use [imports.*] (PackV2) and ship without
// any [[agent]] block; the schema must reflect that.
func TestCitySchemaCityAgentNotRequired(t *testing.T) {
	s, err := GenerateCitySchema()
	if err != nil {
		t.Fatalf("GenerateCitySchema: %v", err)
	}

	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	defs := raw["$defs"].(map[string]interface{})
	city := defs["City"].(map[string]interface{})
	required, _ := city["required"].([]interface{})
	for _, r := range required {
		if r == "agent" {
			t.Errorf("City.required includes %q; PackV2 cities ship without [[agent]] blocks — Agents needs omitempty", "agent")
		}
	}
}

func TestCitySchemaOmitsLegacyPackSourceSurface(t *testing.T) {
	s, err := GenerateCitySchema()
	if err != nil {
		t.Fatalf("GenerateCitySchema: %v", err)
	}

	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	cityProps := defProperties(t, raw, "City")
	if _, ok := cityProps["packs"]; ok {
		t.Fatal("City schema exposes legacy [packs.*] surface")
	}
	defs := raw["$defs"].(map[string]interface{})
	if _, ok := defs["PackSource"]; ok {
		t.Fatal("City schema exposes legacy PackSource ref/path surface")
	}
}

func TestPublicImportSchemaOnlyExposesSourceAndVersion(t *testing.T) {
	for _, tc := range []struct {
		name     string
		generate func() (interface{}, error)
	}{
		{name: "city", generate: func() (interface{}, error) { return GenerateCitySchema() }},
		{name: "pack", generate: func() (interface{}, error) { return GeneratePackSchema() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := tc.generate()
			if err != nil {
				t.Fatalf("generate schema: %v", err)
			}
			data, err := json.Marshal(s)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var raw map[string]interface{}
			if err := json.Unmarshal(data, &raw); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}

			props := defProperties(t, raw, "Import")
			for _, want := range []string{"source", "version"} {
				if _, ok := props[want]; !ok {
					t.Fatalf("Import schema missing public field %q in %v", want, props)
				}
			}
			for _, hidden := range []string{"export", "transitive", "shadow"} {
				if _, ok := props[hidden]; ok {
					t.Fatalf("Import schema exposes compatibility field %q in %v", hidden, props)
				}
			}
			if len(props) != 2 {
				t.Fatalf("Import schema properties = %v, want exactly source and version", props)
			}

			defs := raw["$defs"].(map[string]interface{})
			imp := defs["Import"].(map[string]interface{})
			required, _ := imp["required"].([]interface{})
			if len(required) != 1 || required[0] != "source" {
				t.Fatalf("Import.required = %v, want [source]", required)
			}
		})
	}
}

func TestGeneratePackSchema(t *testing.T) {
	s, err := GeneratePackSchema()
	if err != nil {
		t.Fatalf("GeneratePackSchema: %v", err)
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	props := defProperties(t, raw, "PackConfig")
	for _, expected := range []string{"pack", "imports", "agent", "providers", "service", "commands"} {
		if _, ok := props[expected]; !ok {
			t.Errorf("missing PackConfig property %q", expected)
		}
	}
}

func TestPackSchemaPackMetaRequired(t *testing.T) {
	s, err := GeneratePackSchema()
	if err != nil {
		t.Fatalf("GeneratePackSchema: %v", err)
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	defs := raw["$defs"].(map[string]interface{})
	pack := defs["PackConfig"].(map[string]interface{})
	required, _ := pack["required"].([]interface{})
	found := false
	for _, r := range required {
		if r == "pack" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("PackConfig.required = %v, want to include %q ([pack] block is mandatory in pack.toml)", required, "pack")
	}
}

func TestPackSchemaAliasFieldHidden(t *testing.T) {
	s, err := GeneratePackSchema()
	if err != nil {
		t.Fatalf("GeneratePackSchema: %v", err)
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	props := defProperties(t, raw, "PackConfig")
	if _, ok := props["agents"]; ok {
		t.Errorf("PackConfig should hide the legacy %q alias (jsonschema:\"-\") for agent_defaults", "agents")
	}
}

// TestAddGoCommentsFilteredSkipsHiddenDirs verifies that addGoCommentsFiltered
// does not enter directories whose name begins with ".". This guards against
// the TOCTOU failure where .gc/*/pr-checkout/ dirs are deleted by mpr cleanup
// while a schema-gen walk is in progress: if the hidden dir is unreadable or
// disappears mid-walk, a plain r.AddGoComments(".", ...) surfaces an I/O error;
// the filtered variant must skip hidden dirs entirely so no such error occurs.
func TestAddGoCommentsFilteredSkipsHiddenDirs(t *testing.T) {
	tmp := t.TempDir()

	// Visible source dir with a Go struct — should be processed normally.
	if err := os.MkdirAll(filepath.Join(tmp, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	goSrc := "package pkg\n\n// Widget is a widget.\ntype Widget struct{}\n"
	if err := os.WriteFile(filepath.Join(tmp, "pkg", "widget.go"), []byte(goSrc), 0o644); err != nil {
		t.Fatal(err)
	}

	// Hidden dir made unreadable: if walked it triggers "permission denied".
	gcDir := filepath.Join(tmp, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(gcDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(gcDir, 0o755) })

	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(orig) }()

	r := &jsonschema.Reflector{FieldNameTag: "toml"}
	if err := addGoCommentsFiltered(r, "example.com/test", "."); err != nil {
		t.Errorf("addGoCommentsFiltered failed with unreadable hidden dir: %v", err)
	}
}

// gitOK skips the test if git is not available in PATH.
func gitOK(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available in PATH")
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// TestAddGoCommentsFilteredSkipsUntrackedTopLevelDirs verifies that
// addGoCommentsFiltered only walks top-level directories known to git, so
// stray untracked directories (leaked worktree-stage dirs, abandoned
// PR-checkout dirs, etc. — see ga-vfurlv) are never walked. Without this,
// AddGoComments's recursive filepath.Walk + go/parser.ParseDir cost is
// multiplied by however many stray directories have accumulated at the
// module root, which is what caused schema-gen tests to time out under
// parallel load.
func TestAddGoCommentsFilteredSkipsUntrackedTopLevelDirs(t *testing.T) {
	gitOK(t)
	tmp := t.TempDir()

	runGit(t, tmp, "init", "-q", "-b", "main")
	runGit(t, tmp, "config", "user.email", "test@example.com")
	runGit(t, tmp, "config", "user.name", "test")
	runGit(t, tmp, "config", "commit.gpgsign", "false")

	// Tracked top-level dir — must be walked.
	if err := os.MkdirAll(filepath.Join(tmp, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	trackedSrc := "package pkg\n\n// TrackedWidget is committed to git.\ntype TrackedWidget struct{}\n"
	if err := os.WriteFile(filepath.Join(tmp, "pkg", "widget.go"), []byte(trackedSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, tmp, "add", "pkg")
	runGit(t, tmp, "commit", "-q", "-m", "add pkg")

	// Untracked stray top-level dir, mimicking a leaked worktree-stage or
	// abandoned PR-checkout directory. Must NOT be walked.
	strayDir := filepath.Join(tmp, "ga-leaked-worktree")
	if err := os.MkdirAll(strayDir, 0o755); err != nil {
		t.Fatal(err)
	}
	straySrc := "package stray\n\n// UntrackedGhost must never be walked.\ntype UntrackedGhost struct{}\n"
	if err := os.WriteFile(filepath.Join(strayDir, "ghost.go"), []byte(straySrc), 0o644); err != nil {
		t.Fatal(err)
	}

	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(orig) }()

	r := &jsonschema.Reflector{FieldNameTag: "toml"}
	if err := addGoCommentsFiltered(r, "example.com/test", "."); err != nil {
		t.Fatalf("addGoCommentsFiltered: %v", err)
	}

	if _, ok := r.CommentMap["example.com/test/pkg.TrackedWidget"]; !ok {
		t.Errorf("expected comment for tracked pkg.TrackedWidget, got map: %v", r.CommentMap)
	}
	for k := range r.CommentMap {
		if strings.Contains(k, "ga-leaked-worktree") {
			t.Errorf("addGoCommentsFiltered walked untracked top-level dir, found key %q", k)
		}
	}
}

func TestCitySchemaAgentDefinition(t *testing.T) {
	s, err := GenerateCitySchema()
	if err != nil {
		t.Fatalf("GenerateCitySchema: %v", err)
	}

	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	agentProps := defProperties(t, raw, "Agent")

	// Check expected fields exist.
	for _, field := range []string{"name", "dir", "prompt_template", "provider", "pre_start"} {
		if _, ok := agentProps[field]; !ok {
			t.Errorf("Agent missing field %q", field)
		}
	}

	// Check pre_start is an array type.
	ps, ok := agentProps["pre_start"].(map[string]interface{})
	if !ok {
		t.Fatal("pre_start property not a map")
	}
	if ps["type"] != "array" {
		t.Errorf("pre_start type: got %v, want array", ps["type"])
	}

	// Check name is required.
	defs := raw["$defs"].(map[string]interface{})
	agent := defs["Agent"].(map[string]interface{})
	required, ok := agent["required"].([]interface{})
	if !ok {
		t.Fatal("Agent missing required array")
	}
	found := false
	for _, r := range required {
		if r == "name" {
			found = true
			break
		}
	}
	if !found {
		t.Error("Agent 'name' not in required list")
	}
}
