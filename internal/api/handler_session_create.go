package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

var errSessionTemplateNotFound = errors.New("session template not found")

type sessionCreateRequest struct {
	// Kind discriminates the session target: "agent" or "provider".
	Kind              string            `json:"kind,omitempty"`
	Name              string            `json:"name,omitempty"`
	Alias             string            `json:"alias,omitempty"`
	LegacySessionName *string           `json:"session_name,omitempty"`
	Message           string            `json:"message,omitempty"`
	Async             bool              `json:"async,omitempty"`
	Options           map[string]string `json:"options,omitempty"`
	// ProjectID is an opaque identifier for the MC project context.
	// Stored in bead metadata for session-to-project association.
	ProjectID string `json:"project_id,omitempty"`
	Title     string `json:"title,omitempty"`
}

func (s *Server) handleSessionCreate(w http.ResponseWriter, r *http.Request) {
	store := s.state.CityBeadStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no bead store configured")
		return
	}

	var body sessionCreateRequest
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	if body.LegacySessionName != nil {
		writeError(w, http.StatusBadRequest, "invalid", "session_name is no longer accepted; use alias")
		return
	}

	kind := body.Kind
	name := body.Name
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid", "name is required")
		return
	}
	if kind != "agent" && kind != "provider" {
		writeError(w, http.StatusBadRequest, "invalid_kind", "kind must be 'agent' or 'provider'")
		return
	}

	idemKey := scopedIdemKey(r, r.Header.Get("Idempotency-Key"))
	var bodyHash string
	if idemKey != "" {
		bodyHash = hashBody(body)
		if s.idem.handleIdempotent(w, idemKey, bodyHash) {
			return
		}
	}

	var resolved *config.ResolvedProvider
	var workDir, transport, template string
	var optMeta map[string]string

	switch kind {
	case "agent":
		var err error
		resolved, _, transport, template, err = s.resolveSessionTemplateForCreate(name)
		if err != nil {
			if errors.Is(err, errSessionTemplateNotFound) {
				s.idem.unreserve(idemKey)
				writeError(w, http.StatusNotFound, "agent_not_found", "agent '"+name+"' not found")
				return
			}
			s.idem.unreserve(idemKey)
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		transport, err = validateSessionTransport(resolved, transport, s.state.SessionProvider())
		if err != nil {
			s.idem.unreserve(idemKey)
			writeError(w, http.StatusServiceUnavailable, "provider_unavailable", err.Error())
			return
		}
		// Agent track stores a transport-aligned base command only.
		// Do NOT inject OptionsSchema defaults or explicit overrides here.
		// Options are stored as template_overrides and applied at start time
		// by the session lifecycle via ResolveExplicitOptions.
		if len(body.Options) > 0 {
			if len(resolved.OptionsSchema) == 0 {
				s.idem.unreserve(idemKey)
				writeError(w, http.StatusBadRequest, "unknown_option", "agent '"+name+"' does not accept options")
				return
			}
			// Validate options against the schema without applying defaults.
			if _, err := config.ResolveExplicitOptions(resolved.OptionsSchema, body.Options); err != nil {
				s.idem.unreserve(idemKey)
				if errors.Is(err, config.ErrUnknownOption) {
					writeError(w, http.StatusBadRequest, "unknown_option", err.Error())
					return
				}
				writeError(w, http.StatusBadRequest, "invalid_option_value", err.Error())
				return
			}
		}

	case "provider":
		s.createProviderSession(w, r, store, body, name, idemKey, bodyHash)
		return
	}

	title := body.Title
	if title == "" {
		title = template
	}

	alias, err := session.ValidateAlias(body.Alias)
	if err != nil {
		s.idem.unreserve(idemKey)
		writeSessionManagerError(w, err)
		return
	}
	createCtx, err := s.resolveAgentCreateContext(template, alias)
	if err != nil {
		s.idem.unreserve(idemKey)
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	alias = createCtx.Alias
	workDir = createCtx.WorkDir

	mcpServers, err := s.sessionMCPServers(template, resolved.Name, createCtx.Identity, workDir, transport, kind)
	if err != nil {
		s.idem.unreserve(idemKey)
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	launchCommand, err := config.BuildProviderLaunchCommandWithoutOptions(s.state.CityPath(), resolved, transport)
	if err != nil {
		s.idem.unreserve(idemKey)
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	command := launchCommand.Command

	// Build template_overrides metadata. Includes schema overrides AND
	// the initial message (as "initial_message" key). The reconciler
	// handles both: schema overrides map to CLI flags, initial_message
	// is appended to the prompt on first start only.
	extraMeta := sessionTemplateOverridesMetadata(body.Options, body.Message)
	if extraMeta == nil {
		extraMeta = make(map[string]string)
	}
	extraMeta["agent_name"] = createCtx.Identity
	extraMeta["session_origin"] = "ephemeral"
	if transport == "acp" {
		extraMeta, err = session.WithStoredMCPMetadata(extraMeta, createCtx.Identity, mcpServers)
		if err != nil {
			s.idem.unreserve(idemKey)
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
	}

	// Agent sessions always use async (bead-only) creation. The reconciler
	// starts the agent process on the next tick. This avoids blocking the
	// HTTP response for 10-30s while the agent boots in tmux, and lets MC
	// show the session in the sidebar immediately via optimistic UI.
	resolvedCfg, err := resolvedSessionConfigForProvider(alias, createCtx.ExplicitName, template, title, transport, extraMeta, resolved, command, workDir, mcpServers)
	if err != nil {
		s.idem.unreserve(idemKey)
		writeSessionManagerError(w, err)
		return
	}
	handle, err := s.newResolvedWorkerSessionHandle(store, resolvedCfg)
	if err != nil {
		s.idem.unreserve(idemKey)
		writeSessionManagerError(w, err)
		return
	}
	var info session.Info
	reservationIDs := []string{alias, createCtx.ExplicitName}
	reserveConcreteIdentity := createCtx.Agent.SupportsMultipleSessions() && strings.TrimSpace(createCtx.Identity) != ""
	if reserveConcreteIdentity {
		reservationIDs = append(reservationIDs, createCtx.Identity)
	}
	err = session.WithCitySessionIdentifierLocks(s.state.CityPath(), reservationIDs, func() error {
		if err := session.EnsureAliasAvailableWithConfig(store, s.state.Config(), alias, ""); err != nil {
			return err
		}
		if reserveConcreteIdentity && createCtx.Identity != alias {
			if err := session.EnsureAliasAvailableWithConfig(store, s.state.Config(), createCtx.Identity, ""); err != nil {
				return err
			}
		}
		if err := session.EnsureSessionNameAvailableWithConfig(store, s.state.Config(), createCtx.ExplicitName, ""); err != nil {
			return err
		}
		var createErr error
		info, createErr = handle.Create(r.Context(), worker.CreateModeDeferred)
		return createErr
	})
	if err != nil {
		s.idem.unreserve(idemKey)
		writeSessionManagerError(w, err)
		return
	}

	// Persist kind, option metadata, and project_id on the bead.
	// NOTE: template_overrides (options + initial_message) is already set via
	// extraMeta in CreateAliasedBeadOnlyNamedWithMetadata above. Do NOT
	// overwrite it here — the old code clobbered initial_message by writing
	// only the options portion.
	s.persistSessionMeta(store, info.ID, "agent", body.ProjectID, optMeta)
	s.state.Poke() // wake reconciler to start the agent

	// Auto-generate a title from the user's message if no explicit title was provided.
	titleProvider := s.resolveTitleProvider()
	MaybeGenerateTitleAsync(store, info.ID, body.Title, body.Message, titleProvider, info.WorkDir, func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "session %s: "+format+"\n", append([]any{info.ID}, args...)...)
	})

	resp := sessionToResponse(info, s.state.Config())
	resp.Kind = "agent"
	if catalog, catErr := s.workerSessionCatalog(store); catErr == nil {
		if caps, capErr := catalog.SubmissionCapabilities(info.ID); capErr == nil {
			resp.SubmissionCapabilities = caps
		}
	}
	if handle, handleErr := s.workerHandleForSession(store, info.ID); handleErr == nil {
		s.enrichSessionResponse(&resp, info, s.state.Config(), handle, false, true, true)
	}
	statusCode := http.StatusAccepted // always async for agent sessions
	s.idem.storeResponse(idemKey, bodyHash, statusCode, resp)
	writeJSON(w, statusCode, resp)
}

// createProviderSession handles the "provider" kind session creation.
// Resolves a bare provider (not an agent template) and creates a session.
func (s *Server) createProviderSession(w http.ResponseWriter, r *http.Request, store beads.Store, body sessionCreateRequest, providerName, idemKey, bodyHash string) {
	cfg := s.state.Config()
	if cfg == nil {
		s.idem.unreserve(idemKey)
		writeError(w, http.StatusServiceUnavailable, "unavailable", "city config not loaded yet")
		return
	}
	resolved, err := config.ResolveProvider(
		&config.Agent{Provider: providerName},
		&cfg.Workspace,
		cfg.Providers,
		exec.LookPath,
	)
	if err != nil {
		s.idem.unreserve(idemKey)
		if errors.Is(err, config.ErrProviderNotInPATH) {
			writeError(w, http.StatusServiceUnavailable, "provider_unavailable", err.Error())
			return
		}
		if errors.Is(err, config.ErrProviderNotFound) {
			writeError(w, http.StatusNotFound, "provider_not_found", "provider '"+providerName+"' not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	// Resolve options against the provider's schema.
	var optMeta map[string]string
	if len(body.Options) > 0 && len(resolved.OptionsSchema) == 0 {
		s.idem.unreserve(idemKey)
		writeError(w, http.StatusBadRequest, "unknown_option", "provider '"+providerName+"' does not accept options")
		return
	}
	if len(resolved.OptionsSchema) > 0 {
		var optErr error
		_, optMeta, optErr = config.ResolveOptions(resolved.OptionsSchema, body.Options, resolved.EffectiveDefaults)
		if optErr != nil {
			s.idem.unreserve(idemKey)
			if errors.Is(optErr, config.ErrUnknownOption) {
				writeError(w, http.StatusBadRequest, "unknown_option", optErr.Error())
				return
			}
			writeError(w, http.StatusBadRequest, "invalid_option_value", optErr.Error())
			return
		}
	}

	template := providerName
	title := body.Title
	if title == "" {
		title = resolved.Name
	}
	if body.Async && strings.TrimSpace(body.Message) != "" {
		s.idem.unreserve(idemKey)
		writeError(w, http.StatusBadRequest, "invalid", "message is not supported with async session creation; create the session, then POST /v0/session/{id}/messages")
		return
	}
	if body.Async {
		s.idem.unreserve(idemKey)
		writeError(w, http.StatusBadRequest, "invalid", "async session creation is only supported for configured agent templates")
		return
	}

	workDir := s.state.CityPath()

	alias, err := session.ValidateAlias(body.Alias)
	if err != nil {
		s.idem.unreserve(idemKey)
		writeSessionManagerError(w, err)
		return
	}
	mcpIdentity, err := providerSessionMCPIdentity(providerName, alias)
	if err != nil {
		s.idem.unreserve(idemKey)
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	transport, err := providerSessionTransport(resolved, s.state.SessionProvider())
	if err != nil {
		s.idem.unreserve(idemKey)
		writeError(w, http.StatusServiceUnavailable, "provider_unavailable", err.Error())
		return
	}
	launchCommand, err := config.BuildProviderLaunchCommand(s.state.CityPath(), resolved, body.Options, transport)
	if err != nil {
		s.idem.unreserve(idemKey)
		if errors.Is(err, config.ErrUnknownOption) {
			writeError(w, http.StatusBadRequest, "unknown_option", err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_option_value", err.Error())
		return
	}
	command := launchCommand.Command
	mcpServers, err := s.providerSessionMCPServers(providerName, mcpIdentity, workDir, transport)
	if err != nil {
		s.idem.unreserve(idemKey)
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	extraMeta := map[string]string{
		"session_origin": "manual",
	}
	if transport == "acp" {
		extraMeta, err = session.WithStoredMCPMetadata(extraMeta, mcpIdentity, mcpServers)
		if err != nil {
			s.idem.unreserve(idemKey)
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
	}

	resolvedCfg, err := resolvedSessionConfigForProvider(alias, "", template, title, transport, extraMeta, resolved, command, workDir, mcpServers)
	if err != nil {
		s.idem.unreserve(idemKey)
		writeSessionManagerError(w, err)
		return
	}
	handle, err := s.newResolvedWorkerSessionHandle(store, resolvedCfg)
	if err != nil {
		s.idem.unreserve(idemKey)
		writeSessionManagerError(w, err)
		return
	}
	var info session.Info
	err = session.WithCitySessionAliasLock(s.state.CityPath(), alias, func() error {
		if err := session.EnsureAliasAvailableWithConfig(store, s.state.Config(), alias, ""); err != nil {
			return err
		}
		var createErr error
		info, createErr = handle.Create(r.Context(), worker.CreateModeStarted)
		return createErr
	})
	if err != nil {
		s.idem.unreserve(idemKey)
		writeSessionManagerError(w, err)
		return
	}

	// Persist kind, option metadata, and project_id on the bead.
	s.persistSessionMeta(store, info.ID, "provider", body.ProjectID, optMeta)
	if body.Async {
		s.state.Poke()
	}

	// Auto-generate a title from the user's message if no explicit title was provided.
	titleProvider := s.resolveTitleProvider()
	MaybeGenerateTitleAsync(store, info.ID, body.Title, body.Message, titleProvider, info.WorkDir, func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "session %s: "+format+"\n", append([]any{info.ID}, args...)...)
	})

	// Deliver initial message if provided.
	if msg := strings.TrimSpace(body.Message); msg != "" {
		if _, sendErr := s.submitMessageToSession(r.Context(), store, info.ID, msg, session.SubmitIntentDefault); sendErr != nil {
			log.Printf("session %s: initial message delivery failed: %v", info.ID, sendErr)
			rollbackErr := s.rollbackCreatedSession(store, info.ID)
			s.idem.unreserve(idemKey)
			if rollbackErr != nil {
				writeError(w, http.StatusInternalServerError, "message_delivery_failed",
					fmt.Sprintf("initial message delivery failed: %v (rollback failed: %v)", sendErr, rollbackErr))
				return
			}
			writeError(w, http.StatusInternalServerError, "message_delivery_failed",
				fmt.Sprintf("initial message delivery failed: %v", sendErr))
			return
		}
	}

	resp := sessionToResponse(info, s.state.Config())
	resp.Kind = "provider"
	if catalog, catErr := s.workerSessionCatalog(store); catErr == nil {
		if caps, capErr := catalog.SubmissionCapabilities(info.ID); capErr == nil {
			resp.SubmissionCapabilities = caps
		}
	}
	if handle, handleErr := s.workerHandleForSession(store, info.ID); handleErr == nil {
		s.enrichSessionResponse(&resp, info, s.state.Config(), handle, false, true, true)
	}
	statusCode := http.StatusCreated
	s.idem.storeResponse(idemKey, bodyHash, statusCode, resp)
	writeJSON(w, statusCode, resp)
}

func sessionTemplateOverridesMetadata(options map[string]string, message string) map[string]string {
	allOverrides := make(map[string]string, len(options)+1)
	for k, v := range options {
		allOverrides[k] = v
	}
	if msg := strings.TrimSpace(message); msg != "" {
		allOverrides["initial_message"] = msg
	}
	if len(allOverrides) == 0 {
		return nil
	}
	overridesJSON, err := json.Marshal(allOverrides)
	if err != nil {
		return nil
	}
	return map[string]string{"template_overrides": string(overridesJSON)}
}

func (s *Server) rollbackCreatedSession(store beads.Store, sessionID string) error {
	if store == nil || strings.TrimSpace(sessionID) == "" {
		return nil
	}
	if err := s.sessionManager(store).Close(sessionID); err != nil {
		return fmt.Errorf("close created session: %w", err)
	}
	if err := store.Delete(sessionID); err != nil {
		return fmt.Errorf("delete created session bead: %w", err)
	}
	return nil
}

// persistSessionMeta writes option metadata and project_id to the session bead.
func (s *Server) persistSessionMeta(store beads.Store, sessionID, kind, projectID string, optMeta map[string]string) {
	batch := make(map[string]string)
	for k, v := range optMeta {
		batch[k] = v
	}
	if kind != "" && kind != "provider" {
		batch["mc_session_kind"] = kind
	}
	if projectID != "" {
		batch["mc_project_id"] = projectID
	}
	if len(batch) > 0 {
		if err := store.SetMetadataBatch(sessionID, batch); err != nil {
			log.Printf("persistSessionMeta: session %s: %v", sessionID, err)
		}
	}
}
