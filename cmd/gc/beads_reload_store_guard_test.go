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

const (
	gciMetadata    = `{"backend":"dolt","dolt_database":"gci","dolt_mode":"server"}`
	hqStubMetadata = `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`
)

// staleMetadataShapes are metadata.json contents a city checkout can be left
// holding after its booted binding is lost; each makes the lifecycle's
// fallback resolve "hq".
var staleMetadataShapes = []struct{ name, body string }{
	{"null", "null"},
	{"empty object", "{}"},
	{"backend only", `{"backend":"dolt"}`},
	{"pre-dolt bd", `{"database":"beads.db","jsonl_export":"issues.jsonl","last_bd_version":"0.27.2"}`},
	{"gc hq stub", hqStubMetadata},
}

func TestReloadCityStoreDatabase(t *testing.T) {
	const path = "/city/.beads/metadata.json"
	gci := cityStoreRecord{Recorded: true, Binding: cityStoreBinding{Backend: "dolt", DoltDatabase: "gci"}, Database: "gci"}
	legacy := cityStoreRecord{Recorded: true, Binding: cityStoreBinding{Backend: "dolt"}, Database: "hq"}
	hosted := cityStoreRecord{Recorded: true, Binding: cityStoreBinding{Backend: "postgres", StorageEndpoint: "opaque", StorageDatabase: "work"}}
	tests := []struct {
		name    string
		rec     cityStoreRecord
		data    string
		readErr error
		want    string
		wantErr string
	}{
		{name: "booted binding", rec: gci, data: gciMetadata, want: "gci"},
		{name: "booted binding with extra keys", rec: gci, data: `{"backend":"dolt","dolt_database":"gci","project_id":"p","dolt_server_port":1}`, want: "gci"},
		{name: "legacy binding keeps its database", rec: legacy, data: `{"backend":"dolt"}`, want: "hq"},
		{name: "hosted binding", rec: hosted, data: `{"backend":"postgres","storage_endpoint":"opaque","storage_database":"work"}`},
		{name: "missing", rec: gci, readErr: &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}, wantErr: "(missing)"},
		{name: "permission denied", rec: gci, readErr: &fs.PathError{Op: "open", Path: path, Err: fs.ErrPermission}, wantErr: "permission denied"},
		{name: "empty file", rec: gci, data: "", wantErr: "unparseable"},
		{name: "torn", rec: gci, data: `{"backend":"dolt","dolt_datab`, wantErr: "unparseable"},
		{name: "not an object", rec: gci, data: `"gci"`, wantErr: "unparseable"},
		{name: "null", rec: gci, data: "null", wantErr: "(null)"},
		{name: "empty object", rec: gci, data: "{}", wantErr: "names no store"},
		{name: "pre-dolt bd", rec: gci, data: `{"database":"beads.db","jsonl_export":"issues.jsonl"}`, wantErr: "names no store"},
		{name: "backend only", rec: gci, data: `{"backend":"dolt"}`, wantErr: "now names store [backend=dolt]"},
		{name: "gc hq stub", rec: gci, data: hqStubMetadata, wantErr: "now names store [backend=dolt dolt_database=hq]"},
		{name: "older binding restored by git", rec: gci, data: `{"backend":"dolt","dolt_database":"gas_city_inc"}`, wantErr: "dolt_database=gas_city_inc"},
		{name: "hosted endpoint changed", rec: hosted, data: `{"backend":"postgres","storage_endpoint":"other","storage_database":"work"}`, wantErr: "now names store"},
		{name: "booted without bd binding", rec: cityStoreRecord{}, data: gciMetadata, wantErr: "restart the city to initialize"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := reloadCityStoreDatabase(tt.rec, path, []byte(tt.data), tt.readErr)
			if tt.wantErr == "" {
				if err != nil || got != tt.want {
					t.Fatalf("reloadCityStoreDatabase() = %q, %v; want %q, nil", got, err, tt.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("reloadCityStoreDatabase() = %q, nil; want error containing %q", got, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tt.wantErr)
			}
			if n := strings.Count(err.Error(), path); n != 1 {
				t.Fatalf("error names %s %d times, want once: %q", path, n, err)
			}
		})
	}
}

// establishedBdCity lays out a bd-contract city whose store is already
// initialized: metadata names dolt_database "gci" and the managed data dir
// holds that database. The recording provider logs every op with its
// database argument and, on init, creates that database, standing in for
// gc-beads-bd's CREATE DATABASE + bd init. startScript, when set, runs
// during the provider start op.
func establishedBdCity(t *testing.T, startScript string) (cityPath, callLog string) {
	t.Helper()
	clearGCEnv(t)
	cityPath = t.TempDir()
	callLog = filepath.Join(cityPath, "provider-calls.log")
	script := filepath.Join(cityPath, "gc-beads-bd.sh")
	body := "#!/bin/sh\n" +
		"printf '%s %s\\n' \"$1\" \"${4:-}\" >> \"" + callLog + "\"\n" +
		"if [ \"$1\" = start ]; then " + startScript + ":; fi\n" +
		"if [ \"$1\" = init ]; then mkdir -p \"$2/.beads/dolt/${4:-hq}\"; exit 1; fi\n" +
		"exit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil { //nolint:gosec // the double must be executable
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads", "dolt", "gci"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scopeMetadataJSONPath(cityPath), []byte(gciMetadata), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[beads]\nprovider = \"exec:"+script+"\"\n\n[session]\nprovider = \"fake\"\n\n[daemon]\nshutdown_timeout = \"1s\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_DOLT", "")
	writeReachableProviderManagedDoltState(t, cityPath)
	return cityPath, callLog
}

func providerCalls(t *testing.T, callLog string) []string {
	t.Helper()
	data, err := os.ReadFile(callLog)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			calls = append(calls, line)
		}
	}
	return calls
}

// assertNoCityStoreCreated pins the incident's damage: no provider init ran,
// no database appeared beside the city's real one, and metadata.json still
// holds exactly what the test left there (absent when wantMetadata is nil).
func assertNoCityStoreCreated(t *testing.T, cityPath, callLog string, wantMetadata []byte) {
	t.Helper()
	for _, call := range providerCalls(t, callLog) {
		if strings.HasPrefix(call, "init") {
			t.Fatalf("provider calls = %q, want no init on an established city store", providerCalls(t, callLog))
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
	got, err := os.ReadFile(scopeMetadataJSONPath(cityPath))
	switch {
	case wantMetadata == nil && !errors.Is(err, fs.ErrNotExist):
		t.Fatalf("metadata.json was rewritten while the city's own was missing: %q (err %v)", got, err)
	case wantMetadata != nil && !bytes.Equal(got, wantMetadata):
		t.Fatalf("metadata.json = %q, want it left as %q", got, wantMetadata)
	}
}

// TestReloadBeadsLifecycleRefusesInitWhenCityMetadataLost reproduces
// mc-s8duk: a git fast-forward in the city checkout deleted a previously
// tracked .beads/metadata.json, and the next config reload fell back to the
// default "hq" database, created it, and ran bd init over the live city. A
// lost binding may also leave a stub or an older binding behind; none of them
// may reach the provider at all.
func TestReloadBeadsLifecycleRefusesInitWhenCityMetadataLost(t *testing.T) {
	cases := append([]struct{ name, body string }{{"missing", ""}}, staleMetadataShapes...)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, callLog := establishedBdCity(t, "")
			cfg, err := loadCityConfig(cityPath, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			rec := recordCityStore(cityPath)
			var wantMetadata []byte
			if tc.name == "missing" {
				err = os.Remove(scopeMetadataJSONPath(cityPath))
			} else {
				wantMetadata = []byte(tc.body)
				err = os.WriteFile(scopeMetadataJSONPath(cityPath), wantMetadata, 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}

			err = cityRuntimeStartBeadsLifecycle(cityPath, "test-city", cfg, rec, io.Discard)

			if calls := providerCalls(t, callLog); len(calls) != 0 {
				t.Fatalf("provider calls = %q, want none before the store binding is confirmed", calls)
			}
			assertNoCityStoreCreated(t, cityPath, callLog, wantMetadata)
			if err == nil || !strings.Contains(err.Error(), "metadata.json") {
				t.Fatalf("reload lifecycle error = %v, want a refusal naming metadata.json", err)
			}
		})
	}
}

// TestReloadBeadsLifecycleRefusesInitWhenMetadataLostDuringStart covers the
// window between the reload's first metadata check and init: the provider
// start op can take seconds, and a checkout landing then must not let init
// fall back to "hq".
func TestReloadBeadsLifecycleRefusesInitWhenMetadataLostDuringStart(t *testing.T) {
	cityPath, callLog := establishedBdCity(t, `rm -f "$(dirname "$0")/.beads/metadata.json"; `)
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		t.Fatal(err)
	}

	err = cityRuntimeStartBeadsLifecycle(cityPath, "test-city", cfg, recordCityStore(cityPath), io.Discard)

	if calls := providerCalls(t, callLog); len(calls) != 1 || !strings.HasPrefix(calls[0], "start") {
		t.Fatalf("provider calls = %q, want only start", calls)
	}
	assertNoCityStoreCreated(t, cityPath, callLog, nil)
	if err == nil || !strings.Contains(err.Error(), "(missing)") {
		t.Fatalf("reload lifecycle error = %v, want a refusal for the missing metadata", err)
	}
}

// TestReloadBeadsLifecycleInitsBootedDatabase is the positive control: with
// the booted binding intact the reload lifecycle still reaches init, and
// hands it the booted database rather than a resolved one.
func TestReloadBeadsLifecycleInitsBootedDatabase(t *testing.T) {
	cityPath, callLog := establishedBdCity(t, "")
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		t.Fatal(err)
	}

	// The double fails init so the lifecycle stops there; only the call
	// matters.
	_ = cityRuntimeStartBeadsLifecycle(cityPath, "test-city", cfg, recordCityStore(cityPath), io.Discard)

	sawInit := false
	for _, call := range providerCalls(t, callLog) {
		if strings.HasPrefix(call, "init") {
			sawInit = true
			if call != "init gci" {
				t.Fatalf("provider call %q, want init of the booted database gci", call)
			}
		}
	}
	if !sawInit {
		t.Fatalf("provider calls = %q, want init gci", providerCalls(t, callLog))
	}
}

func TestRecordCityStore(t *testing.T) {
	cityPath, _ := establishedBdCity(t, "")
	if got, want := recordCityStore(cityPath), (cityStoreRecord{Recorded: true, Binding: cityStoreBinding{Backend: "dolt", DoltDatabase: "gci"}, Database: "gci"}); got != want {
		t.Fatalf("recordCityStore() = %+v, want %+v", got, want)
	}
	if err := os.WriteFile(scopeMetadataJSONPath(cityPath), []byte(`{"backend":"dolt"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := recordCityStore(cityPath); !got.Recorded || got.Database != "hq" {
		t.Fatalf("recordCityStore(legacy) = %+v, want recorded with the historical hq default", got)
	}
	if err := os.Remove(scopeMetadataJSONPath(cityPath)); err != nil {
		t.Fatal(err)
	}
	if got := recordCityStore(cityPath); got.Recorded {
		t.Fatalf("recordCityStore(no metadata) = %+v, want not recorded", got)
	}
}

func TestCityRuntimeReloadKeepsOldConfigWhenCityMetadataLost(t *testing.T) {
	cityPath, callLog := establishedBdCity(t, "")
	stubManagedDoltStoreOpeners(t)
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
	if !cr.bootCityStore.Recorded || cr.bootCityStore.Database != "gci" {
		t.Fatalf("runtime recorded city store %+v, want the booted gci binding", cr.bootCityStore)
	}
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

	assertNoCityStoreCreated(t, cityPath, callLog, nil)
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
