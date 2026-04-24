package api

import (
	"context"
	"sort"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/config"
)

// resolvedProvider is the internal record for one provider resolved
// against the city config + built-ins. Both the admin list and the
// public list iterate the same set; only the final DTO mapping differs.
type resolvedProvider struct {
	Name      string
	Spec      config.ProviderSpec
	Merged    config.ProviderSpec
	Builtin   bool
	CityLevel bool
}

// resolveAllProviders returns every provider resolved from the current
// city config — city-level overrides first (sorted), then built-ins not
// already overridden (canonical order).
func (s *Server) resolveAllProviders() []resolvedProvider {
	cfg := s.state.Config()
	builtins := config.BuiltinProviders()
	builtinOrder := config.BuiltinProviderOrder()

	var cityNames []string
	for name := range cfg.Providers {
		cityNames = append(cityNames, name)
	}
	sort.Strings(cityNames)

	seen := make(map[string]bool, len(cityNames)+len(builtinOrder))
	out := make([]resolvedProvider, 0, len(cityNames)+len(builtinOrder))

	for _, name := range cityNames {
		spec := cfg.Providers[name]
		_, isBuiltin := builtins[name]
		merged := spec
		if base, ok := builtins[name]; ok {
			merged = config.MergeProviderOverBuiltin(base, spec)
		} else if base, ok := builtins[spec.Command]; ok {
			merged = config.MergeProviderOverBuiltin(base, spec)
		}
		out = append(out, resolvedProvider{
			Name:      name,
			Spec:      spec,
			Merged:    merged,
			Builtin:   isBuiltin,
			CityLevel: true,
		})
		seen[name] = true
	}
	for _, name := range builtinOrder {
		if seen[name] {
			continue
		}
		spec := builtins[name]
		out = append(out, resolvedProvider{
			Name:      name,
			Spec:      spec,
			Merged:    spec,
			Builtin:   true,
			CityLevel: false,
		})
	}
	return out
}

// humaHandleProviderList is the Huma-typed handler for
// GET /v0/city/{cityName}/providers (admin view). The browser-safe view
// lives at /providers/public.
func (s *Server) humaHandleProviderList(_ context.Context, _ *ProviderListInput) (*ListOutput[providerResponse], error) {
	resolved := s.resolveAllProviders()
	providers := make([]providerResponse, 0, len(resolved))
	for _, p := range resolved {
		providers = append(providers, providerFromSpec(p.Name, p.Spec, p.Builtin, p.CityLevel))
	}
	return &ListOutput[providerResponse]{
		Index: s.latestIndex(),
		Body:  ListBody[providerResponse]{Items: providers, Total: len(providers)},
	}, nil
}

// humaHandleProviderPublicList is the Huma-typed handler for
// GET /v0/city/{cityName}/providers/public. It returns the browser-safe
// projection of every provider — city-level first, then built-ins — and
// never exposes command/args/env or prompt-delivery details.
func (s *Server) humaHandleProviderPublicList(_ context.Context, _ *ProviderPublicListInput) (*ProviderPublicListOutput, error) {
	resolved := s.resolveAllProviders()
	providers := make([]ProviderPublicResponse, 0, len(resolved))
	for _, p := range resolved {
		providers = append(providers, toProviderPublicResponse(p.Name, p.Merged, p.Builtin, p.CityLevel))
	}
	return &ProviderPublicListOutput{
		Index: s.latestIndex(),
		Body:  ProviderPublicListBody{Items: providers, Total: len(providers)},
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
func (s *Server) humaHandleProviderCreate(_ context.Context, input *ProviderCreateInput) (*ProviderCreatedOutput, error) {
	sm, ok := s.state.(StateMutator)
	if !ok {
		return nil, errMutationsNotSupported
	}

	spec := config.ProviderSpec{
		DisplayName: input.Body.DisplayName,
		Base:        input.Body.Base,
		Command:     input.Body.Command,
		Args:        input.Body.Args,
		ArgsAppend:  input.Body.ArgsAppend,
		PromptMode:  input.Body.PromptMode,
		PromptFlag:  input.Body.PromptFlag,
		Env:         input.Body.Env,
	}
	if input.Body.ReadyDelayMs != 0 {
		spec.ReadyDelayMs = input.Body.ReadyDelayMs
	}
	if input.Body.OptionsSchemaMerge != nil {
		spec.OptionsSchemaMerge = *input.Body.OptionsSchemaMerge
	}

	if err := sm.CreateProvider(input.Body.Name, spec); err != nil {
		return nil, mutationError(err)
	}
	resp := &ProviderCreatedOutput{}
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
		DisplayName:        input.Body.DisplayName,
		Command:            input.Body.Command,
		Args:               input.Body.Args,
		ArgsAppend:         input.Body.ArgsAppend,
		PromptMode:         input.Body.PromptMode,
		PromptFlag:         input.Body.PromptFlag,
		ReadyDelayMs:       input.Body.ReadyDelayMs,
		Env:                input.Body.Env,
		OptionsSchemaMerge: input.Body.OptionsSchemaMerge,
	}
	if input.Body.Base != nil {
		patch.Base = &input.Body.Base
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
