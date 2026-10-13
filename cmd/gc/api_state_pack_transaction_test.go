package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/packman"
	"github.com/gastownhall/gascity/internal/runtime"
)

func newPackTransactionControllerState(t *testing.T) (*controllerState, string) {
	t.Helper()
	cityPath := t.TempDir()
	writeCityRuntimeConfig(t, filepath.Join(cityPath, "city.toml"), "fake")
	cfg, _ := loadCityRuntimeControllerConfig(t, cityPath)
	cs := newControllerState(context.Background(), cfg, runtime.NewFake(), events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = beads.NewMemStore()
	cs.pokeCh = make(chan struct{}, 1)
	return cs, cityPath
}

func requireFileAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("%s exists after rollback (lstat err=%v)", path, err)
	}
}

// importsvc writes pack.toml (or city.toml) and then packs.lock. When a later
// step fails, the files it already wrote must not stay on disk as a
// half-written pack set for the next reload to publish.
func TestControllerStatePackConfigWriteRollsBackPartialWriteOnError(t *testing.T) {
	cs, cityPath := newPackTransactionControllerState(t)
	cityToml := filepath.Join(cityPath, "city.toml")
	beforeCity, err := os.ReadFile(cityToml)
	if err != nil {
		t.Fatal(err)
	}
	beforeCfg := cs.Config()

	lockErr := errors.New("writing packs.lock: disk full")
	err = cs.SerializeConfigWrite(func() error {
		if err := os.WriteFile(filepath.Join(cityPath, "pack.toml"), []byte("[pack]\nname = \"test-city\"\nschema = 2\n\n[imports.tools]\nsource = \"https://example.com/tools\"\n"), 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(cityToml, append(append([]byte{}, beforeCity...), []byte("\n[imports.extra]\nsource = \"https://example.com/extra\"\n")...), 0o644); err != nil {
			return err
		}
		return lockErr
	})
	if !errors.Is(err, lockErr) {
		t.Fatalf("SerializeConfigWrite error = %v, want %v", err, lockErr)
	}

	requireFileAbsent(t, filepath.Join(cityPath, "pack.toml"))
	requireFileAbsent(t, filepath.Join(cityPath, packman.LockfileName))
	afterCity, err := os.ReadFile(cityToml)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterCity) != string(beforeCity) {
		t.Fatalf("city.toml not restored:\n%s", afterCity)
	}
	if cs.Config() != beforeCfg {
		t.Fatal("failed pack write changed the controller config snapshot")
	}
	if cs.configMutationPending.Load() {
		t.Fatal("failed pack write marked a config mutation pending")
	}
}

// A pack write whose result does not load is rolled back as a generation:
// pack.toml and packs.lock return to their prior state with city.toml.
func TestControllerStatePackConfigWriteRollsBackPackFilesWhenRefreshFails(t *testing.T) {
	cs, cityPath := newPackTransactionControllerState(t)
	lockPath := filepath.Join(cityPath, packman.LockfileName)
	if err := os.WriteFile(lockPath, []byte("schema = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := cs.SerializeConfigWrite(func() error {
		if err := os.WriteFile(lockPath, []byte("schema = 1\n\n[packs.\"https://example.com/tools\"]\nversion = \"1.0.0\"\n"), 0o644); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace\n"), 0o644)
	})
	if err == nil || !strings.Contains(err.Error(), "refreshing updated city config") {
		t.Fatalf("SerializeConfigWrite error = %v, want refresh failure", err)
	}

	got, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "schema = 1\n" {
		t.Fatalf("packs.lock not restored after refresh failure:\n%s", got)
	}
}

// A successful pack write publishes its generation to the controller state
// inside the transaction, like every other API config mutation, so the
// runtime reload that follows is recognized as the pending mutation.
func TestControllerStatePackConfigWritePublishesGeneration(t *testing.T) {
	cs, cityPath := newPackTransactionControllerState(t)
	cityToml := filepath.Join(cityPath, "city.toml")

	if err := cs.SerializeConfigWrite(func() error {
		f, err := os.OpenFile(cityToml, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer f.Close() //nolint:errcheck // test cleanup
		_, err = f.WriteString("\n[[agent]]\nname = \"helper\"\n")
		return err
	}); err != nil {
		t.Fatalf("SerializeConfigWrite: %v", err)
	}

	if !configHasAgent(cs.Config(), "helper") {
		t.Fatal("pack write did not refresh the controller config snapshot")
	}
	if !cs.configMutationPending.Load() {
		t.Fatal("pack write did not mark its generation pending for the runtime reload")
	}
	select {
	case <-cs.pokeCh:
	default:
		t.Fatal("pack write did not poke the controller")
	}
}

// The runtime reload must never read a pack generation while a pack write is
// still in flight. It defers (with a retry pending) instead of blocking the
// reconciler behind a slow import, and applies the finished generation next.
func TestCityRuntimeReloadDefersWhilePackConfigWriteInFlight(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")
	initialCfg, initialRevision := loadCityRuntimeControllerConfig(t, cityPath)
	provider := runtime.NewFake()
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath:  cityPath,
		CityName:  "test-city",
		TomlPath:  tomlPath,
		ConfigRev: initialRevision,
		Cfg:       initialCfg,
		SP:        provider,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(provider),
		Rec:    events.Discard,
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	cs := newControllerState(context.Background(), initialCfg, provider, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = beads.NewMemStore()
	cr.setControllerState(cs)
	previousLifecycle := cityRuntimeStartBeadsLifecycle
	cityRuntimeStartBeadsLifecycle = func(string, string, *config.City, io.Writer) error { return nil }
	t.Cleanup(func() { cityRuntimeStartBeadsLifecycle = previousLifecycle })

	lastProviderName := "fake"
	var midReply reloadControlReply
	if err := cs.SerializeConfigWrite(func() error {
		// The first file of a multi-file pack write has landed; the rest has not.
		if err := os.WriteFile(tomlPath, []byte("[workspace]\nname = \"test-city\"\n\n[beads]\nprovider = \"file\"\n\n[session]\nprovider = \"fake\"\n\n[daemon]\nshutdown_timeout = \"1s\"\n"), 0o644); err != nil {
			return err
		}
		midReply = cr.reloadConfigTraced(context.Background(), &lastProviderName, cityPath, nil, reloadSourceWatch)
		return nil
	}); err != nil {
		t.Fatalf("SerializeConfigWrite: %v", err)
	}

	if midReply.Outcome != reloadOutcomeFailed || !strings.Contains(midReply.Error, "config mutation in progress") {
		t.Fatalf("mid-write reload reply = %+v, want deferred failure", midReply)
	}
	if cr.cfg != initialCfg || cr.configRev != initialRevision {
		t.Fatal("reload published a generation read while a pack write was in flight")
	}
	if cr.configDirty == nil || !cr.configDirty.Load() {
		t.Fatal("deferred reload did not leave a retry pending")
	}

	reply := cr.reloadConfigTraced(context.Background(), &lastProviderName, cityPath, nil, reloadSourceWatch)
	if reply.Outcome != reloadOutcomeApplied {
		t.Fatalf("post-write reload reply = %+v, want applied", reply)
	}
	if cr.configRev == initialRevision || cs.Config() != cr.cfg {
		t.Fatal("post-write reload did not converge the loop and controller state")
	}
}
