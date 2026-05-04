package api

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
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

// StatusInput is the Huma input for GET /v0/status.
type StatusInput struct {
	CityScope
	BlockingParam
}

// humaHandleStatus is the Huma-typed handler for GET /v0/status.
func (s *Server) humaHandleStatus(ctx context.Context, input *StatusInput) (*IndexOutput[StatusBody], error) {
	bp := input.toBlockingParams()
	if bp.isBlocking() {
		waitForChange(ctx, s.state.EventProvider(), bp)
	}
	index := s.latestIndex()

	// Check typed response cache (Fix 3l).
	cacheKey := "status"
	if body, ok := cachedResponseAs[StatusBody](s, cacheKey, index); ok {
		return &IndexOutput[StatusBody]{Index: index, Body: body}, nil
	}

	resp := s.buildStatusBody()
	s.storeResponse(cacheKey, index, resp)

	return &IndexOutput[StatusBody]{Index: index, Body: resp}, nil
}

// buildStatusBody constructs the status response body.
func (s *Server) buildStatusBody() StatusBody {
	cfg := s.state.Config()
	sp := s.state.SessionProvider()
	cityName := s.state.CityName()
	sessTmpl := cfg.Workspace.SessionTemplate
	sessionSnapshot := s.statusSessionSnapshot()
	partialErrors := append([]string(nil), sessionSnapshot.partialErrors...)

	// Count agents by state.
	var ac agentCounts
	var rawRunning int
	rigAgentCounts := make(map[string]int)
	rigSuspendedCounts := make(map[string]int)
	for _, a := range cfg.Agents {
		rigName := workdirutil.ConfiguredRigName(s.state.CityPath(), a, cfg.Rigs)
		for _, slot := range statusAgentSlots(a, cityName, sessTmpl, sessionSnapshot) {
			ac.Total++
			if rigName != "" {
				rigAgentCounts[rigName]++
			}
			running := statusProviderRunning(sp, slot.sessionName)
			if running {
				rawRunning++
			}
			suspended := a.Suspended || slot.suspended
			if suspended && rigName != "" {
				rigSuspendedCounts[rigName]++
			}
			switch {
			case suspended:
				ac.Suspended++
			case s.state.IsQuarantined(slot.sessionName):
				ac.Quarantined++
			case running:
				ac.Running++
			}
		}
	}

	// Count rigs by state.
	rc := rigCounts{Total: len(cfg.Rigs)}
	for _, rig := range cfg.Rigs {
		if rig.Suspended {
			rc.Suspended++
			continue
		}
		if total := rigAgentCounts[rig.Name]; total > 0 && total == rigSuspendedCounts[rig.Name] {
			rc.Suspended++
		}
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

	uptime := int(time.Since(s.state.StartedAt()).Seconds())

	return StatusBody{
		Name:          cityName,
		Path:          s.state.CityPath(),
		Version:       s.state.Version(),
		UptimeSec:     uptime,
		Suspended:     cfg.Workspace.Suspended,
		AgentCount:    ac.Total,
		RigCount:      rc.Total,
		Running:       rawRunning,
		Agents:        ac,
		Rigs:          rc,
		Work:          wc,
		Mail:          mc,
		Partial:       len(partialErrors) > 0,
		PartialErrors: partialErrors,
	}
}

type statusSessionSnapshot struct {
	bySessionName map[string]statusSessionInfo
	byTemplate    map[string][]statusSessionInfo
	partialErrors []string
}

type statusSessionInfo struct {
	sessionName string
	template    string
	state       session.State
}

type statusAgentSlot struct {
	sessionName string
	suspended   bool
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

	rows, partialErrors, err := sessionReadModelRows(store)
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

func statusAgentSlots(a config.Agent, cityName, sessTmpl string, snapshot statusSessionSnapshot) []statusAgentSlot {
	maxSess := a.EffectiveMaxActiveSessions()
	isMultiSession := maxSess == nil || *maxSess != 1
	if isMultiSession && (maxSess == nil || *maxSess < 0) {
		sessions := snapshot.byTemplate[a.QualifiedName()]
		slots := make([]statusAgentSlot, 0, len(sessions))
		for _, info := range sessions {
			slots = append(slots, statusAgentSlot{
				sessionName: info.sessionName,
				suspended:   info.state == session.StateSuspended,
			})
		}
		return slots
	}

	if !isMultiSession {
		sessionName := agentSessionName(cityName, a.QualifiedName(), sessTmpl)
		info, ok := snapshot.bySessionName[sessionName]
		return []statusAgentSlot{{
			sessionName: sessionName,
			suspended:   ok && info.state == session.StateSuspended,
		}}
	}

	poolMax := 1
	if maxSess != nil && *maxSess > 1 {
		poolMax = *maxSess
	}
	slots := make([]statusAgentSlot, 0, poolMax)
	for i := 1; i <= poolMax; i++ {
		memberName := poolInstanceNameForAPI(a.Name, i, a)
		sessionName := agentSessionName(cityName, a.QualifiedInstanceName(memberName), sessTmpl)
		info, ok := snapshot.bySessionName[sessionName]
		slots = append(slots, statusAgentSlot{
			sessionName: sessionName,
			suspended:   ok && info.state == session.StateSuspended,
		})
	}
	return slots
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
