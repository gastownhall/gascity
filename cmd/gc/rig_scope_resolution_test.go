package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestResolveRigScopeFromFlagOrCwd(t *testing.T) {
	cityPath := t.TempDir()
	rigA := filepath.Join(cityPath, "rig-a")
	rigB := filepath.Join(cityPath, "rig-b")
	for _, d := range []string{rigA, rigB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs: []config.Rig{
			{Name: "rig-a", Path: rigA},
			{Name: "rig-b", Path: rigB},
			{Name: "unbound"}, // declared but no path binding
		},
	}

	t.Run("explicit flag selects named rig", func(t *testing.T) {
		t.Chdir(cityPath)
		rig, err := resolveRigScopeFromFlagOrCwd(cfg, cityPath, "rig-a")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if rig == nil || rig.Name != "rig-a" {
			t.Fatalf("rig = %+v, want rig-a", rig)
		}
	})

	t.Run("explicit flag wins over cwd", func(t *testing.T) {
		t.Chdir(rigA)
		rig, err := resolveRigScopeFromFlagOrCwd(cfg, cityPath, "rig-b")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if rig == nil || rig.Name != "rig-b" {
			t.Fatalf("rig = %+v, want rig-b (flag should override cwd)", rig)
		}
	})

	t.Run("unknown rig flag returns error", func(t *testing.T) {
		t.Chdir(cityPath)
		rig, err := resolveRigScopeFromFlagOrCwd(cfg, cityPath, "nope")
		if err == nil {
			t.Fatalf("want error, got rig=%+v", rig)
		}
		if !strings.Contains(err.Error(), `"nope" not found`) {
			t.Fatalf("error = %v, want 'not found' for unknown rig", err)
		}
	})

	t.Run("unbound rig flag returns error", func(t *testing.T) {
		t.Chdir(cityPath)
		rig, err := resolveRigScopeFromFlagOrCwd(cfg, cityPath, "unbound")
		if err == nil {
			t.Fatalf("want error, got rig=%+v", rig)
		}
		if !strings.Contains(err.Error(), "no path binding") {
			t.Fatalf("error = %v, want 'no path binding' guidance", err)
		}
	})

	t.Run("cwd in rig dir resolves to that rig", func(t *testing.T) {
		t.Chdir(rigB)
		rig, err := resolveRigScopeFromFlagOrCwd(cfg, cityPath, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if rig == nil || rig.Name != "rig-b" {
			t.Fatalf("rig = %+v, want rig-b from cwd", rig)
		}
	})

	t.Run("cwd outside rigs returns nil (city scope)", func(t *testing.T) {
		t.Chdir(cityPath)
		rig, err := resolveRigScopeFromFlagOrCwd(cfg, cityPath, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if rig != nil {
			t.Fatalf("rig = %+v, want nil (city scope)", rig)
		}
	})

	t.Run("whitespace-only flag treated as no flag", func(t *testing.T) {
		t.Chdir(cityPath)
		rig, err := resolveRigScopeFromFlagOrCwd(cfg, cityPath, "   ")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if rig != nil {
			t.Fatalf("rig = %+v, want nil (whitespace flag should not select a rig)", rig)
		}
	})
}
