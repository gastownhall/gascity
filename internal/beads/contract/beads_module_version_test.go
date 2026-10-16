package contract

import (
	"runtime/debug"
	"testing"
)

func TestBeadsModuleVersionReadsMainAndDep(t *testing.T) {
	const pin = "v1.1.1-0.20260805093327-bf97b73749ac"

	bdStyle := &debug.BuildInfo{Main: debug.Module{Path: beadsModulePath, Version: pin}}
	if got := BeadsModuleVersion(bdStyle); got != pin {
		t.Fatalf("bd-style main pin = %q, want %q", got, pin)
	}

	gcStyle := &debug.BuildInfo{
		Main: debug.Module{Path: "github.com/gastownhall/gascity", Version: "v0.0.0"},
		Deps: []*debug.Module{{Path: beadsModulePath, Version: pin}},
	}
	if got := BeadsModuleVersion(gcStyle); got != pin {
		t.Fatalf("gc-style dep pin = %q, want %q", got, pin)
	}

	replaced := &debug.BuildInfo{
		Main: debug.Module{Path: "bdinstall"},
		Deps: []*debug.Module{{
			Path:    beadsModulePath,
			Version: "v1.0.0",
			Replace: &debug.Module{Path: "github.com/fork/beads", Version: pin},
		}},
	}
	if got := BeadsModuleVersion(replaced); got != pin {
		t.Fatalf("replaced dep pin = %q, want replacement version %q", got, pin)
	}

	devel := &debug.BuildInfo{Main: debug.Module{Path: beadsModulePath, Version: "(devel)"}}
	if got := BeadsModuleVersion(devel); got != "" {
		t.Fatalf("untagged local bd build pin = %q, want empty (version unknown)", got)
	}

	if got := BeadsModuleVersion(&debug.BuildInfo{}); got != "" {
		t.Fatalf("empty build info pin = %q, want empty", got)
	}
	if got := BeadsModuleVersion(nil); got != "" {
		t.Fatalf("nil build info pin = %q, want empty", got)
	}
}
