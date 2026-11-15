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
	hostedMetadata = `{"backend":"postgres","storage_endpoint":"opaque","storage_database":"work"}`
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

// restoreBeforeRestart and restartToAdopt are the two remedies a refusal can
// give: a lost binding must be restored before any restart (boot would bind
// a default database), while a deliberate rebinding is adopted by restart.
const (
	restoreBeforeRestart = "do NOT restart until it is restored"
	restartToAdopt       = "if the change is deliberate, restart the city to adopt it"
)

func TestReloadCityStoreError(t *testing.T) {
	const path = "/city/.beads/metadata.json"
	gci := cityStoreRecord{Recorded: true, BdBoot: true, Binding: cityStoreBinding{Backend: "dolt", DoltDatabase: "gci"}, Database: "gci"}
	legacy := cityStoreRecord{Recorded: true, BdBoot: true, Binding: cityStoreBinding{Backend: "dolt"}, Database: "hq"}
	hosted := cityStoreRecord{Recorded: true, BdBoot: true, Binding: cityStoreBinding{Backend: "postgres", StorageEndpoint: "opaque", StorageDatabase: "work"}}
	tests := []struct {
		name     string
		rec      cityStoreRecord
		data     string
		readErr  error
		wantErr  []string
		noRemedy string
	}{
		{name: "booted binding", rec: gci, data: gciMetadata},
		{name: "booted binding with extra keys", rec: gci, data: `{"backend":"dolt","dolt_database":"gci","project_id":"p","dolt_server_port":1}`},
		{name: "legacy binding", rec: legacy, data: `{"backend":"dolt"}`},
		{name: "null field reads as absent", rec: legacy, data: `{"backend":"dolt","dolt_database":null}`},
		{name: "hosted binding", rec: hosted, data: hostedMetadata},
		{name: "missing", rec: gci, readErr: &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}, wantErr: []string{"(missing)", "[backend=dolt dolt_database=gci]", restoreBeforeRestart}, noRemedy: restartToAdopt},
		{name: "permission denied", rec: gci, readErr: &fs.PathError{Op: "open", Path: path, Err: fs.ErrPermission}, wantErr: []string{"permission denied", restoreBeforeRestart}},
		{name: "empty file", rec: gci, data: "", wantErr: []string{"unparseable", restoreBeforeRestart}},
		{name: "torn", rec: gci, data: `{"backend":"dolt","dolt_datab`, wantErr: []string{"unparseable", restoreBeforeRestart}},
		{name: "not an object", rec: gci, data: `"gci"`, wantErr: []string{"unparseable", restoreBeforeRestart}},
		{name: "null", rec: gci, data: "null", wantErr: []string{"(null)", restoreBeforeRestart}},
		{name: "empty object", rec: gci, data: "{}", wantErr: []string{"names no store", restoreBeforeRestart}},
		{name: "pre-dolt bd", rec: gci, data: `{"database":"beads.db","jsonl_export":"issues.jsonl"}`, wantErr: []string{"names no store", restoreBeforeRestart}},
		{name: "non-string database", rec: gci, data: `{"backend":"dolt","dolt_database":123}`, wantErr: []string{"dolt_database is not a string", restoreBeforeRestart}, noRemedy: restartToAdopt},
		{name: "non-string endpoint", rec: hosted, data: `{"backend":"postgres","storage_endpoint":{"url":"postgres://bd:s3cret@db.example.test:5432"},"storage_database":"work"}`, wantErr: []string{"storage_endpoint is not a string", restoreBeforeRestart}, noRemedy: "s3cret"},
		{name: "backend only", rec: gci, data: `{"backend":"dolt"}`, wantErr: []string{"now names store [backend=dolt]", restartToAdopt}, noRemedy: restoreBeforeRestart},
		{name: "gc hq stub", rec: gci, data: hqStubMetadata, wantErr: []string{"now names store [backend=dolt dolt_database=hq]", restartToAdopt}},
		{name: "older binding restored by git", rec: gci, data: `{"backend":"dolt","dolt_database":"gas_city_inc"}`, wantErr: []string{"dolt_database=gas_city_inc", restartToAdopt}},
		{name: "postgres flip", rec: gci, data: `{"backend":"postgres","storage_endpoint":"postgres://bd:s3cret@db.example.test:5432","storage_database":"beads_pg"}`, wantErr: []string{"storage_endpoint=postgres://***@db.example.test:5432", restartToAdopt}, noRemedy: "s3cret"},
		{name: "hosted endpoint changed", rec: hosted, data: `{"backend":"postgres","storage_endpoint":"other","storage_database":"work"}`, wantErr: []string{"now names store", restartToAdopt}},
		{name: "provider switched to bd", rec: cityStoreRecord{}, data: gciMetadata, wantErr: []string{"non-bd beads provider", "restart the city to initialize its bd store"}, noRemedy: restoreBeforeRestart},
		{name: "bd binding unusable at boot", rec: cityStoreRecord{BdBoot: true, Problem: "missing"}, data: gciMetadata, wantErr: []string{"already unusable", "(missing)", restoreBeforeRestart}, noRemedy: "restart the city to initialize"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := reloadCityStoreError(tt.rec, path, []byte(tt.data), tt.readErr)
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("reloadCityStoreError() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("reloadCityStoreError() = nil, want error containing %q", tt.wantErr)
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not contain %q", err, want)
				}
			}
			if tt.noRemedy != "" && strings.Contains(err.Error(), tt.noRemedy) {
				t.Fatalf("error %q must not contain %q", err, tt.noRemedy)
			}
			if n := strings.Count(err.Error(), path); n != 1 {
				t.Fatalf("error names %s %d times, want once: %q", path, n, err)
			}
		})
	}
}

func TestReloadCityStoreDatabaseRefusesNonDoltBinding(t *testing.T) {
	cityPath, _ := establishedBdCity(t, "")
	if err := os.WriteFile(scopeMetadataJSONPath(cityPath), []byte(hostedMetadata), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := recordCityStore(cityPath)
	if !rec.Recorded || rec.Database != "" {
		t.Fatalf("recordCityStore(hosted) = %+v, want recorded with no Dolt database", rec)
	}
	if db, err := reloadCityStoreDatabase(cityPath, rec); err == nil {
		t.Fatalf("reloadCityStoreDatabase(hosted) = %q, nil; want a refusal, never a default", db)
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

// TestReloadBeadsLifecycleRefusesBindingRewrittenDuringStart covers the same
// window for the city init branches that never reach the managed-Dolt
// database resolution: a provider-owned scope, metadata now naming a complete
// storage binding (which init would accept and reload would then publish),
// and metadata now reading as embedded (which init would stamp with the
// default database). Each must be refused before init acts on the rewrite.
func TestReloadBeadsLifecycleRefusesBindingRewrittenDuringStart(t *testing.T) {
	for _, tc := range []struct {
		name, metadata, want string
		providerOwned        bool
	}{
		{name: "provider-owned", want: "(missing)", providerOwned: true},
		{name: "complete storage binding", metadata: hostedMetadata, want: "now names store"},
		{name: "embedded", metadata: `{"backend":"dolt","dolt_mode":"embedded"}`, want: "now names store"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			startScript := `rm -f "$(dirname "$0")/.beads/metadata.json"; `
			var wantMetadata []byte
			if tc.metadata != "" {
				startScript = "printf '%s' '" + tc.metadata + `' > "$(dirname "$0")/.beads/metadata.json"; `
				wantMetadata = []byte(tc.metadata)
			}
			cityPath, callLog := establishedBdCity(t, startScript)
			bdLog := filepath.Join(cityPath, "bd-calls.log")
			fakeBd := filepath.Join(cityPath, "bd")
			if err := os.WriteFile(fakeBd, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \""+bdLog+"\"\nexit 1\n"), 0o755); err != nil { //nolint:gosec // the double must be executable
				t.Fatal(err)
			}
			t.Setenv("BD_BIN", fakeBd)
			if tc.providerOwned {
				if err := persistProviderScopeOwnership(cityPath, cityPath, providerScopeIntent{Transport: "direct", Target: "local"}); err != nil {
					t.Fatal(err)
				}
				if err := markProviderScopeOwnershipReady(cityPath, cityPath); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := loadCityConfig(cityPath, io.Discard)
			if err != nil {
				t.Fatal(err)
			}

			err = cityRuntimeStartBeadsLifecycle(cityPath, "test-city", cfg, recordCityStore(cityPath), io.Discard)

			if calls := providerCalls(t, callLog); len(calls) != 1 || !strings.HasPrefix(calls[0], "start") {
				t.Fatalf("provider calls = %q, want only the start that rewrote the metadata", calls)
			}
			if calls := providerCalls(t, bdLog); len(calls) != 0 {
				t.Fatalf("bd calls = %q, want none against the rewritten store", calls)
			}
			assertNoCityStoreCreated(t, cityPath, callLog, wantMetadata)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("reload lifecycle error = %v, want a refusal containing %q", err, tc.want)
			}
		})
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
	if got, want := recordCityStore(cityPath), (cityStoreRecord{Recorded: true, BdBoot: true, Binding: cityStoreBinding{Backend: "dolt", DoltDatabase: "gci"}, Database: "gci"}); got != want {
		t.Fatalf("recordCityStore() = %+v, want %+v", got, want)
	}
	if err := os.WriteFile(scopeMetadataJSONPath(cityPath), []byte(`{"backend":"dolt"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := recordCityStore(cityPath); !got.Recorded || got.Database != "hq" {
		t.Fatalf("recordCityStore(legacy) = %+v, want recorded with the historical hq default", got)
	}
	// Boot's init reads a numeric dolt_database as its digits; recording it
	// as the legacy hq default would hold reloads to a store boot never used.
	if err := os.WriteFile(scopeMetadataJSONPath(cityPath), []byte(`{"backend":"dolt","dolt_database":123}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, want := recordCityStore(cityPath), (cityStoreRecord{BdBoot: true, Problem: "dolt_database is not a string"}); got != want {
		t.Fatalf("recordCityStore(non-string database) = %+v, want %+v", got, want)
	}
	if err := os.Remove(scopeMetadataJSONPath(cityPath)); err != nil {
		t.Fatal(err)
	}
	if got, want := recordCityStore(cityPath), (cityStoreRecord{BdBoot: true, Problem: "missing"}); got != want {
		t.Fatalf("recordCityStore(no metadata) = %+v, want %+v", got, want)
	}
}

// TestBootCityStoreIsRecordedByBootNotByRuntimeConstruction pins that the
// runtime takes the binding the boot lifecycle left behind: metadata lost
// between boot and runtime construction must not leave the city unrecorded.
func TestBootCityStoreIsRecordedByBootNotByRuntimeConstruction(t *testing.T) {
	cityPath, _ := establishedBdCity(t, "")
	// A complete storage binding boots without a provider op, so the boot
	// lifecycle runs to its end against the stub.
	if err := os.WriteFile(scopeMetadataJSONPath(cityPath), []byte(hostedMetadata), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bootCityStores.Delete(filepath.Clean(cityPath)) })
	if err := startBeadsLifecycle(cityPath, "test-city", cfg, io.Discard); err != nil {
		t.Fatalf("boot lifecycle: %v", err)
	}
	if err := os.Remove(scopeMetadataJSONPath(cityPath)); err != nil {
		t.Fatal(err)
	}
	want := cityStoreRecord{Recorded: true, BdBoot: true, Binding: cityStoreBinding{Backend: "postgres", StorageEndpoint: "opaque", StorageDatabase: "work"}}
	if got := bootCityStoreFor(cityPath); got != want {
		t.Fatalf("bootCityStoreFor() = %+v, want the binding recorded at boot %+v", got, want)
	}
}

// TestReloadNormalizationNeverStampsDefaultCityDatabase covers the
// lifecycle's last metadata write: on reload it must take the booted
// database, and refuse rather than stamp a missing city metadata.json with
// the default name.
func TestReloadNormalizationNeverStampsDefaultCityDatabase(t *testing.T) {
	cityPath, _ := establishedBdCity(t, "")
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	rec := recordCityStore(cityPath)
	if err := os.Remove(scopeMetadataJSONPath(cityPath)); err != nil {
		t.Fatal(err)
	}
	if err := normalizeCanonicalBdScopeFilesResolvingCityDatabase(cityPath, cfg, &rec, io.Discard); err == nil {
		t.Fatal("reload normalization succeeded with the city's metadata.json missing")
	}
	if _, err := os.Stat(scopeMetadataJSONPath(cityPath)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("reload normalization wrote city metadata (stat err = %v), want none", err)
	}

	// Boot keeps its fallback: with no boot record the same call stamps hq.
	if err := normalizeCanonicalBdScopeFilesResolvingCityDatabase(cityPath, cfg, nil, io.Discard); err != nil {
		t.Fatalf("boot normalization: %v", err)
	}
	if data, _ := os.ReadFile(scopeMetadataJSONPath(cityPath)); !strings.Contains(string(data), `"hq"`) {
		t.Fatalf("boot normalization metadata = %s, want the historical hq default (the fallback reload must avoid)", data)
	}
}

// TestCityRuntimeSameRevisionReloadDoesNotAdoptCityStoreChange covers a plain
// gc reload with no config change: changed store metadata used to be adopted
// live, swapping the city store, with no lifecycle (and so no guard) on the
// way.
func TestCityRuntimeSameRevisionReloadDoesNotAdoptCityStoreChange(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"missing", "", restoreBeforeRestart},
		{"gc hq stub", hqStubMetadata, restartToAdopt},
		{"postgres flip", `{"backend":"postgres","storage_endpoint":"postgres://bd@db.example.test:5432","storage_database":"beads_pg"}`, restartToAdopt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, callLog := establishedBdCity(t, "")
			stubManagedDoltStoreOpeners(t)
			cfg, configRev := loadCityRuntimeControllerConfig(t, cityPath)
			sp := runtime.NewFake()
			var stdout, stderr bytes.Buffer
			cr := newTestCityRuntime(t, CityRuntimeParams{
				CityPath:  cityPath,
				CityName:  "test-city",
				TomlPath:  filepath.Join(cityPath, "city.toml"),
				LogPrefix: "gc reload",
				ConfigRev: configRev,
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
			cr.setControllerState(cs)
			cr.sessionDrains = newDrainTracker()
			before := cs.CityBeadStore()
			if tc.body == "" {
				err := os.Remove(scopeMetadataJSONPath(cityPath))
				if err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(scopeMetadataJSONPath(cityPath), []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			lastProviderName := "fake"
			reply := cr.reloadConfigTraced(context.Background(), &lastProviderName, cityPath, nil, reloadSourceManual)

			if cr.configRev != configRev {
				t.Fatalf("configRev = %q, want the unchanged %q (this is the same-revision path)", cr.configRev, configRev)
			}
			if cs.CityBeadStore() != before {
				t.Fatal("same-revision reload swapped the city bead store")
			}
			if reply.Outcome != reloadOutcomeFailed || !strings.Contains(reply.Error, tc.want) {
				t.Fatalf("reply = %q / %q, want failed with %q", reply.Outcome, reply.Error, tc.want)
			}
			if calls := providerCalls(t, callLog); len(calls) != 0 {
				t.Fatalf("provider calls = %q, want none", calls)
			}
		})
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
