package api

import "github.com/gastownhall/gascity/internal/api/genclient"

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
