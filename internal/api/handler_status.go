package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
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

// StatusInput is the Huma input for GET /v0/status.
type StatusInput struct {
	CityScope
	BlockingParam
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
	if bp.isBlocking() {
		waitForChange(ctx, s.state.EventProvider(), bp)
	}
	index := s.latestIndex()

	// Check typed response cache (Fix 3l).
	cacheKey := "status"
	if body, ok := cachedResponseAs[StatusBody](s, cacheKey, index); ok {
		return &IndexOutput[StatusBody]{Index: index, CacheAgeS: cacheAgeSeconds(store), Body: body}, nil
	}

	resp := s.buildStatusBody()
	s.storeResponse(cacheKey, index, resp)

	return &IndexOutput[StatusBody]{Index: index, CacheAgeS: cacheAgeSeconds(store), Body: resp}, nil
}

// buildStatusBody constructs the status response body.
func (s *Server) buildStatusBody() StatusBody {
	cfg := s.state.Config()
	sp := s.state.SessionProvider()
	store := s.state.CityBeadStore()
	cityName := s.state.CityName()
	sessTmpl := cfg.Workspace.SessionTemplate

	// Count agents by state and collect per-agent detail rows in a single
	// pass. Pool expansion emits one detail row per instance with a
	// once-per-group ScaleLabel so the CLI's text formatter can indent the
	// expanded rows the same way it does in the fallback path.
	var ac agentCounts
	var rawRunning int
	agentDetails := make([]StatusAgentDetail, 0, len(cfg.Agents))
	suspendedRigs := make(map[string]bool, len(cfg.Rigs))
	for _, r := range cfg.Rigs {
		if r.Suspended {
			suspendedRigs[r.Name] = true
		}
	}
	perRigAgentTotals := make(map[string]int, len(cfg.Rigs))
	perRigAgentsSuspended := make(map[string]int, len(cfg.Rigs))
	for _, a := range cfg.Agents {
		scope := "city"
		if a.Dir != "" {
			scope = "rig"
		}
		expanded := expandAgent(a, cityName, sessTmpl, sp)
		isPool := len(expanded) > 1 || a.SupportsInstanceExpansion()
		groupName := a.QualifiedName()
		scaleLabelEmitted := false
		for _, ea := range expanded {
			ac.Total++
			sessName := agentSessionName(cityName, ea.qualifiedName, sessTmpl)
			handle, _ := s.workerHandleForSessionTarget(store, sessName)
			obs, _ := worker.ObserveHandle(context.Background(), handle)
			running := obs.Running
			if running {
				rawRunning++
			}
			agentSuspended := ea.suspended || (a.Dir != "" && suspendedRigs[a.Dir])
			suspended := agentSuspended || obs.Suspended
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
		rigSuspended := rig.Suspended
		if !rigSuspended {
			if total := perRigAgentTotals[rig.Name]; total > 0 && total == perRigAgentsSuspended[rig.Name] {
				rigSuspended = true
			}
		}
		if rigSuspended {
			rc.Suspended++
		}
		rigDetails = append(rigDetails, StatusRigDetail{
			Name:      rig.Name,
			Path:      rig.Path,
			Suspended: rigSuspended,
		})
	}

	// Count work items (best-effort).
	var wc workCounts
	stores := s.state.BeadStores()
	seenStores := make(map[string]bool)
	for _, rigName := range sortedRigNames(stores) {
		store := stores[rigName]
		key := fmt.Sprintf("%p", store)
		if seenStores[key] {
			continue
		}
		seenStores[key] = true
		list, err := store.List(beads.ListQuery{AllowScan: true})
		if err != nil {
			continue
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

	// Count mail (best-effort).
	var mc mailCounts
	seenProvs := make(map[string]bool)
	for _, mp := range s.state.MailProviders() {
		key := fmt.Sprintf("%p", mp)
		if seenProvs[key] {
			continue
		}
		seenProvs[key] = true
		if total, unread, err := mp.Count(""); err == nil {
			mc.Total += total
			mc.Unread += unread
		}
	}

	// Collect named sessions (best-effort; skip when unavailable).
	var namedSessionDetails []StatusNamedSessionDetail
	for _, ns := range cfg.NamedSessions {
		identity := ns.QualifiedName()
		mode := ns.ModeOrDefault()
		status := s.namedSessionStatus(cfg, store, cityName, identity, mode, suspendedRigs)
		namedSessionDetails = append(namedSessionDetails, StatusNamedSessionDetail{
			Identity: identity,
			Status:   status,
			Mode:     mode,
		})
	}

	// Session counts: walk the city bead store for session beads.
	var sessionCounts *StatusSessionCountsDetail
	if store != nil {
		active, suspended := s.countSessions(store)
		if active > 0 || suspended > 0 {
			sessionCounts = &StatusSessionCountsDetail{Active: active, Suspended: suspended}
		}
	}

	uptime := int(time.Since(s.state.StartedAt()).Seconds())

	return StatusBody{
		Name:                cityName,
		Path:                s.state.CityPath(),
		Version:             s.state.Version(),
		UptimeSec:           uptime,
		Suspended:           cfg.Workspace.Suspended,
		AgentCount:          ac.Total,
		RigCount:            rc.Total,
		Running:             rawRunning,
		Agents:              ac,
		Rigs:                rc,
		Work:                wc,
		Mail:                mc,
		StoreHealth:         s.cachedStoreHealth(time.Now()),
		AgentDetails:        agentDetails,
		RigDetails:          rigDetails,
		NamedSessionDetails: namedSessionDetails,
		SessionCountsDetail: sessionCounts,
	}
}

// poolScaleLabel renders the "scaled (min=N, max=M)" banner the CLI emits
// once per pool group. Mirrors the label buildCityStatusSnapshot emits
// client-side so human output is identical whether served via API or
// fallback.
func poolScaleLabel(a config.Agent) string {
	min := 0
	if a.MinActiveSessions != nil {
		min = *a.MinActiveSessions
	}
	max := 1
	maxLabel := fmt.Sprintf("max=%d", max)
	if a.MaxActiveSessions != nil {
		max = *a.MaxActiveSessions
		if max < 0 {
			maxLabel = "max=unlimited"
		} else {
			maxLabel = fmt.Sprintf("max=%d", max)
		}
	}
	return fmt.Sprintf("scaled (min=%d, %s)", min, maxLabel)
}

// namedSessionStatus classifies a named session for the StatusBody detail
// block. Mirrors the CLI's namedSessionStatusForCity: reserved when the
// session bead does not resolve, "degraded blocked" when the session is
// always-on but its agent template is blocked by suspension, or the
// session's state metadata when a bead is present.
func (s *Server) namedSessionStatus(
	cfg *config.City,
	store beads.Store,
	cityName, identity, mode string,
	suspendedRigs map[string]bool,
) string {
	status := "reserved-unmaterialized"
	if spec := config.FindNamedSession(cfg, identity); spec != nil {
		if mode == "always" && namedSessionTemplateBlocked(cfg, spec, suspendedRigs) {
			status = "degraded blocked"
		}
	}
	if store == nil {
		return status
	}

	runtimeName := config.NamedSessionRuntimeName(cityName, cfg.Workspace, identity)
	id, err := session.ResolveSessionIDAllowClosed(store, runtimeName)
	if err != nil {
		if errors.Is(err, session.ErrSessionNotFound) {
			return status
		}
		return "lookup error: " + err.Error()
	}
	bead, err := store.Get(id)
	if err != nil {
		return "lookup error: " + err.Error()
	}
	if state := strings.TrimSpace(bead.Metadata["state"]); state != "" {
		return state
	}
	return "materialized"
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

// countSessions walks the city bead store and tallies active / suspended
// session beads. Errors from the underlying List are silently swallowed —
// partial counts are better than a full read failure for a status endpoint.
func (s *Server) countSessions(store beads.Store) (active, suspended int) {
	list, err := store.List(beads.ListQuery{Type: "session", IncludeClosed: false, AllowScan: true})
	if err != nil {
		return 0, 0
	}
	for _, b := range list {
		switch strings.TrimSpace(b.Metadata["state"]) {
		case string(session.StateActive):
			active++
		case string(session.StateSuspended):
			suspended++
		}
	}
	return active, suspended
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
