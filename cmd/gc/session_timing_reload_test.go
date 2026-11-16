package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessiontmux "github.com/gastownhall/gascity/internal/runtime/tmux"
)

// The expectations are the operator contract, independent of the builder's
// observer/table implementation. Each single-key case also rejects extra rows.
var sessionTimingReloadCases = []struct {
	key, field, edit, running, onDisk string
}{
	{"setup_timeout", "SetupTimeout", `setup_timeout = "60s"`, "10s", "1m0s"},
	{"setup_max_timeout", "SetupMaxTimeout", `setup_max_timeout = "2m"`, "0s", "2m0s"},
	{"nudge_ready_timeout", "NudgeReadyTimeout", `nudge_ready_timeout = "20s"`, "10s", "20s"},
	{"nudge_retry_interval", "NudgeRetryInterval", `nudge_retry_interval = "750ms"`, "500ms", "750ms"},
	{"nudge_lock_timeout", "NudgeLockTimeout", `nudge_lock_timeout = "45s"`, "30s", "45s"},
	{"debounce_ms", "DebounceMs", `debounce_ms = 800`, "500", "800"},
	{"display_ms", "DisplayMs", `display_ms = 6000`, "5000", "6000"},
}

type sessionTimingReloadFixture struct {
	t                *testing.T
	cityPath         string
	cr               *CityRuntime
	provider         *runtime.Fake
	lastProviderName string
	stderr           bytes.Buffer
}

func newSessionTimingReloadFixture(t *testing.T, provider, session string) *sessionTimingReloadFixture {
	t.Helper()
	// A reload that carries a validation warning (the invalid-duration case)
	// is refused under strict mode, and other tests leave the package-level
	// switch on.
	oldStrict := strictMode
	strictMode = false
	t.Cleanup(func() { strictMode = oldStrict })
	f := &sessionTimingReloadFixture{t: t, cityPath: t.TempDir(), provider: runtime.NewFake(), lastProviderName: provider}
	f.write(provider, session, "")
	f.cr, _, _ = newPublicationTestRuntime(t, f.cityPath, f.provider, io.Discard, &f.stderr)
	return f
}

func (f *sessionTimingReloadFixture) write(provider, session, extra string) {
	f.t.Helper()
	path := filepath.Join(f.cityPath, "city.toml")
	writeCityRuntimeConfig(f.t, path, provider)
	data, err := os.ReadFile(path)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, []byte("\n"+session+"\n"+extra)...), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *sessionTimingReloadFixture) reload(want reloadOutcome) reloadControlReply {
	f.t.Helper()
	f.stderr.Reset()
	reply := f.cr.reloadConfigTraced(context.Background(), &f.lastProviderName, f.cityPath, nil, reloadSourceManual)
	if reply.Outcome != want {
		f.t.Fatalf("reload = %+v, want outcome %s; stderr=%s", reply, want, f.stderr.String())
	}
	return reply
}

func sessionTimingWarnings(reply reloadControlReply) []string {
	var result []string
	for _, w := range reply.Warnings {
		if strings.HasPrefix(w, "pending restart: ") && strings.Contains(w, "[session]") {
			result = append(result, w)
		}
	}
	return result
}

func (f *sessionTimingReloadFixture) notice(reply reloadControlReply, key, running, onDisk string) string {
	f.t.Helper()
	warnings := sessionTimingWarnings(reply)
	if len(warnings) != 1 {
		f.t.Errorf("reload warnings = %q, want exactly one pending restart notice for [session] %s (running %s, on disk %s)", reply.Warnings, key, running, onDisk)
		return ""
	}
	w := warnings[0]
	for _, part := range []string{key, running, onDisk, "not active until the supervisor restarts"} {
		if !strings.Contains(w, part) {
			f.t.Errorf("notice %q does not contain %q", w, part)
		}
	}
	if !strings.Contains(f.stderr.String(), w) {
		f.t.Errorf("notice missing from supervisor stderr: %q", f.stderr.String())
	}
	return w
}

func (f *sessionTimingReloadFixture) noNotice(reply reloadControlReply) {
	f.t.Helper()
	if warnings := sessionTimingWarnings(reply); len(warnings) != 0 {
		f.t.Errorf("unexpected timing notice: %q", warnings)
	}
	if strings.Contains(f.stderr.String(), "pending restart: [session]") {
		f.t.Errorf("unexpected timing notice on stderr: %q", f.stderr.String())
	}
}

func TestReloadSessionTimingEachKey(t *testing.T) {
	for _, tc := range sessionTimingReloadCases {
		t.Run(tc.key, func(t *testing.T) {
			f := newSessionTimingReloadFixture(t, "fake", "")
			f.noNotice(f.reload(reloadOutcomeNoChange))
			f.write("fake", tc.edit, "")
			w := f.notice(f.reload(reloadOutcomeApplied), tc.key, tc.running, tc.onDisk)
			for _, other := range sessionTimingReloadCases {
				if other.key != tc.key && strings.Contains(w, other.key) {
					t.Errorf("single-key notice %q also names %s", w, other.key)
				}
			}
		})
	}
}

func TestReloadSessionTimingAllKeysInOrder(t *testing.T) {
	f := newSessionTimingReloadFixture(t, "fake", "")
	var edits []string
	for _, tc := range sessionTimingReloadCases {
		edits = append(edits, tc.edit)
	}
	f.write("fake", strings.Join(edits, "\n"), "")
	reply := f.reload(reloadOutcomeApplied)
	w := f.notice(reply, "setup_timeout", "10s", "1m0s")
	previous := -1
	for _, tc := range sessionTimingReloadCases {
		pos := strings.Index(w, tc.key)
		if pos <= previous || strings.Count(w, tc.key) != 1 {
			t.Errorf("notice %q: %s must appear once in declared key order", w, tc.key)
		}
		previous = pos
		// Restrict value assertions to this key's row: several keys share values.
		row := strings.SplitN(strings.TrimPrefix(w[max(0, pos):], tc.key), ";", 2)[0]
		for _, value := range []string{tc.running, tc.onDisk} {
			if !strings.Contains(row, value) {
				t.Errorf("row for %s = %q, want value %s", tc.key, row, value)
			}
		}
	}
}

func TestReloadSessionTimingRepeatsAndReverts(t *testing.T) {
	f := newSessionTimingReloadFixture(t, "fake", `setup_timeout = "2s"`)
	f.write("fake", `setup_timeout = "10s"`, "")
	f.notice(f.reload(reloadOutcomeApplied), "setup_timeout", "2s", "10s")
	f.write("fake", `setup_timeout = "10s"`, "[daemon]\nshutdown_timeout = \"3s\"\n")
	f.notice(f.reload(reloadOutcomeApplied), "setup_timeout", "2s", "10s")
	if f.cr.cfg.Daemon.ShutdownTimeout != "3s" {
		t.Fatal("unrelated config change was not applied")
	}
	same := f.reload(reloadOutcomeNoChange)
	if same.Message != "No config changes detected." {
		t.Errorf("same revision message = %q", same.Message)
	}
	f.notice(same, "setup_timeout", "2s", "10s")
	f.write("fake", `setup_timeout = "2s"`, "")
	f.noNotice(f.reload(reloadOutcomeApplied))
}

func TestReloadSessionTimingResolvedAndLiveControls(t *testing.T) {
	for _, tc := range []struct{ name, session, extra string }{
		{"explicit defaults", "setup_timeout = \"10s\"\nsetup_max_timeout = \"0s\"\nnudge_ready_timeout = \"10s\"\nnudge_retry_interval = \"500ms\"\nnudge_lock_timeout = \"30s\"\ndebounce_ms = 500\ndisplay_ms = 5000", ""},
		{"equivalent spellings", "setup_timeout = \"10000ms\"\nnudge_ready_timeout = \"10000ms\"\nnudge_retry_interval = \"0.5s\"\nnudge_lock_timeout = \"30000ms\"", ""},
		{"invalid duration resolves to default", `setup_timeout = "not-a-duration"`, ""},
		{"startup stays live", `startup_timeout = "90s"`, ""},
		{"nudge poll stays per spawn", `nudge_poll_interval = "3s"`, ""},
		{"unrelated agent", "", "[[agent]]\nname = \"probe\"\nstart_command = \"sleep 3600\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSessionTimingReloadFixture(t, "fake", "")
			f.write("fake", tc.session, tc.extra)
			reply := f.reload(reloadOutcomeApplied)
			f.noNotice(reply)
			if tc.name == "invalid duration resolves to default" && len(reply.Warnings) == 0 {
				t.Error("fixture should load with a duration validation warning")
			}
			if tc.name == "startup stays live" && f.cr.cfg.Session.StartupTimeoutDuration() != 90*time.Second {
				t.Error("startup_timeout did not apply live")
			}
			if tc.name == "nudge poll stays per spawn" && f.cr.cfg.Session.NudgePollIntervalDuration() != 3*time.Second {
				t.Error("nudge_poll_interval did not reach the current config")
			}
		})
	}
	// Positive control prevents a notice implementation that always stays silent.
	f := newSessionTimingReloadFixture(t, "fake", "")
	f.write("fake", `debounce_ms = 0`, "")
	f.notice(f.reload(reloadOutcomeApplied), "debounce_ms", "500", "0")
}

func TestReloadSessionTimingFailedLoadKeepsBaseline(t *testing.T) {
	for _, tc := range []struct{ name, session, extra string }{
		{"syntax error", "setup_timeout = [", ""},
		{"decode error", `debounce_ms = "invalid-type"`, ""},
		{"validation error", `setup_timeout = "10s"`, "[[agent]]\nname = \"probe\"\n[[agent]]\nname = \"probe\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSessionTimingReloadFixture(t, "fake", `setup_timeout = "2s"`)
			oldCfg, oldSP := f.cr.cfg, f.cr.sp
			f.write("fake", tc.session, tc.extra)
			f.noNotice(f.reload(reloadOutcomeFailed))
			if f.cr.cfg != oldCfg || f.cr.sp != oldSP {
				t.Fatal("failed reload changed config or provider")
			}
			f.write("fake", `setup_timeout = "10s"`, "")
			f.notice(f.reload(reloadOutcomeApplied), "setup_timeout", "2s", "10s")
		})
	}
}

func TestReloadSessionTimingStorageRefusalKeepsBaseline(t *testing.T) {
	f := newSessionTimingReloadFixture(t, "fake", `setup_timeout = "2s"`)
	oldCfg := f.cr.cfg
	writeSplitCityConfig(t, filepath.Join(f.cityPath, "city.toml"), filepath.Join(t.TempDir(), "infra"), "")
	data, err := os.ReadFile(filepath.Join(f.cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte("provider = \"fake\""), []byte("provider = \"fake\"\nsetup_timeout = \"10s\""), 1)
	if err := os.WriteFile(filepath.Join(f.cityPath, "city.toml"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	reply := f.reload(reloadOutcomeFailed)
	f.noNotice(reply)
	if !strings.Contains(reply.Error, "[storage]") || f.cr.cfg != oldCfg {
		t.Fatalf("fixture did not retain its boot config on storage refusal: %+v", reply)
	}
	f.write("fake", `setup_timeout = "20s"`, "")
	f.notice(f.reload(reloadOutcomeApplied), "setup_timeout", "2s", "20s")
}

func TestReloadSessionTimingSupersededPublicationKeepsBaseline(t *testing.T) {
	f := newSessionTimingReloadFixture(t, "fake", `setup_timeout = "2s"`)
	provider := &rewriteConfigOnListProvider{Fake: f.provider}
	f.cr, _, _ = newPublicationTestRuntime(t, f.cityPath, provider, io.Discard, &f.stderr)
	stubSessionProviderBuilds(t, map[string]runtime.Provider{"fail": runtime.NewFake()})
	stubReloadBeadsLifecycle(t, func(string, string, *config.City, cityStoreRecord, io.Writer) error { return nil })
	f.write("fail", `setup_timeout = "10s"`, "")
	provider.rewrite = func() { f.write("fake", `setup_timeout = "20s"`, "") }
	reply := f.reload(reloadOutcomeFailed)
	f.noNotice(reply)
	if !strings.Contains(reply.Error, "superseded during runtime publication") || f.cr.sp != provider {
		t.Fatalf("fixture did not reject the superseded provider publication: %+v", reply)
	}
	f.notice(f.reload(reloadOutcomeApplied), "setup_timeout", "2s", "20s")
}

func TestReloadSessionTimingOrdersRescanFailureDoesNotWarn(t *testing.T) {
	f := newSessionTimingReloadFixture(t, "fake", `setup_timeout = "2s"`)
	f.write("fake", `setup_timeout = "10s"`, "")
	f.notice(f.reload(reloadOutcomeApplied), "setup_timeout", "2s", "10s")
	// Keep the revision unchanged and inject a hard scan failure at the
	// existing seam. Individual malformed orders are only soft scan warnings.
	f.cr.orderSetScan = func(string, *config.City, string) (orderSetSnapshot, error) {
		return orderSetSnapshot{}, errors.New("test order scan failure")
	}
	f.cr.orderRescanEnabled = true
	oldCfg := f.cr.cfg
	f.noNotice(f.reload(reloadOutcomeFailed))
	if f.cr.cfg != oldCfg {
		t.Fatal("failed same-revision order scan changed the running config")
	}
	f.cr.orderSetScan = nil
	f.notice(f.reload(reloadOutcomeNoChange), "setup_timeout", "2s", "10s")
}

func TestReloadSessionTimingPackDeclarationRelatches(t *testing.T) {
	f := &sessionTimingReloadFixture{t: t, cityPath: writeRuntimeCityFixture(t, "custom"), provider: runtime.NewFake(), lastProviderName: "custom"}
	imports := "[imports.rtpack]\nsource = \"packs/rtpack\"\n"
	f.write("custom", `setup_timeout = "2s"`, imports)
	f.cr, _, _ = newPublicationTestRuntime(t, f.cityPath, f.provider, io.Discard, &f.stderr)
	next := runtime.NewFake()
	stubSessionProviderBuilds(t, map[string]runtime.Provider{"custom": next})
	packPath := filepath.Join(f.cityPath, "packs", "rtpack", "pack.toml")
	data, err := os.ReadFile(packPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(packPath, bytes.Replace(data, []byte("scripts/provider.sh"), []byte("scripts/new-provider.sh"), 1), 0o644); err != nil {
		t.Fatal(err)
	}
	f.write("custom", `setup_timeout = "10s"`, imports)
	f.noNotice(f.reload(reloadOutcomeApplied))
	if sessionProviderLegs(f.cr.sp).base != next {
		t.Fatal("changed runtime declaration did not publish a new base")
	}
	f.write("custom", `setup_timeout = "20s"`, imports)
	f.notice(f.reload(reloadOutcomeApplied), "setup_timeout", "10s", "20s")
}

func TestReloadSessionTimingNewBaseRelatches(t *testing.T) {
	f := newSessionTimingReloadFixture(t, "fake", `setup_timeout = "2s"`)
	next := runtime.NewFake()
	stubSessionProviderBuilds(t, map[string]runtime.Provider{"fail": next})
	f.write("fail", `setup_timeout = "10s"`, "")
	f.noNotice(f.reload(reloadOutcomeApplied))
	if sessionProviderLegs(f.cr.sp).base != next || f.lastProviderName != "fail" {
		t.Fatal("fixture did not publish the new base provider")
	}
	f.write("fail", `setup_timeout = "20s"`, "")
	f.notice(f.reload(reloadOutcomeApplied), "setup_timeout", "10s", "20s")
}

func TestReloadSessionTimingCarriedBaseKeepsBaseline(t *testing.T) {
	f := newSessionTimingReloadFixture(t, "fake", `setup_timeout = "2s"`)
	stubSessionProviderBuilds(t, map[string]runtime.Provider{"acp": runtime.NewFake()})
	f.write("fake", `setup_timeout = "10s"`, "[[agent]]\nname = \"probe\"\nsession = \"acp\"\n")
	f.notice(f.reload(reloadOutcomeApplied), "setup_timeout", "2s", "10s")
	if f.cr.sp == f.provider || sessionProviderLegs(f.cr.sp).base != f.provider {
		t.Fatal("fixture must recompose the wrapper and carry its original base")
	}
	// Removing the composition also carries the same base.
	f.write("fake", `setup_timeout = "20s"`, "")
	f.notice(f.reload(reloadOutcomeApplied), "setup_timeout", "2s", "20s")
	if sessionProviderLegs(f.cr.sp).base != f.provider {
		t.Fatal("composition removal replaced the base")
	}
}

func TestReloadSessionTimingFailedBuildKeepsBaseline(t *testing.T) {
	f := newSessionTimingReloadFixture(t, "fake", `setup_timeout = "2s"`)
	oldBuild := buildSessionProviderByName
	buildSessionProviderByName = func(_ *config.City, name string, _ config.SessionConfig, _, _ string) (runtime.Provider, error) {
		return nil, fmt.Errorf("test build failure for %s", name)
	}
	t.Cleanup(func() { buildSessionProviderByName = oldBuild })
	f.write("fail", `setup_timeout = "10s"`, "")
	reply := f.reload(reloadOutcomeApplied)
	f.notice(reply, "setup_timeout", "2s", "10s")
	if !strings.Contains(strings.Join(reply.Warnings, "\n"), `new session provider "fail": test build failure for fail (keeping old provider)`) {
		t.Errorf("missing existing provider build warning: %+v", reply)
	}
	if f.cr.sp != f.provider || f.lastProviderName != "fake" {
		t.Fatal("failed build changed the running provider")
	}
}

func TestReloadSessionTimingFailedSwapKeepsBaseline(t *testing.T) {
	f := newSessionTimingReloadFixture(t, "fake", `setup_timeout = "2s"`)
	listing := &sessionTimingListFailureProvider{Fake: f.provider, err: errors.New("test listing failure")}
	f.cr, _, _ = newPublicationTestRuntime(t, f.cityPath, listing, io.Discard, &f.stderr)
	stubSessionProviderBuilds(t, map[string]runtime.Provider{"fail": runtime.NewFake()})
	f.write("fail", `setup_timeout = "10s"`, "")
	f.noNotice(f.reload(reloadOutcomeFailed))
	if f.cr.sp != listing || f.cr.cfg.Session.SetupTimeout != "2s" {
		t.Fatal("failed swap published config or provider")
	}
	listing.err = nil
	f.write("fake", `setup_timeout = "20s"`, "")
	f.notice(f.reload(reloadOutcomeApplied), "setup_timeout", "2s", "20s")
}

type sessionTimingListFailureProvider struct {
	*runtime.Fake
	err error
}

func (p *sessionTimingListFailureProvider) ListRunning(prefix string) ([]string, error) {
	if p.err != nil {
		return nil, p.err
	}
	return p.Fake.ListRunning(prefix)
}

func TestReloadSessionTimingFreshRuntimeHasNewBaseline(t *testing.T) {
	f := newSessionTimingReloadFixture(t, "subprocess", `setup_timeout = "10s"`)
	f.noNotice(f.reload(reloadOutcomeNoChange))
	f.write("subprocess", `setup_timeout = "20s"`, "")
	f.notice(f.reload(reloadOutcomeApplied), "setup_timeout", "10s", "20s")
}

func TestReloadSessionTimingDoesNotRestartActiveSession(t *testing.T) {
	f := newSessionTimingReloadFixture(t, "fake", "")
	store := f.cr.sessionsBeadStore().Store
	sess, err := store.Create(beads.Bead{Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
		"session_name": "active-probe", "template": "probe", "state": "active", "session_key": "stable-provider-key",
	}})
	if err != nil {
		t.Fatal(err)
	}
	work, err := store.Create(beads.Bead{Type: "task", Assignee: sess.ID})
	if err != nil {
		t.Fatal(err)
	}
	inProgress := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatal(err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if work.Status != inProgress || work.Assignee != sess.ID {
		t.Fatalf("fixture did not create assigned in-flight work: %+v", work)
	}
	if err := f.provider.Start(context.Background(), "active-probe", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	before := len(f.provider.Calls)
	f.write("fake", `setup_timeout = "20s"`, "")
	f.reload(reloadOutcomeApplied)
	if f.cr.sp != f.provider || !f.provider.IsRunning("active-probe") {
		t.Fatal("timing edit replaced the provider or stopped the active runtime")
	}
	for _, call := range f.provider.Calls[before:] {
		switch call.Method {
		case "Stop", "Start", "Relaunch", "Interrupt":
			t.Errorf("timing-only reload caused lifecycle operation: %+v", call)
		}
	}
	store = f.cr.sessionsBeadStore().Store
	gotSession, err := store.Get(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range sess.Metadata {
		if gotSession.Metadata[key] != value {
			t.Errorf("timing-only reload changed session %s identity/state %s: %q -> %q", sess.ID, key, value, gotSession.Metadata[key])
		}
	}
	gotWork, err := store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotWork.Status != work.Status || gotWork.Assignee != work.Assignee {
		t.Errorf("timing-only reload changed in-flight work: before=%+v after=%+v", work, gotWork)
	}
}

func TestSessionTimingReloadProviderFieldsHaveCoverage(t *testing.T) {
	// Non-timing construction-time fields: the program-status path, mode and
	// comparison hooks are not among the seven resolved [session] timing values.
	fields := map[string]bool{
		"NudgeIdleTimeout": true, "SocketName": true, "RuntimeDir": true,
		"ProgramStatusBin": true, "ProgramStatus": true,
		"ComparisonSink": true, "ComparisonEnabled": true, "ComparisonFollowUps": true, "ComparisonWork": true,
	}
	for _, tc := range sessionTimingReloadCases {
		fields[tc.field] = true
	}
	typ := reflect.TypeOf(sessiontmux.Config{})
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if !fields[name] {
			t.Errorf("tmux.Config.%s needs a session timing reload case or an explicit non-timing exclusion", name)
		}
		delete(fields, name)
	}
	for name := range fields {
		t.Errorf("session timing reload coverage names missing tmux.Config field %s", name)
	}
}
