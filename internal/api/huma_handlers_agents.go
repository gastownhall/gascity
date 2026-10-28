package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/sse"
	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// agentListResponseTTLFloor lets a non-blocking agent list reuse a body
// built within the floor after its time-bucket entry has rolled over. The
// list fans out per agent (active-bead lookups, environment reads), and on a
// loaded city the build outlasts a bucket (8.19s in #7149). Such a body is
// stored under a bucket no later request computes, so this age bound,
// measured from when the build finished, is what serves it. Same value and
// rationale as statusResponseTTLFloor (#1896). Blocking requests bypass it.
// Var, not const, so tests can pin each cache tier.
var agentListResponseTTLFloor = 3 * time.Second

// agentListSlowBuildThreshold is the build time from which a cached agent
// list is still served after the event index has moved past it. The
// dashboard's Agents view refetches the list when a session, bead or agent
// event arrives and never polls, so a list built before that event would show
// the pre-event state until the next event. Rebuilding a cheap list on every
// such refetch costs little, so only a list slower than this to build, the
// loaded city of #7149, is served across index changes: its readers get a
// list stored at most a floor ago instead of paying a rebuild per event. A
// list built at the live index is served whatever its build time. Var, not
// const, so tests can pin it.
var agentListSlowBuildThreshold = time.Second

// agentListCacheEntry is a cached agent list.
type agentListCacheEntry struct {
	// Index is the event index captured before the build: what a hit reports
	// as X-GC-Index.
	Index uint64 `json:"index"`
	// Started is when the build began; it orders overlapping builds.
	Started time.Time `json:"started"`
	// BuildTime is how long the build took.
	BuildTime time.Duration           `json:"build_time"`
	Body      ListBody[agentResponse] `json:"body"`
}

// cachedAgentList returns the list cached under key that a request which saw
// event index may be served: one stored within its time bucket or the TTL
// floor, and built at or after index unless its build was slow
// (agentListSlowBuildThreshold).
func (s *Server) cachedAgentList(key string, bucket, index uint64) (agentListCacheEntry, bool) {
	hit, ok := cachedResponseAs[agentListCacheEntry](s, key, bucket)
	if !ok {
		// A build that outlasts its start bucket is stored under a bucket no
		// later request computes; the age floor still serves it.
		hit, ok = cachedResponseWithinAgeAs[agentListCacheEntry](s, key, agentListResponseTTLFloor)
	}
	if !ok || (hit.Index < index && hit.BuildTime < agentListSlowBuildThreshold) {
		return agentListCacheEntry{}, false
	}
	return hit, true
}

// storeAgentList caches a complete list under (key, bucket) unless the slot
// holds one whose build started later. On a loaded city builds overlap (an 8s
// build against faster polls) and can finish out of order, and the age floor
// serves whatever the slot holds, so a build that started first and finished
// last would replace the newer list and step its readers back to an older
// snapshot and X-GC-Index.
func (s *Server) storeAgentList(key string, bucket uint64, entry agentListCacheEntry) {
	s.storeResponseUnless(key, bucket, entry, func(cached responseCacheEntry) bool {
		prev, ok := cached.value.(agentListCacheEntry)
		return ok && prev.Started.After(entry.Started)
	})
}

// humaHandleAgentList is the Huma-typed handler for GET /v0/agents.
func (s *Server) humaHandleAgentList(ctx context.Context, input *AgentListInput) (*ListOutput[agentResponse], error) {
	bp := input.toBlockingParams()
	blocking := bp.isBlocking()
	if blocking {
		waitForChange(ctx, s.state.EventProvider(), bp)
	}
	index := s.latestIndex()
	started := time.Now()
	bucket := responseCacheTimeBucket(started)

	cfg := s.state.Config()
	sp := s.state.SessionProvider()
	cityName := s.state.CityName()
	sessTmpl := cfg.Workspace.SessionTemplate
	wantPeek := input.Peek

	// Raw config drives accurate provenance detection (pack-derived vs.
	// city-native). Optional capability: when absent, agentOrigin falls
	// back to the patch-presence heuristic.
	rawCfg := s.rawConfig()

	// The agent list keys its response cache on a TIME bucket, not the event
	// index: on a busy city the sequence advances between polls, so an
	// index-keyed entry missed on nearly every request and paid the
	// per-agent rebuild (gascity#7149, the class #3186 fixed for /status).
	// A hit can therefore predate events recorded since its build, so it
	// reports the index captured before that build, never this request's
	// index: X-GC-Index tells a client where to resume or block
	// (docs/reference/api.md "Snapshot index"). Only a slow build is served
	// once the index has moved past it (cachedAgentList).
	//
	// Blocking requests (?index=&wait=) neither read nor write the cache: the
	// body must reflect the event the request waited for, and identical
	// blocking queries share one slot that can hold a body built before that
	// event. ?peek=true reads live pane output and is never cached either.
	cacheKey := ""
	if !wantPeek && !blocking {
		// Cache key derived from input struct tags — adding a new query
		// param to AgentListInput automatically participates in the key.
		// The list projects the config, whose swaps do not advance the
		// event index, so the key also carries the config generation.
		cacheKey = cacheKeyFor("agents", input) + "#cfg=" + strconv.FormatUint(s.configGeneration(cfg), 10)
		if hit, ok := s.cachedAgentList(cacheKey, bucket, index); ok {
			return &ListOutput[agentResponse]{Index: hit.Index, Body: hit.Body}, nil
		}
	}

	// Active graph-resident work, indexed once per request by agent session
	// name (nil on a single-store city). Computed after the cache-hit return
	// so a cached response never pays for the graph-store list.
	graphWork := s.graphActiveWorkBySession()

	// Optional batch extensions: providers whose per-session attribute reads
	// are otherwise expensive (e.g. one subprocess fork per call, per agent)
	// can implement these to collapse the hot loop below to O(1) execs
	// instead of O(agents). SessionRoster is read once, up front, since it
	// covers every session in a single call; EnvironmentBatchProvider is
	// read per running agent (still one call instead of the two GetMeta
	// calls it replaces).
	var rosterMap map[string]runtime.SessionRosterEntry
	if rp, ok := sp.(runtime.SessionRosterProvider); ok {
		if m, err := rp.SessionRoster(); err == nil {
			rosterMap = m
		}
	}
	envBatch, _ := sp.(runtime.EnvironmentBatchProvider)
	runtimeSessions := s.newAgentSessionResolver(ctx, cfg, cityName)

	var agents []agentResponse
	var partialErrors []string
	for _, a := range cfg.Agents {
		// Provenance is a property of the declared agent, shared by every
		// pool-expanded instance, so compute it once per source agent.
		pack, packDerived := agentPackProvenance(a, rawCfg, cfg)
		expanded, expandErr := expandAgentChecked(a, cityName, sessTmpl, sp, cfg.NamedSessions)
		// A runtime server that is not running at all (tmux before its first
		// session) has no sessions; that is a complete answer, not a failure.
		if runtime.IsRuntimeServerAbsent(expandErr) {
			expandErr = nil
		}
		switch {
		case expandErr != nil:
			// Say the pool's sessions are unknown rather than listing it as
			// stopped or dropping it silently.
			partialErrors = append(partialErrors, fmt.Sprintf("agent %s: listing pool sessions: %v", a.QualifiedName(), expandErr))
		case len(expanded) == 0 && isMultiSessionAgent(a):
			// An on-demand pool with no live session is still a configured
			// agent: list it under its own name, like an idle singleton.
			expanded = []expandedAgent{configuredPoolRow(a)}
		}
		limits := agentPoolLimits(a)
		for _, ea := range expanded {
			if input.Rig != "" && ea.rig != input.Rig {
				continue
			}
			if input.Pool != "" && ea.pool != input.Pool {
				continue
			}

			// Liveness stays on IsRunning: it is already a cached,
			// fleet-wide read that excludes pane_dead corpses, whereas
			// roster membership comes from list-sessions, which still
			// lists a session whose pane exited under remain-on-exit.
			// The roster is an attributes source only. The name probed is
			// the session the row runs under, which for a pool slot is not
			// the name the slot derives (agentSessionResolver).
			sessionName, running := runtimeSessions.resolve(a, ea.qualifiedName, sp.IsRunning)
			var rosterEntry runtime.SessionRosterEntry
			var haveRosterEntry bool
			if rosterMap != nil {
				rosterEntry, haveRosterEntry = rosterMap[sessionName]
			}
			// Fold active graph-resident (wisp) work into the running signal so
			// an agent whose work executes under an agent-agnostic wisp session
			// is reported running even when its named provider session is down.
			// hasGraphWork is always false on a single-store city.
			gw, hasGraphWork := graphWork[sessionName]
			effectiveRunning := running || hasGraphWork

			if input.Running == "true" && !effectiveRunning {
				continue
			}
			if input.Running == "false" && effectiveRunning {
				continue
			}

			// Live per-session env (suspended flag + GC_SESSION_ID) only
			// exists for a running session — a non-running session has no
			// tmux environment to query, so both reads below are gated
			// behind running rather than issued unconditionally.
			var env map[string]string
			if running && envBatch != nil {
				env, _ = envBatch.GetAllEnvironment(sessionName)
			}

			suspended := ea.suspended
			if running {
				if env != nil {
					if env["suspended"] == "true" {
						suspended = true
					}
				} else if v, err := sp.GetMeta(sessionName, "suspended"); err == nil && v == "true" {
					suspended = true
				}
			}

			provider, displayName := agentProviderInfo(&a, cfg)

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
				Running:           effectiveRunning,
				Suspended:         suspended,
				Rig:               ea.rig,
				Pool:              ea.pool,
				Provider:          provider,
				DisplayName:       displayName,
				Available:         available,
				UnavailableReason: unavailableReason,
				PackDerived:       packDerived,
				Pack:              pack,
				PoolLimits:        limits,
			}

			var lastActivity *time.Time
			sessionID := ""
			if running {
				si := &sessionInfo{Name: sessionName}
				if haveRosterEntry {
					if !rosterEntry.LastActivity.IsZero() {
						t := rosterEntry.LastActivity
						si.LastActivity = &t
						lastActivity = &t
					}
					si.Attached = rosterEntry.Attached
				} else {
					if t, err := sp.GetLastActivity(sessionName); err == nil && !t.IsZero() {
						si.LastActivity = &t
						lastActivity = &t
					}
					si.Attached = sp.IsAttached(sessionName)
				}
				resp.Session = si
				if env != nil {
					sessionID = strings.TrimSpace(env["GC_SESSION_ID"])
				} else if id, err := sp.GetMeta(sessionName, "GC_SESSION_ID"); err == nil {
					sessionID = strings.TrimSpace(id)
				}
			}

			resp.ActiveBead = s.findActiveBeadForAssignees(ea.rig, sessionID, sessionName, ea.qualifiedName)
			// A relocated-graph wisp names its assignee after the session, not
			// the work store, so the work-store lookup above misses it. Fall
			// back to the graph bead and use its timestamp as the activity
			// signal so the state reads "working", not "stopped".
			if hasGraphWork {
				if resp.ActiveBead == "" {
					resp.ActiveBead = gw.beadID
				}
				if lastActivity == nil {
					la := gw.lastActivity
					lastActivity = &la
				}
			}
			quarantined := s.state.IsQuarantined(sessionName)
			resp.State = computeAgentState(suspended, quarantined, effectiveRunning, resp.ActiveBead, lastActivity)

			if wantPeek && running {
				if output, err := sp.Peek(sessionName, 5); err == nil {
					resp.LastOutput = output
				}
			}

			if running && provider == "claude" && canAttributeSession(a, ea.qualifiedName, cfg, s.state.CityPath()) {
				s.enrichSessionMeta(&resp, a, ea.qualifiedName)
			}

			agents = append(agents, resp)
		}
	}
	partialErrors = append(partialErrors, runtimeSessions.partialErrors()...)

	if agents == nil {
		agents = []agentResponse{}
	}

	body := ListBody[agentResponse]{
		Items:         agents,
		Total:         len(agents),
		Partial:       len(partialErrors) > 0,
		PartialErrors: partialErrors,
	}
	// A partial list reflects a transient backend failure; don't serve it
	// from cache once the backend recovers.
	if cacheKey != "" && !body.Partial {
		s.storeAgentList(cacheKey, bucket, agentListCacheEntry{Index: index, Started: started, BuildTime: time.Since(started), Body: body})
	}

	return &ListOutput[agentResponse]{
		Index: index,
		Body:  body,
	}, nil
}

// humaHandleAgent is the Huma-typed handler for
// GET /v0/city/{cityName}/agent/{base} (unqualified form).
func (s *Server) humaHandleAgent(ctx context.Context, input *AgentGetInput) (*IndexOutput[agentResponse], error) {
	return s.agentByName(ctx, input.Name)
}

// humaHandleAgentQualified is the Huma-typed handler for
// GET /v0/city/{cityName}/agent/{dir}/{base} (qualified form).
func (s *Server) humaHandleAgentQualified(ctx context.Context, input *AgentGetQualifiedInput) (*IndexOutput[agentResponse], error) {
	return s.agentByName(ctx, input.QualifiedName())
}

// agentByName is the shared agent-get implementation. Both the qualified
// and unqualified routes normalize to a single "name" string before
// dispatching here.
func (s *Server) agentByName(ctx context.Context, name string) (*IndexOutput[agentResponse], error) {
	index := s.latestIndex()
	if name == "" {
		return nil, apierr.InvalidRequest.Msg("agent name required")
	}

	cfg := s.state.Config()
	sp := s.state.SessionProvider()
	cityName := s.state.CityName()

	agentCfg, ok := findAgent(cfg, name)
	if !ok {
		return nil, apierr.AgentNotFound.Msg("agent " + name + " not found")
	}

	sessionName, running := s.newAgentSessionResolver(ctx, cfg, cityName).resolve(agentCfg, name, sp.IsRunning)
	// Fold active graph-resident (wisp) work into the running signal so a
	// graph-working agent reports running even when its named provider session
	// is down. hasGraphWork is always false on a single-store city.
	gw, hasGraphWork := s.graphActiveWorkBySession()[sessionName]
	effectiveRunning := running || hasGraphWork

	suspended := agentCfg.Suspended
	if v, err := sp.GetMeta(sessionName, "suspended"); err == nil && v == "true" {
		suspended = true
	}

	provider, displayName := agentProviderInfo(&agentCfg, cfg)

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

	pack, packDerived := agentPackProvenance(agentCfg, s.rawConfig(), cfg)

	resp := agentResponse{
		Name:              name,
		Description:       agentCfg.Description,
		Running:           effectiveRunning,
		Suspended:         suspended,
		Rig:               agentCfg.Dir,
		Provider:          provider,
		DisplayName:       displayName,
		Available:         available,
		UnavailableReason: unavailableReason,
		PackDerived:       packDerived,
		Pack:              pack,
		PoolLimits:        agentPoolLimits(agentCfg),
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

	resp.ActiveBead = s.findLiveActiveBeadForAssignees(agentCfg.Dir, sessionID, sessionName, name)
	// A relocated-graph wisp names its assignee after the session, not the work
	// store, so the work-store lookup above misses it. Fall back to the graph
	// bead and use its timestamp as the activity signal so the state reads
	// "working", not "stopped".
	if hasGraphWork {
		if resp.ActiveBead == "" {
			resp.ActiveBead = gw.beadID
		}
		if lastActivity == nil {
			la := gw.lastActivity
			lastActivity = &la
		}
	}
	quarantined := s.state.IsQuarantined(sessionName)
	resp.State = computeAgentState(suspended, quarantined, effectiveRunning, resp.ActiveBead, lastActivity)

	if running && provider == "claude" && canAttributeSession(agentCfg, name, cfg, s.state.CityPath()) {
		s.enrichSessionMeta(&resp, agentCfg, name)
	}

	return &IndexOutput[agentResponse]{
		Index: index,
		Body:  resp,
	}, nil
}

// humaHandleAgentCreate is the Huma-typed handler for POST /v0/agents.
// Body validation (Name and Provider required with minLength:"1") is
// enforced by the framework from AgentCreateInput's struct tags.
func (s *Server) humaHandleAgentCreate(ctx context.Context, input *AgentCreateInput) (*AgentCreatedOutput, error) {
	// Idempotency: create at most once per Idempotency-Key. The cached value is
	// the qualified agent name (the response body is rebuilt from it), and the
	// visibility wait stays inside the closure so a cached 201 keeps its strict
	// read-after-write meaning. A create that fails after the durable config
	// write (visibility timeout 503/504) releases the reservation, so a
	// same-key retry re-runs the create and surfaces the conflict — identical
	// to an unkeyed retry today.
	qualifiedName, err := withIdempotency(s.idem, "/v0/agents", input.IdempotencyKey, input.Body,
		func() (string, error) {
			sm, ok := s.state.(StateMutator)
			if !ok {
				return "", errMutationsNotSupported
			}

			a := config.Agent{
				Name:     input.Body.Name,
				Dir:      input.Body.Dir,
				Provider: input.Body.Provider,
				Scope:    input.Body.Scope,
			}

			if err := sm.CreateAgent(a); err != nil {
				return "", mutationError(err)
			}
			// Block until the new agent is reachable through findAgent, so the
			// 201 response is a strict read-after-write signal: a follow-up
			// POST /sling against the same target will not race a stale runtime
			// config snapshot. This is intentionally scoped to agents because sling
			// target resolution reads the agent projection immediately after create.
			name := a.QualifiedName()
			if waiter, ok := s.state.(AgentVisibilityWaiter); ok {
				waitCtx, cancel := context.WithTimeout(ctx, s.agentCreateVisibilityWaitTimeout())
				err := waiter.WaitForAgentVisibility(waitCtx, name)
				cancel()
				if err != nil {
					log.Printf("api: agent %s visibility confirmation failed after create: %v", name, err)
					return "", agentVisibilityWaitHTTPError(err)
				}
			}
			return name, nil
		})
	if err != nil {
		return nil, err
	}
	resp := &AgentCreatedOutput{}
	resp.Body.Status = "created"
	resp.Body.Agent = qualifiedName
	return resp, nil
}

func agentVisibilityWaitHTTPError(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return agentVisibilityRetryableError(apierr.ServiceUnavailable.Msg("agent was created, but visibility confirmation was canceled"))
	case errors.Is(err, context.DeadlineExceeded):
		return agentVisibilityRetryableError(apierr.GatewayTimeout.Msg("agent was created, but visibility was not confirmed before timeout"))
	default:
		return apierr.Internal.Msg("agent was created, but visibility confirmation failed")
	}
}

func agentVisibilityRetryableError(err error) error {
	return huma.ErrorWithHeaders(err, http.Header{"Retry-After": []string{"1"}})
}

// humaHandleAgentUpdate is the Huma-typed handler for
// PATCH /v0/city/{cityName}/agent/{base}.
func (s *Server) humaHandleAgentUpdate(_ context.Context, input *AgentUpdateInput) (*OKResponse, error) {
	return s.updateAgentByName(input.Name, input.Body.Provider, input.Body.Scope, input.Body.Suspended)
}

// humaHandleAgentUpdateQualified is the Huma-typed handler for
// PATCH /v0/city/{cityName}/agent/{dir}/{base}.
func (s *Server) humaHandleAgentUpdateQualified(_ context.Context, input *AgentUpdateQualifiedInput) (*OKResponse, error) {
	return s.updateAgentByName(input.QualifiedName(), input.Body.Provider, input.Body.Scope, input.Body.Suspended)
}

func (s *Server) updateAgentByName(name, provider, scope string, suspended *bool) (*OKResponse, error) {
	sm, ok := s.state.(StateMutator)
	if !ok {
		return nil, errMutationsNotSupported
	}
	if err := rigTemplateInstanceConflict(s.state.Config(), name, "update"); err != nil {
		return nil, err
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
func (s *Server) humaHandleAgentDelete(_ context.Context, input *AgentDeleteInput) (*OKResponse, error) {
	return s.deleteAgentByName(input.Name)
}

// humaHandleAgentDeleteQualified is the Huma-typed handler for
// DELETE /v0/city/{cityName}/agent/{dir}/{base}.
func (s *Server) humaHandleAgentDeleteQualified(_ context.Context, input *AgentDeleteQualifiedInput) (*OKResponse, error) {
	return s.deleteAgentByName(input.QualifiedName())
}

func (s *Server) deleteAgentByName(name string) (*OKResponse, error) {
	sm, ok := s.state.(StateMutator)
	if !ok {
		return nil, errMutationsNotSupported
	}
	if err := rigTemplateInstanceConflict(s.state.Config(), name, "delete"); err != nil {
		return nil, err
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
func (s *Server) humaHandleAgentAction(_ context.Context, input *AgentActionInput) (*OKResponse, error) {
	return s.agentActionByName(input.Name, input.Action)
}

// humaHandleAgentActionQualified is the Huma-typed handler for
// POST /v0/city/{cityName}/agent/{dir}/{base}/{action}.
func (s *Server) humaHandleAgentActionQualified(_ context.Context, input *AgentActionQualifiedInput) (*OKResponse, error) {
	return s.agentActionByName(input.QualifiedName(), input.Action)
}

func (s *Server) agentActionByName(name, action string) (*OKResponse, error) {
	sm, ok := s.state.(StateMutator)
	if !ok {
		return nil, errMutationsNotSupported
	}
	cfg := s.state.Config()
	if _, ok := findAgent(cfg, name); !ok {
		return nil, apierr.AgentNotFound.Msg("agent " + name + " not found")
	}
	if err := rigTemplateInstanceConflict(cfg, name, action); err != nil {
		return nil, err
	}
	var err error
	switch action {
	case "suspend":
		err = sm.SuspendAgent(name)
	case "resume":
		err = sm.ResumeAgent(name)
	default:
		return nil, apierr.InvalidRequest.Msg("unknown agent action: " + action)
	}
	if err != nil {
		return nil, mutationError(err)
	}
	resp := &OKResponse{}
	resp.Body.Status = "ok"
	return resp, nil
}

// rigTemplateInstanceConflict refuses an edit addressed to a per-rig instance
// of a generic rig-scoped template (see rigTemplateInstance): the only config
// behind the instance is the template, so the edit would change every rig. The
// 409 names the template to edit instead and, for suspend and resume, the rig
// action that stays within one rig. It returns nil when name is not such an
// instance.
func rigTemplateInstanceConflict(cfg *config.City, name, verb string) error {
	template, rig, ok := rigTemplateInstance(cfg, name)
	if !ok {
		return nil
	}
	msg := "agent " + name + " is a per-rig instance of the rig-scoped template " + template +
		", which serves every rig; " + verb + " agent " + template + " to " + verb + " it in every rig"
	if verb == "suspend" || verb == "resume" {
		msg += ", or " + verb + " rig " + rig + " to " + verb + " that rig's agents"
	}
	return apierr.ConflictWrongState.Msg(msg)
}

// humaHandleAgentOutput is the Huma-typed handler for GET /v0/agent/{base}/output
// (unqualified agent name, no rig prefix).
func (s *Server) humaHandleAgentOutput(_ context.Context, input *AgentOutputInput) (*struct {
	Body agentOutputResponse
}, error,
) {
	tail, provided := input.Compactions()
	return s.agentOutputByName(input.Name, tail, provided, input.Before)
}

// humaHandleAgentOutputQualified is the Huma-typed handler for
// GET /v0/agent/{dir}/{base}/output (qualified agent name with rig prefix).
func (s *Server) humaHandleAgentOutputQualified(_ context.Context, input *AgentOutputQualifiedInput) (*struct {
	Body agentOutputResponse
}, error,
) {
	tail, provided := input.Compactions()
	return s.agentOutputByName(input.QualifiedName(), tail, provided, input.Before)
}

// agentOutputByName is the shared implementation for the agent output
// handlers. tail carries the client's ?tail= value verbatim; provided
// reports whether the client supplied ?tail= at all. When provided is
// false, the handler applies the default (1 compaction). When provided
// is true and tail==0, return all compactions (sessionlog's
// "no pagination" mode).
func (s *Server) agentOutputByName(name string, tail int, provided bool, before string) (*struct {
	Body agentOutputResponse
}, error,
) {
	cfg := s.state.Config()
	agentCfg, ok := findAgent(cfg, name)
	if !ok {
		return nil, apierr.AgentNotFound.Msg("agent " + name + " not found")
	}

	resp, err := s.trySessionLogOutputHuma(name, agentCfg, tail, provided, before)
	if err != nil {
		return nil, apierr.Internal.Msg("reading session log: " + err.Error())
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
		return nil, apierr.AgentNotFound.Msg("agent " + name + " not running")
	}

	output, err := sp.Peek(sessionName, 100)
	if err != nil {
		return nil, apierr.Internal.Msg(err.Error())
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
	name           string
	logPath        string
	provider       string
	running        bool
	cfg            *config.City
	resolveLogPath func() string
}

// resolveAgentStream is shared between the precheck and stream callback.
// Returns the resolved state or an HTTP error if the agent doesn't exist
// or has no output available.
func (s *Server) resolveAgentStream(name string) (*agentStreamState, error) {
	cfg := s.state.Config()
	agentCfg, ok := findAgent(cfg, name)
	if !ok {
		return nil, apierr.AgentNotFound.Msg("agent " + name + " not found")
	}

	workDir := s.resolveAgentWorkDir(agentCfg, name)
	transcriptState, err := s.resolveAgentTranscript(name, agentCfg)
	if err != nil {
		return nil, apierr.Internal.Msg(err.Error())
	}
	provider := transcriptState.provider
	logPath := transcriptState.path

	sp := s.state.SessionProvider()
	sessionName := transcriptState.sessionName
	running := sp.IsRunning(sessionName)

	if logPath == "" && !running {
		return nil, apierr.AgentNotFound.Msg("agent " + name + " not running")
	}
	return &agentStreamState{
		name:     name,
		logPath:  logPath,
		provider: provider,
		running:  running,
		cfg:      cfg,
		resolveLogPath: func() string {
			if workDir == "" {
				return ""
			}
			resolved, err := s.resolveAgentTranscript(name, agentCfg)
			if err != nil {
				return ""
			}
			return resolved.path
		},
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
	flushSSEHeaders(hctx)
	ctx := hctx.Context()
	workerOps := s.watchAgentWorkerOperationSignals(ctx, state.name, state.cfg)
	if state.logPath != "" {
		s.streamSessionLogHuma(ctx, send, state.name, state.provider, state.logPath, state.resolveLogPath, workerOps)
	} else {
		s.streamPeekOutputHuma(ctx, send, state.name, s.agentWorkerHandle(state.name, state.cfg), workerOps)
	}
}
