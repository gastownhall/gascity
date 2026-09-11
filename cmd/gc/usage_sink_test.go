package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/usage"
)

func TestUsageSinkForCity(t *testing.T) {
	cityPath := t.TempDir()
	tests := []struct {
		name     string
		cfg      *config.City
		cityPath string
		want     string // "local" | "discard" | "exec"
	}{
		{"nil cfg defaults to local", nil, cityPath, "local"},
		{"empty provider defaults to local", &config.City{}, cityPath, "local"},
		{"explicit local", &config.City{Usage: config.UsageConfig{Provider: "local"}}, cityPath, "local"},
		{"discard", &config.City{Usage: config.UsageConfig{Provider: "discard"}}, cityPath, "discard"},
		{"fake is a discard alias", &config.City{Usage: config.UsageConfig{Provider: "fake"}}, cityPath, "discard"},
		{"exec", &config.City{Usage: config.UsageConfig{Provider: "exec:/bin/true"}}, cityPath, "exec"},
		{"empty cityPath demotes local to discard", &config.City{Usage: config.UsageConfig{Provider: "local"}}, "", "discard"},
		{"empty cityPath keeps exec", &config.City{Usage: config.UsageConfig{Provider: "exec:/bin/true"}}, "", "exec"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertSinkKind(t, usageSinkForCity(tc.cfg, tc.cityPath), tc.want)
		})
	}
}

// TestWorkerFactoryWithConfigThreadsUsageSink locks the regression: a configured
// usage provider must reach the CLI-built worker factory, which previously left
// FactoryConfig.UsageSink unset and silently defaulted every CLI worker handle
// to usage.Discard.
func TestWorkerFactoryWithConfigThreadsUsageSink(t *testing.T) {
	cityPath := t.TempDir()
	sp := runtime.NewFake()

	cases := []struct {
		name string
		cfg  *config.City
		want string
	}{
		{"local provider threads a durable sink", &config.City{Usage: config.UsageConfig{Provider: "local"}}, "local"},
		{"discard provider threads discard", &config.City{Usage: config.UsageConfig{Provider: "discard"}}, "discard"},
		{"default provider threads a durable sink", &config.City{}, "local"},
		{"exec provider threads exec", &config.City{Usage: config.UsageConfig{Provider: "exec:/bin/true"}}, "exec"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := workerFactoryWithConfig(cityPath, nil, sp, tc.cfg)
			if err != nil {
				t.Fatalf("workerFactoryWithConfig: %v", err)
			}
			assertSinkKind(t, f.UsageSink(), tc.want)
		})
	}
}

// TestControllerStateReloadRefreshesUsageSink covers both live-reload paths:
// the full store-rebuild update and the store-reuse updateConfigAndProviderOnly.
// A changed [usage].provider must take effect on reload instead of writing to
// the sink built at startup until the controller restarts.
func TestControllerStateReloadRefreshesUsageSink(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	// newControllerState builds a mail provider, which resolves the ambient city
	// (CWD + rig registry). With GC_HOME stripped by the test env and a CWD left
	// elsewhere by a sibling test, that resolution would otherwise hit the
	// supervisor host-isolation guard and panic. Pin GC_HOME to a temp dir so the
	// registry lookup is empty-but-safe and the mail provider simply defaults.
	t.Setenv("GC_HOME", t.TempDir())
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"),
		[]byte("[workspace]\nname = \"city1\"\n\n[beads]\nprovider = \"file\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	localCfg := &config.City{
		Workspace: config.Workspace{Name: "city1"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Usage:     config.UsageConfig{Provider: "local"},
	}
	discardCfg := &config.City{
		Workspace: config.Workspace{Name: "city1"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Usage:     config.UsageConfig{Provider: "discard"},
	}

	cs := newControllerState(context.Background(), discardCfg, runtime.NewFake(), events.NewFake(), "city1", cityDir)
	assertSinkKind(t, cs.UsageSink(), "discard")

	// Full-rebuild reload path, both directions.
	cs.update(localCfg, runtime.NewFake())
	assertSinkKind(t, cs.UsageSink(), "local")
	cs.update(discardCfg, runtime.NewFake())
	assertSinkKind(t, cs.UsageSink(), "discard")

	// Store-reuse reload path, both directions.
	cs.updateConfigAndProviderOnly(localCfg, runtime.NewFake())
	assertSinkKind(t, cs.UsageSink(), "local")
	cs.updateConfigAndProviderOnly(discardCfg, runtime.NewFake())
	assertSinkKind(t, cs.UsageSink(), "discard")
}

func assertSinkKind(t *testing.T, sink usage.Sink, want string) {
	t.Helper()
	switch want {
	case "local":
		if _, ok := sink.(*usage.LocalSink); !ok {
			t.Fatalf("want *usage.LocalSink, got %T", sink)
		}
	case "exec":
		if _, ok := sink.(*usage.ExecSink); !ok {
			t.Fatalf("want *usage.ExecSink, got %T", sink)
		}
	case "discard":
		if sink != usage.Discard {
			t.Fatalf("want usage.Discard, got %T", sink)
		}
	default:
		t.Fatalf("unknown sink kind %q", want)
	}
}
