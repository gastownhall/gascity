package main

import (
	"errors"
	"fmt"
	"io"
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
	// steps write through.
	Nudges       func() beads.NudgesStore
	WorkStore    func() beads.Store
	Observations func() *ObservationCache
	Capacity     func() *endpointCapacityGuard
	Health       func() *providerHealthSnapshot
	Episodes     func() (map[string]session.StartupHealthEpisode, error)
	Suspension   func() suspensionstate.State
	// ResolveTemplate is resolveTemplateForSessionBeadInfo for one row under
	// env, memoized per generation (templateMemo).
	ResolveTemplate func(env *reconcileEnv, info session.Info) (TemplateParams, error)
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
	Paused      bool // starts paused for a provider swap (P7)

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
	// Classified is the planner's classified set (v5 S5): each row's runtime
	// token last classified, for arm A12.
	Classified map[rowKey]string
	// LegStores are the census legs' stores by ref, which effects reach
	// only as fenced writers and the close's release-capable stores
	// (newEffectPass).
	LegStores map[string]beads.Store
	// InputAges are the inputs' ages at Now, for the pass record.
	InputAges map[string]time.Duration
	// SessionsStore and RigStores are the stores the census was planned
	// over, which creates reach (newCreatePass), and the close reaches as
	// read-only and release-capable stores (newEffectPass); SessionsLeg is
	// the sessions leg's ref, which an ambiguous create's alert names.
	SessionsStore beads.Store
	RigStores     map[string]beads.Store
	SessionsLeg   string
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
	w := World{Now: now, CityPath: e.CityPath, Env: env, InFlight: p.inflight.view(), Backoff: p.backoff.Snapshot(), Bucket: p.bucket, Paused: p.startsPaused()}
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
	if e.ReadyWaits != nil {
		w.ReadyWaits = e.ReadyWaits()
	}
	w.SessionsStore, w.RigStores, w.SessionsLeg = store, rigs, legs[0].ref
	w.LegStores = make(map[string]beads.Store, len(legs))
	for _, l := range legs {
		w.LegStores[l.ref] = l.store
	}
	w.InputAges = inputAges(now, w.LegStores, w.Obs, rec)
	rows := w.Census.Canonical()
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
	w.Classified = p.classifiedFor(w.Census, w.Obs, now, w.ObsMaxAge)
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
	p.memo.refresh(e, env, rows, w.Mislabelled)
	w.SleepPolicies, w.TransportRefused, w.Templates = p.memo.sleepPolicies(cfg, env, rows), p.memo.transport, p.memo.templates.Load()

	laneFed := len(externalReadLegs(k1Env(e, env, w.SuspendedRigPaths, dg), io.Discard)) > 0
	w.Boot.RecordingSeen = rec != nil || !laneFed
	p.boot = w.Boot
	return w, nil
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

// templateMemoKey is what one row's template resolution reads: its template
// and the identity fields resolveTemplateForSessionBeadInfo and
// canonicalSessionIdentityWithConfigInfo read off the row.
type templateMemoKey struct {
	Template, SessionName, AgentName, PoolSlot, NamedIdentity string
	Named                                                     bool
	TriggerBeadID, TriggerStoreRef, Pack                      string
}

func templateMemoKeyOf(info session.Info) templateMemoKey {
	return templateMemoKey{
		Template: info.Template, SessionName: info.SessionNameMetadata, AgentName: info.AgentName, PoolSlot: info.PoolSlot,
		NamedIdentity: info.ConfiguredNamedIdentity, Named: info.ConfiguredNamedSession,
		TriggerBeadID: info.TriggerBeadID, TriggerStoreRef: info.TriggerBeadStoreRef, Pack: info.Pack,
	}
}

// templateResolution is one memoized resolution, its error included.
type templateResolution struct {
	TP  TemplateParams
	Err error
}

// templateMemo is one generation's template resolutions (S-17). It is
// immutable once published: a pass that needs a new entry publishes a fresh
// memo, so an effect may read the one it was handed from any goroutine.
// Readers must not edit an entry's TemplateParams.
type templateMemo struct {
	Gen     uint64
	entries map[templateMemoKey]templateResolution
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
	templates atomic.Pointer[templateMemo]
}

// refresh starts a new generation's memos when env is new, then resolves the
// templates of rows the memo lacks and publishes a fresh memo holding the
// pass's rows' entries.
func (m *gatherMemo) refresh(e gatherEnv, env *reconcileEnv, rows []censusRow, mislabelled map[rowKey]bool) {
	if m.sleep == nil || m.gen != env.Gen {
		m.gen, m.sleep = env.Gen, make(map[sleepMemoKey]resolvedSessionSleepPolicy)
		m.transport = make(map[string]string)
		for i := range env.Cfg.Agents {
			a := &env.Cfg.Agents[i]
			if err := validateAgentSessionTransport(&env.Cfg.Workspace, env.Cfg.Providers, e.LookPath, env.SP, a, a.QualifiedName()); err != nil {
				m.transport[a.QualifiedName()] = err.Error()
			}
		}
		m.templates.Store(&templateMemo{Gen: env.Gen})
	}
	if e.ResolveTemplate == nil {
		return
	}
	cur := m.templates.Load()
	next := make(map[templateMemoKey]templateResolution, len(rows))
	missed := false
	for _, row := range rows {
		if mislabelled[row.Key] || findAgentByTemplate(env.Cfg, normalizedSessionTemplateInfo(row.Info, env.Cfg)) == nil {
			continue
		}
		k := templateMemoKeyOf(row.Info)
		if _, done := next[k]; done {
			continue
		}
		r, ok := cur.entries[k]
		if !ok {
			missed = true
			r.TP, r.Err = e.ResolveTemplate(env, row.Info)
		}
		next[k] = r
	}
	if missed || len(next) != len(cur.entries) {
		m.templates.Store(&templateMemo{Gen: env.Gen, entries: next})
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
