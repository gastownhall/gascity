package api

import (
	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/beads"
)

// ConvoyStatusView is the CLI-facing shape for `gc convoy status`. It mirrors
// the convoy detail payload (the convoy bead itself, its direct children, and
// progress counts) so cmd/gc/ never imports genclient directly.
type ConvoyStatusView struct {
	Convoy   beads.Bead
	Children []beads.Bead
	Progress ConvoyProgressView
}

// ConvoyProgressView is the aggregate child count surface used by status and
// list rendering.
type ConvoyProgressView struct {
	Total  int
	Closed int
}

// ConvoyCheckView is the CLI-facing shape for `gc convoy check` per-convoy
// completion evaluation.
type ConvoyCheckView struct {
	ConvoyID string
	Total    int
	Closed   int
	Complete bool
}

// beadFromGen translates one genclient.Bead into the internal beads.Bead used
// by CLI render helpers. Optional pointer fields are dereferenced safely.
func beadFromGen(g genclient.Bead) beads.Bead {
	out := beads.Bead{
		ID:        g.Id,
		Title:     g.Title,
		Status:    g.Status,
		Type:      g.IssueType,
		CreatedAt: g.CreatedAt,
	}
	if g.UpdatedAt != nil {
		out.UpdatedAt = *g.UpdatedAt
	}
	if g.DeferUntil != nil {
		deferUntil := *g.DeferUntil
		out.DeferUntil = &deferUntil
	}
	if g.Ephemeral != nil {
		out.Ephemeral = *g.Ephemeral
	}
	if g.Priority != nil {
		p := int(*g.Priority)
		out.Priority = &p
	}
	if g.Assignee != nil {
		out.Assignee = *g.Assignee
	}
	if g.From != nil {
		out.From = *g.From
	}
	if g.Parent != nil {
		out.ParentID = *g.Parent
	}
	if g.Ref != nil {
		out.Ref = *g.Ref
	}
	if g.Description != nil {
		out.Description = *g.Description
	}
	if g.Needs != nil {
		out.Needs = append([]string(nil), *g.Needs...)
	}
	if g.Labels != nil {
		out.Labels = append([]string(nil), *g.Labels...)
	}
	if g.Metadata != nil {
		out.Metadata = make(map[string]string, len(*g.Metadata))
		for k, v := range *g.Metadata {
			out.Metadata[k] = v
		}
	}
	if g.Dependencies != nil {
		out.Dependencies = make([]beads.Dep, 0, len(*g.Dependencies))
		for _, d := range *g.Dependencies {
			out.Dependencies = append(out.Dependencies, beads.Dep{
				IssueID:     d.IssueId,
				DependsOnID: d.DependsOnId,
				Type:        d.Type,
			})
		}
	}
	return out
}

// convoysFromGenList translates the genclient list body into []beads.Bead.
// Returns an empty slice (never nil) when the body is missing or holds no
// items so callers can uniformly format the empty case.
func convoysFromGenList(body *genclient.ListBodyBead) []beads.Bead {
	if body == nil || body.Items == nil {
		return []beads.Bead{}
	}
	items := *body.Items
	out := make([]beads.Bead, 0, len(items))
	for _, item := range items {
		out = append(out, beadFromGen(item))
	}
	return out
}

// convoyStatusFromGen translates the genclient convoy-get response into a
// ConvoyStatusView. When the response payload is missing a convoy body (e.g.
// graph/workflow branch), the returned ConvoyView's Convoy.ID is empty —
// callers must check and fall back.
func convoyStatusFromGen(g *genclient.ConvoyGetResponse) ConvoyStatusView {
	out := ConvoyStatusView{Children: []beads.Bead{}}
	if g == nil {
		return out
	}
	if g.Convoy != nil {
		out.Convoy = beadFromGen(*g.Convoy)
	}
	if g.Children != nil {
		for _, c := range *g.Children {
			out.Children = append(out.Children, beadFromGen(c))
		}
	}
	if g.Progress != nil {
		out.Progress = ConvoyProgressView{
			Total:  int(g.Progress.Total),
			Closed: int(g.Progress.Closed),
		}
	}
	return out
}

// convoyCheckFromGen translates the genclient check response into a
// ConvoyCheckView.
func convoyCheckFromGen(g *genclient.ConvoyCheckResponse) ConvoyCheckView {
	if g == nil {
		return ConvoyCheckView{}
	}
	return ConvoyCheckView{
		ConvoyID: g.ConvoyId,
		Total:    int(g.Total),
		Closed:   int(g.Closed),
		Complete: g.Complete,
	}
}
