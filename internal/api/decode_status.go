package api

import (
	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/beads"
)

// statusViewFromGen translates the generated StatusBody (served by GET
// /v0/city/{cityName}/status) into the stable CLI-facing StatusView.
// Optional pointer fields are dereferenced safely; missing detail slices
// translate to empty slices (never nil) so the renderer uses uniform empty
// handling without nil checks per-section.
func statusViewFromGen(body *genclient.StatusBody) StatusView {
	if body == nil {
		return StatusView{}
	}
	out := StatusView{
		CityName:  body.Name,
		CityPath:  body.Path,
		UptimeSec: int(body.UptimeSec),
		Suspended: body.Suspended,
		Summary: StatusSummaryView{
			TotalAgents:   int(body.Agents.Total),
			RunningAgents: int(body.Agents.Running),
		},
	}
	if body.Version != nil {
		out.Version = *body.Version
	}
	if body.AgentDetails != nil {
		items := *body.AgentDetails
		out.Agents = make([]StatusAgentView, 0, len(items))
		for _, a := range items {
			out.Agents = append(out.Agents, statusAgentViewFromGen(a))
		}
	} else {
		out.Agents = []StatusAgentView{}
	}
	if body.RigDetails != nil {
		items := *body.RigDetails
		out.Rigs = make([]StatusRigView, 0, len(items))
		for _, r := range items {
			out.Rigs = append(out.Rigs, statusRigViewFromGen(r))
		}
	} else {
		out.Rigs = []StatusRigView{}
	}
	if body.NamedSessionDetails != nil {
		items := *body.NamedSessionDetails
		out.NamedSessions = make([]StatusNamedSessionView, 0, len(items))
		for _, ns := range items {
			out.NamedSessions = append(out.NamedSessions, StatusNamedSessionView{
				Identity: ns.Identity,
				Status:   ns.Status,
				Mode:     ns.Mode,
			})
		}
	} else {
		out.NamedSessions = []StatusNamedSessionView{}
	}
	if body.SessionCountsDetail != nil {
		out.SessionCounts = StatusSessionCountsView{
			Active:    int(body.SessionCountsDetail.Active),
			Suspended: int(body.SessionCountsDetail.Suspended),
		}
	}
	if body.StoreHealth != nil {
		sh := body.StoreHealth
		view := StatusStoreHealthView{
			Path:        sh.Path,
			SizeBytes:   sh.SizeBytes,
			LiveRows:    int(sh.LiveRows),
			RatioMB:     sh.RatioMbPerRow,
			Warning:     sh.Warning,
			ThresholdMB: sh.ThresholdMbPerRow,
		}
		if sh.LastGcAt != nil {
			view.LastGCAt = *sh.LastGcAt
		}
		if sh.LastGcStatus != nil {
			view.LastGCStatus = *sh.LastGcStatus
		}
		out.StoreHealth = &view
	}
	if body.Beads != nil {
		out.Beads = statusBeadsDiagnosticFromGen(body.Beads)
	}
	if body.ConditionalWrites != nil {
		out.ConditionalWrites = statusConditionalWritesFromGen(body.ConditionalWrites)
	}
	return out
}

// statusConditionalWritesFromGen translates the generated conditional-writes
// block back onto the wire struct the server serialized (the CLI renders the
// same shape the dashboard reads).
func statusConditionalWritesFromGen(g *genclient.StatusConditionalWrites) *StatusConditionalWrites {
	if g == nil {
		return nil
	}
	out := &StatusConditionalWrites{
		Mode:      string(g.Mode),
		Origin:    string(g.Origin),
		Effective: string(g.Effective),
	}
	if g.Stores != nil {
		for _, v := range *g.Stores {
			row := StatusConditionalWriteStoreVerdict{
				StoreID: v.StoreId,
				Kind:    v.Kind,
				Probe:   string(v.Probe),
				Latch:   string(v.Latch),
				Capable: v.Capable,
			}
			if v.Reason != nil {
				row.Reason = *v.Reason
			}
			out.Stores = append(out.Stores, row)
		}
	}
	if g.Notices != nil {
		for _, n := range *g.Notices {
			notice := StatusRolloutNotice{
				Kind:    n.Kind,
				FlagKey: n.FlagKey,
				Message: n.Message,
			}
			if n.EnvVar != nil {
				notice.EnvVar = *n.EnvVar
			}
			if n.ConfigValue != nil {
				notice.ConfigValue = *n.ConfigValue
			}
			if n.EnvValue != nil {
				notice.EnvValue = *n.EnvValue
			}
			out.Notices = append(out.Notices, notice)
		}
	}
	return out
}

func statusBeadsDiagnosticFromGen(g *genclient.BeadsDiagnostic) *beads.BeadsDiagnostic {
	if g == nil {
		return nil
	}
	out := &beads.BeadsDiagnostic{
		Store:               g.BeadsStore,
		NativeStoreEligible: g.NativeStoreEligible,
	}
	if g.PreflightGate != nil {
		out.PreflightGate = *g.PreflightGate
	}
	if g.PreflightReason != nil {
		out.PreflightReason = *g.PreflightReason
	}
	return out
}

func statusAgentViewFromGen(g genclient.StatusAgentDetail) StatusAgentView {
	out := StatusAgentView{
		Name:          g.Name,
		QualifiedName: g.QualifiedName,
		Scope:         g.Scope,
		Running:       g.Running,
		Suspended:     g.Suspended,
	}
	if g.SessionName != nil {
		out.SessionName = *g.SessionName
	}
	if g.GroupName != nil {
		out.GroupName = *g.GroupName
	}
	if g.ScaleLabel != nil {
		out.ScaleLabel = *g.ScaleLabel
	}
	if g.Expanded != nil {
		out.Expanded = *g.Expanded
	}
	return out
}

func statusRigViewFromGen(g genclient.StatusRigDetail) StatusRigView {
	return StatusRigView{
		Name:      g.Name,
		Path:      g.Path,
		Suspended: g.Suspended,
	}
}
