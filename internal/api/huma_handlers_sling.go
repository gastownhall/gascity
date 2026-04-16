package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
)

// SlingOutput is the Huma response for POST /v0/sling.
// The HTTP status code varies (200 for direct sling, 201 for workflow launch),
// so we use a custom status field.
type SlingOutput struct {
	Status int `header:"_status" doc:"HTTP status code."`
	Body   slingResponse
}

// humaHandleSling is the Huma-typed handler for POST /v0/sling.
func (s *Server) humaHandleSling(ctx context.Context, input *SlingInput) (*SlingOutput, error) {
	body := slingBody{
		Rig:            input.Body.Rig,
		Target:         input.Body.Target,
		Bead:           input.Body.Bead,
		Formula:        input.Body.Formula,
		AttachedBeadID: input.Body.AttachedBeadID,
		Title:          input.Body.Title,
		Vars:           input.Body.Vars,
		ScopeKind:      input.Body.ScopeKind,
		ScopeRef:       input.Body.ScopeRef,
	}

	if body.Target == "" {
		return nil, huma.Error400BadRequest("target agent or pool is required")
	}

	cfg := s.state.Config()
	agentCfg, ok := findAgent(cfg, body.Target)
	if !ok {
		return nil, huma.Error404NotFound("target " + body.Target + " not found")
	}

	if body.Bead == "" && body.Formula == "" {
		return nil, huma.Error400BadRequest("bead or formula is required")
	}
	if body.Bead != "" && body.Formula != "" {
		return nil, huma.Error400BadRequest("bead and formula are mutually exclusive")
	}
	if body.Bead != "" && body.AttachedBeadID != "" {
		return nil, huma.Error400BadRequest("bead and attached_bead_id are mutually exclusive")
	}

	body.ScopeKind = strings.TrimSpace(body.ScopeKind)
	body.ScopeRef = strings.TrimSpace(body.ScopeRef)
	workflowLaunchOptions := body.AttachedBeadID != "" ||
		len(body.Vars) > 0 ||
		body.Title != "" ||
		body.ScopeKind != "" ||
		body.ScopeRef != ""
	defaultFormulaLaunch := body.Formula == "" &&
		body.AttachedBeadID == "" &&
		body.Bead != "" &&
		agentCfg.EffectiveDefaultSlingFormula() != "" &&
		(len(body.Vars) > 0 || body.Title != "" || body.ScopeKind != "" || body.ScopeRef != "")
	if body.Formula == "" && body.AttachedBeadID != "" {
		return nil, huma.Error400BadRequest("formula is required when attached_bead_id is provided")
	}
	if body.Formula == "" && workflowLaunchOptions && !defaultFormulaLaunch {
		return nil, huma.Error400BadRequest("formula or target default formula is required when vars, title, or scope are provided")
	}
	if (body.ScopeKind == "") != (body.ScopeRef == "") {
		return nil, huma.Error400BadRequest("scope_kind and scope_ref must be provided together")
	}
	if body.ScopeKind != "" && body.ScopeKind != "city" && body.ScopeKind != "rig" {
		return nil, huma.Error400BadRequest("scope_kind must be 'city' or 'rig'")
	}

	resp, status, code, message := s.execSling(ctx, body, agentCfg.EffectiveDefaultSlingFormula())
	if code != "" {
		if status == http.StatusNotFound {
			return nil, huma.Error404NotFound(message)
		}
		return nil, huma.Error400BadRequest(message)
	}

	return &SlingOutput{
		Status: status,
		Body:   *resp,
	}, nil
}
