package formula

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestInputLifecycleContract(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"base":     "input_lifecycle = 'graph'\n[[steps]]\nid = 'work'\ntitle = 'work'\n",
		"external": "input_lifecycle = 'external'\n[[steps]]\nid = 'other'\ntitle = 'other'\n",
		"child":    "extends = ['base']\n",
		"override": "extends = ['base']\ninput_lifecycle = 'external'\n",
		"first":    "extends = ['base', 'external']\n",
		"absent":   "[[steps]]\nid = 'work'\ntitle = 'work'\n",
		"invalid":  "input_lifecycle = 'grahp'\n[[steps]]\nid = 'work'\ntitle = 'work'\n",
	} {
		writeNamedFormula(t, dir, name, "formula = '"+name+"'\ncontract = 'graph.v2'\n"+body)
	}
	for name, want := range map[string]string{"base": "graph", "child": "graph", "override": "external", "first": "graph", "external": "external", "absent": ""} {
		t.Run(name, func(t *testing.T) {
			recipe, err := Compile(context.Background(), name, []string{dir}, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, step := range recipe.Steps {
				got := step.Metadata["gc.input_lifecycle"]
				if step.IsRoot {
					if got != want {
						t.Errorf("root policy=%q want=%q", got, want)
					}
				} else if got != "" {
					t.Errorf("non-root %s policy=%q", step.ID, got)
				}
			}
			if _, present := recipe.Steps[0].Metadata["gc.input_lifecycle"]; name == "absent" && present {
				t.Errorf("undeclared metadata changed: %v", recipe.Steps[0].Metadata)
			}
		})
	}
	if _, err := Compile(context.Background(), "invalid", []string{dir}, nil); err == nil {
		t.Fatal("unknown policy accepted")
	}
	var f Formula
	if err := json.Unmarshal([]byte(`{"formula":"json","input_lifecycle":"graph","steps":[{"id":"work","title":"work"}]}`), &f); err != nil {
		t.Fatal(err)
	}
	r, err := toRecipeWithGraph(&f, true)
	if err != nil || r.Steps[0].Metadata["gc.input_lifecycle"] != "graph" {
		t.Fatalf("JSON policy missing: %+v %v", r, err)
	}
}

func TestInputLifecycleValidation(t *testing.T) {
	for _, policy := range []string{"graph", "external", "grahp"} {
		t.Run(policy, func(t *testing.T) {
			f := &Formula{InputLifecycle: policy}
			err := f.Validate()
			if err == nil || !strings.Contains(err.Error(), "input_lifecycle") || !strings.Contains(err.Error(), "name is required") {
				t.Fatalf("v1 or aggregate validation policy=%s: %v", policy, err)
			}
		})
	}
}

func TestInputLifecycleCompilerRequirement(t *testing.T) {
	dir := t.TempDir()
	writeNamedFormula(t, dir, "guarded", "formula = 'guarded'\ninput_lifecycle = 'graph'\n[requires]\nformula_compiler = '>=2.1.0'\n[[steps]]\nid = 'work'\ntitle = 'work'\n")
	if _, err := Compile(context.Background(), "guarded", []string{dir}, nil); err != nil {
		t.Fatal(err)
	}
}
