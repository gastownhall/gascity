package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// deadPaneProvider is a fake tmux backend over runtime.Fake that can hold a
// remain-on-exit corpse under a name: the name stays listed (its metadata
// and pane text readable), its liveness reads Corpse with the dead object's
// id, IsRunning answers false as tmux's state cache does for a pane_dead
// session, and a Start over it replaces it as tmux's ensureFreshSession
// does.
type deadPaneProvider struct {
	*runtime.Fake
	corpses map[string]runtime.Liveness
	obsErr  error // every liveness read fails with it when set
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
	env := newReconcilerTestEnv(t)
	rec := events.NewFake()
	env.rec = rec
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "test-cmd"}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "always"}},
	}
	name := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[name] = TemplateParams{
		Command:                 "test-cmd",
		SessionName:             name,
		TemplateName:            "worker",
		ConfiguredNamedIdentity: "worker",
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
	session := env.createSessionBead(name, "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"last_woke_at":               env.clk.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		"instance_token":             deadPaneTestToken,
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "always",
		"alias":                      "worker",
		"started_config_hash":        runtime.CoreFingerprint(runtimeCfg),
		"started_live_hash":          runtime.LiveFingerprint(runtimeCfg),
	})
	return &deadPaneEnv{
		reconcilerTestEnv: env,
		sp:                newDeadPaneProvider(env.sp),
		rec:               rec,
		name:              name,
		beadID:            session.ID,
		desired:           map[string]int{"worker": 1},
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
