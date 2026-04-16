package api

import (
	"context"
	"encoding/json"
	"net/http"
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
	wantPeek := input.Peek == "true"

	index := s.latestIndex()
	cacheKey := ""
	if !wantPeek {
		cacheKey = "agents"
		if input.Pool != "" || input.Rig != "" || input.Running != "" {
			cacheKey += "?" + input.Pool + "|" + input.Rig + "|" + input.Running
		}
		if cached, ok := s.cachedResponse(cacheKey, index); ok {
			var body listResponse
			if err := json.Unmarshal(cached, &body); err == nil {
				// Re-marshal items into agent responses.
				itemsJSON, _ := json.Marshal(body.Items)
				var agents []agentResponse
				json.Unmarshal(itemsJSON, &agents) //nolint:errcheck
				if agents == nil {
					agents = []agentResponse{}
				}
				return &ListOutput[agentResponse]{
					Index: index,
					Body:  ListBody[agentResponse]{Items: agents, Total: body.Total},
				}, nil
			}
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
			if running {
				si := &sessionInfo{Name: sessionName}
				if t, err := sp.GetLastActivity(sessionName); err == nil && !t.IsZero() {
					si.LastActivity = &t
					lastActivity = &t
				}
				si.Attached = sp.IsAttached(sessionName)
				resp.Session = si
			}

			resp.ActiveBead = s.findActiveBead(ea.qualifiedName, ea.rig)
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

	if cacheKey != "" {
		resp := listResponse{Items: agents, Total: len(agents)}
		s.storeResponse(cacheKey, index, resp) //nolint:errcheck
	}

	return &ListOutput[agentResponse]{
		Index: index,
		Body:  ListBody[agentResponse]{Items: agents, Total: len(agents)},
	}, nil
}

// humaHandleAgent is the Huma-typed handler for GET /v0/agent/{name}.
// Also handles the /output sub-resource: if the agent isn't found by exact
// name, checks for /output suffix and returns the agent output response
// wrapped in an agentResponse envelope with a special "output_response" field.
// The /output/stream SSE sub-resource is handled by a separate old-mux handler.
func (s *Server) humaHandleAgent(ctx context.Context, input *AgentGetInput) (*IndexOutput[agentResponse], error) {
	name := input.Name
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
	if running {
		si := &sessionInfo{Name: sessionName}
		if t, err := sp.GetLastActivity(sessionName); err == nil && !t.IsZero() {
			si.LastActivity = &t
			lastActivity = &t
		}
		si.Attached = sp.IsAttached(sessionName)
		resp.Session = si
	}

	resp.ActiveBead = s.findActiveBead(name, agentCfg.Dir)
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
func (s *Server) humaHandleAgentCreate(_ context.Context, input *AgentCreateInput) (*CreatedResponse, error) {
	sm, ok := s.state.(StateMutator)
	if !ok {
		return nil, errMutationsNotSupported
	}

	if input.Body.Name == "" {
		return nil, huma.Error400BadRequest("name is required")
	}
	if input.Body.Provider == "" {
		return nil, huma.Error400BadRequest("provider is required")
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
	resp := &CreatedResponse{}
	resp.Body.Status = "created"
	resp.Body.Agent = a.QualifiedName()
	return resp, nil
}

// humaHandleAgentUpdate is the Huma-typed handler for PATCH /v0/agent/{name}.
func (s *Server) humaHandleAgentUpdate(ctx context.Context, input *AgentUpdateInput) (*OKResponse, error) {
	sm, ok := s.state.(StateMutator)
	if !ok {
		return nil, errMutationsNotSupported
	}

	patch := AgentUpdate{
		Provider:  input.Body.Provider,
		Scope:     input.Body.Scope,
		Suspended: input.Body.Suspended,
	}

	if err := sm.UpdateAgent(input.Name, patch); err != nil {
		return nil, mutationError(err)
	}
	resp := &OKResponse{}
	resp.Body.Status = "updated"
	return resp, nil
}

// humaHandleAgentDelete is the Huma-typed handler for DELETE /v0/agent/{name}.
func (s *Server) humaHandleAgentDelete(ctx context.Context, input *AgentDeleteInput) (*OKResponse, error) {
	sm, ok := s.state.(StateMutator)
	if !ok {
		return nil, errMutationsNotSupported
	}

	if err := sm.DeleteAgent(input.Name); err != nil {
		return nil, mutationError(err)
	}
	resp := &OKResponse{}
	resp.Body.Status = "deleted"
	return resp, nil
}

// humaHandleAgentAction is the Huma-typed handler for POST /v0/agent/{name}
// (suspend/resume actions).
func (s *Server) humaHandleAgentAction(ctx context.Context, input *AgentActionInput) (*OKResponse, error) {
	name := input.Name

	sm, ok := s.state.(StateMutator)
	if !ok {
		return nil, errMutationsNotSupported
	}

	var action string
	if after, found := strings.CutSuffix(name, "/suspend"); found {
		name = after
		action = "suspend"
	} else if after, found := strings.CutSuffix(name, "/resume"); found {
		name = after
		action = "resume"
	} else {
		return nil, huma.Error404NotFound("unknown agent action; runtime operations moved to /v0/session/{id}/*")
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
func (s *Server) agentOutputByName(name, tail, before string) (*struct {
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

// registerAgentOutputStreamRoutes wires up the two agent output stream routes
// (unqualified and qualified names) via registerSSE. Both emit "turn" events
// carrying agentOutputResponse, plus periodic heartbeats.
func (s *Server) registerAgentOutputStreamRoutes() {
	eventMap := map[string]any{
		"turn":      agentOutputResponse{},
		"heartbeat": HeartbeatEvent{},
	}

	registerSSE(s.humaAPI, huma.Operation{
		OperationID: "stream-agent-output",
		Method:      http.MethodGet,
		Path:        "/v0/agent/{base}/output/stream",
		Summary:     "Stream agent output in real time",
		Description: "Server-Sent Events stream of agent output (session log tail or tmux pane polling).",
	}, eventMap, s.checkAgentOutputStream, s.streamAgentOutput)

	registerSSE(s.humaAPI, huma.Operation{
		OperationID: "stream-agent-output-qualified",
		Method:      http.MethodGet,
		Path:        "/v0/agent/{dir}/{base}/output/stream",
		Summary:     "Stream agent output in real time (qualified name)",
		Description: "Server-Sent Events stream of agent output for qualified (rig-prefixed) agent names.",
	}, eventMap, s.checkAgentOutputStreamQualified, s.streamAgentOutputQualified)
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
