package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestCrossStoreClaimDirRefusesUnadmittedActorOnGuardedRig(t *testing.T) {
	res, _ := rnsCityBResolver(t)
	cfg := hrgCity(t)
	cityAgent := &config.Agent{Name: "mayor", Scope: "city"}
	rigAgent := &config.Agent{Name: "crew.worker", Dir: "beta"}

	// A city-scoped agent is never inside the rig's namespace: refused,
	// before any redirection is resolved.
	_, ok, err := crossStoreClaimDir(cfg, cityAgent, "bt-1", "mayor", "", res)
	if ok || !errors.Is(err, errRigNamespace) {
		t.Fatalf("city agent: ok=%v err=%v, want a guard refusal", ok, err)
	}
	if !strings.Contains(err.Error(), `rig "beta" (federation = "rig") serves only beta/; claim actor "mayor" is outside it`) {
		t.Fatalf("refusal text = %q", err.Error())
	}
	// A rig agent claiming under a short id is refused too (no redirect is
	// involved, so this is not merely the cross-store path).
	if _, _, err := crossStoreClaimDir(cfg, rigAgent, "bt-1", shortIDB, "beta", res); !errors.Is(err, errRigNamespace) {
		t.Fatalf("short-id actor: err=%v, want a guard refusal", err)
	}
	// An admitted actor proceeds: the rig's own agent, or one of the
	// city's pool sessions; neither is redirected.
	for _, actor := range []string{"beta/crew.worker", poolIDB} {
		if _, ok, err := crossStoreClaimDir(cfg, rigAgent, "bt-1", actor, "beta", res); err != nil || ok {
			t.Fatalf("actor %q: ok=%v err=%v, want no redirect and no refusal", actor, ok, err)
		}
	}
	// An ambiguous guarded prefix admits nobody.
	if _, _, err := crossStoreClaimDir(cfg, rigAgent, "zz-1", "gamma/crew.worker", "beta", res); !errors.Is(err, errRigNamespace) {
		t.Fatalf("ambiguous prefix: err=%v, want a guard refusal", err)
	}
	// An unguarded rig keeps the cross-store redirect.
	if dir, ok, err := crossStoreClaimDir(cfg, cityAgent, "al-1", "mayor", "", res); err != nil || !ok || dir != hrgDir(cfg, "alpha") {
		t.Fatalf("unguarded rig: dir=%q ok=%v err=%v", dir, ok, err)
	}
}

func TestAgentScriptBdClaimRefusalRunsNoBd(t *testing.T) {
	ran := 0
	exec := agentScriptExecutor{
		runCommand: func(string, ...string) error {
			ran++
			return nil
		},
		runCommandInStore: func(string, []string, string, ...string) error {
			ran++
			return nil
		},
		resolveBeadStore: func(string, []string) (string, []string, bool, error) {
			return "", nil, false, newRigNamespacePathError(`rig "beta" (federation = "rig") serves only beta/; claim actor "mayor" is outside it`)
		},
	}
	_, err := exec.runAction(agentScriptAction{"bd_claim": "bt-1"}, agentScriptContext{})
	if err == nil || ran != 0 {
		t.Fatalf("err=%v ran=%d, want a refusal and no bd call", err, ran)
	}
	if !strings.HasPrefix(err.Error(), `bd_claim bt-1: rig "beta" (federation = "rig") serves only beta/; claim actor "mayor" is outside it`) {
		t.Fatalf("error = %q", err.Error())
	}
	_, err = exec.runAction(agentScriptAction{"bd_update": map[string]any{"id": "bt-1", "status": "in_progress"}}, agentScriptContext{})
	if err == nil || ran != 0 || !strings.HasPrefix(err.Error(), "bd_update bt-1: ") {
		t.Fatalf("bd_update: err=%v ran=%d, want a refusal and no bd call", err, ran)
	}
}

func TestRunAgentScriptRefusalExitsOne(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "script.yaml")
	if err := os.WriteFile(path, []byte("schema_version: v1\nturns:\n  - when: {hook: has_work}\n    do:\n      - bd_claim: bt-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	exec := agentScriptExecutor{
		runCommand: func(string, ...string) error {
			t.Fatal("bd ran despite the refusal")
			return nil
		},
		resolveBeadStore: func(string, []string) (string, []string, bool, error) {
			return "", nil, false, newRigNamespacePathError("refused")
		},
	}
	var stdout, stderr strings.Builder
	hook := func(io.Writer) (agentScriptBead, bool, error) {
		return agentScriptBead{ID: "bt-1"}, true, nil
	}
	code := runAgentScriptWithRuntime(path, &stdout, &stderr, exec, hook)
	if code != 1 || !strings.Contains(stderr.String(), "gc agent-script: bd_claim bt-1: refused") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}
