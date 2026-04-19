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

	body.ScopeKind = strings.TrimSpace(body.ScopeKind)
	body.ScopeRef = strings.TrimSpace(body.ScopeRef)

	cfg := s.state.Config()
	body.Target = qualifySlingTarget(cfg, body.Target, slingRigContext(body))
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
	if body.ScopeKind == "rig" && body.ScopeRef != "" {
		if agentCfg.Dir != body.ScopeRef {
			msg := "scope_ref " + body.ScopeRef + " conflicts with resolved target rig " + agentCfg.Dir
			if agentCfg.Dir == "" {
				msg = "scope_ref " + body.ScopeRef + " requires a rig-scoped target; resolved target " + body.Target + " is city-scoped"
			}
			return nil, huma.Error400BadRequest(msg)
		}
		if body.Rig != "" && body.Rig != body.ScopeRef {
			return nil, huma.Error400BadRequest("rig " + body.Rig + " conflicts with scope_ref " + body.ScopeRef)
		}
	}

	resp, status, code, message, conflict := s.execSling(ctx, body, agentCfg.EffectiveDefaultSlingFormula())
	if code != "" {
		if status == http.StatusNotFound {
			return nil, huma.Error404NotFound(message)
		}
		// Source-workflow conflict: render the rich 409 shape the CLI and
		// dashboard use to offer a "force or clean up" decision. Huma's
		// generic Error4xx collapses everything into Problem Details with
		// only a string detail, so we build the Problem Details error
		// manually with structured extensions.
		if conflict != nil && status == http.StatusConflict {
			storeRef := s.slingStoreRef(body.Rig, agentCfg)
			hint := sourceWorkflowCleanupHint(conflict.SourceBeadID, storeRef)
			return nil, &huma.ErrorModel{
				Status: http.StatusConflict,
				Title:  http.StatusText(http.StatusConflict),
				Detail: message,
				Errors: []*huma.ErrorDetail{
					{Location: "body.source_bead_id", Value: conflict.SourceBeadID},
					{Location: "body.blocking_workflow_ids", Value: conflict.WorkflowIDs},
					{Location: "body.hint", Value: hint},
				},
			}
		}
		return nil, huma.Error400BadRequest(message)
	}

	return &SlingOutput{
		Status: status,
		Body:   *resp,
	}, nil
}
