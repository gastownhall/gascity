package core

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestSoftwareFactoryFormulaCarriesIndependentGates(t *testing.T) {
	data, err := fs.ReadFile(PackFS, "formulas/mol-software-factory.toml")
	if err != nil {
		t.Fatalf("read software-factory formula: %v", err)
	}
	var parsed formulaFile
	if _, err := toml.Decode(string(data), &parsed); err != nil {
		t.Fatalf("decode software-factory formula: %v", err)
	}
	if parsed.Formula != "mol-software-factory" {
		t.Fatalf("formula = %q", parsed.Formula)
	}
	for _, step := range []string{
		"collect-signals", "bounded-lookback", "human-digest", "implement-candidate",
		"independent-review", "factory-gate", "deliver-authorized-change",
	} {
		if !strings.Contains(string(data), `id = "`+step+`"`) {
			t.Errorf("formula missing step %q", step)
		}
	}
	if !strings.Contains(string(data), "fail closed if it equals the rig root") {
		t.Error("implementation step does not enforce rig-root isolation")
	}
	for _, action := range []string{"publish", "approve", "merge", "deploy", "close", "reply"} {
		if !strings.Contains(string(data), action) {
			t.Errorf("formula does not preserve %s delivery gate", action)
		}
	}
}

func TestSoftwareFactorySkillIsEmbedded(t *testing.T) {
	data, err := fs.ReadFile(PackFS, "skills/gc-factory/SKILL.md")
	if err != nil {
		t.Fatalf("read gc-factory skill: %v", err)
	}
	text := string(data)
	for _, want := range []string{".agent-factory/config.yaml", "gc worktree verify", "independent", "Publish, approve, merge, deploy"} {
		if !strings.Contains(text, want) {
			t.Errorf("gc-factory skill missing %q", want)
		}
	}
}
