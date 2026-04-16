package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/config"
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

// humaHandleCityCreate is the Huma-typed handler for POST /v0/city.
// This is stateless (no city context needed) so it delegates to the same
// logic as the original handleCityCreate.
func (s *Server) humaHandleCityCreate(_ context.Context, input *CityCreateInput) (*CityCreateOutput, error) {
	if input.Body.Dir == "" {
		return nil, huma.Error400BadRequest("dir is required")
	}
	if input.Body.Provider == "" {
		return nil, huma.Error400BadRequest("provider is required")
	}

	// Validate provider against builtins.
	if _, ok := config.BuiltinProviders()[input.Body.Provider]; !ok {
		return nil, huma.Error400BadRequest(fmt.Sprintf("unknown provider %q", input.Body.Provider))
	}

	// Validate bootstrap profile if present.
	if input.Body.BootstrapProfile != "" {
		switch input.Body.BootstrapProfile {
		case "k8s-cell", "kubernetes", "kubernetes-cell", "single-host-compat":
			// valid
		default:
			return nil, huma.Error400BadRequest(fmt.Sprintf("unknown bootstrap profile %q", input.Body.BootstrapProfile))
		}
	}

	// Resolve absolute path. Relative dirs are resolved against $HOME,
	// not CWD, because the supervisor's CWD may already be the city
	// directory.
	dir := input.Body.Dir
	if !filepath.IsAbs(dir) {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, huma.Error500InternalServerError(fmt.Sprintf("resolving home dir: %v", err))
		}
		dir = filepath.Join(home, dir)
	}

	// Ensure parent directory exists.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, huma.Error500InternalServerError(fmt.Sprintf("creating directory: %v", err))
	}

	// Shell out to `gc init`.
	gcBin, err := os.Executable()
	if err != nil {
		return nil, huma.Error500InternalServerError(fmt.Sprintf("finding gc binary: %v", err))
	}

	args := []string{"init", dir, "--provider", input.Body.Provider}
	if input.Body.BootstrapProfile != "" {
		args = append(args, "--bootstrap-profile", input.Body.BootstrapProfile)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, gcBin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := stderr.String()
		if msg == "" {
			msg = err.Error()
		}
		if bytes.Contains(stderr.Bytes(), []byte("already initialized")) {
			return nil, huma.Error409Conflict("city already initialized at " + dir)
		}
		return nil, huma.Error500InternalServerError(msg)
	}

	resp := &CityCreateOutput{}
	resp.Body.OK = true
	resp.Body.Path = dir
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
