package api

import (
	"context"
	"fmt"
	"slices"

	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/readywork"
)

// ReadyWork asks the controller the `gc ready` question via
// GET /v0/city/{cityName}/work/ready and returns its rows exactly as the
// in-process reader would produce them: the same rows, in the same order,
// carrying the same fields.
//
// A query the in-process reader refuses, such as a negative limit, is refused
// here too, before anything is sent. A limit of zero is unlimited and is not
// sent. ctx bounds the call: the client's own ceiling is sized for slow
// federated control-plane reads, and a caller with a cheaper fallback should
// stop waiting sooner.
func (c *Client) ReadyWork(ctx context.Context, q readywork.Query) ([]readywork.ReadyBead, error) {
	if err := c.requireCityScope(); err != nil {
		return nil, err
	}
	if err := q.Validate(); err != nil {
		return nil, fmt.Errorf("invalid ready-work query: %w", err)
	}
	params := &genclient.GetV0CityByCityNameWorkReadyParams{}
	if q.Assignee != "" {
		params.Assignee = &q.Assignee
	}
	if q.Unassigned {
		params.Unassigned = &q.Unassigned
	}
	if len(q.MetadataFields) > 0 {
		fields := slices.Clone(q.MetadataFields)
		params.MetadataField = &fields
	}
	if len(q.ExcludeTypes) > 0 {
		types := slices.Clone(q.ExcludeTypes)
		params.ExcludeType = &types
	}
	if len(q.ExcludeLabels) > 0 {
		labels := slices.Clone(q.ExcludeLabels)
		params.ExcludeLabel = &labels
	}
	if q.Sort != "" {
		order := genclient.GetV0CityByCityNameWorkReadyParamsSort(q.Sort)
		params.Sort = &order
	}
	if q.Limit > 0 {
		limit := int64(q.Limit)
		params.Limit = &limit
	}
	if q.Status != "" {
		status := genclient.GetV0CityByCityNameWorkReadyParamsStatus(q.Status)
		params.Status = &status
	}
	resp, err := c.cw.GetV0CityByCityNameWorkReadyWithResponse(ctx, c.cityName, params)
	if err != nil {
		return nil, &connError{err: fmt.Errorf("request failed: %w", err)}
	}
	if resp == nil {
		return nil, &connError{err: fmt.Errorf("nil response")}
	}
	if err := apiErrorFromResponse(resp.StatusCode(), pdOf(resp)); err != nil {
		return nil, err
	}
	if resp.JSON200 == nil || resp.JSON200.Items == nil {
		return nil, fmt.Errorf("API returned %d with no ready-work items", resp.StatusCode())
	}
	rows := make([]readywork.ReadyBead, 0, len(*resp.JSON200.Items))
	for _, g := range *resp.JSON200.Items {
		rows = append(rows, readyBeadFromGen(g))
	}
	return rows, nil
}

// readyBeadFromGen converts one generated wire row back to the reader's row.
// Every optional field keeps its absent-versus-present distinction, so the row
// marshals to exactly the bytes the server's row did.
func readyBeadFromGen(g genclient.ReadyBead) readywork.ReadyBead {
	row := readywork.ReadyBead{
		ID:          g.Id,
		Title:       g.Title,
		Status:      g.Status,
		Type:        g.IssueType,
		CreatedAt:   g.CreatedAt,
		Assignee:    derefStr(g.Assignee),
		From:        derefStr(g.From),
		ParentID:    derefStr(g.Parent),
		Ref:         derefStr(g.Ref),
		Description: derefStr(g.Description),
		CloseReason: derefStr(g.CloseReason),
		DeferUntil:  g.DeferUntil,
		IsBlocked:   g.IsBlocked,
	}
	if g.Priority != nil {
		p := int(*g.Priority)
		row.Priority = &p
	}
	if g.UpdatedAt != nil {
		row.UpdatedAt = *g.UpdatedAt
	}
	if g.Needs != nil {
		row.Needs = *g.Needs
	}
	if g.Labels != nil {
		row.Labels = *g.Labels
	}
	if g.Metadata != nil {
		row.Metadata = *g.Metadata
	}
	if g.Ephemeral != nil {
		row.Ephemeral = *g.Ephemeral
	}
	if g.NoHistory != nil {
		row.NoHistory = *g.NoHistory
	}
	if g.Dependencies != nil {
		row.Dependencies = make([]readywork.ReadyBeadDep, 0, len(*g.Dependencies))
		for _, d := range *g.Dependencies {
			row.Dependencies = append(row.Dependencies, readywork.ReadyBeadDep{IssueID: d.IssueId, DependsOnID: d.DependsOnId, Type: d.Type})
		}
	}
	if g.BlockedBy != nil {
		row.BlockedBy = make([]readywork.ReadyBeadBlocker, 0, len(*g.BlockedBy))
		for _, b := range *g.BlockedBy {
			blocker := readywork.ReadyBeadBlocker{ID: b.Id, Status: b.Status}
			if b.Reason != nil {
				blocker.Reason = *b.Reason
			}
			row.BlockedBy = append(row.BlockedBy, blocker)
		}
	}
	return row
}
