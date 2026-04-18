package api

import (
	"context"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/sse"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

// humaHandleAgentList is the Huma-typed handler for GET /v0/agents.
func (s *Server) humaHandleAgentList(ctx context.Context, input *AgentListInput) (*ListOutput[agentResponse], error) {
	bp := input.toBlockingParams()
	if bp.isBlocking() {
		waitForChange(ctx, s.state.EventProvider(), bp)
	}

	cfg := s.state.Config()
	sp := s.state.SessionProvider()
	cityName := s.state.CityName()
	sessTmpl := cfg.Workspace.SessionTemplate
	wantPeek := input.Peek

	index := s.latestIndex()
	cacheKey := ""
	if !wantPeek {
		// Cache key derived from input struct tags — adding a new query
		// param to AgentListInput automatically participates in the key.
		cacheKey = cacheKeyFor("agents", input)
		if body, ok := cachedResponseAs[ListBody[agentResponse]](s, cacheKey, index); ok {
			return &ListOutput[agentResponse]{
				Index: index,
				Body:  body,
			}, nil
		}
	}

	var agents []agentResponse
	for _, a := range cfg.Agents {
		expanded := expandAgent(a, cityName, sessTmpl, sp)
		for _, ea := range expanded {
			if input.Rig != "" && ea.rig != input.Rig {
				continue
			}
			if input.Pool != "" && ea.pool != input.Pool {
				continue
			}

			sessionName := agentSessionName(cityName, ea.qualifiedName, sessTmpl)
			running := sp.IsRunning(sessionName)

			if input.Running == "true" && !running {
				continue
			}
			if input.Running == "false" && running {
				continue
			}

			suspended := ea.suspended
			if v, err := sp.GetMeta(sessionName, "suspended"); err == nil && v == "true" {
				suspended = true
			}

			provider, displayName := resolveProviderInfo(ea.provider, cfg)

			available := true
			var unavailableReason string
			if suspended {
				available = false
				unavailableReason = "agent is suspended"
			} else if provider != "" {
				if !s.cachedLookPath(providerPathCheck(provider, cfg)) {
					available = false
					unavailableReason = "provider '" + provider + "' not found in PATH"
				}
			}

			resp := agentResponse{
				Name:              ea.qualifiedName,
				Description:       ea.description,
				Running:           running,
				Suspended:         suspended,
				Rig:               ea.rig,
				Pool:              ea.pool,
				Provider:          provider,
				DisplayName:       displayName,
				Available:         available,
				UnavailableReason: unavailableReason,
			}

			var lastActivity *time.Time
			sessionID := ""
			if running {
				si := &sessionInfo{Name: sessionName}
				if t, err := sp.GetLastActivity(sessionName); err == nil && !t.IsZero() {
					si.LastActivity = &t
					lastActivity = &t
				}
				si.Attached = sp.IsAttached(sessionName)
				resp.Session = si
				if id, err := sp.GetMeta(sessionName, "GC_SESSION_ID"); err == nil {
					sessionID = strings.TrimSpace(id)
				}
			}

			resp.ActiveBead = s.findActiveBeadForAssignees(ea.rig, sessionID, sessionName, ea.qualifiedName)
			quarantined := s.state.IsQuarantined(sessionName)
			resp.State = computeAgentState(suspended, quarantined, running, resp.ActiveBead, lastActivity)

			if wantPeek && running {
				if output, err := sp.Peek(sessionName, 5); err == nil {
					resp.LastOutput = output
				}
			}

			if running && provider == "claude" && canAttributeSession(a, ea.qualifiedName, cfg, s.state.CityPath()) {
				s.enrichSessionMeta(&resp, a, ea.qualifiedName, cfg)
			}

			agents = append(agents, resp)
		}
	}

	if agents == nil {
		agents = []agentResponse{}
	}

	body := ListBody[agentResponse]{Items: agents, Total: len(agents)}
	if cacheKey != "" {
		s.storeResponse(cacheKey, index, body)
	}

	return &ListOutput[agentResponse]{
		Index: index,
		Body:  body,
	}, nil
}

// humaHandleAgent is the Huma-typed handler for
// GET /v0/city/{cityName}/agent/{base} (unqualified form).
func (s *Server) humaHandleAgent(ctx context.Context, input *AgentGetInput) (*IndexOutput[agentResponse], error) {
	return s.agentByName(input.Name)
}

// humaHandleAgentQualified is the Huma-typed handler for
// GET /v0/city/{cityName}/agent/{dir}/{base} (qualified form).
func (s *Server) humaHandleAgentQualified(ctx context.Context, input *AgentGetQualifiedInput) (*IndexOutput[agentResponse], error) {
	return s.agentByName(input.QualifiedName())
}

// agentByName is the shared agent-get implementation. Both the qualified
// and unqualified routes normalize to a single "name" string before
// dispatching here.
func (s *Server) agentByName(name string) (*IndexOutput[agentResponse], error) {
	if name == "" {
		return nil, huma.Error400BadRequest("agent name required")
	}

	cfg := s.state.Config()
	sp := s.state.SessionProvider()
	cityName := s.state.CityName()

	agentCfg, ok := findAgent(cfg, name)
	if !ok {
		return nil, huma.Error404NotFound("agent " + name + " not found")
	}

	sessionName := agentSessionName(cityName, name, cfg.Workspace.SessionTemplate)
	running := sp.IsRunning(sessionName)

	suspended := agentCfg.Suspended
	if v, err := sp.GetMeta(sessionName, "suspended"); err == nil && v == "true" {
		suspended = true
	}

	provider, displayName := resolveProviderInfo(agentCfg.Provider, cfg)

	available := true
	var unavailableReason string
	if suspended {
		available = false
		unavailableReason = "agent is suspended"
	} else if provider != "" {
		if !s.cachedLookPath(providerPathCheck(provider, cfg)) {
			available = false
			unavailableReason = "provider '" + provider + "' not found in PATH"
		}
	}

	resp := agentResponse{
		Name:              name,
		Description:       agentCfg.Description,
		Running:           running,
		Suspended:         suspended,
		Rig:               agentCfg.Dir,
		Provider:          provider,
		DisplayName:       displayName,
		Available:         available,
		UnavailableReason: unavailableReason,
	}
	if isMultiSessionAgent(agentCfg) {
		resp.Pool = agentCfg.QualifiedName()
	}

	var lastActivity *time.Time
	sessionID := ""
	if running {
		si := &sessionInfo{Name: sessionName}
		if t, err := sp.GetLastActivity(sessionName); err == nil && !t.IsZero() {
			si.LastActivity = &t
			lastActivity = &t
		}
		si.Attached = sp.IsAttached(sessionName)
		resp.Session = si
		if id, err := sp.GetMeta(sessionName, "GC_SESSION_ID"); err == nil {
			sessionID = strings.TrimSpace(id)
		}
	}

	resp.ActiveBead = s.findActiveBeadForAssignees(agentCfg.Dir, sessionID, sessionName, name)
	quarantined := s.state.IsQuarantined(sessionName)
	resp.State = computeAgentState(suspended, quarantined, running, resp.ActiveBead, lastActivity)

	if running && provider == "claude" && canAttributeSession(agentCfg, name, cfg, s.state.CityPath()) {
		s.enrichSessionMeta(&resp, agentCfg, name, cfg)
	}

	return &IndexOutput[agentResponse]{
		Index: s.latestIndex(),
		Body:  resp,
	}, nil
}

// humaHandleAgentCreate is the Huma-typed handler for POST /v0/agents.
// Body validation (Name and Provider required with minLength:"1") is
// enforced by the framework from AgentCreateInput's struct tags.
func (s *Server) humaHandleAgentCreate(_ context.Context, input *AgentCreateInput) (*AgentCreatedOutput, error) {
	sm, ok := s.state.(StateMutator)
	if !ok {
		return nil, errMutationsNotSupported
	}

	a := config.Agent{
		Name:     input.Body.Name,
		Dir:      input.Body.Dir,
		Provider: input.Body.Provider,
		Scope:    input.Body.Scope,
	}

	if err := sm.CreateAgent(a); err != nil {
		return nil, mutationError(err)
	}
	resp := &AgentCreatedOutput{}
	resp.Body.Status = "created"
	resp.Body.Agent = a.QualifiedName()
	return resp, nil
}

// humaHandleAgentUpdate is the Huma-typed handler for
// PATCH /v0/city/{cityName}/agent/{base}.
func (s *Server) humaHandleAgentUpdate(ctx context.Context, input *AgentUpdateInput) (*OKResponse, error) {
	return s.updateAgentByName(input.Name, input.Body.Provider, input.Body.Scope, input.Body.Suspended)
}

// humaHandleAgentUpdateQualified is the Huma-typed handler for
// PATCH /v0/city/{cityName}/agent/{dir}/{base}.
func (s *Server) humaHandleAgentUpdateQualified(ctx context.Context, input *AgentUpdateQualifiedInput) (*OKResponse, error) {
	return s.updateAgentByName(input.QualifiedName(), input.Body.Provider, input.Body.Scope, input.Body.Suspended)
}

func (s *Server) updateAgentByName(name, provider, scope string, suspended *bool) (*OKResponse, error) {
	sm, ok := s.state.(StateMutator)
	if !ok {
		return nil, errMutationsNotSupported
	}
	patch := AgentUpdate{Provider: provider, Scope: scope, Suspended: suspended}
	if err := sm.UpdateAgent(name, patch); err != nil {
		return nil, mutationError(err)
	}
	resp := &OKResponse{}
	resp.Body.Status = "updated"
	return resp, nil
}

// humaHandleAgentDelete is the Huma-typed handler for
// DELETE /v0/city/{cityName}/agent/{base}.
func (s *Server) humaHandleAgentDelete(ctx context.Context, input *AgentDeleteInput) (*OKResponse, error) {
	return s.deleteAgentByName(input.Name)
}

// humaHandleAgentDeleteQualified is the Huma-typed handler for
// DELETE /v0/city/{cityName}/agent/{dir}/{base}.
func (s *Server) humaHandleAgentDeleteQualified(ctx context.Context, input *AgentDeleteQualifiedInput) (*OKResponse, error) {
	return s.deleteAgentByName(input.QualifiedName())
}

func (s *Server) deleteAgentByName(name string) (*OKResponse, error) {
	sm, ok := s.state.(StateMutator)
	if !ok {
		return nil, errMutationsNotSupported
	}
	if err := sm.DeleteAgent(name); err != nil {
		return nil, mutationError(err)
	}
	resp := &OKResponse{}
	resp.Body.Status = "deleted"
	return resp, nil
}

// humaHandleAgentAction is the Huma-typed handler for
// POST /v0/city/{cityName}/agent/{base}/{action}.
func (s *Server) humaHandleAgentAction(ctx context.Context, input *AgentActionInput) (*OKResponse, error) {
	return s.agentActionByName(input.Name, input.Action)
}

// humaHandleAgentActionQualified is the Huma-typed handler for
// POST /v0/city/{cityName}/agent/{dir}/{base}/{action}.
func (s *Server) humaHandleAgentActionQualified(ctx context.Context, input *AgentActionQualifiedInput) (*OKResponse, error) {
	return s.agentActionByName(input.QualifiedName(), input.Action)
}

func (s *Server) agentActionByName(name, action string) (*OKResponse, error) {
	sm, ok := s.state.(StateMutator)
	if !ok {
		return nil, errMutationsNotSupported
	}
	cfg := s.state.Config()
	if _, ok := findAgent(cfg, name); !ok {
		return nil, huma.Error404NotFound("agent " + name + " not found")
	}
	var err error
	switch action {
	case "suspend":
		err = sm.SuspendAgent(name)
	case "resume":
		err = sm.ResumeAgent(name)
	default:
		return nil, huma.Error400BadRequest("unknown agent action: " + action)
	}
	if err != nil {
		return nil, mutationError(err)
	}
	resp := &OKResponse{}
	resp.Body.Status = "ok"
	return resp, nil
}

// humaHandleAgentOutput is the Huma-typed handler for GET /v0/agent/{base}/output
// (unqualified agent name, no rig prefix).
func (s *Server) humaHandleAgentOutput(_ context.Context, input *AgentOutputInput) (*struct {
	Body agentOutputResponse
}, error) {
	return s.agentOutputByName(input.Name, input.Tail, input.Before)
}

// humaHandleAgentOutputQualified is the Huma-typed handler for
// GET /v0/agent/{dir}/{base}/output (qualified agent name with rig prefix).
func (s *Server) humaHandleAgentOutputQualified(_ context.Context, input *AgentOutputQualifiedInput) (*struct {
	Body agentOutputResponse
}, error) {
	return s.agentOutputByName(input.QualifiedName(), input.Tail, input.Before)
}

// agentOutputByName is the shared implementation for the agent output handlers.
func (s *Server) agentOutputByName(name string, tail int, before string) (*struct {
	Body agentOutputResponse
}, error) {
	cfg := s.state.Config()
	agentCfg, ok := findAgent(cfg, name)
	if !ok {
		return nil, huma.Error404NotFound("agent " + name + " not found")
	}

	resp, err := s.trySessionLogOutputHuma(name, agentCfg, tail, before)
	if err != nil {
		return nil, huma.Error500InternalServerError("reading session log: " + err.Error())
	}
	if resp != nil {
		return &struct {
			Body agentOutputResponse
		}{Body: *resp}, nil
	}

	// No session file found — fall back to Peek() (raw terminal text).
	sp := s.state.SessionProvider()
	sessionName := agentSessionName(s.state.CityName(), name, cfg.Workspace.SessionTemplate)
	if !sp.IsRunning(sessionName) {
		return nil, huma.Error404NotFound("agent " + name + " not running")
	}

	output, err := sp.Peek(sessionName, 100)
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}

	turns := []outputTurn{}
	if output != "" {
		turns = append(turns, outputTurn{Role: "output", Text: output})
	}

	return &struct {
		Body agentOutputResponse
	}{Body: agentOutputResponse{
		Agent:  name,
		Format: "text",
		Turns:  turns,
	}}, nil
}

// agentStreamState holds state resolved during the agent output stream
// precheck that the streaming callback needs. Both phases call
// resolveAgentStream() so precheck failures turn into proper HTTP errors
// before the SSE response is committed.
type agentStreamState struct {
	name    string
	logPath string
	running bool
	cfg     *config.City
}

// resolveAgentStream is shared between the precheck and stream callback.
// Returns the resolved state or an HTTP error if the agent doesn't exist
// or has no output available.
func (s *Server) resolveAgentStream(name string) (*agentStreamState, error) {
	cfg := s.state.Config()
	agentCfg, ok := findAgent(cfg, name)
	if !ok {
		return nil, huma.Error404NotFound("agent " + name + " not found")
	}

	workDir := s.resolveAgentWorkDir(agentCfg, name)
	provider := strings.TrimSpace(agentCfg.Provider)
	if provider == "" {
		provider = strings.TrimSpace(cfg.Workspace.Provider)
	}
	searchPaths := s.sessionLogSearchPaths
	if searchPaths == nil {
		searchPaths = sessionlog.MergeSearchPaths(cfg.Daemon.ObservePaths)
	}

	var logPath string
	if workDir != "" {
		logPath = sessionlog.FindSessionFileForProvider(searchPaths, provider, workDir)
	}

	sp := s.state.SessionProvider()
	sessionName := agentSessionName(s.state.CityName(), name, cfg.Workspace.SessionTemplate)
	running := sp.IsRunning(sessionName)

	if logPath == "" && !running {
		return nil, huma.Error404NotFound("agent " + name + " not running")
	}
	return &agentStreamState{
		name:    name,
		logPath: logPath,
		running: running,
		cfg:     cfg,
	}, nil
}

func (s *Server) checkAgentOutputStream(_ context.Context, input *AgentOutputStreamInput) error {
	_, err := s.resolveAgentStream(input.Base)
	return err
}

func (s *Server) streamAgentOutput(hctx huma.Context, input *AgentOutputStreamInput, send sse.Sender) {
	s.doStreamAgentOutput(hctx, input.Base, send)
}

func (s *Server) checkAgentOutputStreamQualified(_ context.Context, input *AgentOutputStreamQualifiedInput) error {
	_, err := s.resolveAgentStream(input.QualifiedName())
	return err
}

func (s *Server) streamAgentOutputQualified(hctx huma.Context, input *AgentOutputStreamQualifiedInput, send sse.Sender) {
	s.doStreamAgentOutput(hctx, input.QualifiedName(), send)
}

// doStreamAgentOutput is the shared streaming implementation.
func (s *Server) doStreamAgentOutput(hctx huma.Context, name string, send sse.Sender) {
	state, err := s.resolveAgentStream(name)
	if err != nil {
		return
	}
	if !state.running {
		hctx.SetHeader("GC-Agent-Status", "stopped")
	}
	ctx := hctx.Context()
	if state.logPath != "" {
		s.streamSessionLog(ctx, send, state.name, state.logPath)
	} else {
		s.streamPeekOutput(ctx, send, state.name, state.cfg)
	}
}
