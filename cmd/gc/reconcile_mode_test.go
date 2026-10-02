package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/supervisor"
)

// loadSessionReconcilerWarnings loads a city whose [daemon] sets
// session_reconciler to value and returns the load warnings that name the key.
func loadSessionReconcilerWarnings(t *testing.T, value string) []string {
	t.Helper()
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte("[workspace]\nname = \"test\"\n\n[daemon]\nsession_reconciler = \"" + value + "\"\n")
	_, prov, err := config.LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	var got []string
	for _, w := range prov.Warnings {
		if strings.Contains(w, "session_reconciler") {
			got = append(got, w)
		}
	}
	return got
}

// TestLoadCitySessionReconcilerAliasIsNonFatalInStrictMode pins F4: a city
// that still carries a gc-enterprise spelling (maintainer-city sets "auto")
// boots under strict mode on both deciders, and the deprecation reaches the
// operator.
func TestLoadCitySessionReconcilerAliasIsNonFatalInStrictMode(t *testing.T) {
	for _, value := range []string{"off", "auto", "require"} {
		t.Run(value, func(t *testing.T) {
			warnings := loadSessionReconcilerWarnings(t, value)
			if len(warnings) != 1 || !config.IsSessionReconcilerAliasWarning(warnings[0]) {
				t.Fatalf("session_reconciler warnings = %q, want one alias deprecation", warnings)
			}
			if fatal, nonFatal := splitStrictConfigWarnings(warnings); len(fatal) != 0 || len(nonFatal) != 1 {
				t.Errorf("strict split: fatal=%q nonFatal=%q, want the alias non-fatal", fatal, nonFatal)
			}
			if fatal := strictFatalLoadConfigWarnings(warnings); len(fatal) != 0 {
				t.Errorf("agent strict decider kept the alias fatal: %q", fatal)
			}
			if !shouldEmitLoadCityConfigWarning(warnings[0]) {
				t.Error("the alias deprecation must reach the operator, not be swallowed")
			}
		})
	}
}

// TestLoadCitySessionReconcilerUnknownIsStrictFatal pins the other half: only
// the alias is soft. An unknown value stays fatal under strict mode on both
// deciders, including one that quotes the alias wording to pass as an alias.
func TestLoadCitySessionReconcilerUnknownIsStrictFatal(t *testing.T) {
	for _, value := range []string{"v3", `x is a deprecated gc-enterprise alias for \"legacy\"; remove the key, legacy is the default`} {
		warnings := loadSessionReconcilerWarnings(t, value)
		if len(warnings) != 1 || config.IsSessionReconcilerAliasWarning(warnings[0]) {
			t.Fatalf("session_reconciler = %q warnings = %q, want one unknown-value warning", value, warnings)
		}
		if fatal, _ := splitStrictConfigWarnings(warnings); len(fatal) != 1 {
			t.Errorf("session_reconciler = %q: strict split kept the unknown value non-fatal", value)
		}
		if fatal := strictFatalLoadConfigWarnings(warnings); len(fatal) != 1 {
			t.Errorf("session_reconciler = %q: agent strict decider kept the unknown value non-fatal", value)
		}
	}
	if warnings := loadSessionReconcilerWarnings(t, "legacy"); len(warnings) != 0 {
		t.Errorf("session_reconciler = legacy warned: %q", warnings)
	}
}

// TestLatchReconcilerModeRefusesUnknownAndInadmissibleV2 pins the admission
// gate (F5, OQ-1): v2 has no session controllers in this build, so selecting it
// refuses controller start rather than running a city that starts nothing; an
// unknown value refuses rather than silently running legacy. An alias latches
// legacy.
func TestLatchReconcilerModeRefusesUnknownAndInadmissibleV2(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		wantErr string
	}{
		{raw: ""},
		{raw: "legacy"},
		{raw: "auto"},
		{raw: "v2", wantErr: "not available in this build"},
		{raw: " V2 ", wantErr: "not available in this build"},
		{raw: "v3", wantErr: "not a known value"},
	} {
		cfg := &config.City{Daemon: config.DaemonConfig{SessionReconciler: tc.raw}}
		mode, err := latchReconcilerMode(cfg)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("latch(%q) err = %v, want %q", tc.raw, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("latch(%q) err = %v, want legacy", tc.raw, err)
			continue
		}
		if mode != reconcilerLegacy {
			t.Errorf("latch(%q) = %v, want legacy", tc.raw, mode)
		}
	}
}

// TestReloadSessionReconcilerDriftWarnsOncePerTransition pins the reload half
// of the boot latch: a changed session_reconciler warns "pending restart" once
// per transition, never on every reload, and never re-latches the running mode.
func TestReloadSessionReconcilerDriftWarnsOncePerTransition(t *testing.T) {
	t.Run("transitions", func(t *testing.T) {
		var d reconcilerModeDrift
		for i, step := range []struct {
			onDisk   string
			wantWarn string
		}{
			{onDisk: ""},
			{onDisk: "auto"},
			{onDisk: "v2", wantWarn: "on disk is v2; this controller runs legacy"},
			{onDisk: "V2"},
			{onDisk: "v3", wantWarn: `on disk is "v3" (not a known value); this controller runs legacy`},
			{onDisk: "v3"},
			{onDisk: "legacy"},
			{onDisk: "v2", wantWarn: "on disk is v2; this controller runs legacy"},
			{onDisk: "legacy"},
			{onDisk: "v2", wantWarn: "on disk is v2; this controller runs legacy"},
		} {
			got := d.observe(&config.City{Daemon: config.DaemonConfig{SessionReconciler: step.onDisk}})
			if step.wantWarn == "" && got != "" {
				t.Errorf("step %d (%q): warned %q, want nothing", i, step.onDisk, got)
			}
			if step.wantWarn != "" && (!strings.HasPrefix(got, "pending restart: ") || !strings.Contains(got, step.wantWarn)) {
				t.Errorf("step %d (%q): warning = %q, want %q", i, step.onDisk, got, step.wantWarn)
			}
			// Every value this build refuses says so, so the warning never
			// presents an inadmissible value as merely pending.
			if step.wantWarn != "" && !strings.Contains(got, "; the next controller start will refuse it: ") {
				t.Errorf("step %d (%q): warning = %q, want the start refusal named", i, step.onDisk, got)
			}
			if d.running != reconcilerLegacy {
				t.Fatalf("step %d (%q): reload re-latched the running mode to %v", i, step.onDisk, d.running)
			}
		}
	})

	t.Run("reload", func(t *testing.T) {
		cityPath := t.TempDir()
		tomlPath := filepath.Join(cityPath, "city.toml")
		writeConfig := func(daemon string) {
			t.Helper()
			writeCityRuntimeConfig(t, tomlPath, "fake")
			data, err := os.ReadFile(tomlPath)
			if err != nil {
				t.Fatalf("read config: %v", err)
			}
			if err := os.WriteFile(tomlPath, append(data, []byte("\n[daemon]\n"+daemon)...), 0o644); err != nil {
				t.Fatalf("write config: %v", err)
			}
		}
		writeConfig("shutdown_timeout = \"3s\"\n")
		var stderr bytes.Buffer
		cr, _, _ := newPublicationTestRuntime(t, cityPath, runtime.NewFake(), io.Discard, &stderr)
		reload := func() reloadControlReply {
			t.Helper()
			lastProviderName := "fake"
			return cr.reloadConfigTraced(context.Background(), &lastProviderName, cityPath, nil, reloadSourceManual)
		}
		pending := func(reply reloadControlReply) int {
			n := 0
			for _, w := range reply.Warnings {
				if strings.HasPrefix(w, "pending restart: session_reconciler") {
					n++
				}
			}
			return n
		}

		writeConfig("shutdown_timeout = \"4s\"\nsession_reconciler = \"v2\"\n")
		first := reload()
		if first.Outcome != reloadOutcomeApplied || pending(first) != 1 {
			t.Fatalf("first reload naming v2 = %+v, want applied with one pending-restart warning", first)
		}
		if !strings.Contains(stderr.String(), "pending restart: session_reconciler on disk is v2") {
			t.Errorf("the pending restart was not reported on stderr: %q", stderr.String())
		}

		writeConfig("shutdown_timeout = \"5s\"\nsession_reconciler = \"v2\"\n")
		second := reload()
		if second.Outcome != reloadOutcomeApplied || pending(second) != 0 {
			t.Fatalf("second reload with the same drift = %+v, want applied with no new warning", second)
		}
		if got := cr.cfg.Daemon.ShutdownTimeout; got != "5s" {
			t.Errorf("shutdown_timeout = %q, want 5s; the drifted reload did not apply the rest of the config", got)
		}
		if cr.reconcilerDrift.running != reconcilerLegacy {
			t.Errorf("reload re-latched the running mode to %v", cr.reconcilerDrift.running)
		}
	})
}

// TestDoctorSessionReconcilerCheck pins the doctor severity mapping: every
// value the controller latch refuses is an error, never a warning.
func TestDoctorSessionReconcilerCheck(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want doctor.CheckStatus
	}{
		{raw: "", want: doctor.StatusOK},
		{raw: "legacy", want: doctor.StatusOK},
		{raw: "auto", want: doctor.StatusWarning},
		{raw: "v2", want: doctor.StatusError}, // refused while !v2ControllersInBuild
		{raw: "v3", want: doctor.StatusError},
	} {
		cfg := &config.City{Daemon: config.DaemonConfig{SessionReconciler: tc.raw}}
		r := newSessionReconcilerDoctorCheck(cfg).Run(&doctor.CheckContext{})
		if r.Status != tc.want {
			t.Errorf("session_reconciler = %q: status = %v (%s), want %v", tc.raw, r.Status, r.Message, tc.want)
		}
		if r.Name != "daemon-session-reconciler" {
			t.Errorf("result name = %q, want daemon-session-reconciler", r.Name)
		}
	}
}

// newRefusedSessionReconcilerCity is newForegroundStartLockCity with
// session_reconciler = "v2": its exec bead-store script logs every op it runs,
// so the log is the store spy.
func newRefusedSessionReconcilerCity(t *testing.T) (cityPath, opsLog string) {
	t.Helper()
	cityPath, opsLog = newForegroundStartLockCity(t, "0")
	tomlPath := filepath.Join(cityPath, "city.toml")
	data, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tomlPath, append(data, "\n[daemon]\nsession_reconciler = \"v2\"\n"...), 0o644); err != nil {
		t.Fatal(err)
	}
	return cityPath, opsLog
}

// assertRefusedCityUntouched checks that a refused start ran no bead-store op
// and holds no handle on the city event log. The scaffold creates an empty
// events.jsonl, so the handle check reads this process's descriptor table; it
// is skipped where /proc/self/fd does not exist.
func assertRefusedCityUntouched(t *testing.T, cityPath, opsLog string) {
	t.Helper()
	if ops := readProviderOps(t, opsLog); len(ops) != 0 {
		t.Errorf("bead-store provider ops = %q, want none: a refused city started its bead store", ops)
	}
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Logf("skipping the events.jsonl handle check: %v", err)
		return
	}
	evPath, err := filepath.EvalSymlinks(filepath.Join(cityPath, ".gc", "events.jsonl"))
	if err != nil {
		evPath = filepath.Join(cityPath, ".gc", "events.jsonl")
	}
	for _, fd := range fds {
		if target, err := os.Readlink(filepath.Join("/proc/self/fd", fd.Name())); err == nil && target == evPath {
			t.Errorf("fd %s holds %s open: a refused city opened its event log", fd.Name(), target)
		}
	}
}

// TestStartStandaloneRefusesSessionReconcilerBeforeInit pins the latch's
// position in doStartStandalone: --foreground and --dry-run refuse v2 right
// after the strict-mode check, before the bead store or the event log.
func TestStartStandaloneRefusesSessionReconcilerBeforeInit(t *testing.T) {
	for _, tc := range []struct {
		name       string
		foreground bool
	}{
		{name: "foreground", foreground: true},
		{name: "dry-run"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, opsLog := newRefusedSessionReconcilerCity(t)
			oldDryRun := dryRunMode
			dryRunMode = !tc.foreground
			t.Cleanup(func() { dryRunMode = oldDryRun })

			var stdout, stderr bytes.Buffer
			if code := doStartStandalone([]string{cityPath}, tc.foreground, &stdout, &stderr); code != 1 {
				t.Fatalf("doStartStandalone exit = %d, want 1\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
			}
			if !strings.Contains(stderr.String(), "session_reconciler = \"v2\" is not available in this build") {
				t.Errorf("stderr = %q, want the v2 refusal", stderr.String())
			}
			assertRefusedCityUntouched(t, cityPath, opsLog)
		})
	}
}

// TestReconcileCitiesRefusesSessionReconcilerBeforeInit pins the latch at the
// supervisor entry point: the city create fails with
// session_reconciler_refused, the error is recorded, init status is cleared,
// no managed city is published, and nothing was initialized first.
func TestReconcileCitiesRefusesSessionReconcilerBeforeInit(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	cityPath, opsLog := newRefusedSessionReconcilerCity(t)

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "refused-city"); err != nil {
		t.Fatal(err)
	}
	supRec := events.NewFake()
	registry := newCityRegistry()
	registry.SetSupervisorRecorder(supRec)
	if err := registry.StorePendingRequestID(cityPath, "req-refused"); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	reconcileCities(context.Background(), reg, registry, supervisor.PublicationConfig{}, &stdout, &stderr)

	if len(supRec.Events) != 1 {
		t.Fatalf("recorded %d supervisor events, want 1; stderr=%s", len(supRec.Events), stderr.String())
	}
	var payload api.RequestFailedPayload
	if err := json.Unmarshal(supRec.Events[0].Payload, &payload); err != nil {
		t.Fatalf("json.Unmarshal(payload): %v", err)
	}
	if supRec.Events[0].Type != events.RequestFailed || payload.ErrorCode != "session_reconciler_refused" {
		t.Fatalf("event = %s/%q, want %s/session_reconciler_refused", supRec.Events[0].Type, payload.ErrorCode, events.RequestFailed)
	}
	registry.ReadCallback(func(
		cities map[string]*managedCity,
		initStatus map[string]cityInitProgress,
		initFailures map[string]*initFailRecord,
		_ map[string]*panicRecord,
	) {
		if mc := cities[cityPath]; mc != nil {
			t.Errorf("refused city was published as managed: %+v", mc)
		}
		if st, ok := initStatus[cityPath]; ok {
			t.Errorf("initStatus = %+v after refusal, want cleared", st)
		}
		rec := initFailures[cityPath]
		if rec == nil || !strings.Contains(rec.lastError, "not available in this build") {
			t.Errorf("initFailures = %+v, want the v2 refusal recorded", rec)
		}
	})
	assertRefusedCityUntouched(t, cityPath, opsLog)
}

// TestReloadSessionReconcilerDriftFollowsLatchedMode pins that the runtime's
// drift tracker runs the mode its controller latched, not its zero value. No
// controller latches v2 in this build, so the test hands the mode in directly.
func TestReloadSessionReconcilerDriftFollowsLatchedMode(t *testing.T) {
	cityPath := t.TempDir()
	writeCityRuntimeConfig(t, filepath.Join(cityPath, "city.toml"), "fake")
	cfg, rev := loadCityRuntimeControllerConfig(t, cityPath)
	sp := runtime.NewFake()
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath:       cityPath,
		CityName:       "test-city",
		TomlPath:       filepath.Join(cityPath, "city.toml"),
		ConfigRev:      rev,
		Cfg:            cfg,
		ReconcilerMode: reconcilerV2,
		SP:             sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	got := cr.reconcilerDrift.observe(&config.City{})
	if !strings.Contains(got, "on disk is legacy; this controller runs v2") {
		t.Fatalf("drift warning for a legacy candidate = %q, want it measured against the latched v2", got)
	}
}
