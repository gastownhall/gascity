package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// Named create-effect tests (P3-6b). Effects run on the P3-6 harness against
// MemStores; the legacy side runs main's desired-state build and sync create
// arm, or the legacy reopen helper, on a separate store in the same city
// directory. The effect runs first: it writes no file, so legacy's
// projections cannot leak into what it resolved.

// namedEffectNow is the clock of every named comparison: legacy's sync clock
// and the effect's host clock read it, so the clock keys match unnormalized.
var namedEffectNow = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

// namedFixture is one named session configuration of the T1/T12 matrix.
type namedFixture struct {
	name     string
	identity string
	city     func(dir string) *config.City
	// prepare seeds the city directory and the legacy store before either
	// side runs. It returns the bound work bead, if any.
	prepare func(t *testing.T, dir string, legacy beads.Store) string
}

// namedTestCity is a city with one named session. Its session template
// makes the runtime name differ from the identity, as on maintainer-city, so
// the two identifier locks are distinct.
func namedTestCity(agent config.Agent, named config.NamedSession, providers map[string]config.ProviderSpec) *config.City {
	return &config.City{
		Workspace:     config.Workspace{Name: "test-city", SessionTemplate: "{{.City}}-{{.Agent}}"},
		Providers:     providers,
		Agents:        []config.Agent{agent},
		NamedSessions: []config.NamedSession{named},
	}
}

// namedFixtures covers the create metadata's resolution inputs (P3-6b T1):
// Claude with and without a projected settings file, a session-key provider,
// ACP, kimi hooks, a templated command, session_live, a bound step, a rig,
// an identity distinct from its template, and both modes. City providers
// with no command layer over the builtins without a PATH lookup.
func namedFixtures() []namedFixture {
	always := config.NamedSession{Template: "mayor", Mode: "always"}
	builtin := func(names ...string) map[string]config.ProviderSpec {
		out := make(map[string]config.ProviderSpec, len(names))
		for _, n := range names {
			out[n] = config.ProviderSpec{}
		}
		return out
	}
	acp := true
	return []namedFixture{
		{name: "claude without settings", identity: "mayor", city: func(string) *config.City {
			return namedTestCity(config.Agent{Name: "mayor", Provider: "claude"}, always, builtin("claude"))
		}},
		{name: "claude with settings", identity: "mayor", city: func(string) *config.City {
			return namedTestCity(config.Agent{Name: "mayor", Provider: "claude"}, always, builtin("claude"))
		}, prepare: func(t *testing.T, dir string, _ beads.Store) string {
			writeNamedFixtureFile(t, filepath.Join(dir, ".gc", "settings.json"), `{"permissions":{"allow":["Bash"]}}`)
			return ""
		}},
		{name: "codex session key", identity: "mayor", city: func(string) *config.City {
			return namedTestCity(config.Agent{Name: "mayor", Provider: "codex", WakeMode: "fresh"}, always, builtin("codex"))
		}},
		{name: "acp transport", identity: "mayor", city: func(string) *config.City {
			return namedTestCity(config.Agent{Name: "mayor", Provider: "claude", Session: "acp"}, always,
				map[string]config.ProviderSpec{"claude": {SupportsACP: &acp}})
		}},
		{name: "kimi install hooks", identity: "mayor", city: func(string) *config.City {
			return namedTestCity(config.Agent{Name: "mayor", Provider: "kimi", InstallAgentHooks: []string{"kimi"}}, always, builtin("kimi"))
		}},
		{name: "templated command and session_live", identity: "mayor", city: func(string) *config.City {
			return namedTestCity(config.Agent{
				Name: "mayor", StartCommand: "run --session {{.Session}} --dir {{.WorkDir}}",
				SessionLive: []string{"tmux set-option -t {{.Session}} status-left {{.Agent}}"},
			}, always, nil)
		}},
		{name: "no session_live", identity: "mayor", city: func(string) *config.City {
			return namedTestCity(config.Agent{Name: "mayor", StartCommand: "true"}, always, nil)
		}},
		{name: "on_demand with a bound step", identity: "mayor", city: func(string) *config.City {
			return namedTestCity(config.Agent{Name: "mayor", StartCommand: "true"}, config.NamedSession{Template: "mayor", Mode: "on_demand"}, nil)
		}, prepare: func(t *testing.T, _ string, legacy beads.Store) string {
			work, err := legacy.Create(beads.Bead{Title: "step", Type: "task", Assignee: "mayor"})
			if err != nil {
				t.Fatal(err)
			}
			status := "in_progress"
			if err := legacy.Update(work.ID, beads.UpdateOpts{Status: &status}); err != nil {
				t.Fatal(err)
			}
			return work.ID
		}},
		{name: "rig scoped", identity: "rig/witness", city: func(dir string) *config.City {
			cfg := namedTestCity(config.Agent{Name: "witness", Dir: "rig", StartCommand: "true"},
				config.NamedSession{Template: "witness", Dir: "rig", Mode: "always"}, nil)
			cfg.Rigs = []config.Rig{{Name: "rig", Path: filepath.Join(dir, "rigs", "rig")}}
			return cfg
		}, prepare: func(t *testing.T, dir string, _ beads.Store) string {
			if err := os.MkdirAll(filepath.Join(dir, "rigs", "rig"), 0o755); err != nil {
				t.Fatal(err)
			}
			return ""
		}},
		{name: "no session template", identity: "mayor", city: func(string) *config.City {
			cfg := namedTestCity(config.Agent{Name: "mayor", StartCommand: "true"}, always, nil)
			cfg.Workspace.SessionTemplate = ""
			return cfg
		}},
		{name: "identity differs from template", identity: "boss", city: func(string) *config.City {
			return namedTestCity(config.Agent{Name: "mayor", StartCommand: "true"}, config.NamedSession{Name: "boss", Template: "mayor", Mode: "always"}, nil)
		}},
	}
}

func writeNamedFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// namedPlan is the plan P3-5a emits for identity in cfg.
func namedPlan(t *testing.T, cfg *config.City, entryID, identity string) createPlan {
	t.Helper()
	spec, ok := findNamedSessionSpec(cfg, "test-city", identity)
	if !ok {
		t.Fatalf("no named session spec for %q", identity)
	}
	return createPlan{EntryID: entryID, Named: &namedCreatePlan{
		Identity: identity, SessionName: spec.SessionName, Template: namedSessionBackingTemplate(spec), Mode: spec.Mode,
	}}
}

// newNamedHarness is a create harness over cityPath at namedEffectNow.
func newNamedHarness(t *testing.T, cityPath string, edit func(*createEffectHost)) *createHarness {
	t.Helper()
	return newCreateHarness(t, func(host *createEffectHost) {
		host.cityPath = cityPath
		host.now = func() time.Time { return namedEffectNow }
		if edit != nil {
			edit(host)
		}
	})
}

// namedComparableRows reads every session row, open or closed, with the IDs,
// timestamps and per-create random values blanked (P3-6b §4).
func namedComparableRows(t *testing.T, store beads.Store) []beads.Bead {
	t.Helper()
	rows, err := store.ListByLabel(sessionBeadLabel, 0, beads.IncludeClosed)
	if err != nil {
		t.Fatal(err)
	}
	for i := range rows {
		rows[i].ID, rows[i].CreatedAt, rows[i].UpdatedAt = "", time.Time{}, time.Time{}
		for _, key := range []string{"instance_token", "session_key", "synced_at", "pending_create_started_at", startupKickoffStartedAtKey} {
			if rows[i].Metadata[key] != "" {
				rows[i].Metadata[key] = "<" + key + ">"
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Title < rows[j].Title })
	return rows
}

// legacyNamedSync runs main's desired-state build and sync on store, with sp
// as the provider legacy probes.
func legacyNamedSync(t *testing.T, cityPath string, cfg *config.City, store beads.Store, sp runtime.Provider) {
	t.Helper()
	ds := buildDesiredState("test-city", cityPath, namedEffectNow, cfg, sp, store, io.Discard)
	syncSessionBeads(cityPath, store, ds.State, sp, allConfiguredDS(ds.State), cfg, &clock.Fake{Time: namedEffectNow}, io.Discard, false)
}

// transportFake is a fake runtime that routes every transport, as the
// production auto provider routes ACP beside tmux.
type transportFake struct{ *runtime.Fake }

func (transportFake) SupportsTransport(string) bool { return true }

// runNamedCreateBeside runs the effect for fx's plan on a fresh store, then
// legacy's build and sync on another, in the same city directory, and returns
// both stores' comparable rows. adopt sets the plan's AdoptLive and runs
// legacy with the configured runtime already alive.
func runNamedCreateBeside(t *testing.T, fx namedFixture, adopt bool) (effect, legacy []beads.Bead) {
	t.Helper()
	dir := t.TempDir()
	cfg := fx.city(dir)
	legacyStore, effectStore := beads.NewMemStore(), beads.NewMemStore()
	bound := ""
	if fx.prepare != nil {
		bound = fx.prepare(t, dir, legacyStore)
	}
	h := newNamedHarness(t, dir, nil)
	h.reserve(t, "c1")
	plan := namedPlan(t, cfg, "c1", fx.identity)
	plan.Named.BoundStepID, plan.Named.AdoptLive = bound, adopt
	h.runAll(t, &createPass{cfg: cfg, store: effectStore}, plan)
	if e := h.entry(t); !e.Landed {
		t.Fatalf("effect settlement = %+v, want landed", e)
	}

	fake := runtime.NewFake()
	if adopt {
		if err := fake.Start(context.Background(), plan.Named.SessionName, runtime.Config{}); err != nil {
			t.Fatal(err)
		}
	}
	legacyNamedSync(t, dir, cfg, legacyStore, transportFake{fake})
	return namedComparableRows(t, effectStore), namedComparableRows(t, legacyStore)
}

// Kills: named create metadata drifting from legacy's sync create arm (AM7),
// the read-only resolver diverging from the side-effecting one, a missing or
// extra key. With the breaker off the rows are byte-identical (P3-6b T1).
func TestCreateEffect_NamedMetadataByteIdenticalToLegacySyncCreate(t *testing.T) {
	for _, fx := range namedFixtures() {
		t.Run(fx.name, func(t *testing.T) {
			effect, legacy := runNamedCreateBeside(t, fx, false)
			if len(legacy) != 1 {
				t.Fatalf("legacy rows = %d (%+v), want the one named row", len(legacy), legacy)
			}
			if !reflect.DeepEqual(effect, legacy) {
				t.Fatalf("effect row differs from legacy's sync create:\neffect: %+v\nlegacy: %+v", effect, legacy)
			}
			if got := effect[0].Metadata["state"]; got != string(session.StateStartPending) || effect[0].Metadata["pending_create_claim"] != "true" {
				t.Fatalf("state = %q claim = %q, want start-pending with the claim", got, effect[0].Metadata["pending_create_claim"])
			}
		})
	}
}

// Kills: breaker state copied onto a fresh named row (P3-6b T1; C7.2
// amended per SIMPLIFICATION-CHECKPOINT C5): with the breaker configured and
// a prior row carrying session_circuit_* state, the effect's rows are still
// byte-identical to legacy's sync create, and the fresh row has no
// session_circuit_* key.
func TestCreateEffect_NamedMetadataByteIdenticalWithBreakerConfigured(t *testing.T) {
	dir := t.TempDir()
	cfg := mayorCity()
	cfg.Daemon.SessionCircuitBreaker = true
	cfg.Daemon.SessionCircuitBreakerWindow = "30m"
	legacyStore, effectStore := beads.NewMemStore(), beads.NewMemStore()
	prior := map[string]string{
		"state": string(session.StateFailedCreate), "close_reason": string(session.StateFailedCreate),
		sessionCircuitStateMetadata: "CIRCUIT_CLOSED", sessionCircuitLastRestartMetadata: namedEffectNow.Add(-time.Minute).Format(time.RFC3339Nano),
	}
	seedClosedNamedRow(t, legacyStore, cfg, prior)
	seedClosedNamedRow(t, effectStore, cfg, prior)
	h := newNamedHarness(t, dir, nil)
	h.reserve(t, "c1")
	h.runAll(t, &createPass{cfg: cfg, store: effectStore}, namedPlan(t, cfg, "c1", "mayor"))
	legacyNamedSync(t, dir, cfg, legacyStore, transportFake{runtime.NewFake()})

	effect, legacy := namedComparableRows(t, effectStore), namedComparableRows(t, legacyStore)
	if len(effect) != 2 || !reflect.DeepEqual(effect, legacy) {
		t.Fatalf("effect rows differ from legacy's sync create:\neffect: %+v\nlegacy: %+v", effect, legacy)
	}
	for _, row := range effect {
		if row.Status == "closed" {
			continue
		}
		for _, key := range sessionCircuitMetadataKeys {
			if v, ok := row.Metadata[key]; ok {
				t.Fatalf("fresh row carries %s = %q, want no session_circuit_* key", key, v)
			}
		}
	}
}

// Kills: AdoptLive mapped to the wrong state, and claim keys written on an
// adopt create (P3-6b T2): legacy's probe-true branch writes active with no
// claim, and AdoptLive stands in for that probe (AM-N6).
func TestCreateEffect_NamedAdoptLiveWritesLegacyRunningShape(t *testing.T) {
	for _, fx := range namedFixtures()[:3] {
		t.Run(fx.name, func(t *testing.T) {
			effect, legacy := runNamedCreateBeside(t, fx, true)
			if !reflect.DeepEqual(effect, legacy) {
				t.Fatalf("adopt row differs from legacy's running-shape create:\neffect: %+v\nlegacy: %+v", effect, legacy)
			}
			meta := effect[0].Metadata
			if meta["state"] != string(session.StateActive) || meta["pending_create_claim"] != "" || meta["pending_create_started_at"] != "" {
				t.Fatalf("adopt row = %v, want active without claim keys", meta)
			}
		})
	}
}

// mayorCity is a city with the always named session mayor on a plain agent.
func mayorCity() *config.City {
	return namedTestCity(config.Agent{Name: "mayor", StartCommand: "true"}, config.NamedSession{Template: "mayor", Mode: "always"}, nil)
}

// seedClosedNamedRow seeds a closed row of the named session mayor in cfg
// carrying its runtime name, with meta merged over a reopen-eligible
// canonical shape.
func seedClosedNamedRow(t *testing.T, store beads.Store, cfg *config.City, meta map[string]string) beads.Bead {
	t.Helper()
	const identity = "mayor"
	spec, ok := findNamedSessionSpec(cfg, "test-city", identity)
	if !ok {
		t.Fatalf("no named session spec for %q", identity)
	}
	full := map[string]string{
		"session_name": spec.SessionName, "alias": identity, "agent_name": identity, "template": namedSessionBackingTemplate(spec),
		"state": "asleep", "sleep_reason": "idle", "close_reason": "suspended", "generation": "4", "instance_token": "tok-old",
		"last_woke_at": namedEffectNow.Add(-time.Hour).Format(time.RFC3339), "started_config_hash": "old-config",
		namedSessionMetadataKey: "true", namedSessionIdentityMetadata: identity, namedSessionModeMetadata: spec.Mode,
		beadmeta.BoundStepIDMetadataKey: "gc-old-step", startupKickoffStateKey: "confirmed",
	}
	for k, v := range meta {
		full[k] = v
	}
	b, err := store.Create(beads.Bead{Title: identity, Type: sessionBeadType, Labels: []string{sessionBeadLabel, "agent:" + identity}, Metadata: full})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(b.ID); err != nil {
		t.Fatal(err)
	}
	closed, err := store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	return closed
}

// namedWriteCounter counts the effect's writes to a session store.
type namedWriteCounter struct {
	beads.Store
	mu     sync.Mutex
	writes []string
}

func (s *namedWriteCounter) note(op string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes = append(s.writes, op)
}

func (s *namedWriteCounter) Create(b beads.Bead) (beads.Bead, error) {
	s.note("create")
	return s.Store.Create(b)
}

func (s *namedWriteCounter) Update(id string, opts beads.UpdateOpts) error {
	s.note("update " + id)
	return s.Store.Update(id, opts)
}

func (s *namedWriteCounter) SetMetadata(id, key, value string) error {
	s.note("set " + id)
	return s.Store.SetMetadata(id, key, value)
}

func (s *namedWriteCounter) SetMetadataBatch(id string, kvs map[string]string) error {
	s.note("set-batch " + id)
	return s.Store.SetMetadataBatch(id, kvs)
}

func (s *namedWriteCounter) Tx(msg string, fn func(beads.Tx) error) error {
	s.note("tx " + msg)
	return s.Store.Tx(msg, fn)
}

func (s *namedWriteCounter) count() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.writes...)
}

// Kills: reopen batch drift from legacy's reopen helper, a second write, and
// instance_token or generation rewritten on reopen (P3-6b T3; P3-6 spec
// name). Legacy's helper runs on one store and the effect on its twin, for
// the stopped and the adopt shape, with and without a bound step.
func TestCreateEffect_NamedReopensClosedCanonicalAndMatchesLegacyMetadata(t *testing.T) {
	for _, tc := range []struct {
		name  string
		adopt bool
		bound string
	}{
		{name: "stopped without bound step"},
		{name: "stopped with bound step", bound: "gc-step"},
		{name: "adopt without bound step", adopt: true},
		{name: "adopt with bound step", adopt: true, bound: "gc-step"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := mayorCity()
			legacyStore := beads.NewMemStore()
			effectStore := &namedWriteCounter{Store: beads.NewMemStore()}
			seedClosedNamedRow(t, legacyStore, cfg, nil)
			closed := seedClosedNamedRow(t, effectStore.Store, cfg, nil)

			h := newNamedHarness(t, dir, nil)
			h.reserve(t, "c1")
			plan := namedPlan(t, cfg, "c1", "mayor")
			plan.Named.AdoptLive, plan.Named.BoundStepID = tc.adopt, tc.bound
			h.runAll(t, &createPass{cfg: cfg, store: effectStore}, plan)

			state := "stopped"
			if tc.adopt {
				state = "active"
			}
			if _, _, ok := reopenClosedConfiguredNamedSessionBead(dir+"-legacy", legacyStore, cfg, "test-city", "mayor", plan.Named.SessionName,
				state, namedEffectNow, startupKickoffReopenMetadata(tc.bound, namedEffectNow), io.Discard); !ok {
				t.Fatal("legacy reopen refused")
			}
			effect, legacy := namedComparableRows(t, effectStore), namedComparableRows(t, legacyStore)
			if !reflect.DeepEqual(effect, legacy) {
				t.Fatalf("reopened row differs from legacy's reopen:\neffect: %+v\nlegacy: %+v", effect, legacy)
			}
			if got := effectStore.count(); len(got) != 1 {
				t.Fatalf("writes = %v, want exactly one", got)
			}
			raw, err := effectStore.Get(closed.ID)
			if err != nil {
				t.Fatal(err)
			}
			if raw.Status != "open" || raw.Metadata["instance_token"] != "tok-old" || raw.Metadata["generation"] != "4" {
				t.Fatalf("reopened row = %s token %q generation %q, want open with its own token and generation",
					raw.Status, raw.Metadata["instance_token"], raw.Metadata["generation"])
			}
			if e := h.entry(t); !e.Landed || e.RowID != closed.ID || e.RetargetRowID != closed.ID {
				t.Fatalf("settlement = %+v, want a landed reopen of %s", e, closed.ID)
			}
		})
	}
}

// Kills: reopening a retired, failed or reconfigured identity (P3-6b T4).
// Every closed row legacy's reopen predicate refuses leaves the row as it
// was and mints a fresh canonical row.
func TestCreateEffect_NamedIneligibleClosedRowsCreateFresh(t *testing.T) {
	cases := map[string]map[string]string{
		"continuity ineligible":  {"continuity_eligible": "false"},
		"empty session_name":     {"session_name": ""},
		"other session_name":     {"session_name": "elsewhere"},
		"failed-create by state": {"state": string(session.StateFailedCreate), "close_reason": "suspended"},
	}
	for _, reason := range []string{"duplicate", "duplicate-repair", "gc_swept", "orphaned", "reconfigured", "stale-session", string(session.StateFailedCreate)} {
		cases["close_reason "+reason] = map[string]string{"close_reason": reason}
		cases["state "+reason] = map[string]string{"state": reason, "close_reason": "suspended"}
	}
	for name, meta := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := mayorCity()
			store := beads.NewMemStore()
			closed := seedClosedNamedRow(t, store, cfg, meta)
			h := newNamedHarness(t, t.TempDir(), nil)
			h.reserve(t, "c1")
			plan := namedPlan(t, cfg, "c1", "mayor")
			h.runAll(t, &createPass{cfg: cfg, store: store}, plan)

			after, err := store.Get(closed.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.Status != "closed" || !reflect.DeepEqual(after.Metadata, closed.Metadata) {
				t.Fatalf("ineligible row = %s %v, want untouched", after.Status, after.Metadata)
			}
			e := h.entry(t)
			if !e.Landed || e.RowID == closed.ID || e.RowID == "" || e.RetargetRowID != "" {
				t.Fatalf("settlement = %+v, want a landed fresh create", e)
			}
			fresh, err := store.Get(e.RowID)
			if err != nil {
				t.Fatal(err)
			}
			if fresh.Metadata["session_name"] != plan.Named.SessionName || fresh.Metadata[namedSessionIdentityMetadata] != "mayor" {
				t.Fatalf("fresh row = %v, want mayor's canonical row", fresh.Metadata)
			}
		})
	}
}

// namedHookStore runs before ahead of the matching writes of the wrapped store.
type namedHookStore struct {
	beads.Store
	beforeTx     func()
	beforeUpdate func(id string)
	failTx       error
}

func (s *namedHookStore) Tx(msg string, fn func(beads.Tx) error) error {
	if s.beforeTx != nil {
		s.beforeTx()
	}
	if s.failTx != nil {
		return s.failTx
	}
	return s.Store.Tx(msg, fn)
}

func (s *namedHookStore) Update(id string, opts beads.UpdateOpts) error {
	if s.beforeUpdate != nil {
		s.beforeUpdate(id)
	}
	return s.Store.Update(id, opts)
}

// Kills: a reopen's settlement that loses the row it reopened (P3-6b T5,
// AM-N2), and a reopen held in flight after it settled: the row keeps its own
// token, which no census would show for the plan's (CONTRACT v5 P5).
func TestCreateEffect_NamedReopenSettlesRetargetAndClears(t *testing.T) {
	cfg := mayorCity()
	store := beads.NewMemStore()
	closed := seedClosedNamedRow(t, store, cfg, nil)
	h := newNamedHarness(t, t.TempDir(), nil)
	token := h.reserve(t, "c1")
	h.runAll(t, &createPass{cfg: cfg, store: store}, namedPlan(t, cfg, "c1", "mayor"))

	e := h.entry(t)
	if !e.Landed || e.RetargetRowID != closed.ID || e.Token != token {
		t.Fatalf("settlement = %+v, want landed and retargeted to %s", e, closed.ID)
	}
	if m := settledCreateEntry(t, e); len(m.view().Entries) != 0 {
		t.Fatalf("in-flight entries after a landed reopen = %+v, want none", m.view().Entries)
	}
}

// Kills: an ambiguous reopen held until the hard bound (and its alert) by the
// plan's token, which it never writes (AM-N2), or refused. Its row exists
// whether or not the reopen landed, so a later create of the identity reads
// it live under the flock; the entry clears at settlement.
func TestCreateEffect_NamedAmbiguousReopenClearsAtSettlement(t *testing.T) {
	cfg := mayorCity()
	mem := beads.NewMemStore()
	closed := seedClosedNamedRow(t, mem, cfg, nil)
	h := newNamedHarness(t, t.TempDir(), nil)
	h.reserve(t, "c1")
	h.runAll(t, &createPass{cfg: cfg, store: &namedHookStore{Store: mem, failTx: errors.New("connection reset during reopen")}}, namedPlan(t, cfg, "c1", "mayor"))

	e := h.entry(t)
	if !e.Ambiguous || e.RowID != closed.ID || e.RetargetRowID != closed.ID {
		t.Fatalf("settlement = %+v, want an ambiguous reopen of %s", e, closed.ID)
	}
	h.assertNoRefusal(t)
	if m := settledCreateEntry(t, e); len(m.view().Entries) != 0 {
		t.Fatalf("in-flight entries after an ambiguous reopen = %+v, want none", m.view().Entries)
	}
}

// namedCASStore is a conditional-write-capable store whose identity-rows read
// lets another writer bump the closed row right after the read.
type namedCASStore struct {
	beads.Store
	bump func()
}

func (s *namedCASStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	rows, err := s.Store.List(q)
	if _, identity := q.Metadata[namedSessionIdentityMetadata]; identity && q.IncludeClosed && s.bump != nil {
		s.bump()
		s.bump = nil
	}
	return rows, err
}

func (s *namedCASStore) ConditionalWritesResolveTarget() beads.Store { return s.Store }

// Kills: clobbering a concurrent CLI reopen or start (P3-6b T7, AM-N4).
// Where the store fences, a writer that lands between the locked read and
// the write wins: the reopen writes nothing and settles refusing its identity.
// Without the capability the reopen keeps legacy's transaction.
func TestCreateEffect_NamedReopenIsConditionalOnRevision(t *testing.T) {
	cfg := mayorCity()
	t.Run("fenced", func(t *testing.T) {
		mem := beads.NewMemStore()
		if err := beads.StampOpenedStore(mem, "MemStore", gate.Auto, nil, nil); err != nil {
			t.Fatal(err)
		}
		closed := seedClosedNamedRow(t, mem, cfg, nil)
		store := &namedCASStore{Store: mem, bump: func() {
			if err := mem.SetMetadata(closed.ID, "note", "cli touched"); err != nil {
				t.Error(err)
			}
		}}
		h := newNamedHarness(t, t.TempDir(), nil)
		h.reserve(t, "c1")
		plan := namedPlan(t, cfg, "c1", "mayor")
		h.runAll(t, &createPass{cfg: cfg, store: store}, plan)

		after, err := mem.Get(closed.ID)
		if err != nil {
			t.Fatal(err)
		}
		if after.Status != "closed" || after.Metadata["state"] != "asleep" || after.Metadata["note"] != "cli touched" {
			t.Fatalf("row = %s %v, want the concurrent write kept and no reopen", after.Status, after.Metadata)
		}
		assertFailedNoWrite(t, h)
		h.assertRefused(t, plan, createStageFence)
	})
	t.Run("unfenced keeps legacy's transaction", func(t *testing.T) {
		mem := beads.NewMemStore()
		closed := seedClosedNamedRow(t, mem, cfg, nil)
		store := &namedCASStore{Store: mem, bump: func() {
			if err := mem.SetMetadata(closed.ID, "note", "cli touched"); err != nil {
				t.Error(err)
			}
		}}
		h := newNamedHarness(t, t.TempDir(), nil)
		h.reserve(t, "c1")
		h.runAll(t, &createPass{cfg: cfg, store: store}, namedPlan(t, cfg, "c1", "mayor"))
		after, err := mem.Get(closed.ID)
		if err != nil {
			t.Fatal(err)
		}
		if after.Status != "open" || !h.entry(t).Landed {
			t.Fatalf("row = %s, entry %+v, want the last-writer-wins reopen", after.Status, h.entry(t))
		}
	})
}

// Kills: the S1 self-deadlock (a helper that takes the city flock again
// under the effect's lock blocks forever: flock belongs to the open file
// description) and a lock set that diverges from the CLI's (P3-6b T8,
// AM-N1). The effect takes exactly {identity, session_name}, once; under
// real city flocks a reopen and a create both finish.
func TestCreateEffect_NamedLocksIdentityAndSessionNameOnce(t *testing.T) {
	cfg := mayorCity()
	sn := config.NamedSessionRuntimeName("test-city", cfg.Workspace, "mayor")
	for _, reopen := range []bool{true, false} {
		store := beads.NewMemStore()
		var closed beads.Bead
		if reopen {
			closed = seedClosedNamedRow(t, store, cfg, nil)
		}
		var calls [][]string
		h := newNamedHarness(t, t.TempDir(), func(host *createEffectHost) {
			host.withLocks = func(cityPath string, ids []string, fn func() error) error {
				calls = append(calls, append([]string(nil), ids...))
				return session.WithCitySessionIdentifierLocks(cityPath, ids, fn)
			}
		})
		h.reserve(t, "c1")
		done := make(chan struct{})
		go func() {
			defer close(done)
			h.runAll(t, &createPass{cfg: cfg, store: store}, namedPlan(t, cfg, "c1", "mayor"))
		}()
		awaitClose(t, done, "named create under real city flocks")
		if len(calls) != 1 {
			t.Fatalf("reopen=%v: lock acquisitions = %v, want exactly one", reopen, calls)
		}
		got := append([]string(nil), calls[0]...)
		sort.Strings(got)
		if want := []string{"mayor", sn}; !reflect.DeepEqual(got, want) {
			t.Fatalf("reopen=%v: lock set = %v, want %v", reopen, got, want)
		}
		if e := h.entry(t); !e.Landed || (e.RowID == closed.ID) != reopen {
			t.Fatalf("reopen=%v: settlement = %+v, want landed", reopen, e)
		}
	}
}

// namedFenceProbeStore records every read of the effect's fenced step: each must
// be live and run under the identifier locks.
type namedFenceProbeStore struct {
	beads.Store
	locked *bool
	t      *testing.T
}

func (s namedFenceProbeStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if !*s.locked || !q.Live {
		s.t.Errorf("fenced read %+v ran locked=%v live=%v, want locked and live", q.Metadata, *s.locked, q.Live)
	}
	return s.Store.List(q)
}

// Kills: cached fence reads and the closed-row lookup outside the lock
// (P3-6b T9). A row written behind a stale cache refuses the create (an open
// canonical row) and the reopen (an alias holder, read by the reopen's own
// availability checks), and every fenced read is live and locked.
func TestCreateEffect_NamedFenceReadsLiveInsideLock(t *testing.T) {
	cfg := mayorCity()
	spec, _ := findNamedSessionSpec(cfg, "test-city", "mayor")
	for _, tc := range []struct {
		name string
		// seed writes before the cache primes; behind writes to the backing
		// only, after it.
		seed, behind func(t *testing.T, backing beads.Store)
	}{
		{name: "create refused by an open canonical row", behind: func(t *testing.T, backing beads.Store) {
			if _, err := backing.Create(beads.Bead{Title: "mayor", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
				"session_name": spec.SessionName, "alias": "mayor", "state": "active",
				namedSessionMetadataKey: "true", namedSessionIdentityMetadata: "mayor", namedSessionModeMetadata: "always",
			}}); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "reopen refused by an alias holder", seed: func(t *testing.T, backing beads.Store) {
			seedClosedNamedRow(t, backing, cfg, nil)
		}, behind: func(t *testing.T, backing beads.Store) {
			aliasSquatter(t, backing)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backing := beads.NewMemStore()
			if tc.seed != nil {
				tc.seed(t, backing)
			}
			before := namedComparableRows(t, backing)
			cache := beads.NewCachingStoreForTest(backing, nil)
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatal(err)
			}
			tc.behind(t, backing)
			if rows, err := cache.List(beads.ListQuery{Metadata: map[string]string{"alias": "mayor"}}); err != nil || len(rows) != 0 {
				t.Fatalf("cached alias read = %v, %v; the fixture needs a cache that misses the row", rows, err)
			}
			locked := false
			h := newNamedHarness(t, t.TempDir(), func(host *createEffectHost) {
				host.withLocks = func(_ string, _ []string, fn func() error) error {
					locked = true
					defer func() { locked = false }()
					return fn()
				}
			})
			h.reserve(t, "c1")
			plan := namedPlan(t, cfg, "c1", "mayor")
			h.runAll(t, &createPass{cfg: cfg, store: namedFenceProbeStore{Store: cache, locked: &locked, t: t}}, plan)

			assertFailedNoWrite(t, h)
			h.assertRefused(t, plan, createStageFence)
			after := namedComparableRows(t, backing)
			if len(after) != len(before)+1 {
				t.Fatalf("rows = %+v, want the seeded rows and the one written behind the cache", after)
			}
			for _, b := range before {
				if !namedRowsContain(after, b) {
					t.Fatalf("seeded row %+v changed: %+v", b, after)
				}
			}
		})
	}
}

// namedRowsContain reports whether rows holds want, compared as
// namedComparableRows blanks them.
func namedRowsContain(rows []beads.Bead, want beads.Bead) bool {
	for _, r := range rows {
		if reflect.DeepEqual(r, want) {
			return true
		}
	}
	return false
}

// namedGuardedStore panics on a write to any row but target, and on any
// create when target is set.
type namedGuardedStore struct {
	beads.Store
	target string
}

// ConditionalWritesResolveTarget resolves the conditional writer LL5's row
// token CAS needs to the wrapped store.
func (s namedGuardedStore) ConditionalWritesResolveTarget() beads.Store { return s.Store }

func (s namedGuardedStore) guard(id string) {
	if id != s.target {
		panic("named create wrote row " + id)
	}
}

func (s namedGuardedStore) Create(b beads.Bead) (beads.Bead, error) {
	s.guard("")
	return s.Store.Create(b)
}

func (s namedGuardedStore) Update(id string, opts beads.UpdateOpts) error {
	s.guard(id)
	return s.Store.Update(id, opts)
}

func (s namedGuardedStore) SetMetadata(id, k, v string) error {
	s.guard(id)
	return s.Store.SetMetadata(id, k, v)
}

func (s namedGuardedStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.guard(id)
	return s.Store.SetMetadataBatch(id, kvs)
}

func (s namedGuardedStore) Close(id string) error {
	s.guard(id)
	return s.Store.Close(id)
}

// namedTreeSnapshot lists every path under dir with its size and mode.
func namedTreeSnapshot(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	if err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, fmt.Sprintf("%s %d %s %d", path, info.Size(), info.Mode(), info.ModTime().UnixNano()))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// adoptStampProvider is probePanicProvider answering only LL5's identity
// stamp on an AdoptLive, which it records: reads of GC_SESSION_ID,
// GC_INSTANCE_TOKEN and BEADS_HOLDER_TOKEN, and writes of those and
// GC_RUNTIME_EPOCH. Any other call panics.
type adoptStampProvider struct {
	probePanicProvider
	meta map[string]string
}

func (p adoptStampProvider) GetMeta(name, key string) (string, error) {
	switch key {
	case "GC_SESSION_ID", "GC_INSTANCE_TOKEN", "BEADS_HOLDER_TOKEN":
		return p.meta[key], nil
	}
	panic("create effect read " + key + " from runtime " + name)
}

func (p adoptStampProvider) SetMeta(name, key, value string) error {
	switch key {
	case "GC_SESSION_ID", "GC_INSTANCE_TOKEN", "BEADS_HOLDER_TOKEN", "GC_RUNTIME_EPOCH":
		p.meta[key] = value
		return nil
	}
	panic("create effect wrote " + key + " to runtime " + name)
}

// Kills: the S2 side effects (a settings projection, a work-dir mkdir, a
// skill snapshot written or removed, a RepairEmptyType write on another row)
// and any provider probe, including SyncRuntimeAlias on an adopt (P3-6b
// T11), other than LL5's identity stamp, which runs here and is the only
// provider call (adoptStampProvider). The agent works in a worktree dir and
// has a skill on a stage-2 session provider, so legacy's resolution would
// create the dir, or rewrite the snapshot its earlier resolution left there.
// (City skills feed the snapshot only through the catalogs read-only
// resolution does not load; the agent-local skill reaches the snapshot step
// without them.)
func TestCreateEffect_NamedNeverProbesNorWritesFiles(t *testing.T) {
	for _, tc := range []struct {
		name string
		// reopen seeds an eligible closed row; primed seeds the work dir
		// with the skill snapshot a legacy resolution leaves behind.
		reopen, primed bool
	}{
		{name: "create"},
		{name: "reopen", reopen: true},
		{name: "create in a primed work dir", primed: true},
		{name: "reopen in a primed work dir", reopen: true, primed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			skills := filepath.Join(dir, "agents", "mayor", "skills")
			writeNamedFixtureFile(t, filepath.Join(skills, "plan", "SKILL.md"), "---\nname: plan\ndescription: test\n---\nbody\n")
			cfg := namedTestCity(config.Agent{Name: "mayor", Provider: "claude", WorkDir: ".gc/worktrees/mayor", SkillsDir: skills},
				config.NamedSession{Template: "mayor", Mode: "always"}, map[string]config.ProviderSpec{"claude": {}})
			cfg.Session.Provider = "tmux"
			workDir := filepath.Join(dir, ".gc", "worktrees", "mayor")
			if tc.primed {
				writeNamedFixtureFile(t, skillSnapshotFilePath(workDir, "mayor"), "c25hcHNob3Q=")
			}
			// A legacy session-name lookup would find, and repair, this
			// type-less row of the same template.
			mem := beads.NewMemStoreFrom(100, []beads.Bead{{
				ID: "gc-bait", Title: "bait", Status: "open", Labels: []string{sessionBeadLabel},
				Metadata: map[string]string{"template": "mayor", "session_name": "bait"},
			}}, nil)
			if err := beads.StampOpenedStore(mem, "MemStore", gate.Auto, nil, nil); err != nil {
				t.Fatal(err)
			}
			target := ""
			if tc.reopen {
				target = seedClosedNamedRow(t, mem, cfg, nil).ID
			}
			before := namedTreeSnapshot(t, dir)
			h := newNamedHarness(t, dir, func(host *createEffectHost) {
				host.lookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
			})
			h.reserve(t, "c1")
			plan := namedPlan(t, cfg, "c1", "mayor")
			plan.Named.AdoptLive = true
			sp := adoptStampProvider{meta: map[string]string{}}
			h.runAll(t, &createPass{cfg: cfg, sp: sp, store: namedGuardedStore{Store: mem, target: target}}, plan)

			e := h.entry(t)
			if !e.Landed {
				t.Fatalf("settlement = %+v, want landed", e)
			}
			if sp.meta["GC_SESSION_ID"] != e.RowID || sp.meta["GC_INSTANCE_TOKEN"] == "" {
				t.Fatalf("runtime meta = %v, want LL5's stamp of row %s to have run", sp.meta, e.RowID)
			}
			if after := namedTreeSnapshot(t, dir); !reflect.DeepEqual(after, before) {
				t.Fatalf("city dir changed:\nbefore %v\nafter  %v", before, after)
			}
			if !tc.reopen {
				row, err := mem.Get(e.RowID)
				if err != nil {
					t.Fatal(err)
				}
				if got := row.Metadata["work_dir"]; got != workDir {
					t.Fatalf("work_dir = %q, want the worktree dir %q resolved without creating it", got, workDir)
				}
			}
		})
	}
}

// Kills: the S4 pass-rate loop for named creates (P3-6b T15, AM-N8). A
// malformed Claude settings override fails every resolution, a refusal the
// census never shows; every attempt settles refusing named:mayor, and a
// landed create settles with no refusal, which resets the record. (A closed
// row that owns the runtime name does not refuse a named create on main: the
// configured owner may reuse it.)
func TestCreateEffect_NamedRefusalRefusesPerIdentity(t *testing.T) {
	cfg := namedTestCity(config.Agent{Name: "mayor", Provider: "claude"}, config.NamedSession{Template: "mayor", Mode: "always"},
		map[string]config.ProviderSpec{"claude": {}})
	broken, healthy := t.TempDir(), t.TempDir()
	writeNamedFixtureFile(t, filepath.Join(broken, ".claude", "settings.json"), "{not json")
	store := beads.NewMemStore()
	h := newCreateHarness(t, nil)
	attempt := func(id, cityPath string) createPlan {
		t.Helper()
		h.x.host.cityPath = cityPath
		h.reserve(t, id)
		plan := namedPlan(t, cfg, id, "mayor")
		h.runAll(t, &createPass{cfg: cfg, store: store}, plan)
		return plan
	}
	for i := range 3 {
		plan := attempt(fmt.Sprintf("c%d", i), broken)
		if k := createBackoffKey(plan.identity().key()); k != "named:mayor" {
			t.Fatalf("refusal key = %q, want named:mayor", k)
		}
		h.assertRefused(t, plan, createStageResolve)
	}
	attempt("ok", healthy)
	if s := h.settlementOf(t, "ok"); !s.Landed || s.Stage != "" {
		t.Fatalf("settlement %+v, want landed with no refusal", s)
	}
}

// aliasSquatter seeds an open manual session row that holds the alias mayor.
func aliasSquatter(t *testing.T, store beads.Store) {
	t.Helper()
	if _, err := store.Create(beads.Bead{
		Title: "squatter", Type: sessionBeadType, Labels: []string{sessionBeadLabel, "agent:someone"},
		Metadata: map[string]string{"session_name": "s-squatter", "alias": "mayor", "agent_name": "someone", "template": "someone", "state": "active", "manual_session": "true"},
	}); err != nil {
		t.Fatal(err)
	}
}

// Kills: rows minted for a removed or reshaped named session (P3-6b T16).
func TestCreateEffect_NamedStalePlanFailsNoWrite(t *testing.T) {
	cases := map[string]func(*namedCreatePlan, *config.City){
		"spec removed":       func(_ *namedCreatePlan, cfg *config.City) { cfg.NamedSessions = nil },
		"session renamed":    func(p *namedCreatePlan, _ *config.City) { p.SessionName = "old-name" },
		"template changed":   func(p *namedCreatePlan, _ *config.City) { p.Template = "other" },
		"mode changed":       func(_ *namedCreatePlan, cfg *config.City) { cfg.NamedSessions[0].Mode = "on_demand" },
		"backing agent gone": func(_ *namedCreatePlan, cfg *config.City) { cfg.Agents = nil },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := mayorCity()
			store := beads.NewMemStore()
			h := newNamedHarness(t, t.TempDir(), nil)
			h.reserve(t, "c1")
			plan := namedPlan(t, cfg, "c1", "mayor")
			edit(plan.Named, cfg)
			h.runAll(t, &createPass{cfg: cfg, store: store}, plan)
			assertFailedNoWrite(t, h)
			h.assertRefused(t, plan, createStageStalePlan)
			if rows := sessionRows(t, store); len(rows) != 0 {
				t.Fatalf("rows = %+v, want none", rows)
			}
		})
	}
}

// Kills: a settlement that loses the row a named create wrote or reopened,
// or names a row for a refusal (P3-6b T17). The settlement is the planner's
// only wake (C5.12), and the pass reads the reopened row from the census.
func TestCreateEffect_NamedSettlementNamesTheRow(t *testing.T) {
	cfg := mayorCity()
	run := func(t *testing.T, store beads.Store) createSettlement {
		t.Helper()
		h := newNamedHarness(t, t.TempDir(), nil)
		h.reserve(t, "c1")
		h.runAll(t, &createPass{cfg: cfg, store: store}, namedPlan(t, cfg, "c1", "mayor"))
		return h.entry(t)
	}
	t.Run("create", func(t *testing.T) {
		if s := run(t, beads.NewMemStore()); !s.Landed || s.RowID == "" || s.RetargetRowID != "" {
			t.Fatalf("settlement %+v, want landed with its new row", s)
		}
	})
	t.Run("reopen", func(t *testing.T) {
		store := beads.NewMemStore()
		closed := seedClosedNamedRow(t, store, cfg, nil)
		if s := run(t, store); !s.Landed || s.RowID != closed.ID || s.RetargetRowID != closed.ID {
			t.Fatalf("settlement %+v, want landed on %s", s, closed.ID)
		}
	})
	t.Run("ambiguous create", func(t *testing.T) {
		if s := run(t, failingCreateStore{Store: beads.NewMemStore()}); !s.Ambiguous || s.RetargetRowID != "" || s.Stage != "" {
			t.Fatalf("settlement %+v, want ambiguous with no row", s)
		}
	})
	t.Run("refused", func(t *testing.T) {
		store := beads.NewMemStore()
		aliasSquatter(t, store)
		if s := run(t, store); s.Landed || s.Ambiguous || s.RowID != "" || s.Stage != createStageFence {
			t.Fatalf("settlement %+v, want a fence refusal with no row", s)
		}
	})
}

// Kills: two canonical rows for one identity when the effect races another
// compliant creator under real city flocks (P3-6b T10, R8 for the named
// kind): the CLI's materialize reopens the same closed row, and legacy's sync
// create arm mints under the same lock set.
func TestCreateEffect_NamedConcurrentWithCLIMaterializeYieldsOneCanonical(t *testing.T) {
	cfg := mayorCity()
	openCanonical := func(t *testing.T, store beads.Store) int {
		t.Helper()
		rows, err := store.List(beads.ListQuery{Metadata: map[string]string{namedSessionIdentityMetadata: "mayor"}})
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}
	race := func(t *testing.T, store beads.Store, cityPath string, rival func()) {
		t.Helper()
		h := newNamedHarness(t, cityPath, func(host *createEffectHost) { host.withLocks = nil })
		h.reserve(t, "c1")
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			h.runAll(t, &createPass{cfg: cfg, store: store}, namedPlan(t, cfg, "c1", "mayor"))
		}()
		go func() {
			defer wg.Done()
			rival()
		}()
		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()
		awaitClose(t, done, "the effect and its rival")
	}
	for round := 0; round < 10; round++ {
		t.Run(fmt.Sprintf("reopen against the CLI %d", round), func(t *testing.T) {
			store, cityPath := beads.NewMemStore(), t.TempDir()
			seedClosedNamedRow(t, store, cfg, nil)
			race(t, store, cityPath, func() {
				if _, err := resolveSessionIDMaterializingNamed(cityPath, cfg, store, "mayor"); err != nil {
					t.Errorf("CLI materialize: %v", err)
				}
			})
			if n, total := openCanonical(t, store), len(sessionRows(t, store)); n != 1 || total != 1 {
				t.Fatalf("open canonical rows = %d of %d rows, want the one reopened row", n, total)
			}
		})
		t.Run(fmt.Sprintf("create against legacy sync %d", round), func(t *testing.T) {
			store, cityPath := beads.NewMemStore(), t.TempDir()
			race(t, store, cityPath, func() { legacyNamedSync(t, cityPath, cfg, store, runtime.NewFake()) })
			if n := openCanonical(t, store); n != 1 {
				t.Fatalf("open canonical rows = %d, want one", n)
			}
		})
	}
}

// Kills: the read-only resolver drifting from the side-effecting one in
// anything create metadata reads, and a malformed Claude override that the
// projection refuses but the read-only resolution accepts (P3-6b T12).
func TestReadOnlyResolveMatchesResolveTemplateMetadata(t *testing.T) {
	resolve := func(t *testing.T, dir string, cfg *config.City, identity string, readOnly bool) (TemplateParams, error) {
		t.Helper()
		spec, ok := findNamedSessionSpec(cfg, "test-city", identity)
		if !ok {
			t.Fatalf("no spec for %q", identity)
		}
		var bp *agentBuildParams
		if readOnly {
			bp = newReadOnlyAgentBuildParams("test-city", dir, cfg, nil, namedEffectNow, io.Discard)
		} else {
			bp = newAgentBuildParams("test-city", dir, cfg, nil, namedEffectNow, nil, io.Discard)
		}
		return resolveTemplate(bp, spec.Agent, identity, buildFingerprintExtra(spec.Agent))
	}
	for _, fx := range namedFixtures() {
		t.Run(fx.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := fx.city(dir)
			if fx.prepare != nil {
				fx.prepare(t, dir, beads.NewMemStore())
			}
			ro, err := resolve(t, dir, cfg, fx.identity, true)
			if err != nil {
				t.Fatalf("read-only resolve: %v", err)
			}
			legacy, err := resolve(t, dir, cfg, fx.identity, false)
			if err != nil {
				t.Fatalf("legacy resolve: %v", err)
			}
			type metadataInputs struct {
				Command, WorkDir, WakeMode, RigName, SessionName, LiveHash string
				Provider                                                   *config.ResolvedProvider
			}
			of := func(tp TemplateParams) metadataInputs {
				return metadataInputs{tp.Command, tp.WorkDir, tp.WakeMode, tp.RigName, tp.SessionName, runtime.LiveFingerprint(templateParamsToConfig(tp)), tp.ResolvedProvider}
			}
			if got, want := of(ro), of(legacy); !reflect.DeepEqual(got, want) {
				t.Fatalf("read-only resolution differs:\nread-only: %+v\nlegacy:    %+v", got, want)
			}
		})
	}
	t.Run("malformed claude override fails both", func(t *testing.T) {
		dir := t.TempDir()
		cfg := namedFixtures()[0].city(dir)
		writeNamedFixtureFile(t, filepath.Join(dir, ".claude", "settings.json"), "{not json")
		if _, err := resolve(t, dir, cfg, "mayor", true); err == nil || !strings.Contains(err.Error(), "invalid JSON") {
			t.Fatalf("read-only resolve = %v, want the override's invalid JSON error", err)
		}
		if _, err := resolve(t, dir, cfg, "mayor", false); err == nil || !strings.Contains(err.Error(), "invalid JSON") {
			t.Fatalf("legacy resolve = %v, want the override's invalid JSON error", err)
		}
	})
}

// Kills: a retarget of an entry that is not an issued create, or one that
// loses the row on its commit (AM-N2).
func TestLedgerRetargetMarksIssuedCreatesOnly(t *testing.T) {
	l := newIntentLedger(func() time.Time { return namedEffectNow })
	create := ledgerEntry{ID: "c1", Kind: kindCreate, Key: rowKey{Leg: "sessions"}, Marker: ledgerMarker{InstanceToken: "tok"}}
	grant := ledgerEntry{ID: "g1", Kind: kindGrant, Key: rowKey{Leg: "sessions", ID: "gc-1"}}
	if !l.Reserve(create) || !l.Reserve(grant) {
		t.Fatal("reserve refused")
	}
	if l.Retarget("c1", "gc-9") {
		t.Fatal("retargeted a reserved create")
	}
	if _, ok := l.IssueCreate("c1"); !ok || !l.Issue("g1", grant.Key) {
		t.Fatal("issue refused")
	}
	if l.Retarget("g1", "gc-9") || l.Retarget("c1", "") || l.Retarget("missing", "gc-9") {
		t.Fatal("retargeted a grant, an empty row or a missing entry")
	}
	if !l.Retarget("c1", "gc-9") {
		t.Fatal("issued create not retargeted")
	}
	e, _ := ledgerEntryOf(l, "c1")
	if e.Key != (rowKey{Leg: "sessions", ID: "gc-9"}) || e.Marker != (ledgerMarker{RowID: "gc-9", InstanceToken: "tok"}) {
		t.Fatalf("retargeted entry = %+v", e)
	}
	l.Commit("c1", ledgerMarker{RowID: "gc-9", InstanceToken: "tok"})
	if l.Retarget("c1", "gc-8") {
		t.Fatal("retargeted a committed create")
	}
	if e, _ = ledgerEntryOf(l, "c1"); e.Key.ID != "gc-9" {
		t.Fatalf("commit dropped the reopened row: %+v", e)
	}
}

// namedLockFiles lists the city identifier lock files a run created.
func namedLockFiles(t *testing.T, cityPath string) []string {
	t.Helper()
	entries, err := os.ReadDir(citylayout.SessionNameLocksDir(cityPath))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// listFailStore fails the identity-rows query.
type namedListFailStore struct{ beads.Store }

func (s namedListFailStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if _, ok := q.Metadata[namedSessionIdentityMetadata]; ok {
		return nil, errors.New("identity index unavailable")
	}
	return s.Store.List(q)
}

// Kills: the extraction changing legacy (P3-6b T18). The legacy reopen
// helper, now its lookup plus the shared batch builder and locked body, runs
// beside its frozen pre-refactor copy on twin worlds: the result, the rows,
// stderr and the lock files must match. The sync create arm's metadata
// builder matches the frozen inline block over pool, dependency, manual and
// named inputs.
func TestSyncSessionBeadsNamedArmMatchesPreRefactor(t *testing.T) {
	type world struct {
		dir    string
		store  beads.Store
		stderr strings.Builder
	}
	// locked and logs say which branch a case must reach: the identifier
	// locks taken, and a stderr report.
	type reopenCase struct {
		state  string
		bound  string
		locked bool
		logs   bool
		cfg    func() *config.City
		seed   func(t *testing.T, store beads.Store, cfg *config.City)
		wrap   func(beads.Store) beads.Store
	}
	eligible := func(t *testing.T, store beads.Store, cfg *config.City) {
		seedClosedNamedRow(t, store, cfg, nil)
	}
	cases := map[string]reopenCase{
		"stopped with bound step": {state: "stopped", bound: "gc-step", locked: true, seed: eligible},
		"active":                  {state: "active", locked: true, seed: eligible},
		"creating":                {state: "creating", locked: true, seed: eligible},
		"no closed row":           {state: "stopped", seed: func(*testing.T, beads.Store, *config.City) {}},
		"other session_name": {state: "stopped", seed: func(t *testing.T, s beads.Store, c *config.City) {
			seedClosedNamedRow(t, s, c, map[string]string{"session_name": "x"})
		}},
		"empty session_name": {state: "stopped", seed: func(t *testing.T, s beads.Store, c *config.City) {
			seedClosedNamedRow(t, s, c, map[string]string{"session_name": ""})
		}},
		"spec removed":             {state: "stopped", seed: eligible, cfg: func() *config.City { c := mayorCity(); c.NamedSessions = nil; return c }},
		"identity rows unreadable": {state: "stopped", logs: true, seed: eligible, wrap: func(s beads.Store) beads.Store { return namedListFailStore{s} }},
		"alias held": {state: "stopped", locked: true, logs: true, seed: func(t *testing.T, s beads.Store, c *config.City) {
			eligible(t, s, c)
			aliasSquatter(t, s)
		}},
		"session_name held": {state: "stopped", locked: true, logs: true, seed: func(t *testing.T, s beads.Store, c *config.City) {
			eligible(t, s, c)
			if _, err := s.Create(beads.Bead{Title: "holder", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
				"session_name": config.NamedSessionRuntimeName("test-city", c.Workspace, "mayor"), "state": "active", "manual_session": "true",
			}}); err != nil {
				t.Fatal(err)
			}
		}},
		"write fails": {state: "stopped", locked: true, logs: true, seed: eligible, wrap: func(s beads.Store) beads.Store {
			return &namedHookStore{Store: s, failTx: errors.New("disk full")}
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			run := func(reopen func(cityPath string, store beads.Store, cfg *config.City, cityName, identity, sessionName, state string, now time.Time, extra map[string]string, stderr io.Writer) (beads.Bead, string, bool)) (*world, beads.Bead, string, bool) {
				cfg := mayorCity()
				if tc.cfg != nil {
					cfg = tc.cfg()
				}
				w := &world{dir: t.TempDir(), store: beads.NewMemStore()}
				tc.seed(t, w.store, mayorCity())
				store := w.store
				if tc.wrap != nil {
					store = tc.wrap(store)
				}
				sn := config.NamedSessionRuntimeName("test-city", cfg.Workspace, "mayor")
				b, gotSN, ok := reopen(w.dir, store, cfg, "test-city", "mayor", sn, tc.state, namedEffectNow, startupKickoffReopenMetadata(tc.bound, namedEffectNow), &w.stderr)
				b.CreatedAt, b.UpdatedAt = time.Time{}, time.Time{}
				return w, b, gotSN, ok
			}
			before, bb, bsn, bok := run(reopenClosedConfiguredNamedSessionBeadPreRefactor)
			after, ab, asn, aok := run(reopenClosedConfiguredNamedSessionBead)
			if bok != aok || bsn != asn || !reflect.DeepEqual(bb, ab) {
				t.Fatalf("result = (%v, %q, %+v), frozen (%v, %q, %+v)", aok, asn, ab, bok, bsn, bb)
			}
			if got, want := namedComparableRows(t, after.store), namedComparableRows(t, before.store); !reflect.DeepEqual(got, want) {
				t.Fatalf("rows differ:\nrefactored %+v\nfrozen     %+v", got, want)
			}
			if got, want := after.stderr.String(), before.stderr.String(); got != want {
				t.Fatalf("stderr = %q, frozen %q", got, want)
			}
			if got, want := namedLockFiles(t, after.dir), namedLockFiles(t, before.dir); !reflect.DeepEqual(got, want) {
				t.Fatalf("lock files = %v, frozen %v", got, want)
			}
			if locks := namedLockFiles(t, before.dir); (len(locks) == 2) != tc.locked || (before.stderr.Len() > 0) != tc.logs {
				t.Fatalf("fixture reached locks %v and stderr %q, want locked=%v logs=%v", locks, before.stderr.String(), tc.locked, tc.logs)
			}
		})
	}

	t.Run("create metadata builder", func(t *testing.T) {
		resolved := &config.ResolvedProvider{Name: "fast-codex", BuiltinAncestor: "codex", ResumeFlag: "--resume", ResumeStyle: "flag", ResumeCommand: "codex resume {{.Key}}", SessionIDFlag: "--session-id"}
		inputs := map[string]struct {
			tp                   TemplateParams
			sn, agentName, state string
			poolSlot             int
		}{
			"named bound active":  {tp: TemplateParams{TemplateName: "mayor", InstanceName: "boss", ConfiguredNamedIdentity: "boss", ConfiguredNamedMode: "on_demand", BoundStepID: "gc-9", WorkDir: "/w", WakeMode: "fresh", Command: "run", ResolvedProvider: resolved}, sn: "test-city--boss", agentName: "boss", state: "active"},
			"named pending":       {tp: TemplateParams{TemplateName: "mayor", InstanceName: "mayor", ConfiguredNamedIdentity: "mayor", ConfiguredNamedMode: "always", RigName: "rig"}, sn: "test-city--mayor", agentName: "mayor", state: "start-pending"},
			"pool slot":           {tp: TemplateParams{TemplateName: "worker", InstanceName: "worker-2", RigName: "rig", DependencyOnly: true, ResolvedProvider: resolved}, sn: "rig--worker-2", agentName: "worker-2", state: "start-pending", poolSlot: 2},
			"ephemeral singleton": {tp: TemplateParams{TemplateName: "rig/crew", InstanceName: "rig/crew", RigName: "rig"}, sn: "crew", agentName: "rig/crew", state: "start-pending"},
			"manual":              {tp: TemplateParams{TemplateName: "helper", InstanceName: "helper", ManualSession: true, Command: "x"}, sn: "helper", agentName: "helper", state: "active"},
		}
		for name, in := range inputs {
			got := syncCreateMetadata(in.tp, in.sn, in.agentName, "live-hash", in.state, "tok", in.poolSlot, namedEffectNow)
			want := syncCreateMetadataPreRefactor(in.tp, in.sn, in.agentName, "live-hash", in.state, "tok", in.poolSlot, namedEffectNow)
			for _, m := range []map[string]string{got, want} {
				if m["session_key"] != "" {
					m["session_key"] = "<key>"
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s: metadata = %v, frozen %v", name, got, want)
			}
		}
	})
}

// Kills: a named create or reopen without the flock (S6, AM-N1): legacy
// creates unlocked on a lock error; the effect writes nothing and records a
// create backoff.
func TestCreateEffect_NamedLockFailureFailsClosed(t *testing.T) {
	cfg := mayorCity()
	for _, reopen := range []bool{false, true} {
		store := beads.NewMemStore()
		if reopen {
			seedClosedNamedRow(t, store, cfg, nil)
		}
		before := namedComparableRows(t, store)
		h := newNamedHarness(t, t.TempDir(), func(host *createEffectHost) {
			host.withLocks = func(string, []string, func() error) error { return errors.New("flock failed") }
		})
		h.reserve(t, "c1")
		plan := namedPlan(t, cfg, "c1", "mayor")
		h.runAll(t, &createPass{cfg: cfg, store: store}, plan)
		assertFailedNoWrite(t, h)
		h.assertRefused(t, plan, createStageLock)
		if after := namedComparableRows(t, store); !reflect.DeepEqual(after, before) {
			t.Fatalf("reopen=%v: rows changed without the lock: %+v", reopen, after)
		}
	}
}

// namedCondStore is a fenced MemStore (conditional writes on) that records
// every write. err, when set, refuses its create and its conditional update
// without writing; panics makes either panic before writing.
type namedCondStore struct {
	*beads.MemStore
	mu     sync.Mutex
	writes []string
	err    error
	panics bool
}

func newNamedCondStore(t *testing.T) *namedCondStore {
	t.Helper()
	mem := beads.NewMemStore()
	if err := beads.StampOpenedStore(mem, "MemStore", gate.Auto, nil, nil); err != nil {
		t.Fatal(err)
	}
	return &namedCondStore{MemStore: mem}
}

func (s *namedCondStore) note(op string) error {
	s.mu.Lock()
	s.writes = append(s.writes, op)
	s.mu.Unlock()
	if s.panics {
		panic("store driver crashed during " + op)
	}
	return s.err
}

func (s *namedCondStore) Create(b beads.Bead) (beads.Bead, error) {
	if err := s.note("create"); err != nil {
		return beads.Bead{}, err
	}
	return s.MemStore.Create(b)
}

func (s *namedCondStore) UpdateIfMatch(id string, rev int64, opts beads.UpdateOpts) error {
	if err := s.note("update-if-match " + id); err != nil {
		return err
	}
	return s.MemStore.UpdateIfMatch(id, rev, opts)
}

func (s *namedCondStore) Update(id string, opts beads.UpdateOpts) error {
	_ = s.note("update " + id)
	return s.MemStore.Update(id, opts)
}

func (s *namedCondStore) SetMetadata(id, key, value string) error {
	_ = s.note("set " + id)
	return s.MemStore.SetMetadata(id, key, value)
}

func (s *namedCondStore) SetMetadataBatch(id string, kvs map[string]string) error {
	_ = s.note("set-batch " + id)
	return s.MemStore.SetMetadataBatch(id, kvs)
}

func (s *namedCondStore) Tx(msg string, fn func(beads.Tx) error) error {
	_ = s.note("tx " + msg)
	return s.MemStore.Tx(msg, fn)
}

func (s *namedCondStore) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.writes...)
}

// Kills: a conditional reopen that drops the open status, or fences on a
// revision other than the one it read, so it never lands (AM-N4). On a
// fenced store with no rival, the reopen is one conditional update that
// opens the row with legacy's reopen batch and the kickoff keys.
func TestCreateEffect_NamedConditionalReopenWritesOnceAndOpensTheRow(t *testing.T) {
	cfg := mayorCity()
	store := newNamedCondStore(t)
	closed := seedClosedNamedRow(t, store.MemStore, cfg, nil)
	h := newNamedHarness(t, t.TempDir(), nil)
	h.reserve(t, "c1")
	plan := namedPlan(t, cfg, "c1", "mayor")
	plan.Named.BoundStepID = "gc-step"
	h.runAll(t, &createPass{cfg: cfg, store: store}, plan)

	if got, want := store.recorded(), []string{"update-if-match " + closed.ID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("writes = %v, want %v", got, want)
	}
	row, err := store.Get(closed.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := reopenNamedSessionBatch("stopped", "idle", namedEffectNow)
	maps.Copy(want, startupKickoffReopenMetadata("gc-step", namedEffectNow))
	for k, v := range want {
		if row.Metadata[k] != v {
			t.Errorf("reopened %s = %q, want %q", k, row.Metadata[k], v)
		}
	}
	if row.Status != "open" || row.Revision == closed.Revision {
		t.Fatalf("row = %s at revision %d (read at %d), want open and written", row.Status, row.Revision, closed.Revision)
	}
	if e := h.entry(t); !e.Landed || e.RowID != closed.ID {
		t.Fatalf("settlement = %+v, want a landed reopen of %s", e, closed.ID)
	}
	h.assertNoRefusal(t)
}

// Kills: a refused write settled as ambiguous, which holds the entry until
// the hard bound and skips the refusal, and a panic in the write settled as
// no write, which refuses a row that may exist (E6-E8, latent 1). A store
// that cannot fence, a gate refusal, a code-less not-found and a lost fence
// prove nothing was written, for the create and the reopen alike; a panic
// or a connection error during the write is ambiguous.
func TestCreateEffect_NamedRefusedWriteIsNoWriteAndPanicIsAmbiguous(t *testing.T) {
	cfg := mayorCity()
	refused := map[string]error{
		"conditional writes unsupported": beads.ErrConditionalWriteUnsupported,
		"gate refusal":                   &beads.GateRefusalError{Verb: "update", Code: "close_authority"},
		"code-less not found":            fmt.Errorf("conditional update: %w", beads.ErrNotFound),
		"precondition failed":            &beads.PreconditionFailedError{Expected: 1, Current: 2},
	}
	for _, reopen := range []bool{false, true} {
		kind := map[bool]string{false: "create", true: "reopen"}[reopen]
		run := func(t *testing.T, store *namedCondStore) (*createHarness, createPlan, string) {
			t.Helper()
			closedID := ""
			if reopen {
				closedID = seedClosedNamedRow(t, store.MemStore, cfg, nil).ID
			}
			h := newNamedHarness(t, t.TempDir(), nil)
			h.reserve(t, "c1")
			plan := namedPlan(t, cfg, "c1", "mayor")
			h.runAll(t, &createPass{cfg: cfg, store: store}, plan)
			if got := store.recorded(); len(got) != 1 {
				t.Fatalf("writes = %v, want the one attempted write", got)
			}
			return h, plan, closedID
		}
		for name, err := range refused {
			t.Run(kind+" "+name, func(t *testing.T) {
				store := newNamedCondStore(t)
				store.err = err
				h, plan, _ := run(t, store)
				assertFailedNoWrite(t, h)
				h.assertRefused(t, plan, createStageFence)
			})
		}
		for name, edit := range map[string]func(*namedCondStore){
			"panic":            func(s *namedCondStore) { s.panics = true },
			"connection error": func(s *namedCondStore) { s.err = errors.New("connection reset by peer") },
		} {
			t.Run(kind+" "+name, func(t *testing.T) {
				store := newNamedCondStore(t)
				edit(store)
				h, _, closedID := run(t, store)
				e := h.entry(t)
				if !e.Ambiguous || e.RowID != closedID {
					t.Fatalf("settlement = %+v, want ambiguous marked with row %q", e, closedID)
				}
				h.assertNoRefusal(t)
			})
		}
	}
}

// Kills: a named create that skips the transport capability gate, minting a
// row its provider cannot start (POOL-042 parity with the pool kind).
func TestCreateEffect_NamedValidatesTransport(t *testing.T) {
	cfg := &config.City{
		Workspace:     config.Workspace{Name: "test-city", Provider: "opencode"},
		Session:       config.SessionConfig{Provider: config.SessionTransportACP},
		Providers:     map[string]config.ProviderSpec{"opencode": {Command: "echo", ACPCommand: "echo", PromptMode: "none", SupportsACP: boolPtr(true)}},
		Agents:        []config.Agent{{Name: "mayor", Provider: "opencode", Session: config.SessionTransportTmux}},
		NamedSessions: []config.NamedSession{{Template: "mayor", Mode: "always"}},
	}
	store := beads.NewMemStore()
	h := newNamedHarness(t, t.TempDir(), func(host *createEffectHost) {
		host.lookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	})
	h.reserve(t, "c1")
	plan := namedPlan(t, cfg, "c1", "mayor")
	h.runAll(t, &createPass{cfg: cfg, sp: &acpOnlyDesiredStateProvider{Fake: runtime.NewFake()}, store: store}, plan)
	assertFailedNoWrite(t, h)
	h.assertRefused(t, plan, createStagePrepare)
	if rows := sessionRows(t, store); len(rows) != 0 {
		t.Fatalf("rows = %+v, want none for an unsupported transport", rows)
	}
}

// Kills: a resolution failure that does not gate the reopen (P3-6b §3.2
// step 4): legacy skips the spec, so it neither creates nor reopens. An
// eligible closed row stays closed, and the identity backs off.
func TestCreateEffect_NamedResolutionFailureGatesTheReopen(t *testing.T) {
	cfg := namedTestCity(config.Agent{Name: "mayor", Provider: "claude"}, config.NamedSession{Template: "mayor", Mode: "always"},
		map[string]config.ProviderSpec{"claude": {}})
	dir := t.TempDir()
	writeNamedFixtureFile(t, filepath.Join(dir, ".claude", "settings.json"), "{not json")
	store := beads.NewMemStore()
	closed := seedClosedNamedRow(t, store, cfg, nil)
	h := newNamedHarness(t, dir, nil)
	h.reserve(t, "c1")
	plan := namedPlan(t, cfg, "c1", "mayor")
	h.runAll(t, &createPass{cfg: cfg, store: store}, plan)

	assertFailedNoWrite(t, h)
	h.assertRefused(t, plan, createStageResolve)
	after, err := store.Get(closed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != "closed" || !reflect.DeepEqual(after.Metadata, closed.Metadata) {
		t.Fatalf("closed row = %s %v, want untouched", after.Status, after.Metadata)
	}
}

// Kills: the shared named overrides drifting from the inline block legacy's
// desired state ran before the extraction, notably the env a named runtime
// reads its identity from (GC_SESSION_ORIGIN, GC_AGENT), which no create
// metadata key records.
func TestApplyNamedTemplateOverridesMatchesPreRefactor(t *testing.T) {
	cfg := namedTestCity(config.Agent{Name: "mayor", StartCommand: "true"}, config.NamedSession{Name: "boss", Template: "mayor", Mode: "on_demand"}, nil)
	spec, ok := findNamedSessionSpec(cfg, "test-city", "boss")
	if !ok {
		t.Fatal("no spec for boss")
	}
	for name, base := range map[string]TemplateParams{
		"nil env":      {TemplateName: "mayor", InstanceName: "mayor"},
		"existing env": {TemplateName: "mayor", Env: map[string]string{"GC_AGENT": "mayor", "GC_SESSION_ORIGIN": "manual", "KEEP": "1"}},
	} {
		for _, bound := range []string{"", "gc-9"} {
			clone := func() TemplateParams {
				tp := base
				tp.Env = maps.Clone(base.Env)
				return tp
			}
			got, want := clone(), clone()
			applyNamedTemplateOverrides(&got, spec, "boss", bound)
			applyNamedTemplateOverridesPreRefactor(&want, spec, "boss", bound)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s, bound %q: overrides = %+v, frozen %+v", name, bound, got, want)
			}
			for k, v := range map[string]string{"GC_SESSION_ORIGIN": "named", "GC_AGENT": "boss", "GC_ALIAS": "boss", "GC_TEMPLATE": "mayor"} {
				if got.Env[k] != v {
					t.Fatalf("%s, bound %q: env %s = %q, want %q", name, bound, k, got.Env[k], v)
				}
			}
		}
	}
}

// Kills: the read-only resolver drifting from legacy's side-effecting
// resolution in the full create metadata over a city with a skill, a
// projected Claude override, a templated prompt, an overlay, scripts, an MCP
// server, a rig and worktree work dirs, in either order (a side effect of
// one run must not change the other's answer).
func TestReadOnlyResolveMatchesLegacyCreateMetadataRichCity(t *testing.T) {
	for _, legacyFirst := range []bool{false, true} {
		city := t.TempDir()
		writeNamedFixtureFile(t, filepath.Join(city, "city.toml"), "[workspace]\nname = \"test-city\"\n\n[beads]\nprovider = \"file\"\n")
		writeNamedFixtureFile(t, filepath.Join(city, "pack.toml"), "[pack]\nname = \"rv\"\nversion = \"0.1.0\"\nschema = 2\n")
		writeNamedFixtureFile(t, filepath.Join(city, "skills", "plan", "SKILL.md"), "---\nname: plan\ndescription: test\n---\nbody\n")
		writeNamedFixtureFile(t, filepath.Join(city, ".claude", "settings.json"), `{"permissions":{"allow":["Bash"]}}`)
		writeNamedFixtureFile(t, filepath.Join(city, "prompts", "mayor.template.md"), "hi {{ session \"mayor\" }} on {{ .DefaultBranch }} in {{ .WorkDir }}\n")
		writeNamedFixtureFile(t, filepath.Join(city, "overlays", "mayor", "CLAUDE.md"), "overlay\n")
		writeNamedFixtureFile(t, filepath.Join(city, ".gc", "scripts", "x.sh"), "#!/bin/sh\n")
		writeNamedFixtureFile(t, filepath.Join(city, "mcp", "notes.toml"), "name = \"notes\"\ncommand = \"uvx\"\nargs = [\"notes-mcp\"]\n")
		rig := filepath.Join(city, "demo")
		if err := os.MkdirAll(rig, 0o755); err != nil {
			t.Fatal(err)
		}
		cfg := &config.City{
			Workspace: config.Workspace{Name: "test-city", SessionTemplate: "{{.City}}-{{.Agent}}"},
			Providers: map[string]config.ProviderSpec{"claude": {}, "codex": {}, "gemini": {}},
			Rigs:      []config.Rig{{Name: "demo", Path: rig}},
			Agents: []config.Agent{
				{
					Name: "mayor", Provider: "claude", PromptTemplate: "prompts/mayor.template.md", WorkDir: ".gc/worktrees/mayor", OverlayDir: "overlays/mayor",
					SessionLive: []string{"tmux set -t {{.Session}} @x {{.DefaultBranch}} {{.WorkDir}} {{.Rig}} {{.ConfigDir}}"},
				},
				{Name: "coder", Provider: "codex", WakeMode: "fresh", SessionLive: []string{"x {{.Session}} {{.DefaultBranch}}"}},
				{
					Name: "witness", Dir: "demo", Provider: "gemini", PromptTemplate: "prompts/mayor.template.md", WorkDir: ".gc/worktrees/witness",
					SessionLive: []string{"y {{.Session}} {{.DefaultBranch}} {{.RigRoot}}"},
				},
			},
			NamedSessions: []config.NamedSession{
				{Template: "mayor", Mode: "always"},
				{Template: "coder", Mode: "on_demand"},
				{Template: "witness", Dir: "demo", Mode: "always"},
			},
			PackMCPDir: filepath.Join(city, "mcp"),
		}
		for _, id := range []string{"mayor", "coder", "demo/witness"} {
			spec, ok := findNamedSessionSpec(cfg, "test-city", id)
			if !ok {
				t.Fatalf("no spec %q", id)
			}
			readOnly := func() (TemplateParams, error) {
				bp := newReadOnlyAgentBuildParams("test-city", city, cfg, stubLookPath, namedEffectNow, io.Discard)
				return resolveTemplate(bp, spec.Agent, id, buildFingerprintExtra(spec.Agent))
			}
			legacy := func() (TemplateParams, error) {
				bp := newAgentBuildParams("test-city", city, cfg, nil, namedEffectNow, nil, io.Discard)
				bp.lookPath = stubLookPath
				return resolveTemplatePrepared(bp, spec.Agent, id, buildFingerprintExtra(spec.Agent))
			}
			var ro, lg TemplateParams
			var roErr, lgErr error
			if legacyFirst {
				lg, lgErr = legacy()
				ro, roErr = readOnly()
			} else {
				ro, roErr = readOnly()
				lg, lgErr = legacy()
			}
			if roErr != nil || lgErr != nil {
				t.Fatalf("legacyFirst=%v %s: read-only err %v, legacy err %v", legacyFirst, id, roErr, lgErr)
			}
			metadata := func(tp TemplateParams) map[string]string {
				applyNamedTemplateOverrides(&tp, spec, id, "gc-9")
				m := syncCreateMetadata(tp, spec.SessionName, id, runtime.LiveFingerprint(templateParamsToConfig(tp)), "start-pending", "tok", 0, namedEffectNow)
				if m["session_key"] != "" {
					m["session_key"] = "<key>"
				}
				return m
			}
			if got, want := metadata(ro), metadata(lg); !reflect.DeepEqual(got, want) {
				t.Fatalf("legacyFirst=%v %s: read-only metadata differs:\nread-only: %v\nlegacy:    %v", legacyFirst, id, got, want)
			}
		}
	}
}

// adoptLiveRun runs mayor's AdoptLive create on store over a stampFake alive
// under the session name, after prep sets it up. It returns the open row,
// the fake, the effect's stderr and the token the create wrote. A stamp
// never fails the create.
func adoptLiveRun(t *testing.T, store beads.Store, prep func(sp *stampFake, name string)) (beads.Bead, *stampFake, string, string) {
	t.Helper()
	cfg := mayorCity()
	var stderr strings.Builder
	h := newNamedHarness(t, t.TempDir(), func(host *createEffectHost) { host.stderr = &stderr })
	createToken := h.reserve(t, "c1")
	plan := namedPlan(t, cfg, "c1", "mayor")
	plan.Named.AdoptLive = true
	sp := newStampFake(t, plan.Named.SessionName)
	if prep != nil {
		prep(sp, plan.Named.SessionName)
	}
	h.runAll(t, &createPass{cfg: cfg, store: store, sp: sp}, plan)
	if s := h.entry(t); !s.Landed || s.Stage != "" || s.Err != nil {
		t.Fatalf("settlement = %+v, want landed: a stamp never fails the create", s)
	}
	rows, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("open rows = %v (%v), want the one named row", rows, err)
	}
	return rows[0], sp, stderr.String(), createToken
}

// fencedMemStore is a MemStore that resolves a conditional writer.
func fencedMemStore(t *testing.T) *beads.MemStore {
	t.Helper()
	mem := beads.NewMemStore()
	if err := beads.StampOpenedStore(mem, "MemStore", gate.Auto, nil, nil); err != nil {
		t.Fatal(err)
	}
	return mem
}

// mayorRuntime is mayor's runtime name in mayorCity.
func mayorRuntime(t *testing.T) string {
	t.Helper()
	return namedPlan(t, mayorCity(), "c1", "mayor").Named.SessionName
}

func setRuntimeMeta(t *testing.T, sp *stampFake, name string, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		if err := sp.Fake.SetMeta(name, k, v); err != nil {
			t.Fatal(err)
		}
	}
}

// Kills: a row token the runtime never carries (v5 O2, LL5). An AdoptLive
// create or reopen records the runtime's own token on the row by CAS and
// stamps the row's ID and generation on the runtime; with no conditional
// writer it never writes blind, and the runtime gets no session ID either.
func TestNamedAdoptLiveRecordsRuntimeToken(t *testing.T) {
	const rt = "rt-own-token"
	own := func(sp *stampFake, name string) {
		setRuntimeMeta(t, sp, name, map[string]string{"GC_INSTANCE_TOKEN": rt})
	}
	t.Run("create", func(t *testing.T) {
		row, sp, _, _ := adoptLiveRun(t, fencedMemStore(t), own)
		name := mayorRuntime(t)
		if got := row.Metadata["instance_token"]; got != rt {
			t.Fatalf("row instance_token = %q, want the runtime's %q", got, rt)
		}
		if sid, epoch := sp.meta(t, name, "GC_SESSION_ID"), sp.meta(t, name, "GC_RUNTIME_EPOCH"); sid != row.ID || epoch != row.Metadata["generation"] || epoch == "" {
			t.Fatalf("runtime GC_SESSION_ID %q epoch %q, want row %s at generation %q", sid, epoch, row.ID, row.Metadata["generation"])
		}
		if got, holder := sp.meta(t, name, "GC_INSTANCE_TOKEN"), sp.meta(t, name, "BEADS_HOLDER_TOKEN"); got != rt || holder != "" {
			t.Fatalf("runtime token %q holder %q, want its own %q kept and no holder written", got, holder, rt)
		}
	})
	t.Run("reopen", func(t *testing.T) {
		store := fencedMemStore(t)
		closed := seedClosedNamedRow(t, store, mayorCity(), nil)
		row, sp, _, _ := adoptLiveRun(t, store, own)
		name := mayorRuntime(t)
		if row.ID != closed.ID || row.Metadata["instance_token"] != rt {
			t.Fatalf("row = %s token %q, want reopened %s with the runtime's %q", row.ID, row.Metadata["instance_token"], closed.ID, rt)
		}
		if sid, epoch := sp.meta(t, name, "GC_SESSION_ID"), sp.meta(t, name, "GC_RUNTIME_EPOCH"); sid != closed.ID || epoch != "4" {
			t.Fatalf("runtime GC_SESSION_ID %q epoch %q, want row %s at the reopened row's generation 4", sid, epoch, closed.ID)
		}
	})
	t.Run("no conditional writer", func(t *testing.T) {
		row, sp, stderr, _ := adoptLiveRun(t, beads.NewMemStore(), own)
		if got := row.Metadata["instance_token"]; got == rt || got == "" {
			t.Fatalf("row instance_token = %q, want the create's own token, unwritten", got)
		}
		if got := sp.meta(t, mayorRuntime(t), "GC_SESSION_ID"); got != "" || !strings.Contains(stderr, "identity not stamped") {
			t.Fatalf("runtime GC_SESSION_ID %q, stderr %q; want none while the row lacks its token, logged", got, stderr)
		}
	})
}

// Kills: minting over a runtime's token, and leaving a tokenless runtime
// tokenless (v5 O2, LL5). A runtime with no token gets one minted on the row
// and the runtime, with BEADS_HOLDER_TOKEN beside it.
func TestNamedAdoptLiveMintsOnlyWithoutToken(t *testing.T) {
	row, sp, _, _ := adoptLiveRun(t, fencedMemStore(t), nil)
	name := mayorRuntime(t)
	token := row.Metadata["instance_token"]
	if token == "" || sp.meta(t, name, "GC_INSTANCE_TOKEN") != token || sp.meta(t, name, "BEADS_HOLDER_TOKEN") != token {
		t.Fatalf("row token %q, runtime token %q, holder %q; want one minted token on all three",
			token, sp.meta(t, name, "GC_INSTANCE_TOKEN"), sp.meta(t, name, "BEADS_HOLDER_TOKEN"))
	}
	if got := sp.meta(t, name, "GC_SESSION_ID"); got != row.ID {
		t.Fatalf("runtime GC_SESSION_ID = %q, want row %s", got, row.ID)
	}
}

// Kills: copying another session's token onto the row, and minting over a
// token the effect could not read (LL5 review). A runtime naming another
// session, or whose identity read fails, leaves the row with the create's
// token and the runtime untouched; the effect logs it.
func TestNamedAdoptLiveLeavesUnownedRuntimeAlone(t *testing.T) {
	boom := errors.New("server busy")
	for _, tc := range []struct {
		name string
		prep func(sp *stampFake, name string)
	}{
		{name: "another session's ID", prep: func(sp *stampFake, name string) {
			setRuntimeMeta(t, sp, name, map[string]string{"GC_SESSION_ID": "gc-other", "GC_INSTANCE_TOKEN": "other-tok"})
		}},
		{name: "unreadable ID", prep: func(sp *stampFake, _ string) { sp.getErr["GC_SESSION_ID"] = boom }},
		{name: "unreadable token", prep: func(sp *stampFake, _ string) { sp.getErr["GC_INSTANCE_TOKEN"] = boom }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var created string
			var before int
			row, sp, stderr, createToken := adoptLiveRun(t, fencedMemStore(t), func(sp *stampFake, name string) {
				tc.prep(sp, name)
				created, before = name, sp.CountCalls("SetMeta", name)
			})
			if got := row.Metadata["instance_token"]; got != createToken {
				t.Fatalf("row instance_token = %q, want the create's own %q kept", got, createToken)
			}
			if n := sp.CountCalls("SetMeta", created) - before; n != 0 || !strings.Contains(stderr, "identity not stamped") {
				t.Fatalf("SetMeta calls %d, stderr %q; want the runtime untouched and the refusal logged", n, stderr)
			}
		})
	}
}

// namedRowCASStore is a fenced store whose live row reads run before, and
// whose UpdateIfMatch can be failed by, the test.
type namedRowCASStore struct {
	*beads.MemStore
	beforeGet func(id string)
	lose      bool
	updates   int
}

func (s *namedRowCASStore) Get(id string) (beads.Bead, error) {
	if s.beforeGet != nil {
		s.beforeGet(id)
	}
	return s.MemStore.Get(id)
}

func (s *namedRowCASStore) UpdateIfMatch(id string, rev int64, opts beads.UpdateOpts) error {
	s.updates++
	if s.lose {
		return &beads.PreconditionFailedError{}
	}
	return s.MemStore.UpdateIfMatch(id, rev, opts)
}

func (s *namedRowCASStore) ConditionalWritesResolveTarget() beads.Store { return s }

// Kills: an unbounded or blind token write, and one that runs on a moved
// premise (LL5 review). A CAS lost every time gives up after rowTokenAttempts;
// a row whose token another writer changed is not written at all; either way
// the runtime gets no session ID, so it reads Unknown.
func TestNamedAdoptLiveCASPremise(t *testing.T) {
	t.Run("lost every time", func(t *testing.T) {
		store := &namedRowCASStore{MemStore: fencedMemStore(t), lose: true}
		_, sp, stderr, _ := adoptLiveRun(t, store, nil)
		if store.updates != rowTokenAttempts || sp.meta(t, mayorRuntime(t), "GC_SESSION_ID") != "" || !strings.Contains(stderr, "identity not stamped") {
			t.Fatalf("updates %d (want %d), runtime ID %q, stderr %q; want bounded retries, no stamp, logged",
				store.updates, rowTokenAttempts, sp.meta(t, mayorRuntime(t), "GC_SESSION_ID"), stderr)
		}
	})
	for _, moved := range []struct{ key, value string }{
		{"instance_token", "cli-token"},
		{"generation", "9"},
	} {
		t.Run(moved.key+" moved", func(t *testing.T) {
			store := &namedRowCASStore{MemStore: fencedMemStore(t)}
			row, sp, stderr, _ := adoptLiveRun(t, store, func(_ *stampFake, _ string) {
				store.beforeGet = func(id string) {
					_ = store.SetMetadata(id, moved.key, moved.value)
					store.beforeGet = nil
				}
			})
			if row.Metadata[moved.key] != moved.value || store.updates != 0 || sp.meta(t, mayorRuntime(t), "GC_SESSION_ID") != "" || !strings.Contains(stderr, "identity not stamped") {
				t.Fatalf("row %s %q, updates %d, stderr %q; want the other writer's value kept, no CAS, no stamp", moved.key, row.Metadata[moved.key], store.updates, stderr)
			}
		})
	}
}

// namedStaleCacheStore serves a stale revision from its cached reads and the
// true row from its Live handle.
type namedStaleCacheStore struct{ *beads.MemStore }

func (s namedStaleCacheStore) Get(id string) (beads.Bead, error) {
	b, err := s.MemStore.Get(id)
	b.Revision--
	return b, err
}

func (s namedStaleCacheStore) Handles() beads.StoreHandles {
	h := beads.HandlesFor(s.MemStore)
	h.Cached = namedStaleReader{h.Cached}
	return h
}

// namedStaleReader is a cached reader one revision behind.
type namedStaleReader struct{ beads.CachedReader }

func (r namedStaleReader) Get(id string) (beads.Bead, error) {
	b, err := r.CachedReader.Get(id)
	b.Revision--
	return b, err
}

func (s namedStaleCacheStore) ConditionalWritesResolveTarget() beads.Store { return s.MemStore }

// Kills: the token CAS reading the row through the cache (LL5 review): its
// revision must come from a live read, or a lagging cache loses every CAS.
func TestNamedAdoptLiveReadsTheRowLive(t *testing.T) {
	row, sp, stderr, _ := adoptLiveRun(t, namedStaleCacheStore{fencedMemStore(t)}, func(sp *stampFake, name string) {
		setRuntimeMeta(t, sp, name, map[string]string{"GC_INSTANCE_TOKEN": "rt-own-token"})
	})
	if row.Metadata["instance_token"] != "rt-own-token" || sp.meta(t, mayorRuntime(t), "GC_SESSION_ID") != row.ID {
		t.Fatalf("row token %q, runtime ID %q, stderr %q; want the CAS landed at the live revision", row.Metadata["instance_token"], sp.meta(t, mayorRuntime(t), "GC_SESSION_ID"), stderr)
	}
}

// panicStampFake panics on the first identity read.
type panicStampFake struct{ *stampFake }

func (panicStampFake) GetMeta(string, string) (string, error) { panic("provider exploded") }

// Kills: a stamp panic escaping adoptLiveIdentity (LL5 review): the create
// already landed, and a panic past it would settle it as ambiguous.
func TestNamedAdoptLiveStampPanicKeepsCreateLanded(t *testing.T) {
	cfg := mayorCity()
	var stderr strings.Builder
	h := newNamedHarness(t, t.TempDir(), func(host *createEffectHost) { host.stderr = &stderr })
	h.reserve(t, "c1")
	plan := namedPlan(t, cfg, "c1", "mayor")
	plan.Named.AdoptLive = true
	sp := panicStampFake{newStampFake(t, plan.Named.SessionName)}
	h.runAll(t, &createPass{cfg: cfg, store: fencedMemStore(t), sp: sp}, plan)
	if s := h.entry(t); !s.Landed || s.Ambiguous || s.Err != nil || !strings.Contains(stderr.String(), "panicked") {
		t.Fatalf("settlement = %+v, stderr %q; want landed, not ambiguous, panic logged", s, stderr.String())
	}
}
