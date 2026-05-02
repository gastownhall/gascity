package api

import (
	"context"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
)

// humaHandleRigList is the Huma-typed handler for GET /v0/rigs.
func (s *Server) humaHandleRigList(ctx context.Context, input *RigListInput) (*ListOutput[rigResponse], error) {
	bp := input.toBlockingParams()
	if bp.isBlocking() {
		waitForChange(ctx, s.state.EventProvider(), bp)
	}

	cfg := s.state.Config()
	sp := s.state.SessionProvider()
	cityName := s.state.CityName()
	wantGit := input.Git

	rigs := make([]rigResponse, 0, len(cfg.Rigs))
	for _, rig := range cfg.Rigs {
		resp := s.buildRigResponse(cfg, rig, sp, cityName, s.state.CityPath())
		if wantGit {
			resp.Git = fetchGitStatus(rig.Path)
		}
		rigs = append(rigs, resp)
	}
	return &ListOutput[rigResponse]{
		Index: s.latestIndex(),
		Body:  ListBody[rigResponse]{Items: rigs, Total: len(rigs)},
	}, nil
}

// humaHandleRigGet is the Huma-typed handler for GET /v0/rig/{name}.
func (s *Server) humaHandleRigGet(_ context.Context, input *RigGetInput) (*IndexOutput[rigResponse], error) {
	name := input.Name
	cfg := s.state.Config()
	sp := s.state.SessionProvider()
	wantGit := input.Git

	for _, rig := range cfg.Rigs {
		if rig.Name == name {
			resp := s.buildRigResponse(cfg, rig, sp, s.state.CityName(), s.state.CityPath())
			if wantGit {
				resp.Git = fetchGitStatus(rig.Path)
			}
			return &IndexOutput[rigResponse]{
				Index: s.latestIndex(),
				Body:  resp,
			}, nil
		}
	}
	return nil, huma.Error404NotFound("rig " + name + " not found")
}

// humaHandleRigCreate is the Huma-typed handler for POST /v0/rigs.
// Name and Path required via struct tags on RigCreateInput.
func (s *Server) humaHandleRigCreate(_ context.Context, input *RigCreateInput) (*RigCreatedOutput, error) {
	sm, ok := s.state.(StateMutator)
	if !ok {
		return nil, errMutationsNotSupported
	}

	rig := config.Rig{
		Name:   input.Body.Name,
		Path:   input.Body.Path,
		Prefix: input.Body.Prefix,
	}

	if err := sm.CreateRig(rig); err != nil {
		return nil, mutationError(err)
	}
	resp := &RigCreatedOutput{}
	resp.Body.Status = "created"
	resp.Body.Rig = input.Body.Name
	return resp, nil
}

// humaHandleRigUpdate is the Huma-typed handler for PATCH /v0/rig/{name}.
func (s *Server) humaHandleRigUpdate(_ context.Context, input *RigUpdateInput) (*OKResponse, error) {
	sm, ok := s.state.(StateMutator)
	if !ok {
		return nil, errMutationsNotSupported
	}

	patch := RigUpdate{
		Path:      input.Body.Path,
		Prefix:    input.Body.Prefix,
		Suspended: input.Body.Suspended,
	}

	if err := sm.UpdateRig(input.Name, patch); err != nil {
		return nil, mutationError(err)
	}
	resp := &OKResponse{}
	resp.Body.Status = "updated"
	return resp, nil
}

// humaHandleRigDelete is the Huma-typed handler for DELETE /v0/rig/{name}.
func (s *Server) humaHandleRigDelete(_ context.Context, input *RigDeleteInput) (*OKResponse, error) {
	sm, ok := s.state.(StateMutator)
	if !ok {
		return nil, errMutationsNotSupported
	}

	if err := sm.DeleteRig(input.Name); err != nil {
		return nil, mutationError(err)
	}
	resp := &OKResponse{}
	resp.Body.Status = "deleted"
	return resp, nil
}

// humaHandleRigAction is the Huma-typed handler for POST /v0/rig/{name}/{action}.
func (s *Server) humaHandleRigAction(_ context.Context, input *RigActionInput) (*RigActionResponse, error) {
	name := input.Name
	action := input.Action

	switch action {
	case "suspend", "resume":
		sm, ok := s.state.(StateMutator)
		if !ok {
			return nil, errMutationsNotSupported
		}
		var err error
		if action == "suspend" {
			err = sm.SuspendRig(name)
		} else {
			err = sm.ResumeRig(name)
		}
		if err != nil {
			return nil, mutationError(err)
		}
		resp := &RigActionResponse{}
		resp.Body.Status = "ok"
		resp.Body.Action = action
		resp.Body.Rig = name
		return resp, nil

	case "restart":
		return s.humaHandleRigRestart(name)

	default:
		return nil, huma.Error404NotFound("unknown rig action: " + action)
	}
}

// humaHandleRigRestart kills all agents in a rig so the reconciler restarts them.
// Uses sp.Stop() directly — no StateMutator dependency for runtime kills.
func (s *Server) humaHandleRigRestart(name string) (*RigActionResponse, error) {
	cfg := s.state.Config()
	sp := s.state.SessionProvider()
	cityName := s.state.CityName()

	// Verify rig exists.
	rigFound := false
	for _, rig := range cfg.Rigs {
		if rig.Name == name {
			rigFound = true
			break
		}
	}
	if !rigFound {
		return nil, huma.Error404NotFound("rig " + name + " not found")
	}

	// Best-effort kill: the agent set may change between config read and each
	// Stop call (pool scaling, config reload). The reconciler is the
	// convergence mechanism — survivors will be caught on its next tick.
	killed := make([]string, 0)
	failed := make([]string, 0)
	for _, a := range cfg.Agents {
		if workdirutil.ConfiguredRigName(s.state.CityPath(), a, cfg.Rigs) != name {
			continue
		}
		expanded := expandAgent(a, cityName, cfg.Workspace.SessionTemplate, sp)
		for _, ea := range expanded {
			sessionName := agentSessionName(cityName, ea.qualifiedName, cfg.Workspace.SessionTemplate)
			if err := sp.Stop(sessionName); err != nil {
				// "session gone" is benign — agent wasn't running.
				if !runtime.IsSessionGone(err) {
					failed = append(failed, ea.qualifiedName)
				}
			} else {
				killed = append(killed, ea.qualifiedName)
			}
		}
	}

	resp := &RigActionResponse{}
	resp.Body.Action = "restart"
	resp.Body.Rig = name
	resp.Body.Killed = killed

	if len(failed) > 0 {
		resp.Body.Failed = failed
		if len(killed) == 0 {
			// Total failure: return 200 with Status="failed" + the
			// populated Failed list. Huma's 5xx path would discard
			// the typed body and emit Problem Details, which strips
			// the agent names operators need to diagnose. The 200
			// carries the full per-agent detail; callers dispatch
			// on Body.Status.
			resp.Body.Status = "failed"
		} else {
			resp.Body.Status = "partial"
		}
	} else {
		resp.Body.Status = "ok"
	}
	return resp, nil
}
