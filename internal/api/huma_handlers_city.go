package api

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

// humaHandleCityGet is the Huma-typed handler for GET /v0/city.
func (s *Server) humaHandleCityGet(_ context.Context, _ *CityGetInput) (*struct{ Body cityGetResponse }, error) {
	cfg := s.state.Config()
	resp := cityGetResponse{
		Name:            s.state.CityName(),
		Path:            s.state.CityPath(),
		Version:         s.state.Version(),
		Suspended:       cfg.Workspace.Suspended,
		Provider:        cfg.Workspace.Provider,
		SessionTemplate: cfg.Workspace.SessionTemplate,
		UptimeSec:       int(time.Since(s.state.StartedAt()).Seconds()),
		AgentCount:      len(cfg.Agents),
		RigCount:        len(cfg.Rigs),
	}
	return &struct{ Body cityGetResponse }{Body: resp}, nil
}

// humaHandleCityPatch is the Huma-typed handler for PATCH /v0/city.
func (s *Server) humaHandleCityPatch(_ context.Context, input *CityPatchInput) (*OKResponse, error) {
	sm, ok := s.state.(StateMutator)
	if !ok {
		return nil, errMutationsNotSupported
	}

	if input.Body.Suspended == nil {
		return nil, huma.Error400BadRequest("no fields to update")
	}

	var err error
	if *input.Body.Suspended {
		err = sm.SuspendCity()
	} else {
		err = sm.ResumeCity()
	}
	if err != nil {
		return nil, mutationError(err)
	}

	resp := &OKResponse{}
	resp.Body.Status = "ok"
	return resp, nil
}

// humaHandleProviderReadiness is the Huma-typed handler for GET /v0/provider-readiness.
func (s *Server) humaHandleProviderReadiness(ctx context.Context, input *ProviderReadinessInput) (*ProviderReadinessOutput, error) {
	providers, err := parseRequestedReadinessItems(
		input.Providers,
		"providers",
		defaultProviderReadinessItems,
		supportedProviderReadiness,
	)
	if err != nil {
		return nil, huma.Error400BadRequest(err.Error())
	}
	fresh, err := parseReadinessFreshParam(input.Fresh)
	if err != nil {
		return nil, huma.Error400BadRequest(err.Error())
	}

	resp, err := buildReadinessResponse(ctx, providers, fresh)
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}

	providerResp := providerReadinessResponse{
		Providers: make(map[string]providerReadiness, len(providers)),
	}
	for _, provider := range providers {
		item := resp.Items[provider]
		providerResp.Providers[provider] = providerReadiness{
			DisplayName: item.DisplayName,
			Status:      item.Status,
		}
	}

	return &ProviderReadinessOutput{Body: providerResp}, nil
}

// humaHandleReadiness is the Huma-typed handler for GET /v0/readiness.
func (s *Server) humaHandleReadiness(ctx context.Context, input *ReadinessInput) (*ReadinessOutput, error) {
	items, err := parseRequestedReadinessItems(
		input.Items,
		"items",
		defaultReadinessItems,
		supportedReadiness,
	)
	if err != nil {
		return nil, huma.Error400BadRequest(err.Error())
	}
	fresh, err := parseReadinessFreshParam(input.Fresh)
	if err != nil {
		return nil, huma.Error400BadRequest(err.Error())
	}

	resp, err := buildReadinessResponse(ctx, items, fresh)
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}

	return &ReadinessOutput{Body: resp}, nil
}

// parseReadinessFreshParam parses the fresh query parameter for readiness endpoints.
func parseReadinessFreshParam(fresh string) (bool, error) {
	fresh = strings.TrimSpace(fresh)
	if fresh == "" {
		return false, nil
	}
	switch fresh {
	case "0":
		return false, nil
	case "1":
		return true, nil
	default:
		return false, errors.New("fresh must be 0 or 1")
	}
}
