package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/opjournal/legacyop"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
	"github.com/gastownhall/gascity/internal/worker"
)

// Command-side session handlers (create, patch, submit, message, stop, kill,
// respond, suspend, close, wake, rename). Split out of huma_handlers_sessions.go
// to isolate mutation logic from reads and streaming.

var (
	sessionMessageAsyncTimeout = sessionMessageTimeout
	// sessionSubmitAsyncTimeout bounds a recorded /submit's delivery. It ends
	// well inside the op's finish_by, ExecutionBound after admission or
	// adoption, so the op closes from the delivery's result, or from this
	// timeout, with time left for that close to land: the sweeper, or a
	// same-key retry, closes an op expired only once no delivery waits on it.
	// An unrecorded /submit has no op to close and is not bounded.
	sessionSubmitAsyncTimeout       = legacyop.ExecutionBound - 5*time.Minute
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
	if err := refuseNonClientSessionMetadata(body.Metadata); err != nil {
		return nil, err
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
	// The controller's reconciler never starts a demand-only singleton's
	// session on request, so refuse up front instead of returning 202 for a
	// bead that would sit start-pending forever (#6858).
	if msg := demandOnlySingletonCreateRefusal(cfg, agentCfg); msg != "" {
		return nil, apierr.DemandOnlySingleton.Msg(msg)
	}
	alias = createCtx.Alias
	explicitName := createCtx.ExplicitName
	workDirQualifiedName := createCtx.Identity
	workDir = createCtx.WorkDir

	command, err := s.agentTemplateLaunchCommand(agentCfg, resolved, transport)
	if err != nil {
		return nil, apierr.Internal.Msg(err.Error())
	}
	extraMeta := sessionTemplateOverridesMetadata(body.Options, body.Message)
	extraMeta = agentSessionCreateMetadata(extraMeta, workDirQualifiedName)
	// The client's keys land with the create, before the controller can see
	// the row.
	extraMeta = withClientMetadata(extraMeta, body.Metadata)
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
			s.state.Enqueue(reconcilekey.Session(info.ID))
			waitCtx, cancel := context.WithTimeout(context.Background(), sessionCreateCommandableTimeout)
			createdID := info.ID
			info, createErr = waiter.WaitForSessionCommandable(waitCtx, createdID)
			cancel()
			if createErr != nil {
				s.emitSessionCreateFailedFor(reqID, createdID, "create_failed", createErr.Error())
				return
			}
		}

		resp := sessionToResponse(info, s.state.Config())
		resp.Kind = "agent"
		s.emitSessionCreateSucceeded(reqID, resp)
		s.persistSessionMeta(store, info.ID, body.ProjectID, nil)
		if !waitForCommandable {
			s.state.Enqueue(reconcilekey.Session(info.ID))
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
	// The client's keys land with the create.
	extraMeta = withClientMetadata(extraMeta, body.Metadata)
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
			if _, sendErr := s.submitMessageToSession(context.Background(), store.Store, info.ID, msg, session.SubmitIntentDefault, false); sendErr != nil {
				if rollbackErr := s.rollbackCreatedSession(store, info.ID); rollbackErr != nil {
					s.emitSessionCreateFailedFor(reqID, info.ID, "message_delivery_failed",
						fmt.Sprintf("initial message delivery failed: %v (rollback failed: %v)", sendErr, rollbackErr))
					return
				}
				s.emitSessionCreateFailed(reqID, "message_delivery_failed", fmt.Sprintf("initial message delivery failed: %v", sendErr))
				return
			}
		}
		resp := sessionToResponse(info, s.state.Config())
		asProviderSession(&resp)
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
	index := s.latestIndex()
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
	//  - `minLength:"1"` on Title → non-empty when provided (a whitespace-only
	//    title passes that and is refused by the session manager → 400)
	// The handler only needs to enforce "at least one field" and the
	// alias-controller-managed rule below.
	titlePtr := input.Body.Title
	aliasPtr := input.Body.Alias

	if titlePtr == nil && aliasPtr == nil && len(input.Body.Metadata) == 0 {
		return nil, apierr.ValidationFailed.Msg("at least one of 'title', 'alias' or 'metadata' is required")
	}
	if err := refuseNonClientSessionMetadata(input.Body.Metadata); err != nil {
		return nil, err
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
		if titlePtr != nil || aliasPtr != nil {
			if err := mgr.UpdatePresentation(id, titlePtr, aliasPtr); err != nil {
				return err
			}
		}
		if len(input.Body.Metadata) > 0 {
			return store.SetMetadataBatch(id, input.Body.Metadata)
		}
		return nil
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
		Index: index,
		Body:  presp,
	}, nil
}

const sessionPermissionModeOptionKey = "permission_mode"

func (s *Server) humaHandleSessionPermissionMode(_ context.Context, input *SessionPermissionModeInput) (*IndexOutput[sessionResponse], error) {
	return s.updateSessionPermissionMode(input.ID, input.Body)
}

func (s *Server) updateSessionPermissionMode(idRef string, body SessionPermissionModeBody) (*IndexOutput[sessionResponse], error) {
	index := s.latestIndex()
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
	s.state.Enqueue(reconcilekey.Session(id))

	info, presponse, err := sessionGetEnriched(session.NewStore(store), mgr, id)
	if err != nil {
		return nil, humaSessionManagerError(err)
	}
	resp := sessionResponseWithReason(info, presponse, s.state.Config(), s.state.SessionProvider(), strings.TrimSpace(s.state.CityPath()) != "")
	return &IndexOutput[sessionResponse]{
		Index: index,
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

// sessionActionIdempotencyPath scopes an Idempotency-Key to one session
// action on one target. The target comes from the URL, not the body, so it
// must be part of the scope or the same key + body against two sessions would
// collide. PathEscape keeps a crafted target from forging the "/<action>"
// boundary. The scope is the target as addressed: the cache treats the same
// key sent once by alias and once by bead ID as two independent requests. For
// /submit and /messages in a recording ops.content mode, the operation
// journal still answers both as one (request_id rule 3 in
// engdocs/design/async-request-result.md).
func sessionActionIdempotencyPath(target, action string) string {
	return "/v0/session/" + url.PathEscape(target) + "/" + action
}

// --- Session Submit ---

// humaHandleSessionSubmit is the Huma-typed handler for POST /v0/session/{id}/submit.

func (s *Server) humaHandleSessionSubmit(ctx context.Context, input *SessionSubmitInput) (*SessionSubmitOutput, error) {
	// Idempotency: accept (and deliver) at most once per Idempotency-Key. A
	// replay returns the original 202 body — same request_id and event_cursor —
	// without starting a second delivery. The target is folded into the scope
	// because it lives in the URL, not the body.
	accepted, err := s.withMessageIdempotency(sessionActionIdempotencyPath(input.ID, "submit"), input.IdempotencyKey, input.Body,
		func() (asyncAcceptedBody, error) {
			return s.acceptSessionSubmit(ctx, input)
		})
	if err != nil {
		return nil, err
	}
	return &SessionSubmitOutput{Body: accepted}, nil
}

// acceptSessionSubmit validates the submit target, records the request as a
// message op under the ops.content gate (session_ops.go), starts the
// asynchronous delivery, and returns the 202 body.
func (s *Server) acceptSessionSubmit(ctx context.Context, input *SessionSubmitInput) (asyncAcceptedBody, error) {
	intent := input.Body.Intent
	if intent == "" {
		intent = session.SubmitIntentDefault
	}
	req := &messageOpRequest{
		target: input.ID, key: input.IdempotencyKey, text: input.Body.Message,
		intent: intent, operation: RequestOperationSessionSubmit,
	}
	message, resume := input.Body.Message, input.Body.Resume
	return s.acceptMessageOp(ctx, req, func(d messageDelivery) {
		s.deliverSubmit(d, message, intent, resume)
	})
}

// deliverSubmit delivers an accepted /submit: it runs the submit on its own
// goroutine, under sessionSubmitAsyncTimeout when the request is recorded,
// closes the op from the submit's result, or from a timeout when the submit
// outlives it, and emits the terminal event. An unrecorded submit has no op
// to close by a finish_by and is waited for however long it takes.
func (s *Server) deliverSubmit(d messageDelivery, message string, intent session.SubmitIntent, resume bool) {
	var timeout time.Duration
	if d.adm != nil && d.adm.Recorded {
		timeout = sessionSubmitAsyncTimeout
	}
	result := awaitSend(d, RequestOperationSessionSubmit, timeout, submitTimeoutError{}, func(ctx context.Context, send *messageSend) {
		s.sendSubmit(ctx, d, send, message, intent, resume)
	})
	op := s.finishMessageOp(d.adm, messageOpResult(result.outcome.Queued, result.err, result.outcome.Interrupt))
	if result.err != nil {
		s.emitSessionSubmitFailed(d.reqID, result.errorCode, result.err.Error(), op)
		return
	}
	s.emitSessionSubmitSucceeded(d.reqID, result.sessionID, result.outcome, string(intent), op)
}

// sendSubmit submits message to d's session and posts the result to send. A
// submit that fails because the delivery's timeout canceled it posts a
// timeout; a panic posts an internal_error result. Any other deadline or
// cancellation keeps /submit's legacy submit_failed, unlike /messages, though
// its op closes deadline_in_flight and a replay reports timeout
// (engdocs/design/async-request-result.md, request_id rule 7).
func (s *Server) sendSubmit(ctx context.Context, d messageDelivery, send *messageSend, message string, intent session.SubmitIntent, resume bool) {
	defer func() {
		if r := recover(); r != nil {
			send.post(messageResult{errorCode: "internal_error", err: fmt.Errorf("panic: %v", r)})
		}
	}()
	id, err := s.resolveDeliveryTarget(ctx, d)
	if err != nil {
		send.post(messageResult{errorCode: "resolve_failed", err: err})
		return
	}
	outcome, err := s.submitMessageToSession(ctx, d.store, id, message, intent, resume)
	if err != nil {
		code := sessionSubmitFailureCode(err)
		if ctx.Err() != nil && errors.Is(err, context.Canceled) {
			code = "timeout"
		}
		send.post(messageResult{sessionID: id, errorCode: code, err: err})
		return
	}
	send.post(messageResult{sessionID: id, outcome: outcome})
}

// messageDelivery is an accepted /submit or /messages request, handed to
// its delivery goroutine.
type messageDelivery struct {
	adm   *legacyop.Admission
	reqID string
	store beads.Store
	// target is the session as addressed; deliverTo, when set, is the
	// session the delivery goes to without re-resolving target.
	target    string
	deliverTo string
}

// acceptMessageOp is the acceptance /submit and /messages share: it
// validates req's target, admits req as a message op under the ops.content
// gate (session_ops.go), starts deliver on its own goroutine and returns the
// 202 body. A replay is answered with its body and starts nothing.
func (s *Server) acceptMessageOp(ctx context.Context, req *messageOpRequest, deliver func(messageDelivery)) (asyncAcceptedBody, error) {
	store := s.state.SessionsBeadStore()
	if store.Store == nil {
		return asyncAcceptedBody{}, apierr.ServiceUnavailable.Msg("no bead store configured")
	}
	conv, err := s.messageTargetConversation(ctx, store.Store, req.target)
	if err != nil {
		return asyncAcceptedBody{}, err
	}
	req.conv = conv
	// The cursor is captured before admission and before the goroutine: a
	// fast terminal event, or a replay's re-emitted one, lands after it.
	eventCursor, err := s.currentCityEventCursor()
	if err != nil {
		return asyncAcceptedBody{}, apierr.Internal.Msg(err.Error())
	}
	adm, replay, err := s.admitMessageOp(ctx, store.Store, req, eventCursor)
	if err != nil {
		return asyncAcceptedBody{}, err
	}
	if replay != nil {
		return *replay, nil
	}
	handedOff := false
	defer releaseOnPanic(adm, &handedOff)
	reqID, err := messageRequestID(adm)
	if err != nil {
		return asyncAcceptedBody{}, apierr.Internal.Msg(err.Error())
	}
	body := acceptedMessageBody(adm, reqID, eventCursor)
	adm.Accept(inflightAnswer{operation: req.operation, body: body})
	s.ops.runAfterAdmit()

	d := messageDelivery{adm: adm, reqID: reqID, store: store.Store, target: req.target}
	// Require delivers to the op's conversation: the op is the request.
	// Otherwise the delivery re-resolves the target, as it always has.
	if adm.Recorded && s.ops.content.Mode == gate.Require {
		d.deliverTo = req.conv
	}
	operation := req.operation
	handedOff = true
	go func() {
		defer s.recoverMessageOp(adm, reqID, operation)
		deliver(d)
	}()
	return body, nil
}

// messageTargetConversation validates a /submit or /messages target: a
// deliverable session with no pending interaction. It returns the target's
// conversation, "" for a configured named session with no row yet.
func (s *Server) messageTargetConversation(ctx context.Context, store beads.Store, target string) (string, error) {
	conv, err := s.sessionTargetDeliverable(ctx, store, target)
	if errors.Is(err, session.ErrSessionNotFound) {
		return "", apierr.SessionNotFound.Msg(fmt.Sprintf("session %q not found and not a configured named session", target))
	}
	if err != nil {
		// Ambiguous bare names and configured-name/live-bead conflicts are
		// deterministic client addressing errors: map them through the resolve
		// helper so they surface as 409 (matching /stop, /respond, and the
		// synchronous message twin) instead of a 500 from humaStoreError.
		return "", humaResolveError(err)
	}
	if err := s.rejectPendingInteraction(ctx, store, target); err != nil {
		return "", err
	}
	return conv, nil
}

// resolveDeliveryTarget is the session d delivers to: d.deliverTo when set,
// else d.target resolved now, materializing a configured named session; a
// target that moved off the op's conversation is noted.
func (s *Server) resolveDeliveryTarget(ctx context.Context, d messageDelivery) (string, error) {
	if d.deliverTo != "" {
		return d.deliverTo, nil
	}
	id, err := s.resolveSessionIDMaterializingNamedWithContext(ctx, d.store, d.target)
	if err != nil {
		return "", err
	}
	s.noteTargetMoved(d.adm, id)
	return id, nil
}

// recoverMessageOp turns a panic in a message delivery goroutine into a
// closed op and a request.failed{internal_error} event carrying it.
func (s *Server) recoverMessageOp(adm *legacyop.Admission, requestID, operation string) {
	if r := recover(); r != nil {
		op := s.finishMessageOp(adm, messageOpResult(false, fmt.Errorf("panic: %v", r), nil))
		s.emitRequestFailedOp(requestID, operation, "internal_error", fmt.Sprintf("panic: %v", r), op)
	}
}

// --- Session Messages ---

// humaHandleSessionMessage is the Huma-typed handler for POST /v0/session/{id}/messages.

func (s *Server) humaHandleSessionMessage(ctx context.Context, input *SessionMessageInput) (*SessionMessageOutput, error) {
	// Idempotency: accept (and deliver) at most once per Idempotency-Key; see
	// humaHandleSessionSubmit.
	accepted, err := s.withMessageIdempotency(sessionActionIdempotencyPath(input.ID, "messages"), input.IdempotencyKey, input.Body,
		func() (asyncAcceptedBody, error) {
			return s.acceptSessionMessage(ctx, input)
		})
	if err != nil {
		return nil, err
	}
	return &SessionMessageOutput{Body: accepted}, nil
}

// acceptSessionMessage validates the message target, records the request as
// a message op under the ops.content gate (session_ops.go), starts the
// asynchronous delivery, and returns the 202 body.
func (s *Server) acceptSessionMessage(ctx context.Context, input *SessionMessageInput) (asyncAcceptedBody, error) {
	req := &messageOpRequest{
		target: input.ID, key: input.IdempotencyKey, text: input.Body.Message,
		intent: session.SubmitIntentDefault, operation: RequestOperationSessionMessage,
	}
	message, resume := input.Body.Message, input.Body.Resume
	return s.acceptMessageOp(ctx, req, func(d messageDelivery) {
		s.deliverMessage(d, message, resume)
	})
}

// messageResult is the result of one /submit or /messages send: the session
// it went to and its outcome, or the error code its terminal event carries.
type messageResult struct {
	sessionID string
	outcome   messageOutcome
	errorCode string
	err       error
}

// messageSend is one /submit or /messages send its delivery waits on. The
// send posts its one result; once the request's terminal event is out, a
// late result is only logged.
type messageSend struct {
	reqID, target, operation string
	results                  chan messageResult
	emitted                  atomic.Bool
}

// post hands r to the delivery waiting on the send, or logs it when the
// request's terminal event is already out.
func (m *messageSend) post(r messageResult) {
	if !m.emitted.Load() {
		m.results <- r
		return
	}
	if r.err != nil {
		log.Printf("api: late %s result after timeout request_id=%s target=%s error_code=%s err=%v", m.operation, m.reqID, m.target, r.errorCode, r.err)
		return
	}
	log.Printf("api: late %s result after timeout request_id=%s target=%s session_id=%s", m.operation, m.reqID, m.target, r.sessionID)
}

// awaitSend runs send on its own goroutine for delivery d of operation and
// returns its result. When timeout is positive and the send outlives it,
// awaitSend cancels the send's context and returns a timeout result carrying
// timedOut; the send's result after that is only logged. A zero timeout
// waits however long the send takes.
func awaitSend(d messageDelivery, operation string, timeout time.Duration, timedOut error, send func(context.Context, *messageSend)) messageResult {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &messageSend{reqID: d.reqID, target: d.target, operation: operation, results: make(chan messageResult, 1)}
	go send(ctx, m)
	var expired <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		expired = timer.C
	}
	var result messageResult
	select {
	case result = <-m.results:
	case <-expired:
		cancel()
		select {
		case result = <-m.results:
		default:
			result = messageResult{errorCode: "timeout", err: timedOut}
		}
	}
	m.emitted.Store(true)
	return result
}

// deliverMessage delivers an accepted /messages: it runs the send on its own
// goroutine under sessionMessageAsyncTimeout, closes the op from the send's
// result, or from a timeout when the send outlives it, and emits the
// terminal event.
func (s *Server) deliverMessage(d messageDelivery, message string, resume bool) {
	result := awaitSend(d, RequestOperationSessionMessage, sessionMessageAsyncTimeout, messageTimeoutError{}, func(ctx context.Context, send *messageSend) {
		s.sendMessage(ctx, d, send, message, resume)
	})
	op := s.finishMessageOp(d.adm, messageOpResult(result.outcome.Queued, result.err, result.outcome.Interrupt))
	if result.err != nil {
		s.emitSessionMessageFailed(d.reqID, result.errorCode, result.err.Error(), op)
		return
	}
	s.emitSessionMessageSucceeded(d.reqID, result.sessionID, result.outcome, op)
}

// sendMessage sends message to d's session and posts the result to send; a
// panic posts an internal_error result.
func (s *Server) sendMessage(ctx context.Context, d messageDelivery, send *messageSend, message string, resume bool) {
	defer func() {
		if r := recover(); r != nil {
			send.post(messageResult{errorCode: "internal_error", err: fmt.Errorf("panic: %v", r)})
		}
	}()
	id, err := s.resolveDeliveryTarget(ctx, d)
	if err != nil {
		send.post(messageResult{errorCode: "resolve_failed", err: err})
		return
	}
	outcome, err := s.sendUserMessageToSession(ctx, d.store, id, message, resume)
	if err != nil {
		code := "message_failed"
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			code = "timeout"
		}
		send.post(messageResult{sessionID: id, errorCode: code, err: err})
		return
	}
	send.post(messageResult{sessionID: id, outcome: outcome})
}

// --- Session Stop ---

// sessionControlNoBody is the request body fingerprinted for the body-less
// session control endpoints (/stop, /wake): every request to one action on
// one target has the same body, so the Idempotency-Key scope alone decides
// replay.
type sessionControlNoBody struct{}

// humaHandleSessionStop is the Huma-typed handler for POST /v0/session/{id}/stop.

func (s *Server) humaHandleSessionStop(_ context.Context, input *SessionControlInput) (*OKWithIDResponse, error) {
	// Idempotency: a completed stop is replayed and a concurrent same-key
	// stop gets 409, so a retry after a successful stop cannot interrupt a
	// later turn. Any error releases the key, even one returned after the
	// interrupt was sent (e.g. StopTurn's idle wait timing out), so such a
	// retry interrupts again. The cached value is the resolved session ID; a
	// replay rebuilds the body.
	id, err := withIdempotency(s.idem, sessionActionIdempotencyPath(input.ID, "stop"), input.IdempotencyKey, sessionControlNoBody{},
		func() (string, error) {
			store := s.state.SessionsBeadStore()
			if store.Store == nil {
				return "", apierr.ServiceUnavailable.Msg("no bead store configured")
			}

			id, err := s.resolveSessionIDWithConfig(store.Store, input.ID)
			if err != nil {
				return "", humaResolveError(err)
			}

			mgr := s.sessionManager(store.Store)
			if err := mgr.StopTurn(id); err != nil {
				return "", humaSessionManagerError(err)
			}
			return id, nil
		})
	if err != nil {
		return nil, err
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

	// Under the kill fence, as `gc session kill`: without it the controller
	// reads the kill as a crash (heal, accrual, a same-pass restart).
	mgr := s.sessionManager(store.Store)
	kill := func(ctx context.Context, by session.Actor) error { return mgr.Kill(ctx, by, id) }
	if err := mgr.KillFenced(context.Background(), s.actor(session.ActorOperator), id, kill); err != nil {
		// A closed session, or one whose runtime is already gone, is already
		// killed: report the outcome rather than an error.
		if errors.Is(err, session.ErrSessionClosed) || errors.Is(err, session.ErrSessionInactive) {
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

// --- Session Reset ---

// humaHandleSessionReset is the Huma-typed handler for POST /v0/session/{id}/reset.
// It records a fresh-restart request through the worker boundary (the same
// path as `gc session reset`) and enqueues the session's reconcile key, which
// restarts the session on the next continuation epoch.
func (s *Server) humaHandleSessionReset(ctx context.Context, input *SessionIDInput) (*OKWithIDResponse, error) {
	store := s.state.SessionsBeadStore()
	if store.Store == nil {
		return nil, apierr.ServiceUnavailable.Msg("no bead store configured")
	}

	id, err := s.resolveSessionIDWithConfig(store.Store, input.ID)
	if err != nil {
		return nil, humaResolveError(err)
	}

	handle, err := s.workerHandleForSession(store.Store, id)
	if err != nil {
		return nil, humaSessionManagerError(err)
	}
	if err := handle.Reset(ctx); err != nil {
		return nil, humaSessionManagerError(err)
	}
	s.state.Enqueue(reconcilekey.Session(id))

	out := &OKWithIDResponse{}
	out.Body.Status = "ok"
	out.Body.ID = id
	return out, nil
}

// --- Session Respond ---

// humaHandleSessionRespond is the Huma-typed handler for POST /v0/session/{id}/respond.

func (s *Server) humaHandleSessionRespond(_ context.Context, input *SessionRespondInput) (*SessionRespondOutput, error) {
	// Idempotency: deliver the interaction response at most once per
	// Idempotency-Key. The cached value is the resolved session ID, so a replay
	// rebuilds the identical body without a second Respond (which would fail
	// with no_pending once the first one cleared the interaction).
	id, err := withIdempotency(s.idem, sessionActionIdempotencyPath(input.ID, "respond"), input.IdempotencyKey, input.Body,
		func() (string, error) {
			store := s.state.SessionsBeadStore()
			if store.Store == nil {
				return "", apierr.ServiceUnavailable.Msg("no bead store configured")
			}

			id, err := s.resolveSessionIDWithConfig(store.Store, input.ID)
			if err != nil {
				return "", humaResolveError(err)
			}

			// Huma validates Body.Action (minLength:1); no handler guard needed.
			mgr := s.sessionManager(store.Store)
			if err := mgr.Respond(id, runtime.InteractionResponse{
				RequestID: input.Body.RequestID,
				Action:    input.Body.Action,
				Text:      input.Body.Text,
				Metadata:  input.Body.Metadata,
			}); err != nil {
				return "", humaSessionManagerError(err)
			}
			return id, nil
		})
	if err != nil {
		return nil, err
	}
	// Publish the session.pending_cleared now rather than on the next pass.
	s.pokePendingInteractions(id)

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
	if err := mgr.Suspend(ctx, s.actor(session.ActorOperator), id, false); err != nil {
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
	closeResult, err := handle.CloseDetailed(ctx, s.actor(session.ActorOperator))
	if err != nil {
		return nil, humaSessionManagerError(err)
	}
	// The city's nudge queue withdraws the nudges and closes their shadows in
	// the nudges class.
	if err := s.withdrawQueuedWaitNudges(closeResult.WaitNudgeIDs); err != nil {
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

func (s *Server) humaHandleSessionWake(ctx context.Context, input *SessionControlInput) (*OKWithIDResponse, error) {
	// Idempotency: record the wake at most once per Idempotency-Key. Only a
	// 200 is cached; a refusal (including 409 wake-will-not-start, whose wake
	// is already recorded) releases the key so a retry re-runs the wake.
	id, err := withIdempotency(s.idem, sessionActionIdempotencyPath(input.ID, "wake"), input.IdempotencyKey, sessionControlNoBody{},
		func() (string, error) {
			return s.wakeSession(ctx, input.ID)
		})
	if err != nil {
		return nil, err
	}
	out := &OKWithIDResponse{}
	out.Body.Status = "ok"
	out.Body.ID = id
	return out, nil
}

// wakeSession records an explicit wake for target, hands the session to the
// controller, and returns the resolved session ID.
func (s *Server) wakeSession(ctx context.Context, target string) (string, error) {
	store := s.state.SessionsBeadStore()
	if store.Store == nil {
		return "", apierr.ServiceUnavailable.Msg("no bead store configured")
	}

	id, err := s.resolveSessionIDMaterializingNamedWithContext(ctx, store.Store, target)
	if err != nil {
		return "", humaResolveError(err)
	}

	res, err := session.NewStore(store).WakeSession(id, time.Now().UTC(), session.WakeOpts{RejectClosed: true})
	if err != nil {
		if errors.Is(err, session.ErrNotSessionBead) {
			return "", apierr.InvalidRequest.Msg(id + " is not a session")
		}
		if state, conflict := session.WakeConflictState(err); conflict {
			return "", apierr.SessionConflict.Msg("session " + id + " is " + state)
		}
		// Route every remaining store error through humaStoreError: the fused
		// Get error keeps its original 404 "not_found: "/500 "internal: " mapping,
		// and a mid-wake write error keeps the "internal: " prefix. (Delta vs the
		// pre-fusion handler: a mid-wake write ErrNotFound now maps 404 instead of
		// 500 — a safe-direction, near-unreachable shift, mirrored in the REST
		// handler's writeStoreError.)
		return "", humaStoreError(err)
	}
	// The city's nudge queue withdraws the nudges and closes their shadows in
	// the nudges class.
	if err := s.withdrawQueuedWaitNudges(res.NudgeIDs); err != nil {
		log.Printf("gc api: withdrawing queued wait nudges after wake %s: %v", id, err)
	}
	// RAW SessionNameMetadata (not Info.SessionName, which falls back to
	// sessionNameFor(ID)) to preserve the skip-when-unset behavior. res.Info is
	// the typed WakeResult projection (WI-4), so no raw bead is cracked here.
	sessionName := res.Info.SessionNameMetadata
	if sessionName != "" {
		s.state.ClearCrashHistory(sessionName)
	}
	// The wake is recorded; the controller starts the session, as for gc
	// session wake. The API never starts a runtime in the controller process
	// beside the controller's own start (CONTRACT v5.9 D8 rule 6). When the
	// controller will not start it, the one will-not-start predicate `gc
	// session wake` uses says why, as 409 (the demand-only singleton, #6858,
	// included).
	s.state.Enqueue(reconcilekey.Session(id))
	woken := res.Info // the predicate reads the row after the wake
	if info, err := session.NewStore(store).Get(id); err == nil {
		woken = info
	}
	why := demandOnlySingletonWakeRefusal(s.state.Config(), woken)
	if refuser, ok := s.state.(WakeStartRefuser); ok {
		why, _ = refuser.WakeStartRefusal(woken)
	}
	if why != "" {
		return "", apierr.WakeWillNotStart.Msg("wake recorded for session " + id + ", but it will not start: " + strings.TrimPrefix(why, "wake recorded for session "+id+", but it will not start: "))
	}
	return id, nil
}

// --- Session Rename ---

// humaHandleSessionRename is the Huma-typed handler for POST /v0/session/{id}/rename.

// clientSessionMetadataPrefix is the session metadata family external
// clients own (session.fields: real_world_app_ is NoWriter in gc): the only
// keys POST /sessions and PATCH /session/{id} write for a client, since the
// generic bead API never writes a session row (architecture B).
const clientSessionMetadataPrefix = "real_world_app_"

// refuseNonClientSessionMetadata refuses, with 422 and an errors[] entry per
// key, a session metadata key outside the client family or one that is not a
// valid bead-metadata key.
func refuseNonClientSessionMetadata(meta map[string]string) error {
	var bad []*huma.ErrorDetail
	for _, k := range slices.Sorted(maps.Keys(meta)) {
		if !strings.HasPrefix(k, clientSessionMetadataPrefix) || !beadmeta.ValidKey(k) {
			bad = append(bad, &huma.ErrorDetail{Location: "body.metadata." + k, Message: "a client writes only valid " + clientSessionMetadataPrefix + "* keys on a session", Value: k})
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return apierr.ValidationFailed.With("session metadata: only "+clientSessionMetadataPrefix+"* keys are a client's; nothing was written", bad...)
}

// withClientMetadata is meta with the client's keys added.
func withClientMetadata(optMeta, client map[string]string) map[string]string {
	if len(client) == 0 {
		return optMeta
	}
	out := maps.Clone(optMeta)
	if out == nil {
		out = make(map[string]string, len(client))
	}
	maps.Copy(out, client)
	return out
}

// humaHandleSessionPin is the Huma-typed handler for POST
// /v0/city/{cityName}/session/{id}/pin: gc session pin's write (pin_awake),
// which the generic bead API no longer makes (architecture B). A configured
// named session not yet materialized is, as the CLI's pin does.
func (s *Server) humaHandleSessionPin(ctx context.Context, input *SessionIDInput) (*IndexOutput[sessionResponse], error) {
	return s.setSessionPin(ctx, input.ID, true)
}

// humaHandleSessionUnpin is the Huma-typed handler for POST
// /v0/city/{cityName}/session/{id}/unpin: gc session unpin's write.
func (s *Server) humaHandleSessionUnpin(ctx context.Context, input *SessionIDInput) (*IndexOutput[sessionResponse], error) {
	return s.setSessionPin(ctx, input.ID, false)
}

func (s *Server) setSessionPin(ctx context.Context, idRef string, pinned bool) (*IndexOutput[sessionResponse], error) {
	index := s.latestIndex()
	store := s.state.SessionsBeadStore()
	if store.Store == nil {
		return nil, apierr.ServiceUnavailable.Msg("no bead store configured")
	}
	// A pin materializes a configured named session not yet created, as gc
	// session pin does; that row is the pin's to keep.
	id, err := s.resolveSessionIDWithConfig(store.Store, idRef)
	materialized := false
	if err != nil && pinned {
		id, err = s.resolveSessionIDMaterializingNamedWithContext(ctx, store.Store, idRef)
		materialized = err == nil
	}
	if err != nil {
		return nil, humaResolveError(err)
	}
	sessFront := session.NewStore(store)
	info, err := sessFront.Get(id)
	if err != nil {
		if errors.Is(err, session.ErrSessionNotFound) {
			return nil, apierr.InvalidRequest.Msg(id + " is not a session")
		}
		return nil, humaStoreError(err)
	}
	if info.Closed {
		return nil, apierr.SessionConflict.Msg("session " + id + " is closed")
	}
	// Pool demand alone starts and keeps a demand-only singleton's session;
	// the reconciler never reads pin_awake for it, so refuse a pin that would
	// do nothing, as gc session pin does (#6858).
	if cfg := s.state.Config(); pinned && !materialized && cfg != nil {
		if agentCfg, ok := findAgentByQualifiedTemplate(cfg, info.Template); ok && session.IsDemandOnlySingletonSession(cfg, &agentCfg, info) {
			return nil, apierr.DemandOnlySingleton.Msg("cannot pin session " + id + ": " + session.DemandOnlySingletonExplanation(agentCfg.QualifiedName()))
		}
	}
	if info.Type == "" {
		sessFront.RepairTypeBestEffort(id)
	}
	if err := sessFront.SetPinAwake(id, pinned); err != nil {
		return nil, humaStoreError(err)
	}
	s.state.Enqueue(reconcilekey.Session(id))
	mgr := s.sessionManager(store.Store)
	info, pr, err := sessionGetEnriched(session.NewStore(store), mgr, id)
	if err != nil {
		return nil, humaSessionManagerError(err)
	}
	return &IndexOutput[sessionResponse]{
		Index: index,
		Body:  sessionResponseWithReason(info, pr, s.state.Config(), s.state.SessionProvider(), strings.TrimSpace(s.state.CityPath()) != ""),
	}, nil
}

func (s *Server) humaHandleSessionRename(_ context.Context, input *SessionRenameInput) (*IndexOutput[sessionResponse], error) {
	index := s.latestIndex()
	store := s.state.SessionsBeadStore()
	if store.Store == nil {
		return nil, apierr.ServiceUnavailable.Msg("no bead store configured")
	}

	id, err := s.resolveSessionIDWithConfig(store.Store, input.ID)
	if err != nil {
		return nil, humaResolveError(err)
	}

	// Huma validates Body.Title (minLength:1); a whitespace-only title passes
	// that and is refused by the session manager (ErrInvalidSessionTitle → 400).
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
		Index: index,
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
