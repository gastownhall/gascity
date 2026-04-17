package api

import (
	"context"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// statusResponse is the JSON body for GET /v0/status.
// TODO(huma): replace with StatusBody once migration is complete.
type statusResponse = StatusBody

type agentCounts = StatusAgentCounts
type rigCounts = StatusRigCounts
type workCounts = StatusWorkCounts
type mailCounts = StatusMailCounts

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

	// Count agents by state.
	var ac agentCounts
	var rawRunning int
	for _, a := range cfg.Agents {
		for _, ea := range expandAgent(a, cityName, sessTmpl, sp) {
			ac.Total++
			sessName := agentSessionName(cityName, ea.qualifiedName, sessTmpl)
			running := sp.IsRunning(sessName)
			if running {
				rawRunning++
			}
			suspended := ea.suspended
			if v, err := sp.GetMeta(sessName, "suspended"); err == nil && v == "true" {
				suspended = true
			}
			switch {
			case suspended:
				ac.Suspended++
			case s.state.IsQuarantined(sessName):
				ac.Quarantined++
			case running:
				ac.Running++
			}
		}
	}

	// Count rigs by state.
	rc := rigCounts{Total: len(cfg.Rigs)}
	for _, rig := range cfg.Rigs {
		if rigSuspended(cfg, rig, sp, cityName, s.state.CityPath()) {
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

	uptime := int(time.Since(s.state.StartedAt()).Seconds())

	return StatusBody{
		Name:       cityName,
		Path:       s.state.CityPath(),
		Version:    s.state.Version(),
		UptimeSec:  uptime,
		Suspended:  cfg.Workspace.Suspended,
		AgentCount: ac.Total,
		RigCount:   rc.Total,
		Running:    rawRunning,
		Agents:     ac,
		Rigs:       rc,
		Work:       wc,
		Mail:       mc,
	}
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
