package api

import (
	"context"
	"sort"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/config"
)

// humaHandleProviderList is the Huma-typed handler for GET /v0/providers.
// Returns providerResponse entries. When view=public, the response contains
// only the browser-safe fields (OptionsSchema, EffectiveDefaults); when view
// is unset/admin, the admin-only fields (Command, Args, Env, prompt details)
// are populated instead. See providerResponse for field details.
func (s *Server) humaHandleProviderList(_ context.Context, input *ProviderListInput) (*ListOutput[providerResponse], error) {
	cfg := s.state.Config()
	builtins := config.BuiltinProviders()
	builtinOrder := config.BuiltinProviderOrder()
	isPublic := input.View == "public"

	index := s.latestIndex()

	// Collect all providers: city-level overrides + builtins.
	seen := make(map[string]bool)
	var providers []providerResponse

	if isPublic {
		// City-level providers first (sorted alphabetically).
		var cityNames []string
		for name := range cfg.Providers {
			cityNames = append(cityNames, name)
		}
		sort.Strings(cityNames)
		for _, name := range cityNames {
			spec := cfg.Providers[name]
			_, isBuiltin := builtins[name]
			merged := spec
			if base, ok := builtins[name]; ok {
				merged = config.MergeProviderOverBuiltin(base, spec)
			} else if base, ok := builtins[spec.Command]; ok {
				merged = config.MergeProviderOverBuiltin(base, spec)
			}
			providers = append(providers, providerPublicFromMerged(name, merged, isBuiltin, true))
			seen[name] = true
		}
		// Builtins not overridden by city-level (in canonical order).
		for _, name := range builtinOrder {
			if seen[name] {
				continue
			}
			providers = append(providers, providerPublicFromMerged(name, builtins[name], true, false))
		}
	} else {
		// City-level providers first (sorted alphabetically).
		var cityNames []string
		for name := range cfg.Providers {
			cityNames = append(cityNames, name)
		}
		sort.Strings(cityNames)
		for _, name := range cityNames {
			spec := cfg.Providers[name]
			_, isBuiltin := builtins[name]
			providers = append(providers, providerFromSpec(name, spec, isBuiltin, true))
			seen[name] = true
		}

		// Builtins not overridden by city-level (in canonical order).
		for _, name := range builtinOrder {
			if seen[name] {
				continue
			}
			providers = append(providers, providerFromSpec(name, builtins[name], true, false))
		}
	}

	return &ListOutput[providerResponse]{
		Index: index,
		Body:  ListBody[providerResponse]{Items: providers, Total: len(providers)},
	}, nil
}

// humaHandleProviderGet is the Huma-typed handler for GET /v0/provider/{name}.
func (s *Server) humaHandleProviderGet(_ context.Context, input *ProviderGetInput) (*IndexOutput[providerResponse], error) {
	name := input.Name
	cfg := s.state.Config()
	builtins := config.BuiltinProviders()

	// Check city-level first.
	if spec, ok := cfg.Providers[name]; ok {
		_, isBuiltin := builtins[name]
		return &IndexOutput[providerResponse]{
			Index: s.latestIndex(),
			Body:  providerFromSpec(name, spec, isBuiltin, true),
		}, nil
	}

	// Check builtins.
	if spec, ok := builtins[name]; ok {
		return &IndexOutput[providerResponse]{
			Index: s.latestIndex(),
			Body:  providerFromSpec(name, spec, true, false),
		}, nil
	}

	return nil, huma.Error404NotFound("provider " + name + " not found")
}

// humaHandleProviderCreate is the Huma-typed handler for POST /v0/providers.
// Name and Command required via struct tags on ProviderCreateInput.
func (s *Server) humaHandleProviderCreate(_ context.Context, input *ProviderCreateInput) (*CreatedResponse, error) {
	sm, ok := s.state.(StateMutator)
	if !ok {
		return nil, errMutationsNotSupported
	}

	spec := config.ProviderSpec{
		DisplayName:  input.Body.DisplayName,
		Command:      input.Body.Command,
		Args:         input.Body.Args,
		PromptMode:   input.Body.PromptMode,
		PromptFlag:   input.Body.PromptFlag,
		ReadyDelayMs: input.Body.ReadyDelayMs,
		Env:          input.Body.Env,
	}

	if err := sm.CreateProvider(input.Body.Name, spec); err != nil {
		return nil, mutationError(err)
	}
	resp := &CreatedResponse{}
	resp.Body.Status = "created"
	resp.Body.Provider = input.Body.Name
	return resp, nil
}

// humaHandleProviderUpdate is the Huma-typed handler for PATCH /v0/provider/{name}.
func (s *Server) humaHandleProviderUpdate(_ context.Context, input *ProviderUpdateInput) (*OKResponse, error) {
	sm, ok := s.state.(StateMutator)
	if !ok {
		return nil, errMutationsNotSupported
	}

	patch := ProviderUpdate{
		DisplayName:  input.Body.DisplayName,
		Command:      input.Body.Command,
		Args:         input.Body.Args,
		PromptMode:   input.Body.PromptMode,
		PromptFlag:   input.Body.PromptFlag,
		ReadyDelayMs: input.Body.ReadyDelayMs,
		Env:          input.Body.Env,
	}

	if err := sm.UpdateProvider(input.Name, patch); err != nil {
		msg := err.Error()
		// Preserve the special builtin-override hint.
		if strings.Contains(msg, "not found") && isBuiltinProvider(input.Name) {
			return nil, huma.Error409Conflict(
				"provider " + input.Name + " is a builtin; use PUT /v0/patches/providers to override")
		}
		return nil, mutationError(err)
	}
	resp := &OKResponse{}
	resp.Body.Status = "updated"
	return resp, nil
}

// humaHandleProviderDelete is the Huma-typed handler for DELETE /v0/provider/{name}.
func (s *Server) humaHandleProviderDelete(_ context.Context, input *ProviderDeleteInput) (*OKResponse, error) {
	sm, ok := s.state.(StateMutator)
	if !ok {
		return nil, errMutationsNotSupported
	}

	if err := sm.DeleteProvider(input.Name); err != nil {
		msg := err.Error()
		// Preserve the special builtin-override hint.
		if strings.Contains(msg, "not found") && isBuiltinProvider(input.Name) {
			return nil, huma.Error409Conflict(
				"provider " + input.Name + " is a builtin; use DELETE /v0/patches/provider/" + input.Name + " to remove overrides")
		}
		return nil, mutationError(err)
	}
	resp := &OKResponse{}
	resp.Body.Status = "deleted"
	return resp, nil
}

