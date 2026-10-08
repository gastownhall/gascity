package main

import (
	"fmt"
	"io"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The v2 start's template resolver (plan C5a1-wire): one build of params per
// env generation, legacy's resolution per row kind, no store write, and the
// memo's key covering every row field the resolution reads.

var templatesNow = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

// templateCity is a city with a configured named session (boss, backed by
// worker, so its identity is not its template's name), a
// multi-session agent for manual rows (helper) and an instance-expanding
// pool (polecat), and its store.
type templateCity struct {
	cfg      *config.City
	cityPath string
	store    *beads.MemStore
}

func newTemplateCity(t *testing.T) *templateCity {
	t.Helper()
	return &templateCity{
		cfg: &config.City{
			Workspace: config.Workspace{Name: "test-city"},
			Agents: []config.Agent{
				{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(2)},
				{Name: "helper", StartCommand: "true", MaxActiveSessions: intPtr(3)},
				{Name: "polecat", StartCommand: "true", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(-1)},
			},
			NamedSessions: []config.NamedSession{{Name: "boss", Template: "worker", Mode: "on_demand"}},
		},
		cityPath: t.TempDir(),
		store:    beads.NewMemStore(),
	}
}

// row creates an open session row with meta and returns its Info as the
// census reads it.
func (c *templateCity) row(t *testing.T, meta map[string]string) session.Info {
	t.Helper()
	b, err := c.store.Create(beads.Bead{Title: meta["session_name"], Type: session.BeadType, Labels: []string{session.LabelSession}, Metadata: meta})
	if err != nil {
		t.Fatal(err)
	}
	return c.info(t, b.ID)
}

func (c *templateCity) info(t *testing.T, id string) session.Info {
	t.Helper()
	snap, err := loadSessionBeadSnapshot(c.store)
	if err != nil {
		t.Fatal(err)
	}
	info, ok := snap.FindInfoByID(id)
	if !ok {
		t.Fatalf("row %s not in the snapshot", id)
	}
	return info
}

func (c *templateCity) params() *agentBuildParams {
	return newAgentBuildParams("test-city", c.cityPath, c.cfg, runtime.NewFake(), templatesNow, c.store, io.Discard)
}

func (c *templateCity) named(t *testing.T, extra map[string]string) session.Info {
	t.Helper()
	meta := map[string]string{
		"template": "worker", "session_name": config.NamedSessionRuntimeName("test-city", c.cfg.Workspace, "boss"), "state": "asleep",
		namedSessionMetadataKey: "true", namedSessionIdentityMetadata: "boss", namedSessionModeMetadata: "always", // stale: config says on_demand
	}
	for k, v := range extra {
		meta[k] = v
	}
	return c.row(t, meta)
}

func (c *templateCity) manual(t *testing.T) session.Info {
	t.Helper()
	return c.row(t, map[string]string{"template": "helper", "session_name": "s-helper-m", "state": "asleep", "session_origin": "manual", "alias": "scout"})
}

func (c *templateCity) pool(t *testing.T) session.Info {
	t.Helper()
	return c.row(t, map[string]string{
		"template": "polecat", "session_name": "polecat-2", "state": "asleep", "pool_slot": "2", "pool_managed": "true",
		beadmeta.TriggerBeadIDMetadataKey: "gc-w1",
	})
}

// Kills a manual or pool row resolved on another kind's path, or with
// another agent: the resolver's params equal those legacy's overlay puts in
// desired state for the row (manual: sessionBeadQualifiedNameInfo and the
// row's alias; pool: canonicalSessionIdentityWithConfigInfo's instance and
// the trigger env).
func TestTemplateResolverMatchesLegacyOverlay(t *testing.T) {
	for _, kind := range []string{"manual", "pool"} {
		t.Run(kind, func(t *testing.T) {
			c := newTemplateCity(t)
			info := map[string]func(*testing.T) session.Info{"manual": c.manual, "pool": c.pool}[kind](t)
			desired := map[string]TemplateParams{"held": {TemplateName: "polecat", SessionName: "held"}} // the pool wants capacity
			discoverSessionBeads(c.params(), c.cfg, desired, io.Discard)
			want, ok := desired[info.SessionNameMetadata]
			if !ok {
				t.Fatalf("legacy's overlay did not resolve %s", info.SessionNameMetadata)
			}
			got := newTemplateResolver(c.params()).Resolve(info)
			if got.Err != nil || !reflect.DeepEqual(got.TP, want) {
				t.Fatalf("resolver = %+v (err %v), want legacy's %+v", got.TP, got.Err, want)
			}
			if got.Agent == nil || got.Agent.QualifiedName() != info.Template {
				t.Fatalf("resolver agent = %v, want %s", got.Agent, info.Template)
			}
		})
	}
}

// Kills a named row resolved off the preserved-named path or without its
// named adjustments, and a resolver that rebinds the trigger: with a live
// trigger the resolver's params equal legacy's; with a parked one, legacy
// clears the stamp by a store write, while the resolver writes nothing and
// differs from legacy only in that stamp's env.
func TestTemplateResolverNamedMatchesLegacyWithoutRebind(t *testing.T) {
	for _, parked := range []bool{false, true} {
		t.Run(fmt.Sprintf("parked=%v", parked), func(t *testing.T) {
			shared := t.TempDir()
			resolve := func(legacy bool) (templateResolution, session.Info, *templateCity) {
				c := newTemplateCity(t)
				c.cityPath = shared
				work, err := c.store.Create(beads.Bead{Title: "work", Status: "open", IsBlocked: &parked})
				if err != nil {
					t.Fatal(err)
				}
				info := c.named(t, map[string]string{beadmeta.TriggerBeadIDMetadataKey: work.ID})
				if !legacy {
					return newTemplateResolver(c.params()).Resolve(info), info, c
				}
				tp, _, err := resolvePreservedConfiguredNamedSessionTemplate(c.cityPath, "test-city", c.cfg, runtime.NewFake(), c.store,
					[]session.Info{info}, info, &clock.Fake{Time: templatesNow}, io.Discard)
				return templateResolution{TP: tp, Err: err}, info, c
			}
			want, _, lc := resolve(true)
			got, info, vc := resolve(false)
			if got.Err != nil || want.Err != nil || got.Agent == nil || got.Agent.QualifiedName() != "worker" {
				t.Fatalf("resolver err %v agent %v, legacy err %v; want both resolved, agent worker", got.Err, got.Agent, want.Err)
			}
			if after := vc.info(t, info.ID); after.TriggerBeadID != info.TriggerBeadID {
				t.Fatalf("the resolver wrote the row: trigger %q -> %q", info.TriggerBeadID, after.TriggerBeadID)
			}
			if cleared := lc.info(t, info.ID).TriggerBeadID == ""; cleared != parked {
				t.Fatalf("legacy cleared the trigger = %v, want %v", cleared, parked)
			}
			if parked {
				if got.TP.Env["GC_TRIGGER_BEAD_ID"] != info.TriggerBeadID || want.TP.Env["GC_TRIGGER_BEAD_ID"] != "" {
					t.Fatalf("trigger env: resolver %q, legacy %q; want the row's stamp and none", got.TP.Env["GC_TRIGGER_BEAD_ID"], want.TP.Env["GC_TRIGGER_BEAD_ID"])
				}
				for _, k := range []string{"GC_SPAWN_ORIGIN", "GC_TRIGGER_BEAD_ID", "GC_TRIGGER_WORK_BEAD_ID"} {
					delete(got.TP.Env, k)
				}
			}
			if !reflect.DeepEqual(got.TP, want.TP) {
				t.Fatalf("resolver = %+v, want legacy's %+v", got.TP, want.TP)
			}
		})
	}
}

// Kills a legacy change in the extraction: the preserved-named resolution
// still rebinds the trigger, returns the bound Info and installs side
// effects exactly as the pre-extraction function did (a failing hook logs
// once per install), and the overlay's row
// resolution returns what its pre-extraction body did, on every seeded
// overlay fixture.
func TestTemplateResolutionExtractionLeavesLegacyUnchanged(t *testing.T) {
	shared := t.TempDir()
	for _, parked := range []bool{false, true} {
		var outs [2]any
		for i, fn := range []func(string, string, *config.City, runtime.Provider, beads.Store, []session.Info, session.Info, clock.Clock, io.Writer) (TemplateParams, session.Info, error){
			resolvePreservedConfiguredNamedSessionTemplate, resolvePreservedNamedPreExtraction,
		} {
			c := newTemplateCity(t)
			c.cityPath = shared
			c.cfg.Agents[0].InstallAgentHooks = []string{"unsupported"}
			work, err := c.store.Create(beads.Bead{Title: "work", Status: "open", IsBlocked: &parked})
			if err != nil {
				t.Fatal(err)
			}
			info := c.named(t, map[string]string{beadmeta.TriggerBeadIDMetadataKey: work.ID})
			var stderr strings.Builder
			tp, bound, err := fn(c.cityPath, "test-city", c.cfg, runtime.NewFake(), c.store, []session.Info{info}, info, &clock.Fake{Time: templatesNow}, &stderr)
			outs[i] = []any{tp, bound.TriggerBeadID, c.info(t, info.ID).TriggerBeadID, err, stderr.String()}
		}
		if !reflect.DeepEqual(outs[0], outs[1]) {
			t.Fatalf("parked=%v: preserved-named resolution %+v, pre-extraction %+v", parked, outs[0], outs[1])
		}
	}
	cityPath := t.TempDir()
	for seed := uint64(0); seed < poolPiecesSeeds; seed++ {
		f := randOverlayFixture(rand.New(rand.NewPCG(seed, 5)), cityPath)
		bp := newAgentBuildParams("city", cityPath, f.cfg, runtime.NewFake(), time.Time{}, nil, io.Discard)
		bp.sessionBeads = newSessionBeadSnapshot(f.rows)
		for _, info := range bp.sessionBeads.OpenInfos() {
			agent := findAgentByTemplate(f.cfg, resolvedSessionTemplateInfo(info, f.cfg))
			if agent == nil {
				continue
			}
			got, gotErr := resolveDiscoveredSessionTemplate(bp, f.cfg, agent, info)
			want, wantErr := resolveDiscoveredPreExtraction(bp, f.cfg, agent, info)
			if !reflect.DeepEqual(got, want) || fmt.Sprint(gotErr) != fmt.Sprint(wantErr) {
				t.Fatalf("seed %d row %s: resolution %+v (%v), pre-extraction %+v (%v)", seed, info.ID, got, gotErr, want, wantErr)
			}
		}
	}
}

// Kills a memo key that misses a field the resolution reads, which would
// hand one row another's params within a generation: changing any row field
// outside the key leaves every kind's resolution unchanged, and changing any
// field the key lists, or the agent label a row without agent_name is
// named by, changes the key. Closed is not perturbed: the census holds open
// rows only.
func TestTemplateMemoKeyCoversResolverReads(t *testing.T) {
	c := newTemplateCity(t)
	r := newTemplateResolver(c.params())
	keyed := map[string]bool{
		"ID": true, "Template": true, "CommonName": true, "SessionNameMetadata": true, "SessionNameExplicit": true,
		"AgentName": true, "Alias": true, "SessionOrigin": true, "ManualSessionMetadata": true, "PoolSlot": true,
		"ConfiguredNamedIdentity": true, "ConfiguredNamedSession": true, "PoolManaged": true, "DependencyOnly": true,
		"TriggerBeadID": true, "TriggerBeadStoreRef": true, "Pack": true, "Closed": true,
	}
	for _, info := range []session.Info{c.named(t, nil), c.manual(t), c.pool(t)} {
		want := r.Resolve(info)
		v := reflect.ValueOf(info)
		for i := range v.NumField() {
			name := v.Type().Field(i).Name
			changed := info
			f := reflect.ValueOf(&changed).Elem().Field(i)
			switch f.Kind() {
			case reflect.String:
				f.SetString(f.String() + "-perturbed")
			case reflect.Bool:
				f.SetBool(!f.Bool())
			case reflect.Int, reflect.Int64:
				f.SetInt(f.Int() + 7)
			default:
				continue
			}
			if inKey := templateMemoKeyOf(changed) != templateMemoKeyOf(info); inKey != (keyed[name] && name != "Closed") {
				t.Fatalf("%s: in the key = %v, listed as keyed = %v", name, inKey, keyed[name])
			}
			if keyed[name] {
				continue
			}
			if got := r.Resolve(changed); !reflect.DeepEqual(got, want) {
				t.Errorf("row %s: changing %s (outside the memo key) changed its resolution", info.SessionNameMetadata, name)
			}
		}
		labeled := info
		labeled.Labels = append(slices.Clone(info.Labels), "unrelated:label")
		if got := r.Resolve(labeled); !reflect.DeepEqual(got, want) {
			t.Errorf("row %s: an unrelated label changed its resolution", info.SessionNameMetadata)
		}
	}
	unnamed := session.Info{ID: "gc-9", Template: "polecat", Labels: []string{"agent:polecat-2"}}
	relabelled := unnamed
	relabelled.Labels = []string{"agent:polecat-3"}
	if templateMemoKeyOf(unnamed) == templateMemoKeyOf(relabelled) {
		t.Error("a row's agent label, its name without agent_name, is not in the key")
	}
}

// Kills params built per pass or per row: gather builds the resolver once
// per env generation, however many rows and passes.
func TestGatherBuildsTemplateResolverOncePerGeneration(t *testing.T) {
	f := newGatherFixture(t,
		poolRow("gc-1", "worker", 1, "active"), poolRow("gc-2", "worker", 2, "active"), poolRow("gc-3", "worker", 3, "asleep"))
	var builds []uint64
	f.env.Templates = func(env *reconcileEnv, now time.Time) templateResolver {
		gen := env.Gen
		builds = append(builds, gen)
		if !now.Equal(gatherNow) {
			t.Errorf("resolver built at %v, want the pass's %v", now, gatherNow)
		}
		return templateResolver{Resolve: func(info session.Info) templateResolution {
			return templateResolution{TP: TemplateParams{SessionName: info.SessionNameMetadata}}
		}}
	}
	var w World
	for range 3 {
		w = f.gather(t)
	}
	f.cur.Store(&reconcileEnv{Gen: 2, Cfg: f.cur.Load().Cfg, SP: f.sp})
	w = f.gather(t)
	if !slices.Equal(builds, []uint64{1, 2}) {
		t.Fatalf("resolver builds by generation %v, want [1 2]", builds)
	}
	if _, ok := w.Templates.lookup(w.Census.Rows[rowKey{Leg: "city:test-city", ID: "gc-2"}].Info); !ok {
		t.Fatal("the memo has no entry for a pool row")
	}
}

// resolvePreservedNamedPreExtraction is resolvePreservedConfiguredNamedSessionTemplate
// as it was before C5a1-wire, verbatim.
func resolvePreservedNamedPreExtraction(cityPath, cityName string, cfg *config.City, sp runtime.Provider, store beads.Store, openInfos []session.Info, info session.Info, clk clock.Clock, stderr io.Writer) (TemplateParams, session.Info, error) {
	if cityPath == "" {
		cityPath = "."
	}
	if cityName == "" && cfg != nil {
		cityName = cfg.EffectiveCityName()
	}
	identity := namedSessionIdentityInfo(info)
	spec, ok := findNamedSessionSpec(cfg, cityName, identity)
	if !ok || spec.Agent == nil {
		return TemplateParams{}, info, fmt.Errorf("configured named session %q not found", identity)
	}
	bp := newAgentBuildParams(cityName, cityPath, cfg, sp, clk.Now().UTC(), store, stderr)
	bp.sessionBeads = newSessionBeadSnapshotFromInfos(openInfos)
	if bound, bindErr := bindNamedSessionTriggerBead(store, info, cityName); bindErr != nil {
		if stderr != nil {
			fmt.Fprintf(stderr, "session reconciler: named session %s trigger bead %s: %v (continuing with existing stamp)\n", identity, info.TriggerBeadID, bindErr) //nolint:errcheck
		}
	} else {
		info = bound
	}
	fpExtra := buildFingerprintExtra(spec.Agent)
	tp, err := resolveTemplateForSessionBeadInfo(bp, spec.Agent, identity, fpExtra, info)
	if err != nil {
		return TemplateParams{}, info, err
	}
	tp.Alias = identity
	tp.TemplateName = namedSessionBackingTemplate(spec)
	tp.InstanceName = identity
	tp.ConfiguredNamedIdentity = identity
	tp.ConfiguredNamedMode = spec.Mode
	if tp.Env == nil {
		tp.Env = make(map[string]string)
	}
	tp.Env["GC_TEMPLATE"] = namedSessionBackingTemplate(spec)
	tp.Env["GC_ALIAS"] = identity
	tp.Env["GC_AGENT"] = identity
	tp.Env["GC_SESSION_ORIGIN"] = "named"
	installAgentSideEffects(bp, spec.Agent, tp, stderr)
	return tp, info, nil
}

// resolveDiscoveredPreExtraction is the overlay loop's row resolution as it
// was before C5a1-wire, verbatim but for the loop's continue.
func resolveDiscoveredPreExtraction(bp *agentBuildParams, cfg *config.City, cfgAgent *config.Agent, info session.Info) (TemplateParams, error) {
	bInfo, sn := info, info.SessionNameMetadata
	var (
		resolveAgent         *config.Agent
		sessionQualifiedName string
	)
	if isManualSessionInfoForAgent(info, cfgAgent) {
		sessionQualifiedName = sessionBeadQualifiedNameInfo(bp.cityPath, cfgAgent, bp.rigs, bInfo)
		resolveAgent = sessionBeadConfigAgent(cfgAgent, sessionQualifiedName)
	} else {
		resolveAgent, sessionQualifiedName = canonicalSessionIdentityWithConfigInfo(cfg, cfgAgent, bInfo)
	}
	fpExtra := buildFingerprintExtra(resolveAgent)
	tp, err := resolveTemplateForSessionBeadInfo(bp, resolveAgent, sessionQualifiedName, fpExtra, bInfo)
	if err != nil {
		return TemplateParams{}, err
	}
	tp.ManualSession = isManualSessionInfoForAgent(info, cfgAgent)
	if tp.ManualSession {
		if manualAlias := strings.TrimSpace(info.Alias); manualAlias != "" {
			tp.Alias = manualAlias
		}
	}
	if isEphemeralSessionInfoForAgent(info, cfgAgent) {
		if !tp.ManualSession || strings.TrimSpace(info.Alias) == "" {
			tp.Alias = ""
		}
		if tp.ManualSession && sessionQualifiedName != "" {
			tp.InstanceName = sessionQualifiedName
		} else {
			tp.InstanceName = sn
		}
	}
	if isNamedSessionInfo(info) {
		tp.ConfiguredNamedIdentity = info.ConfiguredNamedIdentity
		tp.ConfiguredNamedMode = info.ConfiguredNamedMode
		if tp.Env == nil {
			tp.Env = make(map[string]string)
		}
		tp.Env["GC_SESSION_ORIGIN"] = "named"
	}
	return tp, nil
}
