package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestTmuxConfigFromSessionDefaultsSocketToCityName(t *testing.T) {
	sc := config.SessionConfig{}

	cfg := tmuxConfigFromSession(sc, "city", "/tmp/city-a")
	if cfg.SocketName != "city" {
		t.Fatalf("SocketName = %q, want %q", cfg.SocketName, "city")
	}
}

func TestTmuxConfigFromSessionPreservesExplicitSocket(t *testing.T) {
	sc := config.SessionConfig{Socket: "custom-socket"}

	cfg := tmuxConfigFromSession(sc, "city", "/tmp/city-a")
	if cfg.SocketName != "custom-socket" {
		t.Fatalf("SocketName = %q, want %q", cfg.SocketName, "custom-socket")
	}
}
