package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	sessionexec "github.com/gastownhall/gascity/internal/runtime/exec"
	sessiontmux "github.com/gastownhall/gascity/internal/runtime/tmux"
)

// builtinRuntimeNames is the selection-name contract documented in
// internal/runtime/REQUIREMENTS.md (RUNTIME-SEL-002). Removing a name is
// a breaking change to city configs; update the ledger row with this list.
var builtinRuntimeNames = []string{
	"fake", "fail", "subprocess", "acp", "t3bridge", "cloudflare", "k8s", "hybrid",
}

func TestRuntimeRegistryRegistersAllBuiltinNames(t *testing.T) {
	r := buildRuntimeRegistry()
	for _, name := range builtinRuntimeNames {
		if !r.Has(name) {
			t.Errorf("builtin runtime %q not registered", name)
		}
	}
}

func TestNewSessionProviderByName_UnknownNameFallsBackToTmux(t *testing.T) {
	sp, err := newSessionProviderByName("definitely-not-a-runtime", config.SessionConfig{}, "city", t.TempDir())
	if err != nil {
		t.Fatalf("newSessionProviderByName(unknown): %v", err)
	}
	if _, ok := sp.(*sessiontmux.Provider); !ok {
		t.Fatalf("provider type = %T, want *tmux.Provider (documented fallback)", sp)
	}
}

func TestNewSessionProviderByName_ExecPrefixUsesExecProvider(t *testing.T) {
	sp, err := newSessionProviderByName("exec:/usr/local/bin/gc-session-screen", config.SessionConfig{}, "city", t.TempDir())
	if err != nil {
		t.Fatalf("newSessionProviderByName(exec:...): %v", err)
	}
	if _, ok := sp.(*sessionexec.Provider); !ok {
		t.Fatalf("provider type = %T, want *exec.Provider", sp)
	}
}
