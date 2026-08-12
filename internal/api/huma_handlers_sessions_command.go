package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync/atomic"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
	"github.com/gastownhall/gascity/internal/worker"
)

// Command-side session handlers (create, patch, submit, message, stop, kill,
// respond, suspend, close, wake, rename). Split out of huma_handlers_sessions.go
// to isolate mutation logic from reads and streaming.

var (
	sessionMessageAsyncTimeout      = sessionMessageTimeout
	sessionCreateCommandableTimeout = 120 * time.Second
)

type sessionCommandableWaiter interface {
	WaitForSessionCommandable(context.Context, string) (session.Info, error)
}

func (s *Server) humaHandleSessionCreate(ctx context.Context, input *SessionCreateInput) (*SessionCreateOutput, error) {
	store := s.state.SessionsBeadStore()
	if store.Store == nil {
		return nil, apierr.ServiceUnavailable.Msg("no bead store configured")
	}

	body := input.Body
	if body.LegacySessionName != nil {
		return nil, apierr.InvalidRequest.Msg("session_name is no longer accepted; use alias")
	}

	kind := body.Kind
	name := body.Name
	if name == "" {
		return nil, apierr.InvalidRequest.Msg("name is required")
	}
	if kind != "agent" && kind != "provider" {
		return nil, apierr.InvalidRequest.Msg("kind must be 'agent' or 'provider'")
	}

	if kind == "provider" {
		return s.humaCreateProviderSession(ctx, store, body, name)
	}

	// Agent track.
	resolved, workDir, transport, template, err := s.resolveSessionTemplateWithBareNameFallback(name)
	if err != nil {
		if errors.Is(err, errSessionTemplateNotFound) {
			return nil, apierr.AgentNotFound.Msg("agent '" + name + "' not found")
		}
		return nil, apierr.Internal.Msg(err.Error())
	}
	transport, err = validateSessionTransport(resolved, transport, s.state.SessionProvider())
	if err != nil {
		return nil, apierr.ServiceUnavailable.Msg(err.Error())
	}

	if len(body.Options) > 0 {
		if len(resolved.OptionsSchema) == 0 {
			return nil, apierr.InvalidRequest.Msg("agent '" + name + "' does not accept options")
		}
		if _, optErr := config.ResolveExplicitOptions(resolved.OptionsSchema, body.Options); optErr != nil {
			if errors.Is(optErr, config.ErrUnknownOption) {
				return nil, apierr.InvalidRequest.Msg(optErr.Error())
			}
			return nil, apierr.InvalidRequest.Msg(optErr.Error())
		}
	}

	title := body.Title
	if title == "" {
		title = template
	}

	alias, err := session.ValidateAlias(body.Alias)
	if err != nil {
		return nil, humaSessionManagerError(err)
	}
	cfg := s.state.Config()
	if cfg == nil {
		return nil, apierr.Internal.Msg("no city config loaded")
	}
	createCtx, err := s.resolveAgentCreateContext(template, alias)
	if err != nil {
		return nil, apierr.Internal.Msg(err.Error())
	}
	agentCfg := createCtx.Agent
	alias = createCtx.Alias
	explicitName := createCtx.ExplicitName
	workDirQualifiedName := createCtx.Identity
	workDir = createCtx.WorkDir

	launchCommand, err := config.BuildProviderLaunchCommandWithoutOptions(s.state.CityPath(), resolved, transport)
	if err != nil {
		return nil, apierr.Internal.Msg(err.Error())
	}
	command := launchCommand.Command
	extraMeta := sessionTemplateOverridesMetadata(body.Options, body.Message)
	if extraMeta == nil {
		extraMeta = make(map[string]string)
	}
	extraMeta["agent_name"] = workDirQualifiedName
	extraMeta["session_origin"] = "manual"
	mcpServers, err := s.sessionMCPServers(template, resolved.Name, workDirQualifiedName, workDir, transport, kind, nil)
	if err != nil {
		return nil, apierr.Internal.Msg(err.Error())
	}

	reqID, reqIDErr := newRequestID()
	if reqIDErr != nil {
		return nil, apierr.Internal.Msg(reqIDErr.Error())
	}
	eventCursor, cursorErr := s.currentCityEventCursor()
	if cursorErr != nil {
		return nil, apierr.Internal.Msg(cursorErr.Error())
	}

	go func() {
		defer s.recoverAsRequestFailed(reqID, RequestOperationSessionCreate)
		if transport == "acp" {
			var mcpMetaErr error
			extraMeta, mcpMetaErr = session.WithStoredMCPMetadata(extraMeta, workDirQualifiedName, mcpServers)
			if mcpMetaErr != nil {
				s.emitSessionCreateFailed(reqID, "mcp_metadata_failed", mcpMetaErr.Error())
				return
			}
		}
		resolvedCfg, cfgErr := resolvedSessionConfigForProvider(
			s.state.CityPath(),
			configuredWorkspaceSessionEnv(s.state.Config()),
			alias,
			explicitName,
			template,
			title,
			transport,
			extraMeta,
			resolved,
			command,
			workDir,
			mcpServers,
		)
		if cfgErr != nil {
			s.emitSessionCreateFailed(reqID, "create_failed", cfgErr.Error())
			return
		}
		handle, handleErr := s.newResolvedWorkerSessionHandle(store.Store, resolvedCfg)
		if handleErr != nil {
			s.emitSessionCreateFailed(reqID, "create_failed", handleErr.Error())
			return
		}
		var info session.Info
		createMode := worker.CreateModeStarted
		waiter, waitForCommandable := s.state.(sessionCommandableWaiter)
		if waitForCommandable {
			createMode = worker.CreateModeDeferred
		}
		reservationIDs := []string{alias, explicitName}
		reserveConcreteIdentity := agentCfg.SupportsMultipleSessions() && strings.TrimSpace(workDirQualifiedName) != ""
		if reserveConcreteIdentity {
			reservationIDs = append(reservationIDs, workDirQualifiedName)
		}
		createErr := session.WithCitySessionIdentifierLocks(s.state.CityPath(), reservationIDs, func() error {
			if aliasErr := session.EnsureAliasAvailableWithConfig(store.Store, s.state.Config(), alias, ""); aliasErr != nil {
				return aliasErr
			}
			if reserveConcreteIdentity && workDirQualifiedName != alias {
				if aliasErr := session.EnsureAliasAvailableWithConfig(store.Store, s.state.Config(), workDirQualifiedName, ""); aliasErr != nil {
					return aliasErr
				}
			}
			if nameErr := session.EnsureSessionNameAvailableWithConfig(store.Store, s.state.Config(), explicitName, ""); nameErr != nil {
				return nameErr
			}
			var err error
			info, err = handle.Create(context.Background(), createMode)
			return err
		})
		if createErr != nil {
			s.emitSessionCreateFailed(reqID, "create_failed", createErr.Error())
			return
		}
		if waitForCommandable {
			s.state.Poke()
			waitCtx, cancel := context.WithTimeout(context.Background(), sessionCreateCommandableTimeout)
			info, createErr = waiter.WaitForSessionCommandable(waitCtx, info.ID)
			cancel()
			if createErr != nil {
				s.emitSessionCreateFailed(reqID, "create_failed", createErr.Error())
				return
			}
		}

		resp := sessionToResponse(info, s.state.Config())
		resp.Kind = "agent"
		s.emitSessionCreateSucceeded(reqID, resp)
		s.persistSessionMeta(store, info.ID, body.ProjectID, nil)
		if !waitForCommandable {
			s.state.Poke()
		}

		titleProvider := s.resolveTitleProvider()
		MaybeGenerateTitleAsync(store, info.ID, body.Title, body.Message, titleProvider, info.WorkDir, func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "session %s: "+format+"\n", append([]any{info.ID}, args...)...)
		})
	}()

	out := &SessionCreateOutput{Status: http.StatusAccepted}
	out.Body.Status = "accepted"
	out.Body.RequestID = reqID
	out.Body.EventCursor = eventCursor
	return out, nil
}

// humaCreateProviderSession handles the "provider" kind session creation.

func (s *Server) humaCreateProviderSession(_ context.Context, store beads.SessionStore, body sessionCreateBody, providerName string) (*SessionCreateOutput, error) {
	cfg := s.state.Config()
	if cfg == nil {
		return nil, apierr.ServiceUnavailable.Msg("city config not loaded yet")
	}
	resolved, err := config.ResolveProvider(
		&config.Agent{Provider: providerName},
		&cfg.Workspace,
		cfg.Providers,
		exec.LookPath,
	)
	if err != nil {
		if errors.Is(err, config.ErrProviderNotInPATH) {
			return nil, apierr.ServiceUnavailable.Msg(err.Error())
		}
		if errors.Is(err, config.ErrProviderNotFound) {
			return nil, apierr.ProviderNotFound.Msg("provider '" + providerName + "' not found")
		}
		return nil, apierr.Internal.Msg(err.Error())
	}

	var optMeta map[string]string
	if len(body.Options) > 0 && len(resolved.OptionsSchema) == 0 {
		return nil, apierr.InvalidRequest.Msg("provider '" + providerName + "' does not accept options")
	}
	if len(resolved.OptionsSchema) > 0 {
		var optErr error
		_, optMeta, optErr = config.ResolveOptions(resolved.OptionsSchema, body.Options, resolved.EffectiveDefaults)
		if optErr != nil {
			if errors.Is(optErr, config.ErrUnknownOption) {
				return nil, apierr.InvalidRequest.Msg(optErr.Error())
			}
			return nil, apierr.InvalidRequest.Msg(optErr.Error())
		}
	}

	template := providerName
	title := body.Title
	if title == "" {
		title = resolved.Name
	}
	if body.Async && strings.TrimSpace(body.Message) != "" {
		return nil, apierr.InvalidRequest.Msg("message is not supported with async session creation; create the session, then POST /v0/session/{id}/messages")
	}
	if body.Async {
		return nil, apierr.InvalidRequest.Msg("async session creation is only supported for configured agent templates")
	}

	workDir := s.state.CityPath()

	alias, err := session.ValidateAlias(body.Alias)
	if err != nil {
		return nil, humaSessionManagerError(err)
	}
	mcpIdentity, err := providerSessionMCPIdentity(resolved.Name, alias)
	if err != nil {
		return nil, apierr.Internal.Msg(err.Error())
	}

	transport, err := providerSessionTransport(resolved, s.state.SessionProvider())
	if err != nil {
		return nil, apierr.ServiceUnavailable.Msg(err.Error())
	}
	launchCommand, err := config.BuildProviderLaunchCommand(s.state.CityPath(), resolved, body.Options, transport)
	if err != nil {
		return nil, apierr.InvalidRequest.Msg(err.Error())
	}
	command := launchCommand.Command
	mcpServers, err := s.providerSessionMCPServers(resolved.Name, mcpIdentity, workDir, transport)
	if err != nil {
		return nil, apierr.Internal.Msg(err.Error())
	}
	extraMeta := sessionTemplateOverridesMetadata(body.Options, body.Message)
	if extraMeta == nil {
		extraMeta = make(map[string]string)
	}
	extraMeta["session_origin"] = "manual"
	if transport == "acp" {
		extraMeta, err = session.WithStoredMCPMetadata(extraMeta, mcpIdentity, mcpServers)
		if err != nil {
			return nil, apierr.Internal.Msg(err.Error())
		}
	}

	reqID, reqIDErr := newRequestID()
	if reqIDErr != nil {
		return nil, apierr.Internal.Msg(reqIDErr.Error())
	}
	eventCursor, cursorErr := s.currentCityEventCursor()
	if cursorErr != nil {
		return nil, apierr.Internal.Msg(cursorErr.Error())
	}
	go func() {
		defer s.recoverAsRequestFailed(reqID, RequestOperationSessionCreate)
		resolvedCfg, cfgErr := resolvedSessionConfigForProvider(s.state.CityPath(), configuredWorkspaceSessionEnv(s.state.Config()), alias, "", template, title, transport, extraMeta, resolved, command, workDir, mcpServers)
		if cfgErr != nil {
			s.emitSessionCreateFailed(reqID, "create_failed", cfgErr.Error())
			return
		}
		handle, handleErr := s.newResolvedWorkerSessionHandle(store.Store, resolvedCfg)
		if handleErr != nil {
			s.emitSessionCreateFailed(reqID, "create_failed", handleErr.Error())
			return
		}
		var info session.Info
		createErr := session.WithCitySessionAliasLock(s.state.CityPath(), alias, func() error {
			if aliasErr := session.EnsureAliasAvailableWithConfig(store.Store, s.state.Config(), alias, ""); aliasErr != nil {
				return aliasErr
			}
			var err error
			info, err = handle.Create(context.Background(), worker.CreateModeStarted)
			return err
		})
		if createErr != nil {
			s.emitSessionCreateFailed(reqID, "create_failed", createErr.Error())
			return
		}
		if msg := strings.TrimSpace(body.Message); msg != "" {
			if _, sendErr := s.submitMessageToSession(context.Background(), store.Store, info.ID, msg, session.SubmitIntentDefault); sendErr != nil {
				if rollbackErr := s.rollbackCreatedSession(store, info.ID); rollbackErr != nil {
					s.emitSessionCreateFailed(reqID, "message_delivery_failed",
						fmt.Sprintf("initial message delivery failed: %v (rollback failed: %v)", sendErr, rollbackErr))
					return
				}
				s.emitSessionCreateFailed(reqID, "message_delivery_failed", fmt.Sprintf("initial message delivery failed: %v", sendErr))
				return
			}
		}
		resp := sessionToResponse(info, s.state.Config())
		resp.Kind = "provider"
		s.emitSessionCreateSucceeded(reqID, resp)
		s.persistSessionMeta(store, info.ID, body.ProjectID, optMeta)
		titleProvider := s.resolveTitleProvider()
		MaybeGenerateTitleAsync(store, info.ID, body.Title, body.Message, titleProvider, info.WorkDir, func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "session %s: "+format+"\n", append([]any{info.ID}, args...)...)
		})
	}()

	out := &SessionCreateOutput{Status: http.StatusAccepted}
	out.Body.Status = "accepted"
	out.Body.RequestID = reqID
	out.Body.EventCursor = eventCursor
	return out, nil
}

// --- Session Transcript ---

// sessionTranscriptGetResponse is the runtime container for conversation,
// raw, and structured transcript responses. Its OpenAPI schema is a
// discriminated union so generated clients never see raw provider frames on
// the structured response branch.
type sessionTranscriptGetResponse struct {
	ID                 string                      `json:"id"`
	Template           string                      `json:"template"`
	Provider           string                      `json:"provider" doc:"Producing provider identifier (claude, codex, gemini, opencode, etc.). Consumers use this to dispatch per-provider frame parsing."`
	Format             string                      `json:"format" doc:"conversation, text, raw, or structured."`
	SchemaVersion      string                      `json:"schema_version,omitempty" doc:"Structured session transcript schema version when format is structured."`
	Operation          string                      `json:"operation,omitempty" doc:"Structured response application mode. REST structured transcripts are snapshots."`
	ResetReason        string                      `json:"reset_reason,omitempty" doc:"Structured reset reason when operation is reset."`
	History            *SessionStructuredHistory   `json:"history,omitempty" doc:"Normalized worker-history envelope when format is structured."`
	Turns              []outputTurn                `json:"turns,omitempty" doc:"Populated for conversation/text formats."`
	Messages           *[]SessionRawMessageFrame   `json:"messages,omitempty" doc:"Populated for raw format; provider-native frames emitted verbatim as the provider wrote them."`
	StructuredMessages *[]SessionStructuredMessage `json:"structured_messages,omitempty" doc:"Populated for structured format; provider-normalized structured messages."`
	Pagination         *sessionlog.PaginationInfo  `json:"pagination,omitempty"`
}

type sessionTranscriptConversationResponse struct {
	ID         string                     `json:"id"`
	Template   string                     `json:"template"`
	Provider   string                     `json:"provider" doc:"Producing provider identifier (claude, codex, gemini, opencode, etc.)."`
	Format     string                     `json:"format" enum:"conversation,text" doc:"Conversation or text transcript format."`
	Turns      []outputTurn               `json:"turns,omitempty" doc:"Conversation/text transcript turns."`
	Pagination *sessionlog.PaginationInfo `json:"pagination,omitempty"`
}

type sessionTranscriptRawResponse struct {
	ID         string                     `json:"id"`
	Template   string                     `json:"template"`
	Provider   string                     `json:"provider" doc:"Producing provider identifier (claude, codex, gemini, opencode, etc.). Consumers use this to dispatch per-provider frame parsing."`
	Format     string                     `json:"format" enum:"raw" doc:"Raw provider-native transcript format."`
	Messages   []SessionRawMessageFrame   `json:"messages" doc:"Provider-native transcript frames emitted only for raw format."`
	Pagination *sessionlog.PaginationInfo `json:"pagination,omitempty"`
}

type sessionTranscriptStructuredResponse struct {
	ID                 string                     `json:"id"`
	Template           string                     `json:"template"`
	Provider           string                     `json:"provider" doc:"Producing provider identifier (claude, codex, gemini, opencode, etc.)."`
	Format             string                     `json:"format" enum:"structured" doc:"Structured provider-neutral transcript format."`
	SchemaVersion      string                     `json:"schema_version" enum:"session.structured.v1" doc:"Structured session transcript schema version."`
	Operation          string                     `json:"operation" enum:"snapshot" doc:"Always snapshot for a REST structured transcript."`
	History            *SessionStructuredHistory  `json:"history" doc:"Normalized worker-history envelope when format is structured."`
	StructuredMessages []SessionStructuredMessage `json:"structured_messages" doc:"Provider-normalized structured messages."`
	Pagination         *sessionlog.PaginationInfo `json:"pagination,omitempty"`
}

func nonNilStructuredMessages(messages []SessionStructuredMessage) []SessionStructuredMessage {
	if messages == nil {
		return []SessionStructuredMessage{}
	}
	return messages
}

func structuredMessagesField(messages []SessionStructuredMessage) *[]SessionStructuredMessage {
	messages = nonNilStructuredMessages(messages)
	return &messages
}

func nonNilRawMessages(messages []SessionRawMessageFrame) []SessionRawMessageFrame {
	if messages == nil {
		return []SessionRawMessageFrame{}
	}
	return messages
}

func rawMessagesField(messages []SessionRawMessageFrame) *[]SessionRawMessageFrame {
	messages = nonNilRawMessages(messages)
	return &messages
}

func structuredTranscriptMessages(response sessionTranscriptGetResponse) []SessionStructuredMessage {
	if response.StructuredMessages == nil {
		return nil
	}
	return *response.StructuredMessages
}

func rawTranscriptMessages(response sessionTranscriptGetResponse) []SessionRawMessageFrame {
	if response.Messages == nil {
		return nil
	}
	return *response.Messages
}

// Schema publishes session transcript responses as a discriminated union over
// the format field, keeping provider-native raw frames out of the structured
// response schema while preserving the compact runtime container above.
func (sessionTranscriptGetResponse) Schema(r huma.Registry) *huma.Schema {
	const name = "SessionTranscriptGetResponse"
	if _, ok := r.Map()[name]; !ok {
		variants := []struct {
			format string
			name   string
			typ    reflect.Type
		}{
			{format: "conversation", name: "SessionTranscriptConversationResponse", typ: reflect.TypeOf(sessionTranscriptConversationResponse{})},
			{format: "text", name: "SessionTranscriptConversationResponse", typ: reflect.TypeOf(sessionTranscriptConversationResponse{})},
			{format: "raw", name: "SessionTranscriptRawResponse", typ: reflect.TypeOf(sessionTranscriptRawResponse{})},
			{format: "structured", name: "SessionTranscriptStructuredResponse", typ: reflect.TypeOf(sessionTranscriptStructuredResponse{})},
		}
		oneOf := make([]*huma.Schema, 0, 3)
		mapping := make(map[string]string, len(variants))
		seen := make(map[string]bool, 3)
		for _, variant := range variants {
			ref := schemaRefPrefix + variant.name
			if _, ok := r.Map()[variant.name]; !ok {
				r.Schema(variant.typ, true, variant.name)
			}
			if !seen[variant.name] {
				oneOf = append(oneOf, &huma.Schema{Ref: ref})
				seen[variant.name] = true
			}
			mapping[variant.format] = ref
		}
		r.Map()[name] = &huma.Schema{
			Title:       "Session transcript response",
			Description: "Discriminated union of session transcript response shapes. Raw provider-native frames are available only on the raw branch; structured responses contain only provider-neutral typed data.",
			OneOf:       oneOf,
			Discriminator: &huma.Discriminator{
				PropertyName: "format",
				Mapping:      mapping,
			},
		}
	}
	return &huma.Schema{Ref: schemaRefPrefix + name}
}

// humaHandleSessionTranscript is the Huma-typed handler for GET /v0/session/{id}/transcript.

func (s *Server) humaHandleSessionPatch(_ context.Context, input *SessionPatchInput) (*IndexOutput[sessionResponse], error) {
	store := s.state.SessionsBeadStore()
	if store.Store == nil {
		return nil, apierr.ServiceUnavailable.Msg("no bead store configured")
	}

	id, err := s.resolveSessionIDWithConfig(store.Store, input.ID)
	if err != nil {
		return nil, humaResolveError(err)
	}

	// Huma has already validated:
	//  - `additionalProperties: false` → unknown fields (e.g. "template") are 422
	//  - `minLength:"1"` on Title → non-empty when provided
	// The handler only needs to enforce "at least one field" and the
	// alias-controller-managed rule below.
	titlePtr := input.Body.Title
	aliasPtr := input.Body.Alias

	if titlePtr == nil && aliasPtr == nil {
		return nil, apierr.ValidationFailed.Msg("at least one of 'title' or 'alias' is required")
	}

	// Validate through the session front door: the codec stays confined inside
	// Store.Get. A present-but-non-session bead yields ErrSessionNotFound → the
	// existing "not a session" 400; an absent id stays on the beads.ErrNotFound
	// chain → 404.
	sessFront := session.NewStore(store)
	info, err := sessFront.Get(id)
	if err != nil {
		if errors.Is(err, session.ErrSessionNotFound) {
			return nil, apierr.InvalidRequest.Msg(id + " is not a session")
		}
		return nil, humaStoreError(err)
	}
	// Preserve the empty-type heal RepairEmptyType performed on the raw bead.
	if info.Type == "" {
		sessFront.RepairTypeBestEffort(id)
	}

	mgr := s.sessionManager(store.Store)
	updateFn := func() error {
		return mgr.UpdatePresentation(id, titlePtr, aliasPtr)
	}
	if aliasPtr != nil {
		// agent_name off the persisted Info from the front door — the
		// controller-managed-alias gate; the codec projection of the persisted
		// agent_name field, with no raw bead in the handler's hands.
		if strings.TrimSpace(info.AgentName) != "" {
			return nil, apierr.Forbidden.Msg("forbidden: alias is controller-managed for this session")
		}
		if lockErr := session.WithCitySessionAliasLock(s.state.CityPath(), *aliasPtr, func() error {
			if avErr := session.EnsureAliasAvailableWithConfig(store.Store, s.state.Config(), *aliasPtr, id); avErr != nil {
				return avErr
			}
			return updateFn()
		}); lockErr != nil {
			return nil, humaSessionManagerError(lockErr)
		}
	} else if err := updateFn(); err != nil {
		return nil, humaSessionManagerError(err)
	}

	info, presponse, err := sessionGetEnriched(session.NewStore(store), mgr, id)
	if err != nil {
		return nil, humaSessionManagerError(err)
	}
	presp := sessionResponseWithReason(info, presponse, s.state.Config(), s.state.SessionProvider(), strings.TrimSpace(s.state.CityPath()) != "")
	return &IndexOutput[sessionResponse]{
		Index: s.latestIndex(),
		Body:  presp,
	}, nil
}

const sessionPermissionModeOptionKey = "permission_mode"

func (s *Server) humaHandleSessionPermissionMode(_ context.Context, input *SessionPermissionModeInput) (*IndexOutput[sessionResponse], error) {
	return s.updateSessionPermissionMode(input.ID, input.Body)
}

func (s *Server) updateSessionPermissionMode(idRef string, body SessionPermissionModeBody) (*IndexOutput[sessionResponse], error) {
	store := s.state.SessionsBeadStore()
	if store.Store == nil {
		return nil, apierr.ServiceUnavailable.Msg("no bead store configured")
	}

	id, err := s.resolveSessionIDAllowClosedWithConfig(store.Store, idRef)
	if err != nil {
		return nil, humaResolveError(err)
	}
	// WI-6 residual: raw-validation lane NOT converted to the session front door,
	// because the raw bead is read downstream — b.Metadata feeds legacySessionKind
	// and resolveProviderForSessionOptions below (provider/options resolution reads
	// the raw metadata map, not projected Info fields). Converting needs an
	// Info/PersistedResponse-fed provider-options resolution; deferred to the
	// front-door flip (WI-7). The 400/404 contract matches the converted siblings.
	b, err := store.Get(id)
	if err != nil {
		return nil, humaStoreError(err)
	}
	if !session.IsSessionBeadOrRepairable(b) {
		return nil, apierr.InvalidRequest.Msg(id + " is not a session")
	}
	session.RepairEmptyType(store.Store, &b)

	mgr := s.sessionManager(store.Store)
	info, err := mgr.Get(id)
	if err != nil {
		return nil, humaSessionManagerError(err)
	}
	if info.Closed {
		return nil, apierr.SessionConflict.Msg("conflict: session is closed")
	}
	if session.IsTemplateOverrideRuntimeActive(info.State) {
		return nil, apierr.SessionConflict.Msg("conflict: session is running; permission_mode changes use schema options and apply only before the next launch")
	}
	cfg := s.state.Config()
	if cfg == nil {
		return nil, apierr.ServiceUnavailable.Msg("city config not loaded yet")
	}
	agent, agentFound := findAgent(cfg, info.Template)
	if session.UseAgentTemplateForProviderResolution(legacySessionKind(b.Metadata), b.Metadata, info.Provider, agent.Provider, agentFound) {
		if !agentFound {
			return nil, apierr.SessionConflict.Msg("conflict: session agent template no longer resolves; restore the template or recreate the session before changing schema options")
		}
	}

	resolved, resolveErr := resolveProviderForSessionOptions(info, b.Metadata, cfg)
	if resolved == nil {
		if resolveErr != nil {
			return nil, apierr.SessionConflict.Msg("conflict: session provider no longer resolves: " + resolveErr.Error())
		}
		return nil, apierr.NotImplemented.Msg("unsupported: session provider does not accept schema options")
	}
	if !providerHasOption(resolved.OptionsSchema, sessionPermissionModeOptionKey) {
		return nil, apierr.NotImplemented.Msg("unsupported: session provider does not define permission_mode in options_schema")
	}

	mode := strings.TrimSpace(body.PermissionMode)
	if _, optErr := config.ResolveExplicitOptions(resolved.OptionsSchema, map[string]string{sessionPermissionModeOptionKey: mode}); optErr != nil {
		return nil, apierr.InvalidRequest.Msg(optErr.Error())
	}

	if _, err := mgr.UpdateTemplateOverrides(id, map[string]string{sessionPermissionModeOptionKey: mode}); err != nil {
		return nil, humaSessionManagerError(err)
	}
	s.state.Poke()

	info, presponse, err := sessionGetEnriched(session.NewStore(store), mgr, id)
	if err != nil {
		return nil, humaSessionManagerError(err)
	}
	resp := sessionResponseWithReason(info, presponse, s.state.Config(), s.state.SessionProvider(), strings.TrimSpace(s.state.CityPath()) != "")
	return &IndexOutput[sessionResponse]{
		Index: s.latestIndex(),
		Body:  resp,
	}, nil
}

func providerHasOption(schema []config.ProviderOption, key string) bool {
	for _, opt := range schema {
		if opt.Key == key {
			return true
		}
	}
	return false
}

// --- Session Submit ---

// humaHandleSessionSubmit is the Huma-typed handler for POST /v0/session/{id}/submit.

func (s *Server) humaHandleSessionSubmit(ctx context.Context, input *SessionSubmitInput) (*SessionSubmitOutput, error) {
	store := s.state.SessionsBeadStore()
	if store.Store == nil {
		return nil, apierr.ServiceUnavailable.Msg("no bead store configured")
	}
	if err := s.sessionTargetDeliverable(ctx, store.Store, input.ID); err != nil {
		if errors.Is(err, session.ErrSessionNotFound) {
			return nil, apierr.SessionNotFound.Msg(fmt.Sprintf("session %q not found and not a configured named session", input.ID))
		}
		// Ambiguous bare names and configured-name/live-bead conflicts are
		// deterministic client addressing errors: map them through the resolve
		// helper so they surface as 409 (matching /stop, /respond, and the
		// synchronous message twin) instead of a 500 from humaStoreError.
		return nil, humaResolveError(err)
	}

	intent := input.Body.Intent
	if intent == "" {
		intent = session.SubmitIntentDefault
	}

	reqID, reqIDErr := newRequestID()
	if reqIDErr != nil {
		return nil, apierr.Internal.Msg(reqIDErr.Error())
	}
	eventCursor, cursorErr := s.currentCityEventCursor()
	if cursorErr != nil {
		return nil, apierr.Internal.Msg(cursorErr.Error())
	}
	message := input.Body.Message
	sessionTarget := input.ID
	go func() {
		defer s.recoverAsRequestFailed(reqID, RequestOperationSessionSubmit)
		id, err := s.resolveSessionIDMaterializingNamedWithContext(context.Background(), store.Store, sessionTarget)
		if err != nil {
			s.emitSessionSubmitFailed(reqID, "resolve_failed", err.Error())
			return
		}
		outcome, submitErr := s.submitMessageToSession(context.Background(), store.Store, id, message, intent)
		if submitErr != nil {
			s.emitSessionSubmitFailed(reqID, "submit_failed", submitErr.Error())
		} else {
			s.emitSessionSubmitSucceeded(reqID, id, outcome.Queued, string(intent))
		}
	}()

	out := &SessionSubmitOutput{}
	out.Body.Status = "accepted"
	out.Body.RequestID = reqID
	out.Body.EventCursor = eventCursor
	return out, nil
}

// --- Session Messages ---

// humaHandleSessionMessage is the Huma-typed handler for POST /v0/session/{id}/messages.

func (s *Server) humaHandleSessionMessage(ctx context.Context, input *SessionMessageInput) (*SessionMessageOutput, error) {
	store := s.state.SessionsBeadStore()
	if store.Store == nil {
		return nil, apierr.ServiceUnavailable.Msg("no bead store configured")
	}
	if err := s.sessionTargetDeliverable(ctx, store.Store, input.ID); err != nil {
		if errors.Is(err, session.ErrSessionNotFound) {
			return nil, apierr.SessionNotFound.Msg(fmt.Sprintf("session %q not found and not a configured named session", input.ID))
		}
		// Ambiguous bare names and configured-name/live-bead conflicts are
		// deterministic client addressing errors: map them through the resolve
		// helper so they surface as 409 (matching /stop, /respond, and the
		// synchronous message twin) instead of a 500 from humaStoreError.
		return nil, humaResolveError(err)
	}

	reqID, reqIDErr := newRequestID()
	if reqIDErr != nil {
		return nil, apierr.Internal.Msg(reqIDErr.Error())
	}
	eventCursor, cursorErr := s.currentCityEventCursor()
	if cursorErr != nil {
		return nil, apierr.Internal.Msg(cursorErr.Error())
	}
	message := input.Body.Message
	sessionTarget := input.ID
	go func() {
		defer s.recoverAsRequestFailed(reqID, RequestOperationSessionMessage)

		type messageResult struct {
			sessionID string
			errorCode string
			err       error
		}

		resultCh := make(chan messageResult, 1)
		var terminalEmitted atomic.Bool
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sendResult := func(result messageResult) {
			if terminalEmitted.Load() {
				if result.err != nil {
					log.Printf("api: late session.message result after timeout request_id=%s target=%s error_code=%s err=%v", reqID, sessionTarget, result.errorCode, result.err)
				} else {
					log.Printf("api: late session.message result after timeout request_id=%s target=%s session_id=%s", reqID, sessionTarget, result.sessionID)
				}
				return
			}
			resultCh <- result
		}
		go func() {
			defer func() {
				if r := recover(); r != nil {
					sendResult(messageResult{errorCode: "internal_error", err: fmt.Errorf("panic: %v", r)})
				}
			}()
			id, err := s.resolveSessionIDMaterializingNamedWithContext(ctx, store.Store, sessionTarget)
			if err != nil {
				sendResult(messageResult{errorCode: "resolve_failed", err: err})
				return
			}
			if err := s.sendUserMessageToSession(ctx, store.Store, id, message); err != nil {
				code := "message_failed"
				if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
					code = "timeout"
				}
				sendResult(messageResult{sessionID: id, errorCode: code, err: err})
				return
			}
			sendResult(messageResult{sessionID: id})
		}()

		timer := time.NewTimer(sessionMessageAsyncTimeout)
		defer timer.Stop()
		select {
		case result := <-resultCh:
			terminalEmitted.Store(true)
			if result.err != nil {
				s.emitSessionMessageFailed(reqID, result.errorCode, result.err.Error())
				return
			}
			s.emitSessionMessageSucceeded(reqID, result.sessionID)
		case <-timer.C:
			cancel()
			select {
			case result := <-resultCh:
				terminalEmitted.Store(true)
				if result.err != nil {
					s.emitSessionMessageFailed(reqID, result.errorCode, result.err.Error())
					return
				}
				s.emitSessionMessageSucceeded(reqID, result.sessionID)
				return
			default:
			}
			terminalEmitted.Store(true)
			s.emitSessionMessageFailed(reqID, "timeout", fmt.Sprintf("session.message timed out after %s", sessionMessageAsyncTimeout))
		}
	}()

	out := &SessionMessageOutput{}
	out.Body.Status = "accepted"
	out.Body.RequestID = reqID
	out.Body.EventCursor = eventCursor
	return out, nil
}

// --- Session Stop ---

// humaHandleSessionStop is the Huma-typed handler for POST /v0/session/{id}/stop.

func (s *Server) humaHandleSessionStop(_ context.Context, input *SessionIDInput) (*OKWithIDResponse, error) {
	store := s.state.SessionsBeadStore()
	if store.Store == nil {
		return nil, apierr.ServiceUnavailable.Msg("no bead store configured")
	}

	id, err := s.resolveSessionIDWithConfig(store.Store, input.ID)
	if err != nil {
		return nil, humaResolveError(err)
	}

	mgr := s.sessionManager(store.Store)
	if err := mgr.StopTurn(id); err != nil {
		return nil, humaSessionManagerError(err)
	}
	out := &OKWithIDResponse{}
	out.Body.Status = "ok"
	out.Body.ID = id
	return out, nil
}

// --- Session Kill ---

// humaHandleSessionKill is the Huma-typed handler for POST /v0/session/{id}/kill.

func (s *Server) humaHandleSessionKill(_ context.Context, input *SessionIDInput) (*OKWithIDResponse, error) {
	store := s.state.SessionsBeadStore()
	if store.Store == nil {
		return nil, apierr.ServiceUnavailable.Msg("no bead store configured")
	}

	id, err := s.resolveSessionIDWithConfig(store.Store, input.ID)
	if err != nil {
		return nil, humaResolveError(err)
	}

	mgr := s.sessionManager(store.Store)
	if err := mgr.Kill(id); err != nil {
		if errors.Is(err, session.ErrSessionClosed) {
			out := &OKWithIDResponse{}
			out.Body.Status = "ok"
			out.Body.ID = id
			return out, nil
		}
		return nil, humaSessionManagerError(err)
	}
	out := &OKWithIDResponse{}
	out.Body.Status = "ok"
	out.Body.ID = id
	return out, nil
}

// --- Session Respond ---

// humaHandleSessionRespond is the Huma-typed handler for POST /v0/session/{id}/respond.

func (s *Server) humaHandleSessionRespond(_ context.Context, input *SessionRespondInput) (*SessionRespondOutput, error) {
	store := s.state.SessionsBeadStore()
	if store.Store == nil {
		return nil, apierr.ServiceUnavailable.Msg("no bead store configured")
	}

	id, err := s.resolveSessionIDWithConfig(store.Store, input.ID)
	if err != nil {
		return nil, humaResolveError(err)
	}

	// Huma validates Body.Action (minLength:1); no handler guard needed.
	mgr := s.sessionManager(store.Store)
	if err := mgr.Respond(id, runtime.InteractionResponse{
		RequestID: input.Body.RequestID,
		Action:    input.Body.Action,
		Text:      input.Body.Text,
		Metadata:  input.Body.Metadata,
	}); err != nil {
		return nil, humaSessionManagerError(err)
	}

	out := &SessionRespondOutput{}
	out.Body.Status = "accepted"
	out.Body.ID = id
	return out, nil
}

// --- Session Suspend ---

// humaHandleSessionSuspend is the Huma-typed handler for POST /v0/session/{id}/suspend.

func (s *Server) humaHandleSessionSuspend(ctx context.Context, input *SessionIDInput) (*OKResponse, error) {
	store := s.state.SessionsBeadStore()
	if store.Store == nil {
		return nil, apierr.ServiceUnavailable.Msg("no bead store configured")
	}
	mgr := s.sessionManager(store.Store)

	id, err := s.resolveSessionIDMaterializingNamedWithContext(ctx, store.Store, input.ID)
	if err != nil {
		return nil, humaResolveError(err)
	}
	if err := mgr.Suspend(id); err != nil {
		return nil, humaSessionManagerError(err)
	}
	out := &OKResponse{}
	out.Body.Status = "ok"
	return out, nil
}

// --- Session Close ---

// humaHandleSessionClose is the Huma-typed handler for POST /v0/session/{id}/close.

func (s *Server) humaHandleSessionClose(ctx context.Context, input *SessionCloseInput) (*OKResponse, error) {
	store := s.state.SessionsBeadStore()
	if store.Store == nil {
		return nil, apierr.ServiceUnavailable.Msg("no bead store configured")
	}

	id, err := s.resolveSessionIDWithConfig(store.Store, input.ID)
	if err != nil {
		return nil, humaResolveError(err)
	}

	if b, getErr := store.Get(id); getErr == nil &&
		strings.TrimSpace(b.Metadata[apiNamedSessionMetadataKey]) == "true" &&
		strings.TrimSpace(b.Metadata[apiNamedSessionModeKey]) == "always" &&
		strings.Contains(strings.TrimSpace(b.Metadata[apiNamedSessionIdentityKey]), "/") {
		return nil, apierr.SessionConflict.Msg("configured always-on named sessions cannot be closed while config-managed")
	}
	handle, err := s.workerHandleForSession(store.Store, id)
	if err != nil {
		return nil, humaSessionManagerError(err)
	}
	closeResult, err := handle.CloseDetailed(ctx)
	if err != nil {
		return nil, humaSessionManagerError(err)
	}
	// Nudge withdrawal reads the nudges class, so it sources the typed
	// NudgesBeadStore (identity to the work store until that class relocates).
	if err := withdrawQueuedWaitNudges(s.state.NudgesBeadStore(), s.state.CityPath(), closeResult.WaitNudgeIDs); err != nil {
		log.Printf("gc api: withdrawing queued wait nudges after close %s: %v", id, err)
	}

	// Optional: permanently delete the bead after closing.
	if input.Delete {
		if err := deleteSessionBeadAfterClose(store.Store, id); err != nil {
			log.Printf("gc api: deleting bead after close %s: %v", id, err)
			return nil, apierr.Internal.Msg("closed but delete failed: " + err.Error())
		}
	}

	out := &OKResponse{}
	out.Body.Status = "ok"
	return out, nil
}

// --- Session Wake ---

// humaHandleSessionWake is the Huma-typed handler for POST /v0/session/{id}/wake.

func (s *Server) humaHandleSessionWake(ctx context.Context, input *SessionIDInput) (*OKWithIDResponse, error) {
	store := s.state.SessionsBeadStore()
	if store.Store == nil {
		return nil, apierr.ServiceUnavailable.Msg("no bead store configured")
	}

	id, err := s.resolveSessionIDMaterializingNamedWithContext(ctx, store.Store, input.ID)
	if err != nil {
		return nil, humaResolveError(err)
	}

	res, err := session.NewStore(store).WakeSession(id, time.Now().UTC(), session.WakeOpts{RejectClosed: true})
	if err != nil {
		if errors.Is(err, session.ErrNotSessionBead) {
			return nil, apierr.InvalidRequest.Msg(id + " is not a session")
		}
		if state, conflict := session.WakeConflictState(err); conflict {
			return nil, apierr.SessionConflict.Msg("session " + id + " is " + state)
		}
		// Route every remaining store error through humaStoreError: the fused
		// Get error keeps its original 404 "not_found: "/500 "internal: " mapping,
		// and a mid-wake write error keeps the "internal: " prefix. (Delta vs the
		// pre-fusion handler: a mid-wake write ErrNotFound now maps 404 instead of
		// 500 — a safe-direction, near-unreachable shift, mirrored in the REST
		// handler's writeStoreError.)
		return nil, humaStoreError(err)
	}
	// Nudge withdrawal reads the nudges class, so it sources the typed
	// NudgesBeadStore (identity to the work store until that class relocates).
	if err := withdrawQueuedWaitNudges(s.state.NudgesBeadStore(), s.state.CityPath(), res.NudgeIDs); err != nil {
		log.Printf("gc api: withdrawing queued wait nudges after wake %s: %v", id, err)
	}
	// RAW SessionNameMetadata (not Info.SessionName, which falls back to
	// sessionNameFor(ID)) to preserve the skip-when-unset behavior. res.Info is
	// the typed WakeResult projection (WI-4), so no raw bead is cracked here.
	sessionName := res.Info.SessionNameMetadata
	if sessionName != "" {
		s.state.ClearCrashHistory(sessionName)
	}
	handle, err := s.workerHandleForSession(store.Store, id)
	if err != nil {
		return nil, humaSessionManagerError(err)
	}
	go func() {
		if err := handle.Start(context.Background()); err != nil {
			log.Printf("gc api: waking session %s: %v", id, err)
		}
	}()

	out := &OKWithIDResponse{}
	out.Body.Status = "ok"
	out.Body.ID = id
	return out, nil
}

// --- Session Rename ---

// humaHandleSessionRename is the Huma-typed handler for POST /v0/session/{id}/rename.

func (s *Server) humaHandleSessionRename(_ context.Context, input *SessionRenameInput) (*IndexOutput[sessionResponse], error) {
	store := s.state.SessionsBeadStore()
	if store.Store == nil {
		return nil, apierr.ServiceUnavailable.Msg("no bead store configured")
	}

	id, err := s.resolveSessionIDWithConfig(store.Store, input.ID)
	if err != nil {
		return nil, humaResolveError(err)
	}

	// Huma validates Body.Title (minLength:1); no handler guard needed.
	// Validate through the session front door (mirrors humaHandleSessionPatch):
	// nothing downstream reads the raw bead — rename operates by id. Present-but-
	// non-session → the existing "not a session" 400; absent → beads.ErrNotFound
	// → 404.
	sessFront := session.NewStore(store)
	info, err := sessFront.Get(id)
	if err != nil {
		if errors.Is(err, session.ErrSessionNotFound) {
			return nil, apierr.InvalidRequest.Msg(id + " is not a session")
		}
		return nil, humaStoreError(err)
	}
	if info.Type == "" {
		sessFront.RepairTypeBestEffort(id)
	}

	mgr := s.sessionManager(store.Store)
	if err := mgr.Rename(id, input.Body.Title); err != nil {
		return nil, humaSessionManagerError(err)
	}

	info, pr, err := sessionGetEnriched(session.NewStore(store), mgr, id)
	if err != nil {
		return nil, humaSessionManagerError(err)
	}
	rresp := sessionResponseWithReason(info, pr, s.state.Config(), s.state.SessionProvider(), strings.TrimSpace(s.state.CityPath()) != "")
	return &IndexOutput[sessionResponse]{
		Index: s.latestIndex(),
		Body:  rresp,
	}, nil
}

// --- Session Agent List ---

// sessionAgentListResponse is the response for GET /v0/session/{id}/agents.
type sessionAgentListResponse struct {
	Agents []sessionlog.AgentMapping `json:"agents"`
}

// sessionAgentGetResponse is the response for GET /v0/session/{id}/agents/{agentId}.
// Messages carries pre-decoded provider-native transcript frames as
// generic JSON values (arbitrary JSON per spec). Same pattern as
// sessionTranscriptGetResponse.Messages.
type sessionAgentGetResponse struct {
	Messages []any                  `json:"messages"`
	Status   sessionlog.AgentStatus `json:"status,omitempty"`
}

// humaHandleSessionAgentList is the Huma-typed handler for GET /v0/session/{id}/agents.
