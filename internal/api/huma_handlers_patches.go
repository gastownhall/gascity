package api

import (
	"context"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/config"
)

// --- Agent patches ---

// humaHandleAgentPatchList is the Huma-typed handler for GET /v0/patches/agents.
func (s *Server) humaHandleAgentPatchList(_ context.Context, _ *AgentPatchListInput) (*ListOutput[config.AgentPatch], error) {
	cfg := s.state.Config()
	patches := cfg.Patches.Agents
	if patches == nil {
		patches = []config.AgentPatch{}
	}
	return &ListOutput[config.AgentPatch]{
		Index: s.latestIndex(),
		Body:  ListBody[config.AgentPatch]{Items: patches, Total: len(patches)},
	}, nil
}

// humaHandleAgentPatchGet is the Huma-typed handler for
// GET /v0/city/{cityName}/patches/agent/{base} (unqualified).
func (s *Server) humaHandleAgentPatchGet(_ context.Context, input *AgentPatchGetInput) (*IndexOutput[config.AgentPatch], error) {
	return s.agentPatchByName(input.Name)
}

// humaHandleAgentPatchGetQualified is the Huma-typed handler for
// GET /v0/city/{cityName}/patches/agent/{dir}/{base}.
func (s *Server) humaHandleAgentPatchGetQualified(_ context.Context, input *AgentPatchGetQualifiedInput) (*IndexOutput[config.AgentPatch], error) {
	return s.agentPatchByName(input.QualifiedName())
}

func (s *Server) agentPatchByName(name string) (*IndexOutput[config.AgentPatch], error) {
	cfg := s.state.Config()
	dir, base := config.ParseQualifiedName(name)
	for _, p := range cfg.Patches.Agents {
		if p.Dir == dir && p.Name == base {
			return &IndexOutput[config.AgentPatch]{
				Index: s.latestIndex(),
				Body:  p,
			}, nil
		}
	}
	return nil, huma.Error404NotFound("agent patch " + name + " not found")
}

// humaHandleAgentPatchSet is the Huma-typed handler for PUT /v0/patches/agents.
func (s *Server) humaHandleAgentPatchSet(_ context.Context, input *AgentPatchSetInput) (*PatchOKResponse, error) {
	sm, ok := s.state.(StateMutator)
	if !ok {
		return nil, errMutationsNotSupported
	}

	patch := config.AgentPatch{
		Dir:       input.Body.Dir,
		Name:      input.Body.Name,
		WorkDir:   input.Body.WorkDir,
		Scope:     input.Body.Scope,
		Suspended: input.Body.Suspended,
		Env:       input.Body.Env,
	}

	if patch.Name == "" {
		return nil, huma.Error400BadRequest("name is required")
	}

	if err := sm.SetAgentPatch(patch); err != nil {
		return nil, mutationError(err)
	}

	qn := patch.Name
	if patch.Dir != "" {
		qn = patch.Dir + "/" + patch.Name
	}
	resp := &PatchOKResponse{}
	resp.Body.Status = "ok"
	resp.Body.AgentPatch = qn
	return resp, nil
}

// humaHandleAgentPatchDelete is the Huma-typed handler for
// DELETE /v0/city/{cityName}/patches/agent/{base} (unqualified).
func (s *Server) humaHandleAgentPatchDelete(_ context.Context, input *AgentPatchDeleteInput) (*PatchDeletedResponse, error) {
	return s.deleteAgentPatchByName(input.Name)
}

// humaHandleAgentPatchDeleteQualified is the Huma-typed handler for
// DELETE /v0/city/{cityName}/patches/agent/{dir}/{base}.
func (s *Server) humaHandleAgentPatchDeleteQualified(_ context.Context, input *AgentPatchDeleteQualifiedInput) (*PatchDeletedResponse, error) {
	return s.deleteAgentPatchByName(input.QualifiedName())
}

func (s *Server) deleteAgentPatchByName(name string) (*PatchDeletedResponse, error) {
	sm, ok := s.state.(StateMutator)
	if !ok {
		return nil, errMutationsNotSupported
	}
	if err := sm.DeleteAgentPatch(name); err != nil {
		return nil, mutationError(err)
	}
	resp := &PatchDeletedResponse{}
	resp.Body.Status = "deleted"
	resp.Body.AgentPatch = name
	return resp, nil
}

// --- Rig patches ---

// humaHandleRigPatchList is the Huma-typed handler for GET /v0/patches/rigs.
func (s *Server) humaHandleRigPatchList(_ context.Context, _ *RigPatchListInput) (*ListOutput[config.RigPatch], error) {
	cfg := s.state.Config()
	patches := cfg.Patches.Rigs
	if patches == nil {
		patches = []config.RigPatch{}
	}
	return &ListOutput[config.RigPatch]{
		Index: s.latestIndex(),
		Body:  ListBody[config.RigPatch]{Items: patches, Total: len(patches)},
	}, nil
}

// humaHandleRigPatchGet is the Huma-typed handler for GET /v0/patches/rig/{name}.
func (s *Server) humaHandleRigPatchGet(_ context.Context, input *RigPatchGetInput) (*IndexOutput[config.RigPatch], error) {
	name := input.Name
	cfg := s.state.Config()
	for _, p := range cfg.Patches.Rigs {
		if p.Name == name {
			return &IndexOutput[config.RigPatch]{
				Index: s.latestIndex(),
				Body:  p,
			}, nil
		}
	}
	return nil, huma.Error404NotFound("rig patch " + name + " not found")
}

// humaHandleRigPatchSet is the Huma-typed handler for PUT /v0/patches/rigs.
func (s *Server) humaHandleRigPatchSet(_ context.Context, input *RigPatchSetInput) (*PatchOKResponse, error) {
	sm, ok := s.state.(StateMutator)
	if !ok {
		return nil, errMutationsNotSupported
	}

	patch := config.RigPatch{
		Name:      input.Body.Name,
		Path:      input.Body.Path,
		Prefix:    input.Body.Prefix,
		Suspended: input.Body.Suspended,
	}

	if patch.Name == "" {
		return nil, huma.Error400BadRequest("name is required")
	}

	if err := sm.SetRigPatch(patch); err != nil {
		return nil, mutationError(err)
	}

	resp := &PatchOKResponse{}
	resp.Body.Status = "ok"
	resp.Body.RigPatch = patch.Name
	return resp, nil
}

// humaHandleRigPatchDelete is the Huma-typed handler for DELETE /v0/patches/rig/{name}.
func (s *Server) humaHandleRigPatchDelete(_ context.Context, input *RigPatchDeleteInput) (*PatchDeletedResponse, error) {
	sm, ok := s.state.(StateMutator)
	if !ok {
		return nil, errMutationsNotSupported
	}

	if err := sm.DeleteRigPatch(input.Name); err != nil {
		return nil, mutationError(err)
	}
	resp := &PatchDeletedResponse{}
	resp.Body.Status = "deleted"
	resp.Body.RigPatch = input.Name
	return resp, nil
}

// --- Provider patches ---

// humaHandleProviderPatchList is the Huma-typed handler for GET /v0/patches/providers.
func (s *Server) humaHandleProviderPatchList(_ context.Context, _ *ProviderPatchListInput) (*ListOutput[config.ProviderPatch], error) {
	cfg := s.state.Config()
	patches := cfg.Patches.Providers
	if patches == nil {
		patches = []config.ProviderPatch{}
	}
	return &ListOutput[config.ProviderPatch]{
		Index: s.latestIndex(),
		Body:  ListBody[config.ProviderPatch]{Items: patches, Total: len(patches)},
	}, nil
}

// humaHandleProviderPatchGet is the Huma-typed handler for GET /v0/patches/provider/{name}.
func (s *Server) humaHandleProviderPatchGet(_ context.Context, input *ProviderPatchGetInput) (*IndexOutput[config.ProviderPatch], error) {
	name := input.Name
	cfg := s.state.Config()
	for _, p := range cfg.Patches.Providers {
		if p.Name == name {
			return &IndexOutput[config.ProviderPatch]{
				Index: s.latestIndex(),
				Body:  p,
			}, nil
		}
	}
	return nil, huma.Error404NotFound("provider patch " + name + " not found")
}

// humaHandleProviderPatchSet is the Huma-typed handler for PUT /v0/patches/providers.
func (s *Server) humaHandleProviderPatchSet(_ context.Context, input *ProviderPatchSetInput) (*PatchOKResponse, error) {
	sm, ok := s.state.(StateMutator)
	if !ok {
		return nil, errMutationsNotSupported
	}

	patch := config.ProviderPatch{
		Name:         input.Body.Name,
		Command:      input.Body.Command,
		ACPCommand:   input.Body.ACPCommand,
		Args:         input.Body.Args,
		ACPArgs:      input.Body.ACPArgs,
		PromptMode:   input.Body.PromptMode,
		PromptFlag:   input.Body.PromptFlag,
		ReadyDelayMs: input.Body.ReadyDelayMs,
		Env:          input.Body.Env,
	}

	if patch.Name == "" {
		return nil, huma.Error400BadRequest("name is required")
	}

	if err := sm.SetProviderPatch(patch); err != nil {
		return nil, mutationError(err)
	}

	resp := &PatchOKResponse{}
	resp.Body.Status = "ok"
	resp.Body.ProviderPatch = patch.Name
	return resp, nil
}

// humaHandleProviderPatchDelete is the Huma-typed handler for DELETE /v0/patches/provider/{name}.
func (s *Server) humaHandleProviderPatchDelete(_ context.Context, input *ProviderPatchDeleteInput) (*PatchDeletedResponse, error) {
	sm, ok := s.state.(StateMutator)
	if !ok {
		return nil, errMutationsNotSupported
	}

	if err := sm.DeleteProviderPatch(input.Name); err != nil {
		return nil, mutationError(err)
	}
	resp := &PatchDeletedResponse{}
	resp.Body.Status = "deleted"
	resp.Body.ProviderPatch = input.Name
	return resp, nil
}
