package api

import (
	"fmt"
	"net/http"
	"time"
)

type agentCounts struct {
	Total       int `json:"total"`
	Running     int `json:"running"`
	Suspended   int `json:"suspended"`
	Quarantined int `json:"quarantined"`
}

type rigCounts struct {
	Total     int `json:"total"`
	Suspended int `json:"suspended"`
}

type workCounts struct {
	InProgress int `json:"in_progress"`
	Ready      int `json:"ready"`
	Open       int `json:"open"`
}

type mailCounts struct {
	Unread int `json:"unread"`
	Total  int `json:"total"`
}

type statusResponse struct {
	Name       string      `json:"name"`
	Path       string      `json:"path"`
	Version    string      `json:"version,omitempty"`
	UptimeSec  int         `json:"uptime_sec"`
	Suspended  bool        `json:"suspended"`
	AgentCount int         `json:"agent_count"`
	RigCount   int         `json:"rig_count"`
	Running    int         `json:"running"`
	Agents     agentCounts `json:"agents"`
	Rigs       rigCounts   `json:"rigs"`
	Work       workCounts  `json:"work"`
	Mail       mailCounts  `json:"mail"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	bp := parseBlockingParams(r)
	if bp.isBlocking() {
		waitForChange(r.Context(), s.state.EventProvider(), bp)
	}
	index := s.latestIndex()
	cacheKey := responseCacheKey("status", r)
	if body, ok := s.cachedResponse(cacheKey, index); ok {
		writeCachedJSON(w, r, index, body)
		return
	}

	cfg := s.state.Config()
	sp := s.state.SessionProvider()
	cityName := s.state.CityName()
	sessTmpl := cfg.Workspace.SessionTemplate

	// Count agents by state. The top-level Running field preserves backward
	// compatibility (raw process count), while Agents.* uses the mutually
	// exclusive state priority chain from computeAgentState.
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

	// Count work items (best-effort). Deduplicate stores that may be
	// shared across rigs (e.g., when using the "file" bead provider).
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
		list, err := store.ListOpen()
		if err != nil {
			continue
		}
		for _, b := range list {
			// Only count work beads (tasks, molecules). Skip mail,
			// convoys, convergence, and other non-work bead types.
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

	// Count mail (best-effort). Deduplicate shared providers.
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

	resp := statusResponse{
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
	body, err := s.storeResponse(cacheKey, index, resp)
	if err != nil {
		writeIndexJSON(w, index, resp)
		return
	}
	writeCachedJSON(w, r, index, body)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	uptime := int(time.Since(s.state.StartedAt()).Seconds())
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "ok",
		"version":    s.state.Version(),
		"city":       s.state.CityName(),
		"uptime_sec": uptime,
	})
}
