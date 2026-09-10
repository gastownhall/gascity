package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// clearedRestartProvider lets the pre-prompt implementation finish its
// synchronous wait without a patrol loop. It preserves every runtime call for
// assertions while reporting that an already-stopped runtime has no live
// restart flag to wait on.
type clearedRestartProvider struct {
	*runtime.Fake
}

func (p *clearedRestartProvider) GetMeta(name, key string) (string, error) {
	value, err := p.Fake.GetMeta(name, key)
	if key == "GC_RESTART_REQUESTED" {
		return "", err
	}
	return value, err
}

type explicitRestartFixture struct {
	cityPath   string
	sessionID  string
	sessionKey string
	store      beads.Store
	provider   *runtime.Fake
}

func newExplicitRestartFixture(t *testing.T) explicitRestartFixture {
	t.Helper()
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	t.Setenv("GC_SESSION", "fake")
	t.Setenv("GC_HOME", filepath.Join(t.TempDir(), "gc-home"))

	cityPath := shortSocketTempDir(t, "gc-explicit-restart-")
	writeCityTOML(t, cityPath, "test-city", "worker")
	writeBuiltinImportsFixture(t, cityPath, "core")
	t.Setenv("GC_CITY", cityPath)
	t.Setenv("GC_CITY_PATH", cityPath)
	t.Setenv("GC_CEILING_DIRECTORIES", filepath.Dir(cityPath))

	const sessionKey = "test-city--worker"
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	created, err := store.Create(beads.Bead{
		Title:  "explicit restart target",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession, "template:worker"},
		Metadata: map[string]string{
			"alias":        "worker",
			"agent_name":   "worker",
			"template":     "worker",
			"session_name": sessionKey,
			"state":        "awake",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}

	t.Setenv("GC_ALIAS", "worker")
	t.Setenv("GC_AGENT", "worker")
	t.Setenv("GC_SESSION_ID", created.ID)
	t.Setenv("GC_SESSION_NAME", sessionKey)
	t.Setenv("GC_TMUX_SESSION", sessionKey)

	provider := runtime.NewFake()
	wrapped := &clearedRestartProvider{Fake: provider}
	oldBuild := buildSessionProviderByName
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		return wrapped, nil
	}
	t.Cleanup(func() { buildSessionProviderByName = oldBuild })

	return explicitRestartFixture{
		cityPath:   cityPath,
		sessionID:  created.ID,
		sessionKey: sessionKey,
		store:      store,
		provider:   provider,
	}
}

func TestExplicitRestartCommandsPokeControllerWithoutPeriodicTick(t *testing.T) {
	tests := []struct {
		name string
		run  func(stdout, stderr *bytes.Buffer) int
	}{
		{
			name: "runtime request-restart",
			run: func(stdout, stderr *bytes.Buffer) int {
				return cmdRuntimeRequestRestart(stdout, stderr)
			},
		},
		{
			name: "self handoff",
			run: func(stdout, stderr *bytes.Buffer) int {
				return cmdHandoff([]string{"context cycle", "resume from the durable brief"}, "", false, "", stdout, stderr)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newExplicitRestartFixture(t)
			controllerSignal := startFakeControllerSocket(t, fixture.cityPath, "ok\n")
			var stdout, stderr bytes.Buffer

			if code := tc.run(&stdout, &stderr); code != 0 {
				t.Fatalf("command exit = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			assertExplicitRestartPersisted(t, fixture)
			select {
			case <-controllerSignal:
			default:
				t.Fatal("explicit restart returned without signaling the controller")
			}
			assertNoDirectRestartSideEffects(t, fixture)
			if got := strings.ToLower(stdout.String()); strings.Contains(got, "waiting") {
				t.Fatalf("stdout = %q, explicit restart must return asynchronously instead of waiting for a patrol tick", stdout.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty on accepted restart", stderr.String())
			}
		})
	}
}

func TestExplicitRestartControllerUnavailableIsTruthfulAndDurable(t *testing.T) {
	fixture := newExplicitRestartFixture(t)
	var stdout, stderr bytes.Buffer

	if code := cmdRuntimeRequestRestart(&stdout, &stderr); code != 1 {
		t.Fatalf("command exit = %d, want 1 when no controller can be signaled; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	assertExplicitRestartPersisted(t, fixture)
	assertNoDirectRestartSideEffects(t, fixture)
	assertRestartSignalFailure(t, stderr.String())
}

func TestExplicitRestartSignalFailureIsTruthfulAndDurable(t *testing.T) {
	fixture := newExplicitRestartFixture(t)
	accepted := startFakeControllerSocket(t, fixture.cityPath, "")
	var stdout, stderr bytes.Buffer

	if code := cmdRuntimeRequestRestart(&stdout, &stderr); code != 1 {
		t.Fatalf("command exit = %d, want 1 when the controller drops the signal reply; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	select {
	case <-accepted:
	default:
		t.Fatal("controller never accepted the explicit restart signal")
	}
	assertExplicitRestartPersisted(t, fixture)
	assertNoDirectRestartSideEffects(t, fixture)
	assertRestartSignalFailure(t, stderr.String())
}

func TestExplicitRestartRepeatedRequestIsIdempotentAndRepokes(t *testing.T) {
	fixture := newExplicitRestartFixture(t)
	controllerSignal := startFakeControllerSocket(t, fixture.cityPath, "ok\n")

	for attempt := 1; attempt <= 2; attempt++ {
		var stdout, stderr bytes.Buffer
		if code := cmdRuntimeRequestRestart(&stdout, &stderr); code != 0 {
			t.Fatalf("attempt %d exit = %d, want 0; stdout=%q stderr=%q", attempt, code, stdout.String(), stderr.String())
		}
		assertExplicitRestartPersisted(t, fixture)
		select {
		case <-controllerSignal:
		default:
			t.Fatalf("attempt %d returned without signaling the controller", attempt)
		}
	}

	assertNoDirectRestartSideEffects(t, fixture)
}

func assertExplicitRestartPersisted(t *testing.T, fixture explicitRestartFixture) {
	t.Helper()
	got, err := fixture.store.Get(fixture.sessionID)
	if err != nil {
		t.Fatalf("get session bead: %v", err)
	}
	if got.Metadata["restart_requested"] != "true" {
		t.Fatalf("restart_requested = %q, want true", got.Metadata["restart_requested"])
	}
	if got.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("continuation_reset_pending = %q, want true", got.Metadata["continuation_reset_pending"])
	}
}

func assertNoDirectRestartSideEffects(t *testing.T, fixture explicitRestartFixture) {
	t.Helper()
	for _, call := range fixture.provider.SnapshotCalls() {
		switch call.Method {
		case "Stop", "Kill", "Start", "Relaunch":
			t.Fatalf("explicit restart performed %s directly; the controller must retain lifecycle authority", call.Method)
		}
	}
}

func assertRestartSignalFailure(t *testing.T, diagnostic string) {
	t.Helper()
	got := strings.ToLower(diagnostic)
	if !strings.Contains(got, "controller") {
		t.Fatalf("stderr = %q, want controller signal failure", diagnostic)
	}
	if !strings.Contains(got, "restart request remains") {
		t.Fatalf("stderr = %q, want durable pending-request diagnostic", diagnostic)
	}
}

func TestExplicitRestartAlreadyStoppedStillPersistsAndPokes(t *testing.T) {
	fixture := newExplicitRestartFixture(t)
	controllerSignal := startFakeControllerSocket(t, fixture.cityPath, "ok\n")
	if fixture.provider.IsRunning(fixture.sessionKey) {
		t.Fatal("fixture runtime unexpectedly running")
	}
	var stdout, stderr bytes.Buffer

	if code := cmdRuntimeRequestRestart(&stdout, &stderr); code != 0 {
		t.Fatalf("command exit = %d, want 0 for already-stopped runtime; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	assertExplicitRestartPersisted(t, fixture)
	select {
	case <-controllerSignal:
	default:
		t.Fatal("already-stopped restart returned without signaling the controller")
	}
	assertNoDirectRestartSideEffects(t, fixture)
}

var _ runtime.Provider = (*clearedRestartProvider)(nil)
