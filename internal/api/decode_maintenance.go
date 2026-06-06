package api

import (
	"github.com/gastownhall/gascity/internal/api/genclient"
)

// maintenanceRunViewFromGen translates one genclient.MaintenanceRunBody
// into a MaintenanceRunView. Timestamps are passed through verbatim —
// the server already emits RFC3339 UTC strings, so no re-formatting is
// needed on the CLI side.
func maintenanceRunViewFromGen(g genclient.MaintenanceRunBody) MaintenanceRunView {
	out := MaintenanceRunView{
		StartedAt:       g.StartedAt,
		FinishedAt:      g.FinishedAt,
		Stage:           g.Stage,
		BeforeBytes:     g.BeforeBytes,
		AfterBytes:      g.AfterBytes,
		DurationSeconds: g.DurationS,
	}
	if g.Err != nil {
		out.Err = *g.Err
	}
	if g.SnapshotPath != nil {
		out.SnapshotPath = *g.SnapshotPath
	}
	return out
}

// maintenanceStatusViewFromGen translates the genclient status body into
// the CLI-facing MaintenanceStatusView. Returns a zero-value view with an
// empty History slice (never nil) when the body is missing so callers can
// uniformly format the empty case.
func maintenanceStatusViewFromGen(g *genclient.MaintenanceStatusBody) MaintenanceStatusView {
	out := MaintenanceStatusView{History: []MaintenanceRunView{}}
	if g == nil {
		return out
	}
	out.Enabled = g.Enabled
	out.IntervalSec = g.IntervalSeconds
	out.InFlight = g.InFlight
	if g.InFlightStart != nil {
		out.InFlightStart = *g.InFlightStart
	}
	if g.NextScheduled != nil {
		out.NextScheduled = *g.NextScheduled
	}
	if g.LastRun != nil {
		lr := maintenanceRunViewFromGen(*g.LastRun)
		out.LastRun = &lr
	}
	if g.History != nil {
		entries := *g.History
		out.History = make([]MaintenanceRunView, 0, len(entries))
		for _, e := range entries {
			out.History = append(out.History, maintenanceRunViewFromGen(e))
		}
	}
	return out
}

// maintenanceTriggerViewFromGen translates the genclient trigger body into
// MaintenanceTriggerView. nil body is mapped to a zero-value view so
// callers can invoke the wrapper unconditionally even when the server
// returned 202 with no body.
func maintenanceTriggerViewFromGen(g *genclient.MaintenanceTriggerBody) MaintenanceTriggerView {
	if g == nil {
		return MaintenanceTriggerView{}
	}
	out := MaintenanceTriggerView{Accepted: g.Accepted}
	if g.StartedAt != nil {
		out.StartedAt = *g.StartedAt
	}
	if g.Run != nil {
		r := maintenanceRunViewFromGen(*g.Run)
		out.Run = &r
	}
	return out
}
