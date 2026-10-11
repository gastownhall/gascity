package main

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// The v2 pass's gather (architecture §1.2, §1.3 step 3): one immutable World
// built from cached inputs on the planner goroutine. It reads and never
// writes a store, and performs no remote I/O (CONTRACT v5 R4): the census
// reads each leg's CachingStore, demand reads the cache or the external-reads
// recording, and observations come from the inventory lane's snapshot. The
// two inputs that may ask the provider, the sleep policies and the transport
// refusals, and the template resolutions are memoized per env generation, so
// none runs per row per pass.
//
// Unwired in this slice: C2c1 runs gather in the pass, and C2c2 builds the
// gatherEnv from the controller.

var (
	errGatherNoEnv = errors.New("gather: no published env")
	// errGatherCacheUnprimed: no pass runs before the sessions cache is
	// primed (P2). An unprimed cache would serve the census from its backing.
	errGatherCacheUnprimed = errors.New("gather: sessions cache not primed")
)

// gatherEnv is what gather reads. It holds closures only, so gather never
// names the controller. Env, Sessions, RigStores and Recording are required;
// the others may be nil.
type gatherEnv struct {
	CityPath, CityName string
	Env                func() *reconcileEnv
	// Sessions is the sessions-class store: the census's leading leg and the
	// demand gather's city store.
	Sessions   func() beads.Store
	RigStores  func() map[string]beads.Store
	Recording  func() *externalReadsRecording // K1's latest; nil before the first
	ReadyWaits func() map[string]bool         // K1's waits step's latest (I10)
	// Nudges and WorkStore are the nudges-class and city work stores C8's
	// steps write through. WorkStore is also the work leg of the pass's
	// WorkLegs, which the live work read (L5) plans over.
	Nudges       func() beads.NudgesStore
	WorkStore    func() beads.Store
	Observations func() *ObservationCache
	Capacity     func() *endpointCapacityGuard
	Health       func() *providerHealthSnapshot
	Episodes     func() (map[string]session.StartupHealthEpisode, error)
	Suspension   func() suspensionstate.State
	// Templates builds env's template resolver, at now. Gather calls it once
	// per env generation, when the memo is rebuilt, and memoizes what it
	// resolves (templateMemo). Nil resolves nothing.
	Templates func(env *reconcileEnv, now time.Time) templateResolver
	// LookPath is the transport check's provider binary lookup; nil checks
	// nothing, as validateAgentSessionTransport does.
	LookPath config.LookPathFunc
	Stderr   io.Writer
}

// World is one pass's inputs (architecture §1.2), immutable once gathered:
// every map is the pass's own or never mutated after publish.
type World struct {
	Now      time.Time
	CityPath string // the runtime name locks' city (lockRuntimeName)
	Env      *reconcileEnv
	Census   *sessionCensus
	// Mislabelled are the canonical rows with no template and no session
	// name: beads labeled gc:session that are not sessions. Arm A1 makes
	// each None with one trace (CONTRACT v5 AL1).
	Mislabelled map[rowKey]bool
	Demand      demandView
	ScaleCheck  *scaleCheckResult
	Obs         *ObservationSnapshot
	ObsMaxAge   time.Duration
	Observed    map[rowKey]rowObservation // observeCensus's per-row facts
	Gates       map[endpointKey]endpointView
	Backoff     map[string]backoffRecord
	InFlight    inflightView
	Bucket      bucketState
	Boot        bootState
	Paused      bool            // starts paused for a provider swap (P7)
	SoftReload  *softReloadView // the open soft reload this pass read, for A8 (M2)
	AliasOwed   map[rowKey]bool // rows whose runtime is owed their written alias, for A7 (M1)

	CitySuspended     bool
	SuspendedRigPaths map[string]bool
	ProviderHealth    *providerHealthSnapshot
	Episodes          map[string]session.StartupHealthEpisode
	SleepPolicies     map[string]resolvedSessionSleepPolicy // by bead ID
	TransportRefused  map[string]string                     // by qualified name
	ReadyWaits        map[string]bool                       // I10, by session bead ID
	Templates         *templateMemo
	// ExecutionStalled are the execution backstop's drain requests by row
	// ID, for arm A16 (C7b1).
	ExecutionStalled map[string]executionStalledRequest
	// LegStores holds the sessions leg's store by its ref, which effects
	// reach only as a fenced writer (newEffectPass). No other leg's store is
	// here: a census-only row has no writer (AL1, armCensusOnly).
	LegStores map[string]beads.Store
	// InputAges are the inputs' ages at Now, for the pass record.
	InputAges map[string]time.Duration
	// SessionsStore and RigStores are the stores the census was planned
	// over, which only creates reach (newCreatePass). The sessions leg's
	// ref is Census.sessionsLeg().
	SessionsStore beads.Store
	RigStores     map[string]beads.Store
	// OperatorSuspend is the explicit operator suspend cause (city, rig,
	// agent or session) of each canonical row inside INC-003's grace, the
	// only rows that read it: such a row gets no grace (v5.4 D2). Gather
	// reads it, since operatorSuspendCause reads the environment.
	OperatorSuspend map[rowKey]string
	// WorkLegs are the city work legs the effects' live work read (L5) plans
	// over: minted from the city work store, never the sessions store
	// (NEW2-1). Effects reach them only as effectReads.
	WorkLegs WorkLegs
	// Phantoms are the rows that squat a configured name without being its
	// canonical owner (SESS-080), each with the assignees its close keeps (the
	// identity and its runtime name), which A21 reads. Resolving them writes
	// diagnostics, which the pure decide may not (censusPhantoms).
	Phantoms map[rowKey][]string
	// fresh is one row's fresh runtime read, which the fresh accessors read
	// in its pass's stead (withRuntime): set only inside an effect.
	fresh *freshRow
}

// gather builds the pass's World at now. It first drains the settlements
// posted since the pass began. It fails when no env is published, the
// sessions cache is not primed, the sessions leg's census read fails hard
// (an error is not an empty city), or the demand gather fails.
func gather(e gatherEnv, p *planner, now time.Time) (World, error) {
	p.drainSettlements(now)
	env := e.Env()
	if env == nil || env.Cfg == nil {
		return World{}, errGatherNoEnv
	}
	cfg, stderr := env.Cfg, e.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	w := World{Now: now, CityPath: e.CityPath, Env: env, InFlight: p.inflight.view(), Backoff: p.backoff.Snapshot(), Bucket: p.bucket, Paused: p.startsPaused(), SoftReload: p.softReload.Load().view()}
	var st suspensionstate.State
	if e.Suspension != nil {
		st = e.Suspension()
	}
	w.CitySuspended, w.SuspendedRigPaths = effectiveCitySuspended(cfg, st), suspendedRigPathsWithState(cfg, st)
	rec := e.Recording()
	if e.Observations != nil {
		w.Obs = e.Observations().Snapshot()
	}
	store, rigs := e.Sessions(), e.RigStores()
	cache, ok := demandLabelKey(store).(interface{ IsLive() bool }) // a *beads.CachingStore
	w.Boot = bootState{CachePrimed: ok && cache.IsLive(), InventoryComplete: w.Obs.allPrimedOrConfirmedDead()}
	if !w.Boot.CachePrimed {
		p.boot = w.Boot
		return w, errGatherCacheUnprimed
	}

	legs, err := sessionCensusStoreCandidates(e.CityPath, cfg, store, rigs, w.SuspendedRigPaths)
	if err != nil {
		return World{}, fmt.Errorf("gather: census legs: %w", err)
	}
	if w.Census, err = readSessionCensus(now, legs); err != nil {
		return World{}, fmt.Errorf("gather: %w", err)
	}
	w.ExecutionStalled = p.executionStalled(w.Census)
	w.AliasOwed = p.aliasOwedFor(w.Census)
	w.Phantoms = censusPhantoms(w.Census, cfg, e.CityName)
	if e.ReadyWaits != nil {
		w.ReadyWaits = e.ReadyWaits()
	}
	w.SessionsStore, w.RigStores = store, rigs
	if e.WorkStore != nil { // without one, the zero WorkLegs answers every work read unknown
		w.WorkLegs = workLegsFromCensus(e.CityPath, cfg, cityWorkLegOf(beads.WorkStore{Store: e.WorkStore()}), rigs)
	}
	w.LegStores = map[string]beads.Store{legs[0].ref: legs[0].store}
	all := make(map[string]beads.Store, len(legs))
	for _, l := range legs {
		all[l.ref] = l.store
	}
	w.InputAges = inputAges(now, all, w.Obs, rec)
	rows := w.Census.Canonical()
	w.OperatorSuspend = operatorSuspendCauses(cfg, e.CityPath, rows, st, now)
	for _, row := range rows {
		if strings.TrimSpace(row.Info.Template) == "" && strings.TrimSpace(row.Info.SessionNameMetadata) == "" {
			if w.Mislabelled == nil {
				w.Mislabelled = make(map[rowKey]bool)
			}
			w.Mislabelled[row.Key] = true
		}
	}

	dg := censusDemandEnv(w.Census, store, rigs)
	dg.CityName, dg.CityPath, dg.Cfg, dg.SuspendedRigPaths, dg.Stderr = e.CityName, e.CityPath, cfg, w.SuspendedRigPaths, stderr
	if w.Demand, err = gatherDemand(dg, newV2DemandReads(now, rec)); err != nil {
		return World{}, err
	}
	w.ScaleCheck = rec.scaleCheck(now)

	w.ObsMaxAge = 2 * env.patrol()
	w.Observed = observeCensus(w.Obs, w.Census, now, w.ObsMaxAge)
	var guard *endpointCapacityGuard
	if e.Capacity != nil {
		guard = e.Capacity()
	}
	w.Gates = gatherGates(guard, cfg, rows)
	if e.Health != nil {
		w.ProviderHealth = e.Health()
	}
	if e.Episodes != nil {
		// A failed read fails open, as legacy does on a load error (SESS-607).
		w.Episodes, _ = e.Episodes()
	}
	p.memo.refresh(e, env, now, rows, w.Mislabelled)
	w.SleepPolicies, w.TransportRefused, w.Templates = p.memo.sleepPolicies(cfg, env, rows), p.memo.transport, p.memo.templates.Load()

	laneFed := len(externalReadLegs(k1Env(e, env, w.SuspendedRigPaths, dg), io.Discard)) > 0
	w.Boot.RecordingSeen = rec != nil || !laneFed
	p.boot = w.Boot
	return w, nil
}

// operatorSuspendCauses is operatorSuspendCause for each of rows woken
// within INC-003's grace of now, keeping the causes only. The rig resolver
// is costly on path-style dirs, so no other row pays for it.
func operatorSuspendCauses(cfg *config.City, cityPath string, rows []censusRow, st suspensionstate.State, now time.Time) map[rowKey]string {
	causes := make(map[rowKey]string)
	for _, row := range rows {
		if !wakeGracePreservesUndesiredRow(row.Info, now) {
			continue
		}
		if cause := operatorSuspendCause(cfg, cityPath, row.Info, st); cause != "" {
			causes[row.Key] = cause
		}
	}
	return causes
}

// gatherGates reads each endpoint's breaker the pass may act on: every
// census row's (rowEndpoint) and every configured agent's, with the
// census rows' refusals this episode.
func gatherGates(g *endpointCapacityGuard, cfg *config.City, rows []censusRow) map[endpointKey]endpointView {
	gates := make(map[endpointKey]endpointView)
	view := func(k endpointKey) endpointView {
		v, ok := gates[k]
		if !ok {
			v = endpointView{Gate: endpointGateOf(g, k), HoldsPendingCreate: g.HoldsPendingCreate(k)}
		}
		return v
	}
	for i := range cfg.Agents {
		if k := endpointKeyForAgent(cfg, &cfg.Agents[i], session.Info{}); k != "" {
			gates[k] = view(k)
		}
	}
	for _, row := range rows {
		k := rowEndpoint(cfg, row.Info)
		if k == "" {
			continue
		}
		v := view(k)
		if n := g.refusalsInEpisode(k, row.Key.ID); n > 0 {
			if v.Refusals == nil {
				v.Refusals = make(map[string]int)
			}
			v.Refusals[row.Key.ID] = n
		}
		gates[k] = v
	}
	return gates
}

// k1Env is the external-reads lane's env for the planner's environment:
// dg's stores and census, with the default scale_check target stores the
// demand gather probes.
func k1Env(e gatherEnv, env *reconcileEnv, suspendedRigPaths map[string]bool, dg demandGatherEnv) externalReadsEnv {
	targets := buildDemandTargets(e.CityName, e.CityPath, env.Cfg, dg.CityStore, dg.RigStores, suspendedRigPaths, dg.OpenSessions, noProbeEnv, io.Discard)
	var probes []beads.Store
	for _, t := range append(targets.defaultScaleTargets, targets.defaultNamedScaleTargets...) {
		if t.store != nil {
			probes = append(probes, t.store)
		}
	}
	return externalReadsEnv{
		CityPath: e.CityPath, CityName: e.CityName, Cfg: env.Cfg, CityStore: dg.CityStore, RigStores: dg.RigStores,
		SuspendedRigPaths: suspendedRigPaths, ProbeStores: probes, Sessions: dg.Sessions,
	}
}

// externalReadsEnv builds K1's env from e for the lane's goroutine, over the
// cached census as gather reads it. C2c2 starts the lane with it.
func (e gatherEnv) externalReadsEnv() (externalReadsEnv, error) {
	env := e.Env()
	if env == nil || env.Cfg == nil {
		return externalReadsEnv{}, errGatherNoEnv
	}
	var st suspensionstate.State
	if e.Suspension != nil {
		st = e.Suspension()
	}
	suspended, store, rigs := suspendedRigPathsWithState(env.Cfg, st), e.Sessions(), e.RigStores()
	legs, err := sessionCensusStoreCandidates(e.CityPath, env.Cfg, store, rigs, suspended)
	if err != nil {
		return externalReadsEnv{}, err
	}
	census, err := readSessionCensus(time.Now(), legs)
	if err != nil {
		return externalReadsEnv{}, err
	}
	k1 := k1Env(e, env, suspended, censusDemandEnv(census, store, rigs))
	k1.SP = env.SP
	if e.Nudges != nil {
		k1.Nudges = e.Nudges()
	}
	if e.WorkStore != nil {
		k1.WorkStore = e.WorkStore()
		k1.WorkLegs = workLegsFromCensus(e.CityPath, env.Cfg, cityWorkLegOf(beads.WorkStore{Store: k1.WorkStore}), rigs)
	}
	return k1, nil
}

// censusDemandEnv is the demand gather's stores and its two session inputs
// from c, as legacy reads them: OpenSessions every canonical row
// (collectAllOpenSessionInfos), Sessions the sessions leg's rows.
func censusDemandEnv(c *sessionCensus, store beads.Store, rigs map[string]beads.Store) demandGatherEnv {
	var open, sessionsLeg []session.Info
	for _, row := range c.Canonical() {
		open = append(open, row.Info)
		if row.Key.Leg == c.Legs[0].Ref {
			sessionsLeg = append(sessionsLeg, row.Info)
		}
	}
	return demandGatherEnv{CityStore: store, RigStores: rigs, OpenSessions: open, Sessions: newSessionBeadSnapshotFromInfos(sessionsLeg)}
}

// templateMemoKey is what one row's template resolution reads off the row:
// the row's ID (the session bead ID and pool-name ownership), the template
// and identity fields its kind's path reads (rowTemplateResolver), the
// trigger fields resolveTemplateForSessionBeadInfo stamps, and whether a
// pool seat's unassigned alias defers (poolRuntimeAliasIsDeferredInfo, which
// the realize path reads off the state, the claim and an alias conflict).
type templateMemoKey struct {
	ID, Template, CommonName, SessionName, SessionNameExplicit, AgentName, Alias string
	Origin, ManualMetadata, PoolSlot, NamedIdentity                              string
	Named, PoolManaged, DependencyOnly                                           bool
	TriggerBeadID, TriggerStoreRef, Pack                                         string
	AliasDeferred                                                                bool
}

func templateMemoKeyOf(info session.Info) templateMemoKey {
	return templateMemoKey{
		ID: info.ID, Template: info.Template, CommonName: info.CommonName, SessionName: info.SessionNameMetadata,
		SessionNameExplicit: info.SessionNameExplicit, AgentName: session.AgentNameInfo(info), Alias: info.Alias,
		Origin: info.SessionOrigin, ManualMetadata: info.ManualSessionMetadata, PoolSlot: info.PoolSlot,
		NamedIdentity: info.ConfiguredNamedIdentity, Named: info.ConfiguredNamedSession, PoolManaged: info.PoolManaged,
		DependencyOnly: info.DependencyOnly,
		TriggerBeadID:  info.TriggerBeadID, TriggerStoreRef: info.TriggerBeadStoreRef, Pack: info.Pack,
		AliasDeferred: poolRuntimeAliasIsDeferredInfo(info),
	}
}

// templateResolution is one memoized resolution, its error included. Agent
// is the configured agent whose side effects a launch installs.
type templateResolution struct {
	TP    TemplateParams
	Agent *config.Agent
	Err   error
}

// templateResolver is one env generation's template resolution: Resolve
// resolves one row (on the planner goroutine), and Install installs a
// resolved agent's side effects before its launch (on an effect's).
type templateResolver struct {
	Resolve func(info session.Info) templateResolution
	// ResolveAt resolves one row with its beacon at now: the launch's
	// resolution of the row it starts, as legacy binds, then resolves, at
	// the tick (on an effect's goroutine).
	ResolveAt func(info session.Info, now time.Time) templateResolution
	Install   func(agent *config.Agent, tp TemplateParams)
	// Retry asks for a new resolver on the next pass: this one's params
	// failed to load the city's or a rig's skill catalog, which legacy loads
	// again each tick.
	Retry bool
}

// newTemplateResolver resolves rows under bp, one env generation's build
// params, as legacy resolves each kind for a start: a configured named row
// through resolveConfiguredNamedSessionTemplate (legacy's preserved-named
// resolution without its trigger rebind, a blind write that A6/POOL-056
// owns), and a manual or pool row through the overlay's
// resolveDiscoveredSessionTemplate. Resolving writes no store; it does
// legacy's local work-dir staging. Each row resolves against a snapshot of
// itself alone, so its session bead ID is its own without a store read.
// Resolutions serialize on templateResolveMu, the planner's and the
// launches' alike, as legacy resolves on one goroutine.
func newTemplateResolver(bp *agentBuildParams) templateResolver {
	resolveAt := func(info session.Info, now time.Time) templateResolution {
		templateResolveMu.Lock()
		defer templateResolveMu.Unlock()
		row := *bp
		row.sessionBeads = newSessionBeadSnapshotFromInfos([]session.Info{info})
		if !now.IsZero() {
			row.beaconTime = now
		}
		cfg := bp.city
		if isNamedSessionInfo(info) {
			identity := namedSessionIdentityInfo(info)
			spec, ok := findNamedSessionSpec(cfg, bp.cityName, identity)
			if !ok || spec.Agent == nil {
				return templateResolution{Err: fmt.Errorf("configured named session %q not found", identity)}
			}
			tp, err := resolveConfiguredNamedSessionTemplate(&row, spec, identity, info)
			return templateResolution{TP: tp, Agent: spec.Agent, Err: err}
		}
		agent := findAgentByTemplate(cfg, resolvedSessionTemplateInfo(info, cfg))
		if agent == nil {
			return templateResolution{Err: fmt.Errorf("template %q not configured", info.Template)}
		}
		if slot, _ := strconv.Atoi(strings.TrimSpace(info.PoolSlot)); (slot > 0 || agent.UsesCanonicalSingletonPoolIdentity()) && info.PoolManaged && !isManualSessionInfoForAgent(info, agent) {
			// A wanted pool seat: legacy's realize path (realizePoolDesiredSessions).
			resolveAgent, qualifiedInstance, poolSlot := poolDesiredRequestIdentity(agent, slot)
			tp, err := resolveTemplateForSessionBeadInfo(&row, resolveAgent, qualifiedInstance, buildFingerprintExtra(resolveAgent), info)
			if err == nil {
				applyPoolInstanceIdentity(&tp, agent, qualifiedInstance, poolSlot, info)
			}
			return templateResolution{TP: tp, Agent: resolveAgent, Err: err}
		}
		tp, err := resolveDiscoveredSessionTemplate(&row, cfg, agent, info)
		return templateResolution{TP: tp, Agent: agent, Err: err}
	}
	install := func(agent *config.Agent, tp TemplateParams) { installAgentSideEffects(bp, agent, tp, bp.stderr) }
	resolve := func(info session.Info) templateResolution { return resolveAt(info, time.Time{}) }
	return templateResolver{Resolve: resolve, ResolveAt: resolveAt, Install: install, Retry: bp.skillCatalogFailed || len(bp.failedRigSkillCatalogs) > 0}
}

// templateMemo is one generation's template resolutions (S-17). It is
// immutable once published: a pass that needs a new entry publishes a fresh
// memo, so an effect may read the one it was handed from any goroutine.
// Readers must not edit an entry's TemplateParams.
type templateMemo struct {
	Gen       uint64
	entries   map[templateMemoKey]templateResolution
	install   func(agent *config.Agent, tp TemplateParams)
	resolveAt func(info session.Info, now time.Time) templateResolution
}

// launchTemplate is info's template resolved at now, for its launch, and
// false when the memo has no resolver or the resolution failed.
func (m *templateMemo) launchTemplate(info session.Info, now time.Time) (templateResolution, bool) {
	if m == nil || m.resolveAt == nil {
		return templateResolution{}, false
	}
	res := m.resolveAt(info, now)
	return res, res.Err == nil
}

// installSideEffects installs agent's side effects for a launch of tp
// (installAgentSideEffects, which legacy runs every tick); a memo with no
// resolver installs none.
func (m *templateMemo) installSideEffects(agent *config.Agent, tp TemplateParams) {
	if m != nil && m.install != nil {
		m.install(agent, tp)
	}
}

// lookup returns info's resolution, and false when the memo has none.
func (m *templateMemo) lookup(info session.Info) (templateResolution, bool) {
	if m == nil {
		return templateResolution{}, false
	}
	r, ok := m.entries[templateMemoKeyOf(info)]
	return r, ok
}

// sleepMemoKey is what resolveSessionSleepPolicyInfo reads off a row: its
// normalized template and its raw session name.
type sleepMemoKey struct{ Template, SessionName string }

// gatherMemo holds the per-generation memos. Only the planner goroutine
// touches it; templates is published for the effects.
type gatherMemo struct {
	gen       uint64
	sleep     map[sleepMemoKey]resolvedSessionSleepPolicy
	transport map[string]string // immutable once built: World shares it
	resolver  templateResolver
	templates atomic.Pointer[templateMemo]
}

// refresh starts a new generation's memos when env is new, building its
// template resolver at now (again on a later pass when the resolver asks
// to: its skill catalog failed to load), then resolves the templates of
// rows the memo lacks or holds only an error for, and publishes a fresh
// memo holding the pass's rows' entries.
func (m *gatherMemo) refresh(e gatherEnv, env *reconcileEnv, now time.Time, rows []censusRow, mislabelled map[rowKey]bool) {
	newGen := m.sleep == nil || m.gen != env.Gen
	if newGen {
		m.gen, m.sleep = env.Gen, make(map[sleepMemoKey]resolvedSessionSleepPolicy)
		m.transport = make(map[string]string)
		for i := range env.Cfg.Agents {
			a := &env.Cfg.Agents[i]
			if err := validateAgentSessionTransport(&env.Cfg.Workspace, env.Cfg.Providers, e.LookPath, env.SP, a, a.QualifiedName()); err != nil {
				m.transport[a.QualifiedName()] = err.Error()
			}
		}
	}
	if newGen || m.resolver.Retry {
		m.resolver = templateResolver{}
		if e.Templates != nil {
			m.resolver = e.Templates(env, now)
		}
		m.templates.Store(&templateMemo{Gen: env.Gen, install: m.resolver.Install, resolveAt: m.resolver.ResolveAt})
	}
	if m.resolver.Resolve == nil {
		return
	}
	cur := m.templates.Load()
	next := make(map[templateMemoKey]templateResolution, len(rows))
	missed := false
	for _, row := range rows {
		if mislabelled[row.Key] || findAgentByTemplate(env.Cfg, resolvedSessionTemplateInfo(row.Info, env.Cfg)) == nil {
			continue
		}
		k := templateMemoKeyOf(row.Info)
		if _, done := next[k]; done {
			continue
		}
		r, ok := cur.entries[k]
		if !ok || r.Err != nil { // an error is not kept: legacy resolves again each tick
			missed = true
			r = m.resolver.Resolve(row.Info)
		}
		next[k] = r
	}
	if missed || len(next) != len(cur.entries) {
		m.templates.Store(&templateMemo{Gen: env.Gen, entries: next, install: cur.install, resolveAt: cur.resolveAt})
	}
}

// sleepPolicies returns each row's sleep policy by bead ID, resolving only
// the (template, session name) pairs this generation has not seen.
func (m *gatherMemo) sleepPolicies(cfg *config.City, env *reconcileEnv, rows []censusRow) map[string]resolvedSessionSleepPolicy {
	out := make(map[string]resolvedSessionSleepPolicy, len(rows))
	for _, row := range rows {
		k := sleepMemoKey{normalizedSessionTemplateInfo(row.Info, cfg), row.Info.SessionNameMetadata}
		policy, ok := m.sleep[k]
		if !ok {
			policy = resolveSessionSleepPolicyInfo(row.Info, cfg, env.SP)
			m.sleep[k] = policy
		}
		out[row.Key.ID] = policy
	}
	return out
}

// censusPhantoms is c's SESS-080 phantoms under cfg (World.Phantoms).
func censusPhantoms(c *sessionCensus, cfg *config.City, cityName string) map[rowKey][]string {
	out := map[rowKey][]string{}
	if cfg == nil || len(cfg.NamedSessions) == 0 {
		return out
	}
	// names are the configured names' runtime names, built once: only a row
	// under one can be a phantom, so the predicate runs on those alone.
	names := map[string]bool{}
	city := cmp.Or(strings.TrimSpace(cityName), config.EffectiveCityName(cfg, ""))
	for i := range cfg.NamedSessions {
		if identity := cfg.NamedSessions[i].QualifiedName(); identity != "" {
			names[config.NamedSessionRuntimeName(city, cfg.Workspace, identity)] = true
		}
	}
	for k, row := range c.Rows {
		if !names[strings.TrimSpace(row.Info.SessionNameMetadata)] {
			continue
		}
		identity, ok := recyclableDeadConfiguredNamePhantomInfo(row.Info, cfg, cityName)
		if !ok {
			continue
		}
		keep := []string{identity}
		if rn := config.NamedSessionRuntimeName(cityName, cfg.Workspace, identity); rn != "" && rn != identity {
			keep = append(keep, rn)
		}
		out[k] = keep
	}
	return out
}
