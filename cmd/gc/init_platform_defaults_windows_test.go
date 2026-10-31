//go:build windows

package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
)

// A new city on Windows keeps the portable session selection unnamed. Runtime
// resolution supplies herdr here instead of serializing it into city.toml.
func TestInitSelectsHerdrSessionsOnWindows(t *testing.T) {
	for _, template := range []string{"empty", "minimal"} {
		t.Run(template, func(t *testing.T) {
			f := fsys.NewFake()
			city := "/" + template + "-city"
			var stdout, stderr bytes.Buffer
			if code := doInit(f, city, wizardConfig{configName: template}, "", &stdout, &stderr, false); code != 0 {
				t.Fatalf("doInit = %d; stderr: %s", code, stderr.String())
			}
			data, ok := f.Files[filepath.Join(city, "city.toml")]
			if !ok {
				t.Fatal("doInit wrote no city.toml")
			}
			cfg, err := config.Parse(data)
			if err != nil {
				t.Fatalf("parsing the written city.toml: %v\n%s", err, data)
			}
			if cfg.Session.Provider != "" {
				t.Fatalf("[session] provider = %q, want unnamed:\n%s", cfg.Session.Provider, data)
			}
			ctx := sessionProviderContextForCity(cfg, city, "")
			if ctx.providerName != "herdr" {
				t.Fatalf("runtime provider = %q, want herdr for unnamed Windows selection", ctx.providerName)
			}
		})
	}
}

func TestStatusSessionProviderUsesHerdrDefaultOnWindows(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "city"}}
	oldBuild := buildSessionProviderByName
	t.Cleanup(func() { buildSessionProviderByName = oldBuild })

	var gotName string
	buildSessionProviderByName = func(_ *config.City, name string, _ config.SessionConfig, _, _ string) (runtime.Provider, error) {
		gotName = name
		return runtime.NewFake(), nil
	}

	sp, err := newStatusSessionProviderForCity(cfg, t.TempDir())
	if err != nil {
		t.Fatalf("newStatusSessionProviderForCity: %v", err)
	}
	if sp == nil {
		t.Fatal("newStatusSessionProviderForCity returned nil provider")
	}
	if gotName != "herdr" {
		t.Fatalf("status provider selected %q, want herdr", gotName)
	}
}

// A fresh provider-owned scope takes the direct transport on Windows, where bd
// cannot initialize a proxied store (measured with bd 1.3.0: "Access is denied"
// syncing the .beads directory). An explicit selector still wins.
func TestFreshScopeTakesTheDirectTransportOnWindows(t *testing.T) {
	got, err := hostedDoltInitOptions{}.providerOwnershipIntent(config.City{})
	if err != nil {
		t.Fatalf("providerOwnershipIntent: %v", err)
	}
	if want := (providerScopeIntent{Transport: "direct", Target: "local"}); got != want {
		t.Fatalf("fresh scope intent = %+v, want %+v", got, want)
	}
	got, err = hostedDoltInitOptions{Transport: "proxied", Target: "local"}.providerOwnershipIntent(config.City{})
	if err != nil {
		t.Fatalf("providerOwnershipIntent with a selector: %v", err)
	}
	if want := (providerScopeIntent{Transport: "proxied", Target: "local"}); got != want {
		t.Fatalf("selected scope intent = %+v, want %+v", got, want)
	}
	if usage := beadsTransportFlagUsage(t); !strings.Contains(usage, "Default direct on Windows") {
		t.Fatalf("--beads-transport help does not state the Windows default: %q", usage)
	}
}

// The tmux refusal names the provider that runs here.
func TestTmuxRefusalNamesHerdrOnWindows(t *testing.T) {
	hint := unavailableBackendHint("tmux", "brew install tmux")
	if !strings.Contains(hint, `provider = "herdr"`) {
		t.Fatalf("tmux hint does not name herdr: %q", hint)
	}
}
