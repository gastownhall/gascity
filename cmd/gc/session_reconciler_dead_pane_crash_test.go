package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// deadPaneProvider is a fake tmux backend over runtime.Fake that can hold a
// remain-on-exit corpse under a name: the name stays listed (its metadata
// and pane text readable), its liveness reads Corpse with the dead object's
// id, IsRunning answers false as tmux's state cache does for a pane_dead
// session, and a Start over it replaces it as tmux's ensureFreshSession
// does.
type deadPaneProvider struct {
	*runtime.Fake
	corpses  map[string]runtime.Liveness
	obsErr   error // every liveness read fails with it when set
	startErr error // every Start fails with it, the corpse left listed, when set
	peekErr  error // every pane capture fails with it when set
}

func newDeadPaneProvider(f *runtime.Fake) *deadPaneProvider {
	return &deadPaneProvider{Fake: f, corpses: make(map[string]runtime.Liveness)}
}

// kill turns name's runtime into a corpse of the dead object id.
func (p *deadPaneProvider) kill(name, objectID string) {
	p.corpses[name] = runtime.Liveness{Corpse: true, ObjectID: objectID, ObjectCreated: "1700000000"}
}

func (p *deadPaneProvider) ObserveLivenessWithError(name string, processNames []string) (runtime.Liveness, error) {
	if p.obsErr != nil {
		return runtime.Liveness{}, p.obsErr
	}
	if l, ok := p.corpses[name]; ok {
		return l, nil
	}
	return runtime.ObserveLiveness(p.Fake, name, processNames), nil
}

func (p *deadPaneProvider) IsRunning(name string) bool {
	if _, ok := p.corpses[name]; ok {
		return false
	}
	return p.Fake.IsRunning(name)
}

func (p *deadPaneProvider) ProcessAlive(name string, processNames []string) bool {
	if _, ok := p.corpses[name]; ok {
		return false
	}
	return p.Fake.ProcessAlive(name, processNames)
}

func (p *deadPaneProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if p.startErr != nil {
		return p.startErr
	}
	if _, ok := p.corpses[name]; ok {
		delete(p.corpses, name)
		if err := p.Fake.Stop(name); err != nil {
			return fmt.Errorf("recycling corpse %s: %w", name, err)
		}
	}
	if err := p.Fake.Start(ctx, name, cfg); err != nil {
		return err
	}
	if token := cfg.Env["GC_INSTANCE_TOKEN"]; token != "" {
		_ = p.SetMeta(name, "GC_INSTANCE_TOKEN", token)
	}
	return nil
}

func (p *deadPaneProvider) Stop(name string) error {
	delete(p.corpses, name)
	return p.Fake.Stop(name)
}

func (p *deadPaneProvider) Peek(name string, lines int) (string, error) {
	if p.peekErr != nil {
		return "", p.peekErr
	}
	return p.Fake.Peek(name, lines)
}

// deadPaneEnv is an active always-mode configured named row over its own
// running runtime, which a test turns into a corpse.
type deadPaneEnv struct {
	*reconcilerTestEnv
	sp      *deadPaneProvider
	rec     *events.Fake
	name    string
	beadID  string
	desired map[string]int
}

const deadPaneTestToken = "tok-dead-pane"

func newDeadPaneEnv(t *testing.T) *deadPaneEnv {
	t.Helper()
	return newDeadPaneEnvFor(t, deadPaneCity(), "worker")
}

// deadPaneCity is a city with one always-mode named session of worker.
func deadPaneCity() *config.City {
	return &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "test-cmd"}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "always"}},
	}
}

// deadPaneRigCity is deadPaneCity with the named session scoped to rig-a,
// suspended when suspended is set.
func deadPaneRigCity(cityPath string, suspended bool) *config.City {
	return &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Rigs:          []config.Rig{{Name: "rig-a", Path: filepath.Join(cityPath, "rig-a"), SuspendedOnStart: suspended}},
		Agents:        []config.Agent{{Name: "worker", Scope: "rig", StartCommand: "test-cmd"}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "always", Scope: "rig", Dir: "rig-a"}},
	}
}

// newDeadPaneEnvFor builds the row of identity, cfg's always-mode named
// session, over its own running runtime.
func newDeadPaneEnvFor(t *testing.T, cfg *config.City, identity string) *deadPaneEnv {
	t.Helper()
	env := newReconcilerTestEnv(t)
	rec := events.NewFake()
	env.rec = rec
	env.cfg = cfg
	name := config.NamedSessionRuntimeName(env.cfg.EffectiveCityName(), env.cfg.Workspace, identity)
	env.desiredState[name] = TemplateParams{
		Command:                 "test-cmd",
		SessionName:             name,
		TemplateName:            identity,
		ConfiguredNamedIdentity: identity,
		ConfiguredNamedMode:     "always",
		ResolvedProvider:        &config.ResolvedProvider{Name: "fake", Command: "test-cmd"},
		Hints:                   agent.StartupHints{ProcessNames: []string{"test-cmd"}},
	}
	runtimeCfg := runtime.Config{Command: "test-cmd"}
	if err := env.sp.Start(context.Background(), name, runtimeCfg); err != nil {
		t.Fatalf("Start(%s): %v", name, err)
	}
	if err := env.sp.SetMeta(name, "GC_INSTANCE_TOKEN", deadPaneTestToken); err != nil {
		t.Fatalf("stamp runtime token: %v", err)
	}
	session := env.createSessionBead(name, identity)
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"last_woke_at":               env.clk.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		"instance_token":             deadPaneTestToken,
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: identity,
		namedSessionModeMetadata:     "always",
		"alias":                      identity,
		"started_config_hash":        runtime.CoreFingerprint(runtimeCfg),
		"started_live_hash":          runtime.LiveFingerprint(runtimeCfg),
	})
	return &deadPaneEnv{
		reconcilerTestEnv: env,
		sp:                newDeadPaneProvider(env.sp),
		rec:               rec,
		name:              name,
		beadID:            session.ID,
		desired:           map[string]int{identity: 1},
	}
}

// tick runs one legacy reconcile pass over the row as the store holds it.
func (e *deadPaneEnv) tick(t *testing.T) {
	t.Helper()
	b, err := e.store.Get(e.beadID)
	if err != nil {
		t.Fatalf("Get(%s): %v", e.beadID, err)
	}
	reconcileWithSequencedRuntimeObservation(e.reconcilerTestEnv, []beads.Bead{b}, e.sp, e.desired)
}

func (e *deadPaneEnv) crashes() []events.Event {
	var out []events.Event
	for _, ev := range e.rec.Events {
		if ev.Type == events.SessionCrashed {
			out = append(out, ev)
		}
	}
	return out
}

func crashReason(t *testing.T, ev events.Event) map[string]string {
	t.Helper()
	var payload map[string]string
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatalf("decode session.crashed payload %s: %v", ev.Payload, err)
	}
	return payload
}

// TestReconcileNamedAlwaysCorpseRecordsDeadPaneCrash: an active always-mode
// named row whose runtime is its own remain-on-exit corpse records one
// session.crashed for the dead pane (#6298), carrying the dead-pane reason,
// the row's id and template, and the truncated pane tail.
func TestReconcileNamedAlwaysCorpseRecordsDeadPaneCrash(t *testing.T) {
	e := newDeadPaneEnv(t)
	lines := make([]string, 0, rateLimitPeekLines)
	for i := 0; i < rateLimitPeekLines; i++ {
		lines = append(lines, fmt.Sprintf("line %d: agent output", i))
	}
	pane := strings.Join(lines, "\n")
	e.sp.SetPeekOutput(e.name, pane)
	e.sp.kill(e.name, "$7")

	e.tick(t)

	crashes := e.crashes()
	if len(crashes) != 1 {
		t.Fatalf("session.crashed events = %d, want 1 for the dead pane; events=%#v stderr=%q", len(crashes), e.rec.Events, e.stderr.String())
	}
	payload := crashReason(t, crashes[0])
	if payload["reason"] != "dead pane" || payload["session_id"] != e.beadID || payload["template"] != "worker" {
		t.Fatalf("session.crashed payload = %v, want reason \"dead pane\", session_id %q, template worker", payload, e.beadID)
	}
	msg := crashes[0].Message
	if got := len(strings.Split(msg, "\n")); got > crashEventPaneOutputMaxLines+1 {
		t.Fatalf("session.crashed message has %d lines, want <= %d (plus the elision marker line)", got, crashEventPaneOutputMaxLines+1)
	}
	if !strings.HasSuffix(msg, lines[len(lines)-1]) || !strings.Contains(msg, "lines omitted") {
		t.Fatalf("session.crashed message = %q, want the truncated pane capture ending in %q", msg, lines[len(lines)-1])
	}
	if crashes[0].Actor != "gc" {
		t.Fatalf("session.crashed actor = %q, want gc", crashes[0].Actor)
	}
}

// TestReconcileNamedAlwaysCorpseRecoversAfterCrash: after the dead pane's
// crash is recorded, the legacy ticks replace the corpse with a fresh
// runtime on a new incarnation, the first session.woke follows the
// session.crashed, and the row keeps its durable identity (#2437).
func TestReconcileNamedAlwaysCorpseRecoversAfterCrash(t *testing.T) {
	e := newDeadPaneEnv(t)
	e.sp.SetPeekOutput(e.name, "agent exited")
	e.sp.kill(e.name, "$7")
	startsBefore := e.sp.CountCalls("Start", e.name)

	for i := 0; i < 5; i++ {
		e.tick(t)
		if e.sp.CountCalls("Start", e.name) > startsBefore {
			break
		}
		e.clk.Advance(time.Minute)
	}

	if got := e.sp.CountCalls("Start", e.name); got != startsBefore+1 {
		t.Fatalf("Start(%s) calls = %d, want %d: the corpse is not replaced; stderr=%q", e.name, got, startsBefore+1, e.stderr.String())
	}
	if _, corpse := e.sp.corpses[e.name]; corpse || !e.sp.IsRunning(e.name) {
		t.Fatalf("runtime %s: corpse=%v running=%v, want a fresh running runtime", e.name, corpse, e.sp.IsRunning(e.name))
	}
	after, err := e.store.Get(e.beadID)
	if err != nil {
		t.Fatalf("Get(%s): %v", e.beadID, err)
	}
	if after.Status == "closed" {
		t.Fatalf("named row closed over its dead pane; want it kept open (state %q)", after.Metadata["state"])
	}
	if state := after.Metadata["state"]; state != "active" && state != "awake" {
		t.Fatalf("row state = %q, want active after the restart committed", state)
	}
	if tok := after.Metadata["instance_token"]; tok == "" || tok == deadPaneTestToken {
		t.Fatalf("row instance_token = %q, want a new incarnation's token", tok)
	}
	if after.Metadata["alias"] != "worker" || after.Metadata["session_name"] != e.name {
		t.Fatalf("row alias=%q session_name=%q, want worker and %q unchanged", after.Metadata["alias"], after.Metadata["session_name"], e.name)
	}
	crashAt, wokeAt := -1, -1
	for i, ev := range e.rec.Events {
		switch {
		case ev.Type == events.SessionCrashed && crashAt < 0:
			crashAt = i
		case ev.Type == events.SessionWoke && wokeAt < 0:
			wokeAt = i
		}
	}
	if crashAt < 0 || wokeAt < 0 || crashAt > wokeAt {
		t.Fatalf("session.crashed at %d, first session.woke at %d, want the crash recorded before the wake; events=%#v", crashAt, wokeAt, e.rec.Events)
	}
}

// claimLive puts the row back on its committed active state, as a row that
// still claims its runtime live reads.
func (e *deadPaneEnv) claimLive(t *testing.T) {
	t.Helper()
	b, err := e.store.Get(e.beadID)
	if err != nil {
		t.Fatalf("Get(%s): %v", e.beadID, err)
	}
	e.setSessionMetadata(&b, map[string]string{"state": "active", "instance_token": deadPaneTestToken, "sleep_reason": "", "wake_attempts": "0", "quarantined_until": ""})
}

// TestReconcileNamedAlwaysCorpseCrashOncePerDeadObject: the same dead
// object observed again under a row that still claims it live records no
// further session.crashed; a later object of the row dying records one more.
func TestReconcileNamedAlwaysCorpseCrashOncePerDeadObject(t *testing.T) {
	e := newDeadPaneEnv(t)
	e.sp.SetPeekOutput(e.name, "agent exited")
	e.sp.startErr = errors.New("start held for the test")

	for i := 0; i < 3; i++ {
		e.sp.kill(e.name, "$7")
		e.claimLive(t)
		e.tick(t)
		e.clk.Advance(time.Minute)
	}
	if got := len(e.crashes()); got != 1 {
		t.Fatalf("session.crashed events over 3 ticks of one dead object = %d, want 1; stderr=%q", got, e.stderr.String())
	}

	e.sp.kill(e.name, "$8")
	e.claimLive(t)
	e.tick(t)
	if got := len(e.crashes()); got != 2 {
		t.Fatalf("session.crashed events after a later object died = %d, want 2", got)
	}
}

func (e *deadPaneEnv) count(method string, eventType string) (calls, recorded int) {
	for _, ev := range e.rec.Events {
		if ev.Type == eventType {
			recorded++
		}
	}
	return e.sp.CountCalls(method, e.name), recorded
}

// TestReconcileNamedCorpseCrashNotRecordedWithoutCorpse: an incomplete
// liveness observation, or a live runtime, records no dead-pane crash and
// neither stops nor starts the row's runtime.
func TestReconcileNamedCorpseCrashNotRecordedWithoutCorpse(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(e *deadPaneEnv)
	}{
		{"liveness unavailable", func(e *deadPaneEnv) {
			e.sp.kill(e.name, "$7")
			e.sp.obsErr = fmt.Errorf("probing %s: %w", e.name, runtime.ErrRuntimeUnavailable)
		}},
		{"runtime running and alive", func(*deadPaneEnv) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newDeadPaneEnv(t)
			e.sp.SetPeekOutput(e.name, "agent output")
			tc.setup(e)
			stops := e.sp.CountCalls("Stop", e.name)
			starts := e.sp.CountCalls("Start", e.name)

			e.tick(t)

			if got := len(e.crashes()); got != 0 {
				t.Fatalf("session.crashed events = %d, want 0; events=%#v", got, e.rec.Events)
			}
			if got := e.sp.CountCalls("Stop", e.name); got != stops {
				t.Fatalf("Stop(%s) calls = %d, want %d", e.name, got, stops)
			}
			if got := e.sp.CountCalls("Start", e.name); got != starts {
				t.Fatalf("Start(%s) calls = %d, want %d", e.name, got, starts)
			}
		})
	}
}

// TestReconcileNamedCorpseCrashUnderSuspension: while the city, the row's
// rig or the session itself is suspended or held over its corpse, the legacy
// ticks start nothing and record no wake, and the dead object's crash is
// recorded at most once.
func TestReconcileNamedCorpseCrashUnderSuspension(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  func(t *testing.T) *deadPaneEnv
	}{
		{"city suspended", func(t *testing.T) *deadPaneEnv {
			cfg := deadPaneCity()
			cfg.Workspace.SuspendedOnStart = true
			return newDeadPaneEnvFor(t, cfg, "worker")
		}},
		{"rig suspended", func(t *testing.T) *deadPaneEnv {
			cfg := deadPaneRigCity(t.TempDir(), true)
			return newDeadPaneEnvFor(t, cfg, "rig-a/worker")
		}},
		{"session held", func(t *testing.T) *deadPaneEnv {
			e := newDeadPaneEnv(t)
			b, err := e.store.Get(e.beadID)
			if err != nil {
				t.Fatalf("Get(%s): %v", e.beadID, err)
			}
			e.setSessionMetadata(&b, map[string]string{
				"sleep_intent": "user-hold",
				"held_until":   e.clk.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
			})
			return e
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := tc.env(t)
			e.sp.SetPeekOutput(e.name, "agent exited")
			e.sp.kill(e.name, "$7")
			starts := e.sp.CountCalls("Start", e.name)

			for i := 0; i < 4; i++ {
				e.tick(t)
				e.clk.Advance(time.Minute)
			}

			if got, woke := e.count("Start", events.SessionWoke); got != starts || woke != 0 {
				t.Fatalf("Start(%s) calls = %d (want %d), session.woke = %d (want 0) while suspended; stderr=%q", e.name, got, starts, woke, e.stderr.String())
			}
			if got := len(e.crashes()); got > 1 {
				t.Fatalf("session.crashed events = %d, want at most 1 for one dead object", got)
			}
		})
	}
}

// setMeta stamps the bead's metadata for a gate case.
func (e *deadPaneEnv) setMeta(t *testing.T, kv map[string]string) {
	t.Helper()
	b, err := e.store.Get(e.beadID)
	if err != nil {
		t.Fatalf("Get(%s): %v", e.beadID, err)
	}
	e.setSessionMetadata(&b, kv)
}

// TestReconcileNamedCorpseCrashGates: a corpse under a row that is not
// committed active (quarantined, asleep), whose row or runtime is owned by
// another handler, or whose pane shows a provider rate-limit screen, records
// no dead-pane crash; the rate-limited dead object stays unrecorded once the
// screen scrolls away.
func TestReconcileNamedCorpseCrashGates(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pane  string
		setup func(t *testing.T, e *deadPaneEnv)
		later string
	}{
		{name: "pool seat", setup: func(t *testing.T, e *deadPaneEnv) {
			e.setMeta(t, map[string]string{namedSessionMetadataKey: "", "pool_managed": "true", "pool_slot": "1"})
		}},
		{name: "quarantined row", setup: func(t *testing.T, e *deadPaneEnv) {
			e.setMeta(t, map[string]string{
				"state":             string(session.StateQuarantined),
				"state_reason":      "crash-loop",
				"quarantine_cycle":  "1",
				"sleep_reason":      string(session.SleepReasonQuarantine),
				"quarantined_until": e.clk.Now().Add(15 * time.Minute).UTC().Format(time.RFC3339),
			})
		}},
		{name: "asleep row", setup: func(t *testing.T, e *deadPaneEnv) {
			e.setMeta(t, map[string]string{"state": string(session.StateAsleep), "sleep_reason": string(session.SleepReasonIdle)})
		}},
		{name: "rate-limit screen", pane: "Usage limit reached for this account", later: "agent exited"},
		{name: "runtime drain ack", setup: func(t *testing.T, e *deadPaneEnv) {
			if err := e.sp.SetMeta(e.name, "GC_DRAIN_ACK", "1"); err != nil {
				t.Fatalf("SetMeta(GC_DRAIN_ACK): %v", err)
			}
		}},
		{name: "runtime restart requested", setup: func(t *testing.T, e *deadPaneEnv) {
			if err := e.sp.SetMeta(e.name, "GC_RESTART_REQUESTED", "1700000000"); err != nil {
				t.Fatalf("SetMeta(GC_RESTART_REQUESTED): %v", err)
			}
		}},
		{name: "foreign runtime token", setup: func(t *testing.T, e *deadPaneEnv) {
			if err := e.sp.SetMeta(e.name, "GC_INSTANCE_TOKEN", "tok-other-incarnation"); err != nil {
				t.Fatalf("SetMeta(GC_INSTANCE_TOKEN): %v", err)
			}
		}},
		{name: "row restart requested", setup: func(t *testing.T, e *deadPaneEnv) {
			e.setMeta(t, map[string]string{"restart_requested": "true"})
		}},
		{name: "pending create claim", setup: func(t *testing.T, e *deadPaneEnv) {
			e.setMeta(t, map[string]string{"pending_create_claim": "true"})
		}},
		{name: "tracked drain", setup: func(_ *testing.T, e *deadPaneEnv) {
			e.dt.set(e.beadID, &drainState{reason: "idle", generation: 1})
		}},
		{name: "kill pending", setup: func(t *testing.T, e *deadPaneEnv) {
			e.setMeta(t, map[string]string{
				"state":        "asleep",
				"state_reason": session.KillPendingReason,
				"sleep_reason": string(session.SleepReasonKilled),
				"slept_at":     e.clk.Now().UTC().Format(time.RFC3339),
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newDeadPaneEnv(t)
			pane := tc.pane
			if pane == "" {
				pane = "agent exited"
			}
			e.sp.SetPeekOutput(e.name, pane)
			e.sp.startErr = errors.New("start refused")
			e.sp.kill(e.name, "$7")
			if tc.setup != nil {
				tc.setup(t, e)
			}

			e.tick(t)
			if tc.later != "" {
				e.sp.SetPeekOutput(e.name, tc.later)
				e.claimLive(t)
				e.tick(t)
			}

			if got := len(e.crashes()); got != 0 {
				t.Fatalf("session.crashed events = %d, want 0; events=%#v", got, e.rec.Events)
			}
		})
	}
}

// TestReconcilePreservedNamedCorpseRecordsDeadPaneCrash: a configured named
// row absent from the tick's desired set is kept by the preserved-named arm,
// which heals it before the dead-pane classification runs. An active row over
// its own corpse there records one session.crashed all the same, classified
// from the row as the tick found it. While the city, its rig or its agent is
// suspended the row is not restarted (suspension holds the restart, not the
// record), nor while its template does not resolve; such a row's record
// names its own identity and template.
func TestReconcilePreservedNamedCorpseRecordsDeadPaneCrash(t *testing.T) {
	for _, tc := range []struct {
		name     string
		env      func(t *testing.T) *deadPaneEnv
		identity string // the row's named identity and backing template
		held     bool   // the row is not restarted
		log      string // a line stderr must carry
	}{
		{name: "not desired", env: newDeadPaneEnv, identity: "worker"},
		{name: "city suspended", identity: "worker", held: true, env: func(t *testing.T) *deadPaneEnv {
			cfg := deadPaneCity()
			cfg.Workspace.SuspendedOnStart = true
			return newDeadPaneEnvFor(t, cfg, "worker")
		}},
		{name: "rig suspended", identity: "rig-a/worker", held: true, env: func(t *testing.T) *deadPaneEnv {
			return newDeadPaneEnvFor(t, deadPaneRigCity(t.TempDir(), true), "rig-a/worker")
		}},
		{name: "agent suspended", identity: "worker", held: true, env: func(t *testing.T) *deadPaneEnv {
			cfg := deadPaneCity()
			cfg.Agents[0].Suspended = true
			return newDeadPaneEnvFor(t, cfg, "worker")
		}},
		{name: "template unresolved", identity: "worker", held: true, log: "resolve preserved named session ", env: func(t *testing.T) *deadPaneEnv {
			cfg := deadPaneCity()
			cfg.Agents[0].StartCommand = ""
			cfg.Agents[0].Provider = "stub"
			cfg.Providers = map[string]config.ProviderSpec{"stub": {Command: filepath.Join(t.TempDir(), "missing-agent"), PromptMode: "none"}}
			return newDeadPaneEnvFor(t, cfg, "worker")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := tc.env(t)
			delete(e.desiredState, e.name)
			e.sp.SetPeekOutput(e.name, "agent exited")
			e.sp.kill(e.name, "$7")
			starts := e.sp.CountCalls("Start", e.name)

			for i := 0; i < 3; i++ {
				e.tick(t)
				e.clk.Advance(time.Minute)
			}

			crashes := e.crashes()
			if len(crashes) != 1 {
				t.Fatalf("session.crashed events = %d, want 1 for the preserved row's dead pane; events=%#v stderr=%q", len(crashes), e.rec.Events, e.stderr.String())
			}
			payload := crashReason(t, crashes[0])
			if payload["reason"] != deadPaneCrashReason || payload["session_id"] != e.beadID || payload["template"] != tc.identity || crashes[0].Subject != tc.identity {
				t.Fatalf("session.crashed subject %q payload %v, want subject and template %q, reason %q, session_id %q", crashes[0].Subject, payload, tc.identity, deadPaneCrashReason, e.beadID)
			}
			calls, woke := e.count("Start", events.SessionWoke)
			if tc.held && (calls != starts || woke != 0) {
				t.Fatalf("Start(%s) calls = %d (want %d), session.woke = %d (want 0) for a held row; stderr=%q", e.name, calls, starts, woke, e.stderr.String())
			}
			if !tc.held && calls != starts+1 {
				t.Fatalf("Start(%s) calls = %d, want %d: the preserved row is restarted over its corpse; stderr=%q", e.name, calls, starts+1, e.stderr.String())
			}
			if !strings.Contains(e.stderr.String(), tc.log) {
				t.Fatalf("stderr = %q, want %q", e.stderr.String(), tc.log)
			}
		})
	}
}

// TestReconcileNamedCorpseCrashReadFailures: a failed pane capture still
// records the dead pane's crash, with no pane tail. A failed runtime
// metadata read leaves the exit's owner unknown, so it records nothing and
// logs the read; the pass then heals and restarts the row as usual, so a
// read that succeeds on the next pass finds no dead object left to record.
func TestReconcileNamedCorpseCrashReadFailures(t *testing.T) {
	t.Run("pane capture fails", func(t *testing.T) {
		e := newDeadPaneEnv(t)
		e.sp.peekErr = errors.New("capture-pane failed")
		e.sp.kill(e.name, "$7")

		e.tick(t)

		crashes := e.crashes()
		if len(crashes) != 1 || crashes[0].Message != "" {
			t.Fatalf("session.crashed events = %#v, want exactly one with no pane tail; stderr=%q", crashes, e.stderr.String())
		}
		if want := "reading dead pane of " + e.name; !strings.Contains(e.stderr.String(), want) {
			t.Fatalf("stderr = %q, want the failed capture logged (%q)", e.stderr.String(), want)
		}
	})
	t.Run("runtime metadata read fails once", func(t *testing.T) {
		e := newDeadPaneEnv(t)
		e.sp.SetPeekOutput(e.name, "agent exited")
		e.sp.kill(e.name, "$7")
		e.sp.GetMetaErrors[e.name] = map[string]error{"GC_DRAIN_ACK": errors.New("show-environment failed")}
		starts := e.sp.CountCalls("Start", e.name)

		e.tick(t)
		if got := e.sp.CountCalls("Start", e.name); got != starts+1 {
			t.Fatalf("Start(%s) calls = %d, want %d: the failed read's pass restarts the row over its corpse", e.name, got, starts+1)
		}
		delete(e.sp.GetMetaErrors, e.name)
		e.clk.Advance(time.Minute)
		e.tick(t)

		if got := len(e.crashes()); got != 0 {
			t.Fatalf("session.crashed events = %d, want 0: the read failure fails closed; events=%#v", got, e.rec.Events)
		}
		if want := "classifying dead pane of " + e.name + ": reading GC_DRAIN_ACK"; !strings.Contains(e.stderr.String(), want) {
			t.Fatalf("stderr = %q, want the failed read logged (%q)", e.stderr.String(), want)
		}
	})
}
