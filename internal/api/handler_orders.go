package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/orders"
)

type orderResponse struct {
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

func (s *Server) handleOrderList(w http.ResponseWriter, _ *http.Request) {
	aa := s.state.Orders()
	resp := make([]orderResponse, len(aa))
	for i, a := range aa {
		resp[i] = toOrderResponse(a)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"orders": resp,
	})
}

func (s *Server) handleOrderGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	a, err := resolveOrder(s.state.Orders(), name)
	if err != nil {
		if strings.Contains(err.Error(), "ambiguous") {
			writeError(w, http.StatusConflict, "ambiguous", err.Error())
		} else {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, toOrderResponse(*a))
}

func (s *Server) handleOrderEnable(w http.ResponseWriter, r *http.Request) {
	s.setOrderEnabled(w, r, true)
}

func (s *Server) handleOrderDisable(w http.ResponseWriter, r *http.Request) {
	s.setOrderEnabled(w, r, false)
}

func (s *Server) setOrderEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	sm, ok := s.state.(StateMutator)
	if !ok {
		writeError(w, http.StatusNotImplemented, "internal", "mutations not supported")
		return
	}

	name := r.PathValue("name")

	// Resolve name and rig from the order list.
	a, err := resolveOrder(s.state.Orders(), name)
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
		err = sm.EnableOrder(autoName, autoRig)
	} else {
		err = sm.DisableOrder(autoName, autoRig)
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
	writeJSON(w, http.StatusOK, map[string]string{"status": action, "order": autoName})
}

// resolveOrder finds an order by name or scoped name. If a bare
// name matches multiple orders across rigs, it returns an error
// requiring the caller to use the scoped name instead.
func resolveOrder(aa []orders.Order, name string) (*orders.Order, error) {
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
		return nil, fmt.Errorf("order %s not found", name)
	case 1:
		return &aa[matches[0]], nil
	default:
		var scoped []string
		for _, idx := range matches {
			scoped = append(scoped, aa[idx].ScopedName())
		}
		return nil, fmt.Errorf("ambiguous order name %q; use scoped name: %s", name, strings.Join(scoped, ", "))
	}
}

func toOrderResponse(a orders.Order) orderResponse {
	typ := "formula"
	if a.IsExec() {
		typ = "exec"
	}
	return orderResponse{
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
		CaptureOutput: a.IsExec(), // exec orders capture output
	}
}

// handleOrderCheck evaluates gate conditions for all orders.
//
//	GET /v0/orders/check
//	Response: { "checks": [{ "name", "scoped_name", "rig", "due", "reason", "last_run", "last_run_outcome" }] }
func (s *Server) handleOrderCheck(w http.ResponseWriter, _ *http.Request) {
	aa := s.state.Orders()
	if aa == nil {
		writeJSON(w, http.StatusOK, map[string]any{"checks": []any{}})
		return
	}

	store := s.state.CityBeadStore()
	lastRunFn := beadLastRunFunc(store)
	ep := s.state.EventProvider()

	// Build cursor function from bead labels if store is available.
	var cursorFn orders.CursorFunc
	if store != nil {
		cursorFn = func(name string) uint64 {
			label := "order-run:" + name
			results, err := store.ListByLabel(label, 10, beads.IncludeClosed)
			if err != nil || len(results) == 0 {
				return 0
			}
			var labelSets [][]string
			for _, b := range results {
				labelSets = append(labelSets, b.Labels)
			}
			return orders.MaxSeqFromLabels(labelSets)
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
		result := orders.CheckGate(a, now, lastRunFn, ep, cursorFn)
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
			label := "order-run:" + a.ScopedName()
			if results, err := store.ListByLabel(label, 1, beads.IncludeClosed); err == nil && len(results) > 0 {
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

// handleOrderHistory returns run history for an order from bead labels.
//
//	GET /v0/orders/history?scoped_name=X&limit=N&before=TIMESTAMP
//	Response: [{ bead_id, name, scoped_name, rig, created_at, labels, duration_ms, exit_code, ... }]
func (s *Server) handleOrderHistory(w http.ResponseWriter, r *http.Request) {
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

	// Resolve order for metadata (rig, name, capture_output).
	aa := s.state.Orders()
	var auto *orders.Order
	for i, a := range aa {
		if a.ScopedName() == scopedName {
			auto = &aa[i]
			break
		}
	}

	label := "order-run:" + scopedName
	// Fetch more than limit to allow filtering by before.
	fetchLimit := limit
	if !beforeTime.IsZero() {
		fetchLimit = limit * 3 // over-fetch to account for before filter
	}
	results, err := store.ListByLabel(label, fetchLimit, beads.IncludeClosed)
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

		// Extract order name from scoped_name.
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

		// Determine has_output: exec orders with capture always have potential output.
		entry.HasOutput = entry.CaptureOutput

		entries = append(entries, entry)
		if len(entries) >= limit {
			break
		}
	}

	writeJSON(w, http.StatusOK, entries)
}

// handleOrderHistoryDetail returns full output for a single order run.
//
//	GET /v0/order/history/{bead_id}
//	Response: { bead_id, name, scoped_name, ..., output }
func (s *Server) handleOrderHistoryDetail(w http.ResponseWriter, r *http.Request) {
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
// recent bead labeled order-run:<name>.
func beadLastRunFunc(store beads.Store) orders.LastRunFunc {
	return func(name string) (time.Time, error) {
		if store == nil {
			return time.Time{}, nil
		}
		label := "order-run:" + name
		results, err := store.ListByLabel(label, 1, beads.IncludeClosed)
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
	switch {
	case containsString(labels, "exec-failed"), containsString(labels, "wisp-failed"):
		return "failed"
	case containsString(labels, "wisp-canceled"):
		return "canceled"
	case containsString(labels, "exec"), containsString(labels, "wisp"):
		return "success"
	default:
		return ""
	}
}
