package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestInitSelectorDefaultsToProxiedLocal(t *testing.T) {
	cfg := config.DefaultCity("fresh")
	if err := (hostedDoltInitOptions{}).applySelectorToCityConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Dolt.Mode != "proxied-server" || cfg.Dolt.Host != "" || cfg.Dolt.Port != 0 {
		t.Fatalf("got dolt config %+v, want proxied local", cfg.Dolt)
	}
}

func TestInitSelectorDirectLocal(t *testing.T) {
	cfg := config.DefaultCity("fresh")
	if err := (hostedDoltInitOptions{Transport: "direct", Target: "local"}).applySelectorToCityConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Dolt.Mode != "server" {
		t.Fatalf("mode = %q, want server", cfg.Dolt.Mode)
	}
}

func TestInitSelectorProxiedExternal(t *testing.T) {
	cfg := config.DefaultCity("fresh")
	opts := hostedDoltInitOptions{Transport: "proxied", Target: "external", Host: "db.example", Port: "3306", Database: "bd_proj", ProjectID: "proj"}
	if err := opts.applySelectorToCityConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Dolt.Mode != "proxied-server" || cfg.Dolt.Host != "db.example" || cfg.Dolt.Port != 3306 {
		t.Fatalf("got dolt config %+v, want proxied external", cfg.Dolt)
	}
}

func TestInitSelectorRejectsIncompleteIntent(t *testing.T) {
	cfg := config.DefaultCity("fresh")
	if err := (hostedDoltInitOptions{Transport: "proxied"}).applySelectorToCityConfig(&cfg); err == nil {
		t.Fatal("incomplete selector accepted")
	}
}
