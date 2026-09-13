package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/suspensionstate"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
)

// statusResponse is the JSON body for GET /v0/status.
// TODO(huma): replace with StatusBody once migration is complete.
type statusResponse = StatusBody

type (
	agentCounts = StatusAgentCounts
	rigCounts   = StatusRigCounts
	workCounts  = StatusWorkCounts
	mailCounts  = StatusMailCounts
)

var statusStoreReadTimeout = time.Second

// statusResponseTTLFloor lets non-blocking status requests reuse a recently
// built body after the time-bucket entry has rolled over. The shared
// time-bucket cache (responseCacheTimeBucket / timeBucketResponseCacheTTL in
// response_cache.go) bounds the rebuild rate within a bucket; the floor
// smooths the bucket-boundary miss so interactive callers with short budgets
// never pay a full fan-out rebuild more than once per floor window (#1896).
// Blocking (long-poll) requests bypass it because they explicitly wait for
// change. Var, not const, so tests can pin index-driven invalidation behavior.
var statusResponseTTLFloor = 3 * time.Second

// statusWorkExcludedTypes are bead types counted as infrastructure, not
// work, by the status endpoint's work-count buckets.
var statusWorkExcludedTypes = []string{"message", "convoy", "convergence"}

type statusPartialReporter interface {
	StatusPartial() bool
}

func statusProviderPartial(sp any) bool {
	reporter, ok := sp.(statusPartialReporter)
	return ok && reporter.StatusPartial()
}

// StatusInput is the Huma input for GET /v0/status.
type StatusInput struct {
	CityScope
	BlockingParam
	// Lite trims the body to the cheap fleet-overview fields for
	// high-frequency dashboard polls, omitting the expensive per-request
	// blocks: StoreHealth (full closed-history Dolt scan), the
	// session-count detail, and the per-rig work-count fan-out. The
	// default/full body that `gc status` renders is unchanged (gascity#3186).
	Lite bool `query:"lite" required:"false" doc:"When true, omit the expensive store-health, session-count, and work-count blocks for low-cost dashboard polls."`
}

// humaHandleStatus is the Huma-typed handler for GET /v0/status.
//
// Read-path gate: refuses to serve while the city-scope CachingStore is
// priming (cacheLiveOr503 → typed 503) so the CLI falls back to its local
// snapshot instead of rendering partial/empty data. CacheAgeS surfaces the
// age of the latest fresh observation so `gc status` can append a staleness
// banner when the supervisor is lagging.
//
// The gate and the age both read the WORK store, which is the class the body's
// expensive legs come from (work counts, store health). It deliberately does
// not gate the session-class store: a CachingStore that cannot serve a read
// from cache falls through to its backing store rather than answering empty,
// so a priming sessions binding surfaces as a "sessions:" partial error from
// statusSessionSnapshot, never as a silent zero-session fleet.
func (s *Server) humaHandleStatus(ctx context.Context, input *StatusInput) (*IndexOutput[StatusBody], error) {
	store := s.state.CityBeadStore()
	if err := cacheLiveOr503(store); err != nil {
		return nil, err
	}
	bp := input.toBlockingParams()
	blocking := bp.isBlocking()
	if blocking {
		waitForChange(ctx, s.state.EventProvider(), bp)
	}
	index := s.latestIndex()

	// /status keys its response cache on a TIME bucket, not the event index:
	// on a busy city the sequence advances every poll, so an index-keyed
	// entry would miss on nearly every request and force a full O(store-size)
	// rebuild (gascity#3186). The bucket changes only once per
	// timeBucketResponseCacheTTL, so high-frequency dashboard polls reuse the
	// built body. The ?lite variant caches under its own key (the shared
	// cache map keys on the string key, so the suffix is enough).
	//
	// Strict-freshness callers (blocking ?index=&wait=) bypass this cache so
	// the body they receive reflects the event they waited for, never a body
	// built before it.
	cacheKey := "status"
	if input.Lite {
		cacheKey = "status?lite"
	}
	bucket := responseCacheTimeBucket(time.Now())
	if !blocking {
		if body, ok := cachedResponseAs[StatusBody](s, cacheKey, bucket); ok {
			return &IndexOutput[StatusBody]{Index: index, CacheAgeS: cacheAgeSeconds(store), Body: body}, nil
		}
		if body, ok := cachedResponseWithinAgeAs[StatusBody](s, cacheKey, statusResponseTTLFloor); ok {
			return &IndexOutput[StatusBody]{Index: index, CacheAgeS: cacheAgeSeconds(store), Body: body}, nil
		}
		// Stale-while-revalidate (ra-4u2eqc): the bucket and TTL-floor caches
		// both missed, so any entry that exists is older than
		// statusResponseTTLFloor. buildStatusBody's fan-out (per-rig work
		// counts, the session snapshot, StoreHealth's closed-history scan)
		// measured ~3.65s on a 26-agent/1.2GB city — the same failure class
		// that used to 503 the legacy runs/census endpoint at its internal
		// budget. Rather than let a request pay that cost inline, serve the
		// stale body immediately and refresh in the background so the next
		// poll gets a fresh body. A genuine cold cache (nothing ever built)
		// has nothing to serve here and falls through to the synchronous
		// build below, same as before this change.
		//
		// CacheAgeS reports the GREATER of the two staleness signals: how
		// long ago this response entry was built, and cacheAgeSeconds(store)
		// — the age of the CachingStore snapshot the body was built from.
		// Reporting only the response-entry age would let a recently built
		// entry sitting on top of a lagging reconciler under-report true
		// staleness and suppress the banner `gc status` renders above
		// cacheAgeBannerThresholdSeconds; reporting only the store age would
		// hide the SWR delay. The max preserves both.
		if body, age, ok := staleResponseAs[StatusBody](s, cacheKey); ok {
			s.refreshStatusResponseInBackground(cacheKey, input.Lite)
			return &IndexOutput[StatusBody]{
				Index:     index,
				CacheAgeS: max(age.Seconds(), cacheAgeSeconds(store)),
				Body:      body,
			}, nil
		}
	}

	resp := s.buildStatusBody(ctx, input.Lite)
	if !blocking {
		s.storeResponse(cacheKey, bucket, resp)
	}

	return &IndexOutput[StatusBody]{Index: index, CacheAgeS: cacheAgeSeconds(store), Body: resp}, nil
}

// refreshStatusResponseInBackground kicks a detached rebuild of the /status
// body for cacheKey and stores the result under the time bucket current at
// completion, so the next poll (bucket or TTL-floor lookup) is served a
// fresh body without any caller paying the rebuild cost inline (ra-4u2eqc).
// Coalesced via beginResponseRefresh: a refresh already in flight for
// cacheKey is not duplicated. Uses runBackground's detached context (bounded
// by extmsgNotifyTimeout) rather than the triggering request's context,
// since the refresh must outlive the request that happened to trigger it and
// benefits every subsequent poller, not just this one.
func (s *Server) refreshStatusResponseInBackground(cacheKey string, lite bool) {
	if !s.beginResponseRefresh(cacheKey) {
		return
	}
	s.runBackground(func(ctx context.Context) {
		defer func() {
			if r := recover(); r != nil {
				// The background refresh is best-effort: a panic here must not
				// take the controller down. The request path's recover
				// (withRecovery, middleware.go) does not cover detached
				// goroutines, and the next poll simply rebuilds.
				log.Printf("api: panic in background /status refresh: %v\n%s", r, debug.Stack())
			}
		}()
		defer s.endResponseRefresh(cacheKey)
		resp := s.buildStatusBody(ctx, lite)
		s.storeResponse(cacheKey, responseCacheTimeBucket(time.Now()), resp)
	})
}

// buildStatusBody constructs the status response body. ctx bounds the
// per-store work-count queries; cancellation aborts in-flight backend
// counts.
//
// When lite is true the expensive per-request blocks are omitted for
// high-frequency dashboard polls (gascity#3186): the work-count fan-out
// (a query per rig store), the StoreHealth block (a full closed-history
// Dolt row scan), and the session-count detail. The session snapshot itself
// is still read because agent running/suspended state depends on it. The full
// (non-lite) body that `gc status` renders is unchanged.
func (s *Server) buildStatusBody(ctx context.Context, lite bool) StatusBody {
	cfg := s.state.Config()
	sp := s.state.SessionProvider()
	cityName := s.state.CityName()
	sessTmpl := cfg.Workspace.SessionTemplate
	sessionSnapshot := s.statusSessionSnapshot(ctx)
	partialErrors := append([]string(nil), sessionSnapshot.partialErrors...)

	citySt, _ := suspensionstate.Load(fsys.OSFS{}, s.state.CityPath())

	// Count agents by state and collect per-agent detail rows in a single
	// pass. Pool expansion emits one detail row per instance with a
	// once-per-group ScaleLabel so the CLI's text formatter can indent the
	// expanded rows the same way it does in the fallback path.
	var ac agentCounts
	var rawRunning int
	agentDetails := make([]StatusAgentDetail, 0, len(cfg.Agents))
	suspendedRigs := make(map[string]bool, len(cfg.Rigs))
	// cacheColdRigs mirrors the controller's per-rig cache refresh gate
	// (rigStoreBackgroundRefresh): a rig suspended by EFFECTIVE state gets no
	// async full prime and no reconciler, so its cache never reaches live and
	// the cache-only Ready projection can never answer. It is deliberately not
	// the same set as suspendedRigs, which grows below to include rigs merely
	// inferred suspended because every one of their agents is — those keep a
	// refreshing cache and must still be asked for ready work.
	cacheColdRigs := make(map[string]bool, len(cfg.Rigs))
	for _, r := range cfg.Rigs {
		if suspensionstate.EffectiveRigSuspended(citySt, r.Name, r.EffectiveSuspendedOnStart()) {
			suspendedRigs[r.Name] = true
			cacheColdRigs[r.Name] = true
		}
	}
	perRigAgentTotals := make(map[string]int, len(cfg.Rigs))
	perRigAgentsSuspended := make(map[string]int, len(cfg.Rigs))
	// Active graph-resident work, indexed once per request by agent session
	// name. On a single-store city this is nil, so every lookup below misses
	// and the counts stay byte-identical to the provider-only behavior.
	graphWork := s.graphActiveWorkBySession()
	for _, a := range cfg.Agents {
		rigName := workdirutil.ConfiguredRigName(s.state.CityPath(), a, cfg.Rigs)
		scope := "city"
		if rigName != "" {
			scope = "rig"
		}
		expanded := expandAgent(a, cityName, sessTmpl, sp)
		expanded = appendUnlimitedPoolSessionBeads(expanded, a, cityName, sessTmpl, sessionSnapshot)
		isPool := len(expanded) > 1 || a.SupportsInstanceExpansion()
		groupName := a.QualifiedName()
		scaleLabelEmitted := false
		for _, ea := range expanded {
			ac.Total++
			if rigName != "" {
				perRigAgentTotals[rigName]++
			}
			sessName := agentSessionName(cityName, ea.qualifiedName, sessTmpl)
			info, hasInfo := sessionSnapshot.bySessionName[sessName]
			running := statusProviderRunning(sp, sessName)
			// An agent whose work runs under a relocated-graph wisp session is
			// effectively running even when its named provider session is down.
			// hasGraphWork is always false on a single-store city.
			_, hasGraphWork := graphWork[sessName]
			effectiveRunning := running || hasGraphWork
			if effectiveRunning {
				rawRunning++
			}
			suspended := ea.suspended || a.Suspended || (rigName != "" && suspendedRigs[rigName]) || (hasInfo && info.state == session.StateSuspended)
			if suspended && rigName != "" {
				perRigAgentsSuspended[rigName]++
			}
			switch {
			case suspended:
				ac.Suspended++
			case s.state.IsQuarantined(sessName):
				ac.Quarantined++
			case effectiveRunning:
				ac.Running++
			}

			detail := StatusAgentDetail{
				QualifiedName: ea.qualifiedName,
				Scope:         scope,
				Running:       effectiveRunning,
				Suspended:     suspended,
				SessionName:   sessName,
				GroupName:     groupName,
				Expanded:      isPool,
			}
			if isPool {
				_, instanceName := config.ParseQualifiedName(ea.qualifiedName)
				detail.Name = instanceName
				if !scaleLabelEmitted {
					detail.ScaleLabel = poolScaleLabel(a)
					scaleLabelEmitted = true
				}
			} else {
				detail.Name = a.Name
			}
			agentDetails = append(agentDetails, detail)
			if a.Dir != "" {
				perRigAgentTotals[a.Dir]++
				if suspended {
					perRigAgentsSuspended[a.Dir]++
				}
			}
		}
	}

	if statusProviderPartial(sp) {
		partialErrors = append(partialErrors, "runtime status probe incomplete; non-running agent rows are unknown")
	}

	// Count rigs by state + collect per-rig detail rows.
	rc := rigCounts{Total: len(cfg.Rigs)}
	rigDetails := make([]StatusRigDetail, 0, len(cfg.Rigs))
	for _, rig := range cfg.Rigs {
		rigSuspended := suspensionstate.EffectiveRigSuspended(citySt, rig.Name, rig.EffectiveSuspendedOnStart())
		if !rigSuspended {
			if total := perRigAgentTotals[rig.Name]; total > 0 && total == perRigAgentsSuspended[rig.Name] {
				rigSuspended = true
			}
		}
		if rigSuspended {
			rc.Suspended++
			suspendedRigs[rig.Name] = true
		}
		rigDetails = append(rigDetails, StatusRigDetail{
			Name:      rig.Name,
			Path:      rig.Path,
			Suspended: rigSuspended,
		})
	}

	// Count work items (best-effort). Skipped in lite mode: querying every
	// rig store is one of the per-request costs the lite poll avoids.
	var wc workCounts
	if !lite {
		var workErrs []string
		wc, workErrs = s.statusWorkCounts(ctx, cacheColdRigs)
		partialErrors = append(partialErrors, workErrs...)
	}

	// Count mail (best-effort).
	var mc mailCounts
	seenProvs := make(map[string]bool)
	for _, mp := range s.state.MailProviders() {
		key := fmt.Sprintf("%p", mp)
		if seenProvs[key] {
			continue
		}
		seenProvs[key] = true
		if total, unread, err := statusMailCountWithTimeout(mp); err == nil {
			mc.Total += total
			mc.Unread += unread
		} else {
			partialErrors = append(partialErrors, fmt.Sprintf("mail: %v", err))
		}
	}

	// Collect named sessions (best-effort; skip when unavailable).
	var namedSessionDetails []StatusNamedSessionDetail
	for _, ns := range cfg.NamedSessions {
		identity := ns.QualifiedName()
		mode := ns.ModeOrDefault()
		status := s.namedSessionStatus(cfg, sessionSnapshot, cityName, identity, mode, suspendedRigs)
		namedSessionDetails = append(namedSessionDetails, StatusNamedSessionDetail{
			Identity: identity,
			Status:   status,
			Mode:     mode,
		})
	}

	// Session counts: derived from the session-class snapshot. Omitted in
	// lite mode (detail block, not needed for the high-frequency overview).
	var sessionCounts *StatusSessionCountsDetail
	if !lite && len(sessionSnapshot.bySessionName) > 0 {
		active, suspended := s.countSessions(sessionSnapshot)
		if active > 0 || suspended > 0 {
			sessionCounts = &StatusSessionCountsDetail{Active: active, Suspended: suspended}
		}
	}

	uptime := int(time.Since(s.state.StartedAt()).Seconds())
	versions := s.resolveComponentVersions()

	// StoreHealth carries a full closed-history Dolt row scan (behind its
	// sub-cache). Omitted in lite mode so a cold lite poll never triggers it.
	var storeHealth *StatusStoreHealth
	if !lite {
		var err error
		storeHealth, err = s.cachedStoreHealth(ctx, time.Now())
		if err != nil {
			partialErrors = append(partialErrors, fmt.Sprintf("store health: %v", err))
		}
	}

	return StatusBody{
		Name:                cityName,
		Path:                s.state.CityPath(),
		Version:             s.state.Version(),
		DoltVersion:         versions.Dolt,
		BeadsVersion:        versions.Beads,
		UptimeSec:           uptime,
		Suspended:           suspensionstate.EffectiveCitySuspended(citySt, cfg.Workspace.EffectiveSuspendedOnStart()),
		AgentCount:          ac.Total,
		RigCount:            rc.Total,
		Running:             rawRunning,
		Agents:              ac,
		Rigs:                rc,
		Work:                wc,
		Mail:                mc,
		Partial:             len(partialErrors) > 0,
		PartialErrors:       partialErrors,
		StoreHealth:         storeHealth,
		Beads:               s.cityBeadsDiagnostic(),
		ConditionalWrites:   s.conditionalWritesStatus(),
		AgentDetails:        agentDetails,
		RigDetails:          rigDetails,
		NamedSessionDetails: namedSessionDetails,
		SessionCountsDetail: sessionCounts,
	}
}

type cityBeadsDiagnosticProvider interface {
	CityBeadsDiagnostic() *beads.BeadsDiagnostic
}

// conditionalWritesStatusProvider is implemented by the controller State to
// expose its latched conditional-writes snapshot (§12.5). The State builds
// the block because only it holds the boot-latched rollout flags, the drift
// notices, and every controller-owned store handle.
type conditionalWritesStatusProvider interface {
	ConditionalWritesStatus() *StatusConditionalWrites
}

func (s *Server) conditionalWritesStatus() *StatusConditionalWrites {
	provider, ok := s.state.(conditionalWritesStatusProvider)
	if !ok {
		return nil
	}
	return provider.ConditionalWritesStatus()
}

func (s *Server) cityBeadsDiagnostic() *beads.BeadsDiagnostic {
	provider, ok := s.state.(cityBeadsDiagnosticProvider)
	if !ok {
		return nil
	}
	return provider.CityBeadsDiagnostic()
}

// poolScaleLabel renders the "scaled (min=N, max=M)" banner the CLI emits
// once per pool group. Mirrors the label buildCityStatusSnapshot emits
// client-side so human output is identical whether served via API or
// fallback.
func poolScaleLabel(a config.Agent) string {
	minSessions := 0
	if a.MinActiveSessions != nil {
		minSessions = *a.MinActiveSessions
	}
	maxSessions := 1
	maxLabel := fmt.Sprintf("max=%d", maxSessions)
	if a.MaxActiveSessions != nil {
		maxSessions = *a.MaxActiveSessions
		if maxSessions < 0 {
			maxLabel = "max=unlimited"
		} else {
			maxLabel = fmt.Sprintf("max=%d", maxSessions)
		}
	}
	return fmt.Sprintf("scaled (min=%d, %s)", minSessions, maxLabel)
}

// namedSessionStatus classifies a named session for the StatusBody detail
// block. Mirrors the CLI's namedSessionStatusForCity: reserved when the
// session bead does not resolve, "degraded blocked" when the session is
// always-on but its agent template is blocked by suspension, or the
// session's state metadata when a bead is present.
func (s *Server) namedSessionStatus(
	cfg *config.City,
	snapshot statusSessionSnapshot,
	cityName, identity, mode string,
	suspendedRigs map[string]bool,
) string {
	status := "reserved-unmaterialized"
	if spec := config.FindNamedSession(cfg, identity); spec != nil {
		if mode == "always" && namedSessionTemplateBlocked(cfg, spec, suspendedRigs) {
			status = "degraded blocked"
		}
	}

	runtimeName := config.NamedSessionRuntimeName(cityName, cfg.Workspace, identity)
	if info, ok := snapshot.bySessionName[runtimeName]; ok {
		if info.state != "" {
			return string(info.state)
		}
		return "materialized"
	}
	if len(snapshot.partialErrors) > 0 {
		return "lookup error: " + strings.Join(snapshot.partialErrors, "; ")
	}
	return status
}

// namedSessionTemplateBlocked reports whether a named-session's target
// agent template is blocked by suspension (city suspended, agent template
// suspended, or the template's rig is suspended).
func namedSessionTemplateBlocked(cfg *config.City, ns *config.NamedSession, suspendedRigs map[string]bool) bool {
	if cfg == nil {
		return false
	}
	if cfg.Workspace.Suspended {
		return true
	}
	if ns == nil {
		return false
	}
	for _, a := range cfg.Agents {
		if a.Name != ns.Template {
			continue
		}
		if ns.Dir != "" && a.Dir != ns.Dir {
			continue
		}
		if a.Suspended {
			return true
		}
		if a.Dir != "" && suspendedRigs[a.Dir] {
			return true
		}
		return false
	}
	return false
}

// countSessions tallies active / suspended sessions from the status snapshot.
func (s *Server) countSessions(snapshot statusSessionSnapshot) (active, suspended int) {
	for _, info := range snapshot.bySessionName {
		switch info.state {
		case session.StateActive:
			active++
		case session.StateSuspended:
			suspended++
		}
	}
	return active, suspended
}

type statusSessionSnapshot struct {
	bySessionName map[string]statusSessionInfo
	byTemplate    map[string][]statusSessionInfo
	partialErrors []string
}

type statusSessionInfo struct {
	sessionName string
	agentName   string
	template    string
	state       session.State
}

// statusSessionSnapshot reads the session-class beads every session-derived
// field of the status body is built from: per-agent running/suspended state,
// named-session status, the unlimited-pool expansion, and the session counts.
//
// It reads SessionsBeadStore(), not CityBeadStore(). Those are the same store
// on a city that relocates nothing, so this is byte-identical there; on a city
// with [beads.classes.sessions] relocated the session beads live in the class
// binding, and reading them off the work ledger returned an empty fleet at
// whatever the work ledger costs — on a cross-region hosted work store that is
// seconds, so the read blew statusStoreReadTimeout and /status reported
// "sessions: loading session snapshot timed out after 1s" for data sitting in a
// local store.
func (s *Server) statusSessionSnapshot(ctx context.Context) statusSessionSnapshot {
	snapshot := statusSessionSnapshot{
		bySessionName: make(map[string]statusSessionInfo),
		byTemplate:    make(map[string][]statusSessionInfo),
	}
	sessions := s.state.SessionsBeadStore()
	store := sessions.Store
	if store == nil {
		// A nil session-class store is benign only when the city has no bead
		// store at all. When the work store IS present, the sessions binding
		// failed to resolve and this projection cannot see the class: say so
		// rather than reporting an empty fleet, and do NOT fall back to the
		// work store. Reading session beads off the work ledger is precisely
		// what kept this mis-routing invisible.
		if s.state.CityBeadStore() != nil {
			snapshot.partialErrors = []string{"sessions: session-class bead store unavailable"}
		}
		return snapshot
	}

	// reqCtx bounds the scoped-store read below; defer cancel() fires on
	// every return path (including the time.After timeout), killing an
	// in-flight bd child instead of leaking it past this function's budget
	// (gascity ga-cdmx6x).
	reqCtx, cancel := context.WithTimeout(ctx, statusStoreReadTimeout)
	defer cancel()

	type snapshotResult struct {
		infos         []session.Info
		partialErrors []string
		err           error
	}
	done := make(chan snapshotResult, 1)
	go func() {
		// Resolve the ctx-bound scoped store INSIDE the timed goroutine.
		// ScopedStoreLike hands back a bd-CLI-backed clone reqCtx can cancel,
		// or (nil, nil) for non-bd backends (native/file/mem) — those read
		// through store unchanged. Its resolution (bd env / managed-dolt
		// connection state) is synchronous and can block on a mutex the
		// reconcile loop holds without honoring reqCtx; kept before the select
		// it hung the whole handler past its read budget, dragging the
		// supervisor loop (gc-08qgn). Under the goroutine the same time.After
		// as the read bounds it.
		readSessions := sessions
		if scoped, err := s.state.ScopedStoreLike(reqCtx, store); err != nil {
			done <- snapshotResult{err: fmt.Errorf("resolving scoped store: %w", err)}
			return
		} else if scoped != nil {
			readSessions = beads.SessionStore{Store: scoped}
		}
		infos, partialErrors, err := sessionReadModelInfos(session.NewStore(readSessions))
		done <- snapshotResult{infos: infos, partialErrors: partialErrors, err: err}
	}()

	var infos []session.Info
	var partialErrors []string
	var err error
	select {
	case result := <-done:
		infos = result.infos
		partialErrors = result.partialErrors
		err = result.err
	case <-time.After(statusStoreReadTimeout):
		snapshot.partialErrors = []string{fmt.Sprintf("sessions: loading session snapshot timed out after %s", statusStoreReadTimeout)}
		return snapshot
	}

	if err != nil {
		snapshot.partialErrors = []string{fmt.Sprintf("sessions: %v", err)}
		return snapshot
	}
	for _, partialErr := range partialErrors {
		snapshot.partialErrors = append(snapshot.partialErrors, fmt.Sprintf("sessions: %s", partialErr))
	}

	seenSessionName := make(map[string]bool, len(infos))
	for _, sessInfo := range infos {
		if sessInfo.Closed {
			continue
		}
		info := statusSessionInfo{
			sessionName: strings.TrimSpace(sessInfo.SessionNameMetadata),
			agentName:   strings.TrimSpace(sessInfo.AgentName),
			template:    strings.TrimSpace(sessInfo.Template),
			state:       statusSessionStateInfo(sessInfo),
		}
		if info.sessionName == "" {
			continue
		}
		if info.state == session.StateArchived {
			continue
		}
		if seenSessionName[info.sessionName] {
			continue
		}
		seenSessionName[info.sessionName] = true
		snapshot.bySessionName[info.sessionName] = info
		if info.template != "" {
			snapshot.byTemplate[info.template] = append(snapshot.byTemplate[info.template], info)
		}
	}
	return snapshot
}

// statusWorkResult is one store's contribution to the work counts.
type statusWorkResult struct {
	wc       workCounts
	readyIDs []string
	errs     []string
}

// statusWorkCounts tallies persisted open/in_progress work across BeadStores
// and federates canonical Ready work the way GET /beads/ready does over the
// work stores: the city store first, then BeadStores excluding the CityName
// alias. Stores exposing beads.Counter answer persisted counts without
// hydrating rows — the caching layer counts matches in memory when its cache is
// clean (#1896). Stores are queried concurrently; results aggregate in
// deterministic city/rig order.
//
// NOT identical to GET /beads/ready on a split city: that handler grew a
// relocated-graph-store leg (huma_handlers_beads.go) and this one has none, so
// on a city with [beads.classes.graph] relocated the status ready count omits
// graph-class ready work while /beads/ready includes it. Left divergent
// deliberately rather than fixed here: this read is cache-only and error-lenient
// by design (see cacheColdRigs below), which is the opposite of the graph leg's
// fail-loud contract, so wiring one in is its own slice, not a rider.
//
// Rigs in cacheColdRigs are asked for persisted counts but not for ready work.
// Their store runs no background cache refresh, so the cache-only Ready
// projection is guaranteed to decline with ErrCacheUnavailable — reporting that
// as a partial error made every city with a suspended rig permanently partial,
// which greys out unrelated status tiles in the dashboard. Skipping the read
// changes no count: the failing read already contributed zero ready work.
func (s *Server) statusWorkCounts(ctx context.Context, cacheColdRigs map[string]bool) (workCounts, []string) {
	stores := s.state.BeadStores()
	// sortedRigNames deduplicates rigs sharing one store instance, so each
	// store's persisted statuses are counted exactly once.
	rigNames := sortedRigNames(stores)
	type workQuery struct {
		label         string
		store         beads.Store
		includeStored bool
		includeReady  bool
	}
	queries := make([]workQuery, 0, len(rigNames)+1)
	if cityStore := s.state.CityBeadStore(); cityStore != nil {
		queries = append(queries, workQuery{
			label:        "city",
			store:        cityStore,
			includeReady: true,
		})
	}
	cityName := s.state.CityName()
	for _, rigName := range rigNames {
		queries = append(queries, workQuery{
			label:         "rig " + rigName,
			store:         stores[rigName],
			includeStored: true,
			includeReady:  rigName != cityName && !cacheColdRigs[rigName],
		})
	}

	results := make([]statusWorkResult, len(queries))
	var wg sync.WaitGroup
	for i, query := range queries {
		wg.Add(1)
		go func(i int, query workQuery) {
			defer wg.Done()
			results[i] = statusStoreWorkCountsFor(
				ctx,
				s.state,
				query.label,
				query.store,
				query.includeStored,
				query.includeReady,
			)
		}(i, query)
	}
	wg.Wait()

	var wc workCounts
	var errs []string
	seenReady := make(map[string]bool)
	for _, r := range results {
		wc.Open += r.wc.Open
		wc.InProgress += r.wc.InProgress
		for _, id := range r.readyIDs {
			if seenReady[id] {
				continue
			}
			seenReady[id] = true
			wc.Ready++
		}
		errs = append(errs, r.errs...)
	}
	return wc, errs
}

// statusStoreWorkCounts counts one store's persisted open/in-progress work
// and independently derives ready work through the canonical live Ready
// projection. Both reads share one per-store deadline. Operational Count
// failures report a partial error without retrying via List — the List scan
// would hit the same backend — but do not discard a successful Ready result.
func statusStoreWorkCounts(ctx context.Context, state State, rigName string, store beads.Store) statusWorkResult {
	return statusStoreWorkCountsFor(ctx, state, "rig "+rigName, store, true, true)
}

func statusStoreWorkCountsFor(
	ctx context.Context,
	state State,
	label string,
	store beads.Store,
	includeStored bool,
	includeReady bool,
) statusWorkResult {
	ctx, cancel := context.WithTimeout(ctx, statusStoreReadTimeout)
	defer cancel()

	type storedResult struct {
		wc  workCounts
		err error
	}
	type readyResult struct {
		rows []beads.Bead
		err  error
	}
	storedDone := make(chan storedResult, 1)
	stored := &storedResult{}
	if includeStored {
		stored = nil
		go func() {
			wc, err := statusStoredWorkCounts(ctx, state, store)
			storedDone <- storedResult{wc: wc, err: err}
		}()
	}
	ready := &readyResult{}
	if includeReady {
		// ContextReadyReader and ScopedStoreLike both guarantee cleanup before
		// returning after cancellation. Invoke this branch synchronously so the
		// coordinator cannot return while scoped resolution is still cleaning up.
		rows, err := statusReadyStoreWithTimeout(ctx, state, store)
		ready = &readyResult{rows: rows, err: err}
	}

	for stored == nil {
		select {
		case value := <-storedDone:
			stored = &value
			storedDone = nil
		case <-ctx.Done():
			// Prefer a result that completed at the deadline boundary. The channel
			// is buffered, so a context-blind legacy operation can finish later
			// without blocking on its abandoned result send.
			if stored == nil {
				select {
				case value := <-storedDone:
					stored = &value
					storedDone = nil
				default:
				}
			}
			if stored == nil {
				err := ctx.Err()
				if errors.Is(err, context.DeadlineExceeded) {
					err = fmt.Errorf("stored counts timed out: %w", err)
				}
				stored = &storedResult{err: err}
			}
		}
	}

	result := statusWorkResult{wc: stored.wc}
	if stored.err != nil {
		result.errs = append(result.errs, fmt.Sprintf("%s work: %v", label, stored.err))
	}
	if ready.err != nil {
		result.errs = append(result.errs, fmt.Sprintf("%s work ready: %v", label, ready.err))
	}
	if ready.err == nil || (beads.IsPartialResult(ready.err) && len(ready.rows) > 0) {
		result.wc.Ready = len(ready.rows)
		result.readyIDs = make([]string, 0, len(ready.rows))
		for _, row := range ready.rows {
			result.readyIDs = append(result.readyIDs, row.ID)
		}
	}
	return result
}

// statusStoredWorkCounts counts the persisted open/in-progress buckets,
// preferring the hydration-free Counter path and falling back to List only
// when Count explicitly reports that the query shape is unsupported.
func statusStoredWorkCounts(ctx context.Context, state State, store beads.Store) (workCounts, error) {
	if counter, ok := store.(beads.Counter); ok {
		wc, err := statusCountWork(ctx, counter)
		if err == nil || !errors.Is(err, beads.ErrCountUnsupported) {
			return wc, err
		}
	}

	list, err := statusListStoreWithTimeout(ctx, state, store, beads.ListQuery{AllowScan: true})
	var wc workCounts
	if err != nil && (!beads.IsPartialResult(err) || len(list) == 0) {
		return wc, err
	}
	for _, b := range list {
		if slices.Contains(statusWorkExcludedTypes, b.Type) {
			continue
		}
		switch b.Status {
		case "in_progress":
			wc.InProgress++
		case "open":
			wc.Open++
		}
	}
	return wc, err
}

// statusCountWork fills the persisted work-count buckets via beads.Counter.
// Readiness is not a stored status; statusStoreWorkCounts derives it through
// Ready instead. The caller supplies the shared per-store deadline.
func statusCountWork(ctx context.Context, counter beads.Counter) (workCounts, error) {
	var wc workCounts
	for _, bucket := range []struct {
		status string
		dst    *int
	}{
		{"open", &wc.Open},
		{"in_progress", &wc.InProgress},
	} {
		n, err := counter.Count(ctx, beads.ListQuery{Status: bucket.status, AllowScan: true}, statusWorkExcludedTypes...)
		if err != nil {
			return wc, err
		}
		*bucket.dst = n
	}
	return wc, nil
}

// statusListStoreWithTimeout lists with the per-store read timeout.
// Store.List takes no context, so on timeout the goroutine is abandoned
// (it keeps its connection until the scan returns) — unless state offers a
// ctx-bound scoped clone of store (bd-CLI-backed stores do; native/file/mem
// stores don't and are read unchanged), in which case cancellation kills
// the in-flight backend command instead of abandoning it (gascity
// ga-cdmx6x). Counter-capable stores avoid this path entirely.
func statusListStoreWithTimeout(ctx context.Context, state State, store beads.Store, query beads.ListQuery) ([]beads.Bead, error) {
	if store == nil {
		return nil, nil
	}
	reqCtx, cancel := context.WithTimeout(ctx, statusStoreReadTimeout)
	defer cancel()
	if err := reqCtx.Err(); err != nil {
		return nil, err
	}
	type listResult struct {
		rows []beads.Bead
		err  error
	}
	done := make(chan listResult, 1)
	go func() {
		// Resolve the ctx-bound scoped store INSIDE the timed goroutine so a
		// slow, ctx-blind resolution (a store mutex held by the reconcile
		// loop) is bounded by the same request deadline as the list instead of
		// hanging the handler synchronously (gc-08qgn).
		readStore := store
		if scoped, err := state.ScopedStoreLike(reqCtx, store); err != nil {
			done <- listResult{err: fmt.Errorf("resolving scoped store: %w", err)}
			return
		} else if scoped != nil {
			readStore = scoped
		}
		rows, err := readStore.List(query)
		done <- listResult{rows: rows, err: err}
	}()
	select {
	case result := <-done:
		return result.rows, result.err
	case <-reqCtx.Done():
		if errors.Is(reqCtx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("list timed out: %w", reqCtx.Err())
		}
		return nil, reqCtx.Err()
	}
}

// statusReadyStoreWithTimeout reads the same live canonical Ready projection
// as GET /beads/ready. Policy-aware stores retain their tier behavior through
// ScopedStoreLike; the scoped clone binds bd subprocesses to reqCtx so timeout
// cancellation cannot leak a child beyond the status request.
func statusReadyStoreWithTimeout(ctx context.Context, state State, store beads.Store) ([]beads.Bead, error) {
	if store == nil {
		return nil, nil
	}
	reqCtx, cancel := context.WithTimeout(ctx, statusStoreReadTimeout)
	defer cancel()
	if err := reqCtx.Err(); err != nil {
		return nil, err
	}
	var capabilityErr error
	if reader, ok := store.(beads.ContextReadyReader); ok {
		rows, err := reader.ReadyContext(reqCtx)
		if !errors.Is(err, beads.ErrReadyContextUnsupported) {
			return rows, statusReadyError(err)
		}
		capabilityErr = err
	}

	// ScopedStoreLike is part of the context-aware read contract: resolution
	// must finish its own cleanup before returning after reqCtx cancellation.
	// Keep it synchronous so the status deadline cannot abandon a resolver
	// goroutine after the response has returned.
	scoped, err := state.ScopedStoreLike(reqCtx, store)
	if err != nil {
		return nil, statusReadyError(fmt.Errorf("resolving scoped store: %w", err))
	}
	if scoped == nil {
		if capabilityErr == nil {
			capabilityErr = fmt.Errorf("reading canonical ready projection: %w", beads.ErrReadyContextUnsupported)
		}
		return nil, capabilityErr
	}

	// ScopedStoreLike guarantees the clone and its legacy Ready operation are
	// bound to reqCtx, including child-process cleanup, so no outer goroutine is
	// needed to enforce the deadline.
	rows, err := beads.HandlesFor(scoped).Live.Ready()
	return rows, statusReadyError(err)
}

func statusReadyError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("ready timed out: %w", err)
	}
	return err
}

func statusMailCountWithTimeout(mp interface {
	Count(string) (total int, unread int, err error)
},
) (int, int, error) {
	if mp == nil {
		return 0, 0, nil
	}
	type countResult struct {
		total  int
		unread int
		err    error
	}
	done := make(chan countResult, 1)
	go func() {
		total, unread, err := mp.Count("")
		done <- countResult{total: total, unread: unread, err: err}
	}()
	select {
	case result := <-done:
		return result.total, result.unread, result.err
	case <-time.After(statusStoreReadTimeout):
		return 0, 0, fmt.Errorf("count timed out after %s", statusStoreReadTimeout)
	}
}

func appendUnlimitedPoolSessionBeads(expanded []expandedAgent, a config.Agent, cityName, sessTmpl string, snapshot statusSessionSnapshot) []expandedAgent {
	maxSess := a.EffectiveMaxActiveSessions()
	if !a.SupportsInstanceExpansion() || (maxSess != nil && *maxSess >= 0) {
		return expanded
	}

	seenSessionNames := make(map[string]bool, len(expanded))
	for _, ea := range expanded {
		seenSessionNames[agentSessionName(cityName, ea.qualifiedName, sessTmpl)] = true
	}

	poolName := a.QualifiedName()
	for _, info := range snapshot.byTemplate[poolName] {
		if seenSessionNames[info.sessionName] {
			continue
		}
		qn := statusSessionQualifiedName(cityName, sessTmpl, info)
		if qn == "" {
			continue
		}
		expanded = append(expanded, expandedAgent{
			qualifiedName: qn,
			rig:           a.Dir,
			pool:          poolName,
			suspended:     a.Suspended,
			provider:      a.Provider,
			description:   a.Description,
		})
		seenSessionNames[info.sessionName] = true
	}
	return expanded
}

func statusSessionQualifiedName(cityName, sessTmpl string, info statusSessionInfo) string {
	if info.agentName != "" && info.agentName != info.template {
		return info.agentName
	}
	qnSanitized := info.sessionName
	templatePrefix := agent.SessionNameFor(cityName, "", sessTmpl)
	if templatePrefix != "" && strings.HasPrefix(qnSanitized, templatePrefix) {
		qnSanitized = qnSanitized[len(templatePrefix):]
	}
	return agent.UnsanitizeQualifiedNameFromSession(qnSanitized)
}

// statusSessionStateInfo maps the raw persisted state metadata (Info.MetadataState,
// not the closed-blanked Info.State) onto the display state the status snapshot
// reports, folding the awake/drained aliases exactly as the retired bead form did.
func statusSessionStateInfo(info session.Info) session.State {
	state := session.State(strings.TrimSpace(info.MetadataState))
	switch state {
	case "awake":
		return session.StateActive
	case "drained":
		return session.StateAsleep
	default:
		return state
	}
}

func statusProviderRunning(sp interface{ IsRunning(string) bool }, sessionName string) bool {
	sessionName = strings.TrimSpace(sessionName)
	if sp == nil || sessionName == "" {
		return false
	}
	return sp.IsRunning(sessionName)
}

// HealthInput is the Huma input for GET /v0/city/{cityName}/health.
type HealthInput struct {
	CityScope
}

// humaHandleHealth is the Huma-typed handler for GET /v0/city/{cityName}/health.
func (s *Server) humaHandleHealth(_ context.Context, _ *HealthInput) (*HealthOutput, error) {
	uptime := int(time.Since(s.state.StartedAt()).Seconds())
	out := &HealthOutput{}
	out.Body.Status = "ok"
	out.Body.Version = s.state.Version()
	out.Body.City = s.state.CityName()
	out.Body.UptimeSec = uptime
	return out, nil
}
