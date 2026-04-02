package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestNewSessionProviderByNameSubprocessUsesCityScopedDir(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "city-a")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatalf("mkdir city: %v", err)
	}

	sp, err := newSessionProviderByName("subprocess", config.SessionConfig{}, "city-a", cityPath)
	if err != nil {
		t.Fatalf("newSessionProviderByName: %v", err)
	}

	const sessionName = "worker"
	if err := sp.Start(context.Background(), sessionName, runtime.Config{Command: "sleep 3600"}); err != nil {
		t.Fatalf("Start(%q): %v", sessionName, err)
	}
	t.Cleanup(func() { _ = sp.Stop(sessionName) })

	socketDir := providerStateDir("subprocess", cityPath)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if matches, _ := filepath.Glob(filepath.Join(socketDir, "*.sock")); len(matches) == 1 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	if matches, _ := filepath.Glob(filepath.Join(socketDir, "*.sock")); len(matches) != 1 {
		t.Fatalf("expected one city-scoped subprocess socket in %s, got %v", socketDir, matches)
	}
}

func TestNewSessionProviderByNameSubprocessAllowsSameSessionNameAcrossCities(t *testing.T) {
	cityA := filepath.Join(t.TempDir(), "city-a")
	cityB := filepath.Join(t.TempDir(), "city-b")
	for _, cityPath := range []string{cityA, cityB} {
		if err := os.MkdirAll(cityPath, 0o755); err != nil {
			t.Fatalf("mkdir city %s: %v", cityPath, err)
		}
	}

	spA, err := newSessionProviderByName("subprocess", config.SessionConfig{}, "city-a", cityA)
	if err != nil {
		t.Fatalf("newSessionProviderByName(city-a): %v", err)
	}
	spB, err := newSessionProviderByName("subprocess", config.SessionConfig{}, "city-b", cityB)
	if err != nil {
		t.Fatalf("newSessionProviderByName(city-b): %v", err)
	}

	const sessionName = "worker"
	if err := spA.Start(context.Background(), sessionName, runtime.Config{Command: "sleep 3600"}); err != nil {
		t.Fatalf("spA.Start(%q): %v", sessionName, err)
	}
	t.Cleanup(func() { _ = spA.Stop(sessionName) })
	if err := spB.Start(context.Background(), sessionName, runtime.Config{Command: "sleep 3600"}); err != nil {
		t.Fatalf("spB.Start(%q): %v", sessionName, err)
	}
	t.Cleanup(func() { _ = spB.Stop(sessionName) })
}
