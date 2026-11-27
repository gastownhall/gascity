package config

import (
	"strings"
	"testing"
)

func TestParseRigDefaultMergeStrategy(t *testing.T) {
	data := []byte(`
[workspace]
name = "bright-lights"

[[rigs]]
name = "gascity"
path = "/repos/gascity"
default_branch = "develop"
default_merge_strategy = "mr"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Rigs) != 1 {
		t.Fatalf("got %d rigs, want 1", len(cfg.Rigs))
	}
	if got := cfg.Rigs[0].DefaultMergeStrategy; got != "mr" {
		t.Errorf("DefaultMergeStrategy = %q, want %q", got, "mr")
	}
	if got := cfg.Rigs[0].EffectiveDefaultMergeStrategy(); got != "mr" {
		t.Errorf("EffectiveDefaultMergeStrategy = %q, want %q", got, "mr")
	}
}

func TestEffectiveDefaultMergeStrategy_EmptyWhenUnset(t *testing.T) {
	r := Rig{Name: "rig"}
	if got := r.EffectiveDefaultMergeStrategy(); got != "" {
		t.Errorf("EffectiveDefaultMergeStrategy() = %q, want empty", got)
	}
}

func TestEffectiveDefaultMergeStrategy_TrimsWhitespace(t *testing.T) {
	r := Rig{Name: "rig", DefaultMergeStrategy: "  mr\n"}
	if got := r.EffectiveDefaultMergeStrategy(); got != "mr" {
		t.Errorf("EffectiveDefaultMergeStrategy() = %q, want %q", got, "mr")
	}
}

func TestValidateRigs_AcceptsKnownDefaultMergeStrategies(t *testing.T) {
	for _, strategy := range []string{"", "direct", "mr", "local", " mr "} {
		rigs := []Rig{{Name: "gascity", Path: "/repos/gascity", Prefix: "ga", DefaultMergeStrategy: strategy}}
		if err := ValidateRigs(rigs, "hq"); err != nil {
			t.Errorf("ValidateRigs(default_merge_strategy=%q) = %v, want nil", strategy, err)
		}
	}
}

func TestValidateRigs_RejectsUnknownDefaultMergeStrategy(t *testing.T) {
	rigs := []Rig{{Name: "gascity", Path: "/repos/gascity", Prefix: "ga", DefaultMergeStrategy: "rebase"}}
	err := ValidateRigs(rigs, "hq")
	if err == nil {
		t.Fatal("ValidateRigs(default_merge_strategy=\"rebase\") = nil, want error")
	}
	// The message must name the rig and the accepted values so an operator can
	// fix city.toml without reading the source.
	for _, want := range []string{"gascity", "rebase", "direct", "mr", "local"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ValidateRigs error %q does not mention %q", err, want)
		}
	}
}

func TestApplyRigPatch_SetsDefaultMergeStrategy(t *testing.T) {
	cfg := &City{Rigs: []Rig{{Name: "gascity", Path: "/repos/gascity"}}}
	strategy := "mr"
	if err := applyRigPatch(cfg, &RigPatch{Name: "gascity", DefaultMergeStrategy: &strategy}); err != nil {
		t.Fatalf("applyRigPatch: %v", err)
	}
	if got := cfg.Rigs[0].DefaultMergeStrategy; got != "mr" {
		t.Errorf("DefaultMergeStrategy = %q, want %q", got, "mr")
	}
}

func TestApplyRigPatch_LeavesDefaultMergeStrategyWhenUnset(t *testing.T) {
	cfg := &City{Rigs: []Rig{{Name: "gascity", Path: "/repos/gascity", DefaultMergeStrategy: "mr"}}}
	if err := applyRigPatch(cfg, &RigPatch{Name: "gascity"}); err != nil {
		t.Fatalf("applyRigPatch: %v", err)
	}
	if got := cfg.Rigs[0].DefaultMergeStrategy; got != "mr" {
		t.Errorf("DefaultMergeStrategy = %q, want the pre-patch %q", got, "mr")
	}
}

func TestApplyRigPatch_ClearsDefaultMergeStrategy(t *testing.T) {
	cfg := &City{Rigs: []Rig{{Name: "gascity", Path: "/repos/gascity", DefaultMergeStrategy: "mr"}}}
	cleared := ""
	if err := applyRigPatch(cfg, &RigPatch{Name: "gascity", DefaultMergeStrategy: &cleared}); err != nil {
		t.Fatalf("applyRigPatch: %v", err)
	}
	if got := cfg.Rigs[0].DefaultMergeStrategy; got != "" {
		t.Errorf("DefaultMergeStrategy = %q, want empty after explicit clear", got)
	}
}

func TestApplyRigPatch_SetsAndClearsUnroutedWorkTarget(t *testing.T) {
	cfg := &City{Rigs: []Rig{{Name: "hello-world", Path: "/repos/hw"}}}
	target := "hello-world/researcher"
	if err := applyRigPatch(cfg, &RigPatch{Name: "hello-world", UnroutedWorkTarget: &target}); err != nil {
		t.Fatalf("applyRigPatch(set): %v", err)
	}
	if got := cfg.Rigs[0].UnroutedWorkTarget; got != target {
		t.Fatalf("UnroutedWorkTarget = %q, want %q", got, target)
	}
	if err := applyRigPatch(cfg, &RigPatch{Name: "hello-world"}); err != nil {
		t.Fatalf("applyRigPatch(unset): %v", err)
	}
	if got := cfg.Rigs[0].UnroutedWorkTarget; got != target {
		t.Fatalf("UnroutedWorkTarget = %q after a patch that leaves it unset, want %q", got, target)
	}
	cleared := ""
	if err := applyRigPatch(cfg, &RigPatch{Name: "hello-world", UnroutedWorkTarget: &cleared}); err != nil {
		t.Fatalf("applyRigPatch(clear): %v", err)
	}
	if got := cfg.Rigs[0].UnroutedWorkTarget; got != "" {
		t.Fatalf("UnroutedWorkTarget = %q, want empty after explicit clear", got)
	}
}
