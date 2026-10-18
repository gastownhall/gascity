package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestEstablishedCityStoreMetadataError(t *testing.T) {
	tests := []struct {
		name    string
		data    []byte
		readErr error
		wantErr bool
	}{
		{name: "readable metadata", data: []byte(`{"backend":"dolt","dolt_database":"gci"}`)},
		{name: "legacy metadata without database", data: []byte(`{"backend":"dolt"}`)},
		{name: "missing", readErr: fs.ErrNotExist, wantErr: true},
		{name: "unreadable", readErr: fs.ErrPermission, wantErr: true},
		{name: "empty", data: []byte{}, wantErr: true},
		{name: "torn", data: []byte(`{"backend":"dolt","dolt_datab`), wantErr: true},
		{name: "not an object", data: []byte(`"gci"`), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := establishedCityStoreMetadataError("/city/.beads/metadata.json", tt.data, tt.readErr)
			if (err != nil) != tt.wantErr {
				t.Fatalf("establishedCityStoreMetadataError() = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "/city/.beads/metadata.json") {
				t.Fatalf("error %q does not name the metadata path", err)
			}
		})
	}
}

// establishedBdCity lays out a bd-contract city whose store is already
// initialized: metadata names dolt_database "gci" and the managed data dir
// holds that database. The recording provider logs every op and, on init,
// creates the database it was asked for, standing in for gc-beads-bd's
// CREATE DATABASE + bd init.
func establishedBdCity(t *testing.T) (cityPath, callLog string) {
	t.Helper()
	clearGCEnv(t)
	cityPath = t.TempDir()
	callLog = filepath.Join(cityPath, "provider-calls.log")
	script := filepath.Join(cityPath, "gc-beads-bd.sh")
	body := "#!/bin/sh\n" +
		"printf '%s\\n' \"$1\" >> \"" + callLog + "\"\n" +
		"if [ \"$1\" = init ]; then mkdir -p \"$2/.beads/dolt/${4:-hq}\"; fi\n" +
		"exit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil { //nolint:gosec // the double must be executable
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads", "dolt", "gci"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scopeMetadataJSONPath(cityPath), []byte(`{"backend":"dolt","dolt_database":"gci","dolt_mode":"server"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[beads]\nprovider = \"exec:"+script+"\"\n\n[session]\nprovider = \"fake\"\n\n[daemon]\nshutdown_timeout = \"1s\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_DOLT", "")
	writeReachableProviderManagedDoltState(t, cityPath)
	return cityPath, callLog
}

// assertNoCityStoreCreated pins the incident's damage: no provider init ran,
// no new database appeared beside the city's real one, and gc did not write a
// replacement metadata.json naming some other database.
func assertNoCityStoreCreated(t *testing.T, cityPath, callLog string) {
	t.Helper()
	if data, err := os.ReadFile(callLog); err == nil {
		for _, op := range strings.Fields(string(data)) {
			if op == "init" {
				t.Fatalf("provider ops = %q, want no init on an established city store", strings.Fields(string(data)))
			}
		}
	}
	entries, err := os.ReadDir(filepath.Join(cityPath, ".beads", "dolt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "gci" {
			t.Fatalf("managed data dir gained database %q; want only the existing gci", e.Name())
		}
	}
	if _, err := os.Stat(scopeMetadataJSONPath(cityPath)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("metadata.json was rewritten while the city's own was missing (stat err = %v)", err)
	}
}

// TestReloadBeadsLifecycleRefusesInitWhenCityMetadataTransientlyMissing
// reproduces mc-s8duk: a git fast-forward in the city checkout deleted a
// previously tracked .beads/metadata.json, and the next config reload fell
// back to the default "hq" database, created it, and ran bd init over the
// live city.
func TestReloadBeadsLifecycleRefusesInitWhenCityMetadataTransientlyMissing(t *testing.T) {
	cityPath, callLog := establishedBdCity(t)
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(scopeMetadataJSONPath(cityPath)); err != nil {
		t.Fatal(err)
	}

	err = cityRuntimeStartBeadsLifecycle(cityPath, "test-city", cfg, io.Discard)
	assertNoCityStoreCreated(t, cityPath, callLog)
	if err == nil {
		t.Fatal("reload bead lifecycle succeeded with the city's metadata.json missing")
	}
	if !strings.Contains(err.Error(), "metadata.json") {
		t.Fatalf("error = %v, want it to name metadata.json", err)
	}
}

func TestCityRuntimeReloadKeepsOldConfigWhenCityMetadataTransientlyMissing(t *testing.T) {
	cityPath, callLog := establishedBdCity(t)
	tomlPath := filepath.Join(cityPath, "city.toml")
	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sp := runtime.NewFake()
	var stdout, stderr bytes.Buffer
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath:  cityPath,
		CityName:  "test-city",
		TomlPath:  tomlPath,
		LogPrefix: "gc reload",
		Cfg:       cfg,
		SP:        sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: &stdout,
		Stderr: &stderr,
	})
	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = beads.NewMemStore()
	cr.setControllerState(cs)
	cr.sessionDrains = newDrainTracker()
	oldCfg, oldRev := cr.cfg, cr.configRev

	if err := os.Remove(scopeMetadataJSONPath(cityPath)); err != nil {
		t.Fatal(err)
	}
	lastProviderName := "fake"
	reply := cr.reloadConfigTraced(context.Background(), &lastProviderName, cityPath, nil, reloadSourceManual)

	assertNoCityStoreCreated(t, cityPath, callLog)
	if reply.Outcome != reloadOutcomeFailed {
		t.Fatalf("reply.Outcome = %q, want %q (error %q)", reply.Outcome, reloadOutcomeFailed, reply.Error)
	}
	if cr.cfg != oldCfg || cr.configRev != oldRev {
		t.Fatal("reload published a new config with the city's metadata.json missing")
	}
	if !strings.Contains(stderr.String(), "(keeping old config)") || !strings.Contains(stderr.String(), "metadata.json") {
		t.Fatalf("stderr = %q, want a keeping-old-config failure naming metadata.json", stderr.String())
	}
}
