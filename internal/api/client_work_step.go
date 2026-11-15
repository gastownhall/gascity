package api

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/gastownhall/gascity/internal/api/genclient"
)

// ActiveWorkStep asks the controller for a session's step reminder via
// GET /v0/city/{cityName}/work/active-step: the bead the in-process read would
// name for assignees (tried in order) over the work store at scopeRoot (the
// city when empty). It returns nil with a nil error when there is no current
// step.
//
// ctx bounds the call: a hook has a cheaper answer than waiting, its own read,
// and stops sooner than the client's ceiling.
func (c *Client) ActiveWorkStep(ctx context.Context, assignees []string, scopeRoot string) (*ActiveWorkStep, error) {
	if err := c.requireCityScope(); err != nil {
		return nil, err
	}
	if len(assignees) == 0 {
		return nil, errors.New("invalid active-step query: no assignee")
	}
	params := &genclient.GetV0CityByCityNameWorkActiveStepParams{Assignee: slices.Clone(assignees)}
	if scopeRoot != "" {
		params.ScopeRoot = &scopeRoot
	}
	resp, err := c.cw.GetV0CityByCityNameWorkActiveStepWithResponse(ctx, c.cityName, params)
	if err != nil {
		return nil, &connError{err: fmt.Errorf("request failed: %w", err)}
	}
	if resp == nil {
		return nil, &connError{err: fmt.Errorf("nil response")}
	}
	if err := apiErrorFromResponse(resp.StatusCode(), pdOf(resp)); err != nil {
		return nil, err
	}
	if resp.JSON200 == nil {
		return nil, fmt.Errorf("API returned %d with no active-step body", resp.StatusCode())
	}
	step := resp.JSON200.Step
	if step == nil {
		return nil, nil
	}
	return &ActiveWorkStep{ID: step.Id, Title: step.Title, Description: step.Description}, nil
}
