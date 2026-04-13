package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestDesiredSessionProviderForAgentResolvesCityRelativeExecPath(t *testing.T) {
	cityDir := t.TempDir()
	script := writeExecutable(t, filepath.Join(cityDir, "scripts", "worker.sh"))
	agent := config.Agent{Name: "worker", Session: "exec:scripts/worker.sh"}

	provider, profile, err := desiredSessionProviderForAgent(&agent, cityDir, "tmux")
	if err != nil {
		t.Fatalf("desiredSessionProviderForAgent: %v", err)
	}
	if provider != "exec:"+script {
		t.Fatalf("provider = %q, want exec:%s", provider, script)
	}
	if profile != remoteWorkerProfile {
		t.Fatalf("profile = %q, want %q", profile, remoteWorkerProfile)
	}
}

func TestDesiredSessionProviderForAgentResolvesPackRelativeExecPath(t *testing.T) {
	cityDir := t.TempDir()
	packDir := filepath.Join(cityDir, "packs", "remote")
	script := writeExecutable(t, filepath.Join(packDir, "bin", "worker.sh"))
	agent := config.Agent{
		Name:      "worker",
		Session:   "exec:bin/worker.sh",
		SourceDir: packDir,
	}

	provider, _, err := desiredSessionProviderForAgent(&agent, cityDir, "tmux")
	if err != nil {
		t.Fatalf("desiredSessionProviderForAgent: %v", err)
	}
	if provider != "exec:"+script {
		t.Fatalf("provider = %q, want exec:%s", provider, script)
	}
}

func TestDesiredSessionProviderForAgentRejectsRelativeTraversal(t *testing.T) {
	cityDir := t.TempDir()
	outside := writeExecutable(t, filepath.Join(t.TempDir(), "worker.sh"))
	rel, err := filepath.Rel(cityDir, outside)
	if err != nil {
		t.Fatalf("Rel: %v", err)
	}
	agent := config.Agent{Name: "worker", Session: "exec:" + rel}

	if _, _, err := desiredSessionProviderForAgent(&agent, cityDir, "tmux"); err == nil {
		t.Fatal("desiredSessionProviderForAgent error = nil, want traversal error")
	}
}

func TestDesiredSessionProviderForAgentRejectsNonExecutable(t *testing.T) {
	cityDir := t.TempDir()
	script := filepath.Join(cityDir, "worker.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	agent := config.Agent{Name: "worker", Session: "exec:worker.sh"}

	if _, _, err := desiredSessionProviderForAgent(&agent, cityDir, "tmux"); err == nil {
		t.Fatal("desiredSessionProviderForAgent error = nil, want executable error")
	}
}

func writeExecutable(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	return resolved
}
