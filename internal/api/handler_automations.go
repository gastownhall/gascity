package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/automations"
	"github.com/gastownhall/gascity/internal/beads"
)

type automationResponse struct {
	Name          string `json:"name"`
	ScopedName    string `json:"scoped_name"`
	Description   string `json:"description,omitempty"`
	Type          string `json:"type"`
	Gate          string `json:"gate"`
	Interval      string `json:"interval,omitempty"`
	Schedule      string `json:"schedule,omitempty"`
	Check         string `json:"check,omitempty"`
	On            string `json:"on,omitempty"`
	Formula       string `json:"formula,omitempty"`
	Exec          string `json:"exec,omitempty"`
	Pool          string `json:"pool,omitempty"`
	Timeout       string `json:"timeout,omitempty"`
	TimeoutMs     int64  `json:"timeout_ms"`
	Enabled       bool   `json:"enabled"`
	Rig           string `json:"rig,omitempty"`
	CaptureOutput bool   `json:"capture_output"`
}

func (s *Server) handleAutomationList(w http.ResponseWriter, _ *http.Request) {
	aa := s.state.Automations()
	resp := make([]automationResponse, len(aa))
	for i, a := range aa {
		resp[i] = toAutomationResponse(a)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"automations": resp,
	})
}

func (s *Server) handleAutomationGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	a, err := resolveAutomation(s.state.Automations(), name)
	if err != nil {
		if strings.Contains(err.Error(), "ambiguous") {
			writeError(w, http.StatusConflict, "ambiguous", err.Error())
		} else {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, toAutomationResponse(*a))
}

func (s *Server) handleAutomationEnable(w http.ResponseWriter, r *http.Request) {
	s.setAutomationEnabled(w, r, true)
}

func (s *Server) handleAutomationDisable(w http.ResponseWriter, r *http.Request) {
	s.setAutomationEnabled(w, r, false)
}

func (s *Server) setAutomationEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	sm, ok := s.state.(StateMutator)
	if !ok {
		writeError(w, http.StatusNotImplemented, "internal", "mutations not supported")
		return
	}

	name := r.PathValue("name")

	// Resolve name and rig from the automation list.
	a, err := resolveAutomation(s.state.Automations(), name)
	if err != nil {
		if strings.Contains(err.Error(), "ambiguous") {
			writeError(w, http.StatusConflict, "ambiguous", err.Error())
		} else {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		}
		return
	}
	autoName := a.Name
	autoRig := a.Rig

	if enabled {
		err = sm.EnableAutomation(autoName, autoRig)
	} else {
		err = sm.DisableAutomation(autoName, autoRig)
	}
	if err != nil {
		if strings.Contains(err.Error(), "validating") {
			writeError(w, http.StatusBadRequest, "invalid", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	action := "enabled"
	if !enabled {
		action = "disabled"
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": action, "automation": autoName})
}

// resolveAutomation finds an automation by name or scoped name. If a bare
// name matches multiple automations across rigs, it returns an error
// requiring the caller to use the scoped name instead.
func resolveAutomation(aa []automations.Automation, name string) (*automations.Automation, error) {
	// Scoped name is always unambiguous — try it first.
	for i, a := range aa {
		if a.ScopedName() == name {
			return &aa[i], nil
		}
	}
	// Bare name match — collect all matches to detect ambiguity.
	var matches []int
	for i, a := range aa {
		if a.Name == name {
			matches = append(matches, i)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("automation %s not found", name)
	case 1:
		return &aa[matches[0]], nil
	default:
		var scoped []string
		for _, idx := range matches {
			scoped = append(scoped, aa[idx].ScopedName())
		}
		return nil, fmt.Errorf("ambiguous automation name %q; use scoped name: %s", name, strings.Join(scoped, ", "))
	}
}

func toAutomationResponse(a automations.Automation) automationResponse {
	typ := "formula"
	if a.IsExec() {
		typ = "exec"
	}
	return automationResponse{
		Name:          a.Name,
		ScopedName:    a.ScopedName(),
		Description:   a.Description,
		Type:          typ,
		Gate:          a.Gate,
		Interval:      a.Interval,
		Schedule:      a.Schedule,
		Check:         a.Check,
		On:            a.On,
		Formula:       a.Formula,
		Exec:          a.Exec,
		Pool:          a.Pool,
		Timeout:       a.Timeout,
		TimeoutMs:     a.TimeoutOrDefault().Milliseconds(),
		Enabled:       a.IsEnabled(),
		Rig:           a.Rig,
		CaptureOutput: a.IsExec(), // exec automations capture output
	}
}

// handleAutomationCheck evaluates gate conditions for all automations.
//
//	GET /v0/automations/check
//	Response: { "checks": [{ "name", "scoped_name", "rig", "due", "reason", "last_run", "last_run_outcome" }] }
func (s *Server) handleAutomationCheck(w http.ResponseWriter, _ *http.Request) {
	aa := s.state.Automations()
	if aa == nil {
		writeJSON(w, http.StatusOK, map[string]any{"checks": []any{}})
		return
	}

	store := s.state.CityBeadStore()
	lastRunFn := beadLastRunFunc(store)
	ep := s.state.EventProvider()

	// Build cursor function from bead labels if store is available.
	var cursorFn automations.CursorFunc
	if store != nil {
		cursorFn = func(name string) uint64 {
			label := "automation-run:" + name
			results, err := store.ListByLabel(label, 10)
			if err != nil || len(results) == 0 {
				return 0
			}
			var labelSets [][]string
			for _, b := range results {
				labelSets = append(labelSets, b.Labels)
			}
			return automations.MaxSeqFromLabels(labelSets)
		}
	}

	now := time.Now()
	type checkResponse struct {
		Name           string  `json:"name"`
		ScopedName     string  `json:"scoped_name"`
		Rig            string  `json:"rig,omitempty"`
		Due            bool    `json:"due"`
		Reason         string  `json:"reason"`
		LastRun        *string `json:"last_run,omitempty"`
		LastRunOutcome *string `json:"last_run_outcome,omitempty"`
	}

	checks := make([]checkResponse, 0, len(aa))
	for _, a := range aa {
		result := automations.CheckGate(a, now, lastRunFn, ep, cursorFn)
		cr := checkResponse{
			Name:       a.Name,
			ScopedName: a.ScopedName(),
			Rig:        a.Rig,
			Due:        result.Due,
			Reason:     result.Reason,
		}
		if !result.LastRun.IsZero() {
			ts := result.LastRun.Format(time.RFC3339)
			cr.LastRun = &ts
		}
		// Look up last run outcome from the most recent tracking bead's labels.
		if store != nil {
			label := "automation-run:" + a.ScopedName()
			if results, err := store.ListByLabel(label, 1); err == nil && len(results) > 0 {
				outcome := lastRunOutcomeFromLabels(results[0].Labels)
				if outcome != "" {
					cr.LastRunOutcome = &outcome
				}
			}
		}
		checks = append(checks, cr)
	}

	writeJSON(w, http.StatusOK, map[string]any{"checks": checks})
}

// handleAutomationHistory returns run history for an automation from bead labels.
//
//	GET /v0/automations/history?scoped_name=X&limit=N&before=TIMESTAMP
//	Response: [{ bead_id, name, scoped_name, rig, created_at, labels, duration_ms, exit_code, ... }]
func (s *Server) handleAutomationHistory(w http.ResponseWriter, r *http.Request) {
	store := s.state.CityBeadStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no bead store configured")
		return
	}

	q := r.URL.Query()
	scopedName := q.Get("scoped_name")
	if scopedName == "" {
		writeError(w, http.StatusBadRequest, "invalid", "scoped_name is required")
		return
	}

	limit := 20
	if l := q.Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}

	var beforeTime time.Time
	if b := q.Get("before"); b != "" {
		if t, err := time.Parse(time.RFC3339, b); err == nil {
			beforeTime = t
		}
	}

	// Resolve automation for metadata (rig, name, capture_output).
	aa := s.state.Automations()
	var auto *automations.Automation
	for i, a := range aa {
		if a.ScopedName() == scopedName {
			auto = &aa[i]
			break
		}
	}

	label := "automation-run:" + scopedName
	// Fetch more than limit to allow filtering by before.
	fetchLimit := limit
	if !beforeTime.IsZero() {
		fetchLimit = limit * 3 // over-fetch to account for before filter
	}
	results, err := store.ListByLabel(label, fetchLimit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	type historyEntry struct {
		BeadID        string   `json:"bead_id"`
		Name          string   `json:"name"`
		ScopedName    string   `json:"scoped_name"`
		Rig           string   `json:"rig,omitempty"`
		CreatedAt     string   `json:"created_at"`
		Labels        []string `json:"labels"`
		DurationMs    *string  `json:"duration_ms,omitempty"`
		ExitCode      *string  `json:"exit_code,omitempty"`
		Signal        *string  `json:"signal,omitempty"`
		Error         *string  `json:"error,omitempty"`
		WispRootID    *string  `json:"wisp_root_id,omitempty"`
		CaptureOutput bool     `json:"capture_output"`
		HasOutput     bool     `json:"has_output"`
	}

	entries := make([]historyEntry, 0, len(results))
	for _, b := range results {
		if !beforeTime.IsZero() && !b.CreatedAt.Before(beforeTime) {
			continue
		}

		// Extract automation name from scoped_name.
		name := scopedName
		rig := ""
		if auto != nil {
			name = auto.Name
			rig = auto.Rig
		} else if idx := strings.Index(scopedName, ":rig:"); idx >= 0 {
			name = scopedName[:idx]
			rig = scopedName[idx+5:]
		}

		entry := historyEntry{
			BeadID:        b.ID,
			Name:          name,
			ScopedName:    scopedName,
			Rig:           rig,
			CreatedAt:     b.CreatedAt.Format(time.RFC3339),
			Labels:        b.Labels,
			CaptureOutput: auto != nil && auto.IsExec(),
		}

		// Extract metadata fields if available.
		if b.Metadata != nil {
			if v, ok := b.Metadata["convergence.gate_duration_ms"]; ok && v != "" {
				entry.DurationMs = &v
			}
			if v, ok := b.Metadata["convergence.gate_exit_code"]; ok && v != "" {
				entry.ExitCode = &v
			}
		}

		// Determine has_output: exec automations with capture always have potential output.
		entry.HasOutput = entry.CaptureOutput

		entries = append(entries, entry)
		if len(entries) >= limit {
			break
		}
	}

	writeJSON(w, http.StatusOK, entries)
}

// handleAutomationHistoryDetail returns full output for a single automation run.
//
//	GET /v0/automation/history/{bead_id}
//	Response: { bead_id, name, scoped_name, ..., output }
func (s *Server) handleAutomationHistoryDetail(w http.ResponseWriter, r *http.Request) {
	store := s.state.CityBeadStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no bead store configured")
		return
	}

	beadID := r.PathValue("bead_id")
	b, err := store.Get(beadID)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "not_found", "bead not found")
		} else {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
		}
		return
	}

	// Extract output from metadata.
	output := ""
	if b.Metadata != nil {
		if stdout := b.Metadata["convergence.gate_stdout"]; stdout != "" {
			output = stdout
		}
		if stderr := b.Metadata["convergence.gate_stderr"]; stderr != "" {
			if output != "" {
				output += "\n"
			}
			output += stderr
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"bead_id":    b.ID,
		"created_at": b.CreatedAt.Format(time.RFC3339),
		"labels":     b.Labels,
		"output":     output,
	})
}

// beadLastRunFunc returns a LastRunFunc that queries the bead store for the most
// recent bead labeled automation-run:<name>.
func beadLastRunFunc(store beads.Store) automations.LastRunFunc {
	return func(name string) (time.Time, error) {
		if store == nil {
			return time.Time{}, nil
		}
		label := "automation-run:" + name
		results, err := store.ListByLabel(label, 1)
		if err != nil {
			return time.Time{}, err
		}
		if len(results) == 0 {
			return time.Time{}, nil
		}
		return results[0].CreatedAt, nil
	}
}

// lastRunOutcomeFromLabels extracts the run outcome from bead labels.
func lastRunOutcomeFromLabels(labels []string) string {
	for _, l := range labels {
		switch l {
		case "exec":
			return "success"
		case "exec-failed":
			return "failed"
		case "wisp":
			return "success"
		case "wisp-canceled":
			return "canceled"
		}
	}
	return ""
}
