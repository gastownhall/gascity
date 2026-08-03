package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/api/dashboardbff"
)

// SlingOutput is the Huma response for POST /v0/sling.
// The HTTP status code is supplied by the domain sling result.
type SlingOutput struct {
	Status   int    `header:"_status" doc:"HTTP status code."`
	Location string `header:"Location" doc:"Canonical Run resource URL: the specific run when a graph workflow was launched, otherwise the runs list."`
	Body     slingResponse
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
		Force:          input.Body.Force,
		Reassign:       input.Body.Reassign,
		Merge:          input.Body.Merge,
		NoConvoy:       input.Body.NoConvoy,
		Owned:          input.Body.Owned,
		NoFormula:      input.Body.NoFormula,
	}

	if body.Target == "" {
		return nil, apierr.InvalidRequest.Msg("target agent or pool is required")
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
		return nil, apierr.InvalidRequest.Msg("bead or formula is required")
	}
	if body.Bead != "" && body.Formula != "" {
		return nil, apierr.InvalidRequest.Msg("bead and formula are mutually exclusive")
	}
	if body.Bead != "" && body.AttachedBeadID != "" {
		return nil, apierr.InvalidRequest.Msg("bead and attached_bead_id are mutually exclusive")
	}

	workflowLaunchOptions := body.AttachedBeadID != "" ||
		len(body.Vars) > 0 ||
		body.Title != "" ||
		body.ScopeKind != "" ||
		body.ScopeRef != ""
	defaultFormulaLaunch := body.Formula == "" &&
		body.AttachedBeadID == "" &&
		body.Bead != "" &&
		!body.NoFormula &&
		agentCfg.EffectiveDefaultSlingFormula() != "" &&
		(len(body.Vars) > 0 || body.Title != "" || body.ScopeKind != "" || body.ScopeRef != "")
	if body.Formula == "" && body.AttachedBeadID != "" {
		return nil, apierr.InvalidRequest.Msg("formula is required when attached_bead_id is provided")
	}
	if body.Formula == "" && workflowLaunchOptions && !defaultFormulaLaunch {
		return nil, apierr.InvalidRequest.Msg("formula or target default formula is required when vars, title, or scope are provided")
	}
	if (body.ScopeKind == "") != (body.ScopeRef == "") {
		return nil, apierr.InvalidRequest.Msg("scope_kind and scope_ref must be provided together")
	}
	if body.ScopeKind != "" && body.ScopeKind != "city" && body.ScopeKind != "rig" {
		return nil, apierr.InvalidRequest.Msg("scope_kind must be 'city' or 'rig'")
	}
	if body.Owned && body.NoConvoy {
		return nil, huma.Error400BadRequest("owned requires a convoy (cannot use with no_convoy)")
	}
	if body.Merge != "" && body.Merge != "direct" && body.Merge != "mr" && body.Merge != "local" {
		return nil, huma.Error400BadRequest("merge must be 'direct', 'mr', or 'local'")
	}
	if body.NoFormula && (body.Formula != "" || body.AttachedBeadID != "") {
		return nil, huma.Error400BadRequest("no_formula conflicts with formula/attached_bead_id")
	}
	if body.ScopeKind == "rig" && body.ScopeRef != "" {
		if agentCfg.Dir != body.ScopeRef {
			msg := "scope_ref " + body.ScopeRef + " conflicts with resolved target rig " + agentCfg.Dir
			if agentCfg.Dir == "" {
				msg = "scope_ref " + body.ScopeRef + " requires a rig-scoped target; resolved target " + body.Target + " is city-scoped"
			}
			return nil, apierr.InvalidRequest.Msg(msg)
		}
		if body.Rig != "" && body.Rig != body.ScopeRef {
			return nil, apierr.InvalidRequest.Msg("rig " + body.Rig + " conflicts with scope_ref " + body.ScopeRef)
		}
	}

	resp, status, code, message, conflict := s.execSling(ctx, body, agentCfg.EffectiveDefaultSlingFormula())
	if code != "" {
		if status == http.StatusNotFound {
			return nil, huma.Error404NotFound(message)
		}
		// Source-workflow conflict: render the rich 409 shape the CLI and
		// dashboard use to offer a "force or clean up" decision. The structured
		// Errors[] entries (source_bead_id, blocking_workflow_ids, hint) are the
		// wire contract those clients read; the catalog constructor preserves
		// them and adds the stable type/code.
		if conflict != nil && status == http.StatusConflict {
			storeRef := s.slingStoreRef(body.Rig, agentCfg, slingStoreBeadID(body))
			hint := sourceWorkflowCleanupHint(conflict.SourceBeadID, storeRef)
			return nil, apierr.SlingSourceWorkflowConflict.With(message,
				&huma.ErrorDetail{Location: "body.source_bead_id", Value: conflict.SourceBeadID},
				&huma.ErrorDetail{Location: "body.blocking_workflow_ids", Value: conflict.WorkflowIDs},
				&huma.ErrorDetail{Location: "body.hint", Value: hint},
			)
		}
		if status >= http.StatusInternalServerError {
			return nil, apierr.Internal.Msg(message)
		}
		if code == "missing_bead" {
			return nil, apierr.SlingMissingBead.Msg(message)
		}
		if code == "cross_rig" {
			return nil, apierr.SlingCrossRig.Msg(message)
		}
		if code == "cross_store" {
			return nil, apierr.SlingCrossStoreRoute.Msg(message)
		}
		return nil, apierr.InvalidRequest.Msg(message)
	}

	// Successful sling: surface a dashboard deep link when this process also
	// hosts the dashboard. This endpoint never produces batch shapes (no
	// DoSlingBatch call), so resp.WorkflowID alone discriminates the single
	// graph-workflow launch (run detail) from every other successful shape
	// (wisps, plain bead routes, idempotent skips → runs list), matching the
	// CLI's link policy.
	resp.DashboardURL = s.slingDashboardURL(input.CityName, resp.WorkflowID)

	// Point the caller at the canonical Run resource. A graph-workflow launch has
	// an addressable run root (resp.WorkflowID); every other successful shape
	// (wisps, plain bead routes, idempotent skips) has no single run, so its
	// Location is the runs list — matching resp.DashboardURL's own discriminator.
	location := runsListPath(input.CityName)
	if resp.WorkflowID != "" {
		location = runResourcePath(input.CityName, resp.WorkflowID)
		resp.Run = &RunRef{RunID: resp.WorkflowID, Kind: RunKindSling, Status: RunStatusPending}
	}
	return &SlingOutput{
		Status:   status,
		Location: location,
		Body:     *resp,
	}, nil
}

// slingDashboardURL returns the dashboard deep link surfaced on a successful
// sling response, or "" when no link should be emitted. The dashboard SPA is
// mounted only on the supervisor listener (same-origin with this /v0 API);
// the standalone controller's [api] port serves /v0 without the SPA, so the
// link resolves only when the serving process installed a base via
// SupervisorMux.WithDashboardBase. Any resolution failure degrades silently
// to no link — the link is a convenience and must never fail the sling.
//
// cityName is the cityName path parameter (on the supervisor it is the
// registry name the dashboard routes by); a name outside the BFF grammar is
// dashboard-unreachable, so no link is minted for it. A non-empty workflowID
// (a graph.v2 run root) links to that run's detail view; every other
// successful shape links to the runs list.
func (s *Server) slingDashboardURL(cityName, workflowID string) string {
	if s.dashboardBase == nil {
		return ""
	}
	base := strings.TrimRight(strings.TrimSpace(s.dashboardBase()), "/")
	if base == "" {
		return ""
	}
	if !dashboardbff.ValidCityName(cityName) {
		return ""
	}
	if workflowID != "" {
		return base + dashboardbff.RunDetailPath(cityName, workflowID)
	}
	return base + dashboardbff.RunsListPath(cityName)
}
