package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestTmuxConfigFromSessionDefaultsSocketToCityPathScopedName(t *testing.T) {
	sc := config.SessionConfig{}

	cfg := tmuxConfigFromSession(sc, "city", "/tmp/city-a")
	if cfg.SocketName != "city-310b37bd" {
		t.Fatalf("SocketName = %q, want %q", cfg.SocketName, "city-310b37bd")
	}
}

func TestTmuxConfigFromSessionPreservesExplicitSocket(t *testing.T) {
	sc := config.SessionConfig{Socket: "custom-socket"}

	cfg := tmuxConfigFromSession(sc, "city", "/tmp/city-a")
	if cfg.SocketName != "custom-socket" {
		t.Fatalf("SocketName = %q, want %q", cfg.SocketName, "custom-socket")
	}
}

func TestDefaultTmuxSocketNameSanitizesCityName(t *testing.T) {
	got := defaultTmuxSocketName("maintainer city", "/tmp/city-a")
	if got != "maintainer-city-310b37bd" {
		t.Fatalf("defaultTmuxSocketName = %q, want %q", got, "maintainer-city-310b37bd")
	}
}
