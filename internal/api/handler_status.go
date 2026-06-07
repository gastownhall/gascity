package api

import (
	"context"
	"fmt"
	"strings"
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

// StatusInput is the Huma input for GET /v0/status.
type StatusInput struct {
	CityScope
	BlockingParam
	// Lite trims the body to the cheap fleet-overview fields for
	// high-frequency dashboard polls, omitting the expensive per-request
	// blocks: StoreHealth (full closed-history Dolt scan), the
	// session-count detail, and the per-rig work loop. The default/full
	// body that `gc status` renders is unchanged (gascity#3186).
	Lite bool `query:"lite" required:"false" doc:"When true, omit the expensive store-health, session-count, and work-count blocks for low-cost dashboard polls."`
}

// humaHandleStatus is the Huma-typed handler for GET /v0/status.
//
// Read-path gate: refuses to serve while the city-scope CachingStore is
// priming (cacheLiveOr503 → typed 503) so the CLI falls back to its local
// snapshot instead of rendering partial/empty data. CacheAgeS surfaces the
// age of the latest fresh observation so `gc status` can append a staleness
// banner when the supervisor is lagging.
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
	}

	resp := s.buildStatusBody(input.Lite)
	if !blocking {
		s.storeResponse(cacheKey, bucket, resp)
	}

	return &IndexOutput[StatusBody]{Index: index, CacheAgeS: cacheAgeSeconds(store), Body: resp}, nil
}

// buildStatusBody constructs the status response body.
//
// When lite is true the expensive per-request blocks are omitted for
// high-frequency dashboard polls (gascity#3186): the per-rig work-count loop
// (a scan of every rig store), the StoreHealth block (a full closed-history
// Dolt row scan), and the session-count detail. The session snapshot itself
// is still read because agent running/suspended state depends on it. The full
// (non-lite) body that `gc status` renders is unchanged.
func (s *Server) buildStatusBody(lite bool) StatusBody {
	cfg := s.state.Config()
	sp := s.state.SessionProvider()
	cityName := s.state.CityName()
	sessTmpl := cfg.Workspace.SessionTemplate
	sessionSnapshot := s.statusSessionSnapshot()
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
	for _, r := range cfg.Rigs {
		if suspensionstate.EffectiveRigSuspended(citySt, r.Name, r.EffectiveSuspendedOnStart()) {
			suspendedRigs[r.Name] = true
		}
	}
	perRigAgentTotals := make(map[string]int, len(cfg.Rigs))
	perRigAgentsSuspended := make(map[string]int, len(cfg.Rigs))
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
			if running {
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
			case running:
				ac.Running++
			}

			detail := StatusAgentDetail{
				QualifiedName: ea.qualifiedName,
				Scope:         scope,
				Running:       running,
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

	// Count work items (best-effort). Skipped in lite mode: scanning every
	// rig store is one of the per-request costs the lite poll avoids.
	var wc workCounts
	if !lite {
		stores := s.state.BeadStores()
		seenStores := make(map[string]bool)
		for _, rigName := range sortedRigNames(stores) {
			store := stores[rigName]
			key := fmt.Sprintf("%p", store)
			if seenStores[key] {
				continue
			}
			seenStores[key] = true
			list, err := statusListStoreWithTimeout(store, beads.ListQuery{AllowScan: true})
			if err != nil {
				partialErrors = append(partialErrors, fmt.Sprintf("rig %s work: %v", rigName, err))
				if !beads.IsPartialResult(err) || len(list) == 0 {
					continue
				}
			}
			for _, b := range list {
				switch b.Type {
				case "message", "convoy", "convergence":
					continue
				}
				switch b.Status {
				case "in_progress":
					wc.InProgress++
				case "ready":
					wc.Ready++
				case "open":
					wc.Open++
				}
			}
		}
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

	// Session counts: walk the city bead store for session beads. Omitted in
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

	// StoreHealth carries a full closed-history Dolt row scan (behind a 30s
	// sub-cache). Omitted in lite mode so a cold lite poll never triggers it.
	var storeHealth *StatusStoreHealth
	if !lite {
		storeHealth = s.cachedStoreHealth(time.Now())
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
		AgentDetails:        agentDetails,
		RigDetails:          rigDetails,
		NamedSessionDetails: namedSessionDetails,
		SessionCountsDetail: sessionCounts,
	}
}

type cityBeadsDiagnosticProvider interface {
	CityBeadsDiagnostic() *beads.BeadsDiagnostic
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

func (s *Server) statusSessionSnapshot() statusSessionSnapshot {
	snapshot := statusSessionSnapshot{
		bySessionName: make(map[string]statusSessionInfo),
		byTemplate:    make(map[string][]statusSessionInfo),
	}
	store := s.state.CityBeadStore()
	if store == nil {
		return snapshot
	}

	type snapshotResult struct {
		rows          []beads.Bead
		partialErrors []string
		err           error
	}
	done := make(chan snapshotResult, 1)
	go func() {
		rows, partialErrors, err := sessionReadModelRows(store)
		done <- snapshotResult{rows: rows, partialErrors: partialErrors, err: err}
	}()

	var rows []beads.Bead
	var partialErrors []string
	var err error
	select {
	case result := <-done:
		rows = result.rows
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

	seenSessionName := make(map[string]bool, len(rows))
	for _, b := range rows {
		if b.Status == "closed" {
			continue
		}
		info := statusSessionInfo{
			sessionName: strings.TrimSpace(b.Metadata["session_name"]),
			agentName:   strings.TrimSpace(b.Metadata["agent_name"]),
			template:    strings.TrimSpace(b.Metadata["template"]),
			state:       statusSessionState(b),
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

func statusListStoreWithTimeout(store beads.Store, query beads.ListQuery) ([]beads.Bead, error) {
	if store == nil {
		return nil, nil
	}
	type listResult struct {
		rows []beads.Bead
		err  error
	}
	done := make(chan listResult, 1)
	go func() {
		rows, err := store.List(query)
		done <- listResult{rows: rows, err: err}
	}()
	select {
	case result := <-done:
		return result.rows, result.err
	case <-time.After(statusStoreReadTimeout):
		return nil, fmt.Errorf("list timed out after %s", statusStoreReadTimeout)
	}
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

func statusSessionState(b beads.Bead) session.State {
	state := session.State(strings.TrimSpace(b.Metadata["state"]))
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
