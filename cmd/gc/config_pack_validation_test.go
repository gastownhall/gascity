package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigValidatePackReceivesInvocation(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("GC_CITY_PATH", dir)
	put := func(name, data string) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	put("city.toml", `[workspace]
name = "demo"
[imports.tools]
source = "./tools"
[[agent]]
name = "worker"
env = { ORIGINAL = "preserved" }
[[rigs]]
name = "alpha"
path = "./alpha"
[rigs.formula_vars]
command = ""
[[rigs]]
name = "beta"
path = "./beta"
[rigs.formula_vars]
command = "{{unresolved}}"
[rigs.imports.beta]
source = "./beta-tools"
`)
	put("tools/pack.toml", `[pack]
name = "tools"
schema = 1
[[doctor]]
name = "plan"
script = "check.sh"
config_validate = true
config_validate_formulas = ["arbitrary-plan", "absent-plan"]
`)
	put("tools/check.sh", `#!/bin/sh
if [ "${GC_CONFIG_VALIDATION:-}" != 1 ]; then exit 0; fi
cat > "$GC_CITY_PATH/input.json"
printf 'beta: unresolved command\nset rigs.beta.formula_vars.command\n'
exit 2
`)
	parent := "formula = \"base\"\n[vars.command]\ndefault = \"pinned default\"\n[vars.optional]\ndefault = \"\"\n[[steps]]\nid = \"work\"\ntitle = \"Work\"\n"
	child := "formula = \"arbitrary-plan\"\nextends = [\"base\"]\n[vars.required]\nrequired = true\n"
	put("tools/formulas/base.formula.toml", parent)
	put("tools/formulas/arbitrary-plan.formula.toml", child)
	betaParent := strings.ReplaceAll(parent, "pinned default", "beta pinned default")
	put("beta-tools/pack.toml", "[pack]\nname = \"beta-tools\"\nschema = 1\n")
	put("beta-tools/formulas/base.formula.toml", betaParent)
	// Pin the compiler source independently of subsequent working-tree edits.
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=Fixture", "-c", "user.email=fixture@example.com", "commit", "-qm", "fixture"}, {"tag", "pinned-input"}} {
		runGit(t, dir, args...)
	}
	t.Setenv("GC_FORMULA_REF", "pinned-input")
	put("tools/formulas/base.formula.toml", strings.ReplaceAll(parent, "pinned default", "uncommitted default"))
	put("overlay.toml", `[[patches.agent]]
name = "worker"
env = { ADDED = "yes" }
`)
	for _, overlay := range []bool{false, true} {
		args := []string{"config", "show", "--validate", "--json"}
		if overlay {
			args = append(args, "-f", "overlay.toml")
		}
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 1 {
			t.Fatalf("validate = %d, want 1: %s %s", code, &stdout, &stderr)
		}
		if !strings.Contains(stdout.String(), "beta: unresolved command") || !strings.Contains(stdout.String(), "set rigs.beta") {
			t.Fatalf("missing actionable diagnostics: %s stderr: %s", &stdout, &stderr)
		}
		validateConfigShowJSONSchema(t, stdout.Bytes())
		data, err := os.ReadFile(filepath.Join(dir, "input.json"))
		if err != nil {
			t.Fatal(err)
		}
		var input struct {
			SchemaVersion string `json:"schema_version"`
			Config        struct {
				Agents []struct {
					Name string
					Env  map[string]string
				}
				Rigs []struct {
					Name        string
					FormulaVars map[string]string
				}
			} `json:"config"`
			Formulas map[string]map[string]struct {
				Name  string `json:"name"`
				Error string `json:"error"`
				Vars  []struct {
					Name    string  `json:"name"`
					Default *string `json:"default"`
				} `json:"vars"`
				Provenance struct {
					Status  string `json:"status"`
					Sources []struct {
						Path string `json:"path"`
						Hash string `json:"content_sha256"`
					} `json:"sources"`
				} `json:"provenance"`
			} `json:"formulas"`
		}
		if err := json.Unmarshal(data, &input); err != nil {
			t.Fatal(err)
		}
		if input.SchemaVersion != "1" || len(input.Config.Rigs) != 2 || len(input.Formulas) != 2 {
			t.Fatalf("input = %s", data)
		}
		env := input.Config.Agents[0].Env
		if env["ORIGINAL"] != "preserved" || (overlay && env["ADDED"] != "yes") {
			t.Fatalf("env = %v", env)
		}
		if v, ok := input.Config.Rigs[0].FormulaVars["command"]; !ok || v != "" {
			t.Fatalf("explicit empty lost: %s", data)
		}
		if input.Config.Rigs[1].FormulaVars["command"] != "{{unresolved}}" {
			t.Fatal("unresolved value hidden")
		}
		for _, rig := range []string{"alpha", "beta"} {
			got := input.Formulas[rig]["arbitrary-plan"]
			if got.Error != "" || got.Name != "arbitrary-plan" || got.Provenance.Status != "complete" {
				t.Fatalf("%s formula: %+v", rig, got)
			}
			defaults := map[string]*string{}
			for _, v := range got.Vars {
				defaults[v.Name] = v.Default
			}
			wantDefault := "pinned default"
			if rig == "beta" {
				wantDefault = "beta pinned default"
			}
			if defaults["command"] == nil || *defaults["command"] != wantDefault || defaults["optional"] == nil || *defaults["optional"] != "" || defaults["required"] != nil {
				t.Fatalf("vars: %+v", got.Vars)
			}
			want := map[string]string{}
			for name, content := range map[string]string{"base": parent, "arbitrary-plan": child} {
				baseDir := "tools/formulas"
				if name == "base" && rig == "beta" {
					baseDir = "beta-tools/formulas"
					content = betaParent
				}
				h := sha256.Sum256([]byte(content))
				want[filepath.Join(dir, baseDir, name+".formula.toml")] = hex.EncodeToString(h[:])
			}
			if len(got.Provenance.Sources) != len(want) {
				t.Fatalf("sources: %+v", got.Provenance.Sources)
			}
			for _, src := range got.Provenance.Sources {
				if want[src.Path] != src.Hash {
					t.Fatalf("source must hash actual pinned bytes: %+v", src)
				}
			}
			if msg := input.Formulas[rig]["absent-plan"].Error; !strings.Contains(msg, "absent-plan") {
				t.Fatalf("missing per-rig discovery error: %q", msg)
			}
		}
	}
	// Ordinary show never runs the opted-in script, even with an ambient marker.
	if err := os.Remove(filepath.Join(dir, "input.json")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CONFIG_VALIDATION", "1")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"config", "show", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("show: %d %s", code, &stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "input.json")); !os.IsNotExist(err) {
		t.Fatalf("ordinary show invoked validator: %v", err)
	}
}

func TestConfigValidatePackDiagnosticOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, requests, message string
		status, want            int
		json                    bool
	}{
		{"empty-requests", "", "alpha: valid optional blanks", 0, 0, true},
		{"irrelevant-compile-error", "config_validate_formulas = [\"unused-formula\"]", "alpha: unused formula ignored by pack", 0, 0, true},
		{"warning", "", "alpha: supported instruction fallback", 1, 0, true},
		{"error-json", "", "alpha: missing instructions", 2, 1, true},
		{"error-text", "", "alpha: unresolved executable value", 3, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearGCEnv(t)
			dir := t.TempDir()
			t.Chdir(dir)
			t.Setenv("GC_CITY_PATH", dir)
			put := func(name, content string) {
				t.Helper()
				p := filepath.Join(dir, name)
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			put("city.toml", "[workspace]\nname = \"demo\"\n[[rigs]]\nname = \"alpha\"\npath = \"./alpha\"\n")
			put("pack.toml", "[pack]\nname = \"demo\"\nschema = 1\n")
			put("doctor/plan/doctor.toml", "config_validate = true\n"+tc.requests)
			put("doctor/plan/run.sh", fmt.Sprintf("#!/bin/sh\ncat > \"$GC_CITY_PATH/input.json\"\nprintf '%%s\\n' '%s' 'actionable detail'\nexit %d\n", tc.message, tc.status))
			put("doctor/plan/fix.sh", "#!/bin/sh\ntouch \"$GC_CITY_PATH/FIX_RAN\"\n")
			put("doctor/ordinary/run.sh", "#!/bin/sh\ntouch \"$GC_CITY_PATH/ORDINARY_RAN\"\nexit 2\n")
			var stdout, stderr bytes.Buffer
			args := []string{"config", "show", "--validate"}
			if tc.json {
				args = append(args, "--json")
			}
			if got := run(args, &stdout, &stderr); got != tc.want {
				t.Fatalf("exit=%d want=%d: %s %s", got, tc.want, &stdout, &stderr)
			}
			output := stdout.String() + stderr.String()
			if tc.status != 0 && (!strings.Contains(output, tc.message) || !strings.Contains(output, "actionable detail")) {
				t.Fatalf("missing diagnostics: %s", output)
			}
			if tc.json {
				validateConfigShowJSONSchema(t, stdout.Bytes())
				var result struct {
					Validation struct {
						PackChecks []struct {
							Status string `json:"status"`
						} `json:"pack_checks"`
					} `json:"validation"`
				}
				if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				want := "ok"
				if tc.status == 1 {
					want = "warning"
				}
				if tc.status > 1 {
					want = "error"
				}
				if len(result.Validation.PackChecks) != 1 || result.Validation.PackChecks[0].Status != want {
					t.Fatalf("typed results = %+v", result)
				}
			}
			input, err := os.ReadFile(filepath.Join(dir, "input.json"))
			if err != nil {
				t.Fatal(err)
			}
			var payload struct {
				Config struct {
					Agents []json.RawMessage
					Rigs   []json.RawMessage
				}
				Formulas map[string]map[string]json.RawMessage
			}
			if err := json.Unmarshal(input, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Config.Agents == nil || len(payload.Config.Agents) != 0 || len(payload.Config.Rigs) != 1 || payload.Formulas["alpha"] == nil {
				t.Fatalf("required collections: %s", input)
			}
			for _, name := range []string{"FIX_RAN", "ORDINARY_RAN"} {
				if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
					t.Fatalf("explicit validation ran %s", name)
				}
			}
		})
	}
}
