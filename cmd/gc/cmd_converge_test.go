package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/convergence"
)

func TestConvergeCreateGateTimeoutDefaultMatchesSharedDefault(t *testing.T) {
	cmd := newConvergeCreateCmd(io.Discard, io.Discard)
	flag := cmd.Flags().Lookup("gate-timeout")
	if flag == nil {
		t.Fatal("gate-timeout flag not found")
	}

	want := convergence.DefaultGateTimeout.String()
	if flag.DefValue != want {
		t.Fatalf("gate-timeout default = %q, want %q", flag.DefValue, want)
	}
	if got := flag.Value.String(); got != want {
		t.Fatalf("gate-timeout bound value = %q, want %q", got, want)
	}
}

func TestOpenContextStore_RigErrors(t *testing.T) {
	cityDir := t.TempDir()
	cityToml := "[workspace]\nname = \"convtest\"\n\n" +
		"[beads]\nprovider = \"file\"\n\n" +
		"[session]\nprovider = \"fake\"\n\n" +
		"[[rigs]]\nname = \"unbound-rig\"\n"
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	t.Run("unknown rig", func(t *testing.T) {
		_, err := openContextStore(resolvedContext{CityPath: cityDir, RigName: "ghost-rig"})
		if err == nil {
			t.Fatal("expected error for an unregistered rig")
		}
		if !strings.Contains(err.Error(), "ghost-rig") || !strings.Contains(err.Error(), "not registered") {
			t.Errorf("error = %q, want it to name the rig and say it is not registered", err)
		}
	})

	t.Run("unbound rig", func(t *testing.T) {
		_, err := openContextStore(resolvedContext{CityPath: cityDir, RigName: "unbound-rig"})
		if err == nil {
			t.Fatal("expected error for a registered but unbound rig")
		}
		if !strings.Contains(err.Error(), "unbound-rig") || !strings.Contains(err.Error(), "no rig path") {
			t.Errorf("error = %q, want it to name the rig and mention the missing path", err)
		}
	})
}
