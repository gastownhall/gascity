package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// sessionResponse is the JSON representation of a chat session.
type sessionResponse struct {
	ID          string `json:"id"`
	Kind        string `json:"kind,omitempty"`
	Template    string `json:"template"`
	State       string `json:"state"`
	Reason      string `json:"reason,omitempty"`
	Title       string `json:"title"`
	Alias       string `json:"alias,omitempty"`
	Provider    string `json:"provider"`
	DisplayName string `json:"display_name,omitempty"`
	SessionName string `json:"session_name"`
	WorkDir     string `json:"work_dir,omitempty"`
	CreatedAt   string `json:"created_at"`
	LastActive  string `json:"last_active,omitempty"`
	// LastNudgeDeliveredAt is the most recent successful nudge delivery
	// timestamp for this session.
	LastNudgeDeliveredAt string `json:"last_nudge_delivered_at,omitempty"`
	Attached             bool   `json:"attached"`

	// Classification fields derived from config (for dashboard grouping).
	Rig  string `json:"rig,omitempty"`
	Pool string `json:"pool,omitempty"`

	// AgentKind classifies the agent backing the session so dashboards can
	// route it to the right panel without re-deriving from template names.
	// One of: "crew" (persistent named worker under a <rig>/crew dir),
	// "pool" (multi-instance agent), or "role" (singleton). Empty when the
	// session's template does not resolve to a configured agent.
	AgentKind string `json:"agent_kind,omitempty"`

	// Enrichment fields for dashboard consumption.
	Running       bool   `json:"running"`
	ActiveBead    string `json:"active_bead,omitempty"`
	LastOutput    string `json:"last_output,omitempty"`
	Model         string `json:"model,omitempty"`
	ContextPct    *int   `json:"context_pct,omitempty"`
	ContextWindow *int   `json:"context_window,omitempty"`

	// Activity indicates session turn state: "idle", "in-turn", or omitted.
	Activity string `json:"activity,omitempty"`

	// SubmissionCapabilities describes which semantic submit intents the
	// session runtime can honor.
	SubmissionCapabilities session.SubmissionCapabilities `json:"submission_capabilities,omitempty"`

	// ConfiguredNamedSession marks canonical singleton sessions materialized from
	// [[named_session]] configuration.
	ConfiguredNamedSession bool `json:"configured_named_session,omitempty"`

	// Options contains the effective per-session option overrides from
	// template_overrides bead metadata (e.g., {"permission_mode":"unrestricted"}).
	Options map[string]string `json:"options,omitempty"`

	// Metadata exposes real_world_app_-prefixed bead metadata for external consumers.
	Metadata map[string]string `json:"metadata,omitempty"`
}

type sessionResponseHandle interface {
	worker.StateHandle
	worker.PeekHandle
}

func (s *Server) runtimeSessionResponseHandle(info session.Info) sessionResponseHandle {
	if info.State != session.StateActive {
		return nil
	}
	return newProviderSessionResponseHandle(s.state.SessionProvider(), info.SessionName, info.Provider)
}

func sessionToResponse(info session.Info, cfg *config.City) sessionResponse {
	provider, displayName := info.Provider, ""
	if cfg != nil {
		provider, displayName = resolveProviderInfo(info.Provider, cfg)
	}
	rig, _ := config.ParseQualifiedName(info.Template)
	r := sessionResponse{
		ID:          info.ID,
		Template:    info.Template,
		State:       string(info.State),
		Title:       info.Title,
		Alias:       info.Alias,
		Provider:    provider,
		DisplayName: displayName,
		SessionName: info.SessionName,
		WorkDir:     info.WorkDir,
		CreatedAt:   info.CreatedAt.Format(time.RFC3339),
		Attached:    info.Attached,
		Rig:         rig,
	}
	// Populate pool and agent_kind from config lookup. The pool field is
	// the agent's base name (e.g., "polecat"), useful for dashboard type
	// classification. AgentKind tells the dashboard which panel a session
	// belongs to (crew/pool/role).
	if cfg != nil {
		if agent, ok := findAgent(cfg, info.Template); ok {
			if isMultiSessionAgent(agent) {
				r.Pool = agent.Name
			}
			r.AgentKind = classifyAgentKind(agent)
		}
	}
	if !info.LastActive.IsZero() {
		r.LastActive = info.LastActive.Format(time.RFC3339)
	}
	if !info.LastNudgeDeliveredAt.IsZero() {
		r.LastNudgeDeliveredAt = info.LastNudgeDeliveredAt.Format(time.RFC3339)
	}
	return r
}

// sessionResponseWithReason builds a session response from session.Info plus the
// persisted-response projection (status + metadata). It is the keystone of the
// session-response path: scalar fields come from Info, and the
// status/metadata-derived fields (reason, options, kind, submission
// capabilities, configured-named-session, exposable metadata) come from the
// PersistedResponse projection. No raw *beads.Bead crosses into the response
// builder; bead serialization is confined to session.PersistedResponseFromBead.
//
// A zero-value PersistedResponse (Status == "" and nil Metadata) corresponds to
// "no persisted bead found" — the same case the pre-S2 path handled with a nil
// bead — and the reason and metadata-derived fields are omitted.
func sessionResponseWithReason(info session.Info, pr session.PersistedResponse, cfg *config.City, sp runtime.Provider, hasDeferredQueue bool) sessionResponse {
	r := sessionToResponse(info, cfg)
	hasPersisted := pr.Status != "" || pr.Metadata != nil
	// Expose effective options: provider EffectiveDefaults merged with
	// per-session template_overrides. The dashboard uses this to display
	// the actual permission mode and other settings.
	if hasPersisted && cfg != nil {
		agentTemplateOK := true
		agent, agentFound := findAgent(cfg, info.Template)
		if session.UseAgentTemplateForProviderResolution(legacySessionKind(pr.Metadata), pr.Metadata, info.Provider, agent.Provider, agentFound) {
			r.Kind = "agent"
			agentTemplateOK = agentFound
		} else {
			r.Kind = "provider"
		}
		if agentTemplateOK {
			rp, _ := resolveProviderForSessionOptions(info, pr.Metadata, cfg)
			if rp != nil {
				merged := make(map[string]string, len(rp.EffectiveDefaults))
				for k, v := range rp.EffectiveDefaults {
					merged[k] = v
				}
				hasOverrides := false
				if overrides, err := session.ParseTemplateOverrides(pr.Metadata); err == nil {
					for k, v := range overrides {
						if k != "initial_message" {
							merged[k] = v
							hasOverrides = true
						}
					}
				}
				if len(rp.EffectiveDefaults) > 0 || hasOverrides {
					r.Options = merged
				}
			}
		}
	}
	if !hasPersisted || info.Closed {
		return r
	}
	var isRunning func(string) bool
	if sp != nil {
		isRunning = sp.IsRunning
	}
	r.Reason = session.LifecycleDisplayReasonWithLiveness(pr.Status, pr.Metadata, time.Now().UTC(), info.SessionName, isRunning)
	r.ConfiguredNamedSession = strings.TrimSpace(pr.Metadata[apiNamedSessionMetadataKey]) == "true"
	r.SubmissionCapabilities = session.SubmissionCapabilitiesForMetadata(pr.Metadata, hasDeferredQueue)
	// Expose only real_world_app_* prefixed metadata keys to API consumers.
	// Internal fields (session_key, command, work_dir, etc.) are redacted.
	r.Metadata = filterMetadata(pr.Metadata)
	return r
}

// filterMetadataAllowedKeys lists non-real_world_app_ metadata keys that are safe to expose.
var filterMetadataAllowedKeys = map[string]bool{
	"template_overrides": true,
}

// filterMetadata returns only metadata keys with the "real_world_app_" prefix plus
// explicitly allowlisted keys. This prevents leaking internal bead fields
// (session_key, command, work_dir, quarantine state) to API consumers.
func filterMetadata(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	filtered := make(map[string]string)
	for k, v := range m {
		if k == "real_world_app_session_kind" {
			continue
		}
		if strings.HasPrefix(k, "real_world_app_") || filterMetadataAllowedKeys[k] {
			filtered[k] = v
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	return filtered
}

// writeResolveError maps session.ResolveSessionID errors to HTTP responses.
func writeResolveError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, session.ErrAmbiguous), errors.Is(err, errConfiguredNamedSessionConflict):
		writeError(w, http.StatusConflict, "ambiguous", err.Error())
	case errors.Is(err, session.ErrSessionNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

func (s *Server) handleSessionList(w http.ResponseWriter, r *http.Request) {
	store := s.state.SessionsBeadStore()
	if store.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no bead store configured")
		return
	}
	mgr := s.sessionManager(store.Store)
	cfg := s.state.Config()

	q := r.URL.Query()
	stateFilter := q.Get("state")
	templateFilter := q.Get("template")
	wantPeek := q.Get("peek") == "true"

	listings, partialErrors, err := sessionReadModelListings(session.NewStore(store))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	sessions, responseByID := filterEnrichReadModel(mgr, listings, stateFilter, templateFilter)

	// Resolve the legacy offset page before runtime/transcript enrichment so
	// off-page sessions do not perform filesystem discovery on every list poll.
	pp := parsePagination(r, maxPaginationLimit)
	rowIdx := make([]int, len(sessions))
	for i := range rowIdx {
		rowIdx[i] = i
	}
	pageIdx := rowIdx
	var total int
	nextCursor := ""
	if !pp.IsPaging {
		if pp.Limit < len(pageIdx) {
			pageIdx = pageIdx[:pp.Limit]
		}
		total = len(pageIdx)
	} else {
		pageIdx, total, nextCursor = paginate(rowIdx, pp)
		if pageIdx == nil {
			pageIdx = []int{}
		}
	}

	pageSessions := make([]session.Info, len(pageIdx))
	for i, row := range pageIdx {
		pageSessions[i] = sessions[row]
	}
	keyedTranscriptPaths := session.ResolveKeyedTranscriptPaths(sessionTranscriptLookupCandidates(pageSessions), s.sessionLogPaths(), sessionTranscriptProviderFallback(cfg))
	items := make([]sessionResponse, len(pageSessions))
	hasDeferredQueue := strings.TrimSpace(s.state.CityPath()) != ""
	for i, sess := range pageSessions {
		items[i] = sessionResponseWithReason(sess, responseByID[sess.ID], cfg, s.state.SessionProvider(), hasDeferredQueue)
		s.enrichSessionResponseWithKeyedPaths(&items[i], sess, cfg, s.runtimeSessionResponseHandle(sess), wantPeek, false, false, 0, keyedTranscriptPaths)
	}

	if !pp.IsPaging {
		writeJSON(w, http.StatusOK, listResponse{
			Items:         items,
			Total:         total,
			Partial:       len(partialErrors) > 0,
			PartialErrors: partialErrors,
		})
		return
	}
	writeJSON(w, http.StatusOK, listResponse{
		Items:         items,
		Total:         total,
		NextCursor:    nextCursor,
		Partial:       len(partialErrors) > 0,
		PartialErrors: partialErrors,
	})
}

func sessionTranscriptLookupCandidates(infos []session.Info) []session.Info {
	candidates := make([]session.Info, 0, len(infos))
	for _, info := range infos {
		if info.State == session.StateActive && strings.TrimSpace(info.WorkDir) != "" && strings.TrimSpace(info.SessionKey) != "" {
			candidates = append(candidates, info)
		}
	}
	return candidates
}

func sessionTranscriptProviderFallback(cfg *config.City) string {
	if cfg == nil {
		return ""
	}
	return strings.TrimSpace(cfg.Workspace.Provider)
}

func (s *Server) handleSessionGet(w http.ResponseWriter, r *http.Request) {
	store := s.state.SessionsBeadStore()
	if store.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no bead store configured")
		return
	}
	cfg := s.state.Config()

	id, err := s.resolveSessionIDAllowClosedWithConfig(store.Store, r.PathValue("id"))
	if err != nil {
		writeResolveError(w, err)
		return
	}
	info, pr, err := sessionGetEnriched(session.NewStore(store), s.sessionManager(store.Store), id)
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}
	wantPeek := r.URL.Query().Get("peek") == "true"
	resp := sessionResponseWithReason(info, pr, cfg, s.state.SessionProvider(), strings.TrimSpace(s.state.CityPath()) != "")
	handle, err := s.workerHandleForSession(store.Store, id)
	if err == nil {
		s.enrichSessionResponse(&resp, info, cfg, handle, wantPeek, true, true, 0)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleSessionSuspend(w http.ResponseWriter, r *http.Request) {
	store := s.state.SessionsBeadStore()
	if store.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no bead store configured")
		return
	}

	id, err := s.resolveSessionIDMaterializingNamedWithContext(r.Context(), store.Store, r.PathValue("id"))
	if err != nil {
		writeResolveError(w, err)
		return
	}
	handle, err := s.workerHandleForSession(store.Store, id)
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}
	if err := handle.Stop(r.Context()); err != nil {
		writeSessionManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleSessionClose(w http.ResponseWriter, r *http.Request) {
	store := s.state.SessionsBeadStore()
	if store.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no bead store configured")
		return
	}
	id, err := s.resolveSessionIDWithConfig(store.Store, r.PathValue("id"))
	if err != nil {
		writeResolveError(w, err)
		return
	}
	handle, err := s.workerHandleForSession(store.Store, id)
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}
	closeResult, err := handle.CloseDetailed(r.Context())
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}
	// Nudge withdrawal reads the nudges class, so it sources the typed
	// NudgesBeadStore (identity to the work store until that class relocates).
	if err := withdrawQueuedWaitNudges(s.state.NudgesBeadStore(), s.state.CityPath(), closeResult.WaitNudgeIDs); err != nil {
		log.Printf("gc api: withdrawing queued wait nudges after close %s: %v", id, err)
	}

	// Optional: permanently delete the bead after closing.
	if r.URL.Query().Get("delete") == "true" {
		if err := deleteSessionBeadAfterClose(store.Store, id); err != nil {
			log.Printf("gc api: deleting bead after close %s: %v", id, err)
			writeError(w, http.StatusInternalServerError, "internal", "closed but delete failed: "+err.Error())
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func deleteSessionBeadAfterClose(store beads.Store, id string) error {
	const maxAttempts = 5
	var err error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		err = store.Delete(id)
		if err == nil {
			return nil
		}
		if errors.Is(err, beads.ErrNotFound) {
			log.Printf("gc api: deleting bead after close %s: already gone", id)
			return nil
		}
		if !isTransientBeadDeleteConflict(err) {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 25 * time.Millisecond)
	}
	return err
}

func isTransientBeadDeleteConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Error 1213") ||
		strings.Contains(msg, "40001") ||
		strings.Contains(msg, "serialization failure")
}

func (s *Server) handleSessionPermissionMode(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(csrfHeaderName) == "" {
		writeError(w, http.StatusForbidden, "csrf", "X-GC-Request header required on mutation endpoints")
		return
	}
	var body SessionPermissionModeBody
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	resp, err := s.updateSessionPermissionMode(r.PathValue("id"), body)
	if err != nil {
		writeHumaStatusError(w, err)
		return
	}
	w.Header().Set("X-GC-Index", fmt.Sprintf("%d", resp.Index))
	writeJSON(w, http.StatusOK, resp.Body)
}

// handleSessionWake clears hold and quarantine on a session.
func (s *Server) handleSessionWake(w http.ResponseWriter, r *http.Request) {
	store := s.state.SessionsBeadStore()
	if store.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no bead store configured")
		return
	}

	id, err := s.resolveSessionIDMaterializingNamedWithContext(r.Context(), store.Store, r.PathValue("id"))
	if err != nil {
		writeResolveError(w, err)
		return
	}

	res, err := session.NewStore(store).WakeSession(id, time.Now().UTC(), session.WakeOpts{})
	if err != nil {
		if errors.Is(err, session.ErrNotSessionBead) {
			writeError(w, http.StatusBadRequest, "invalid", id+" is not a session")
			return
		}
		if state, conflict := session.WakeConflictState(err); conflict {
			writeError(w, http.StatusConflict, "conflict", "session "+id+" is "+state)
			return
		}
		writeStoreError(w, err)
		return
	}
	// Nudge withdrawal reads the nudges class, so it sources the typed
	// NudgesBeadStore (identity to the work store until that class relocates).
	if err := withdrawQueuedWaitNudges(s.state.NudgesBeadStore(), s.state.CityPath(), res.NudgeIDs); err != nil {
		log.Printf("gc api: withdrawing queued wait nudges after wake %s: %v", id, err)
	}
	// Clear in-memory crash tracker so the reconciler doesn't immediately
	// re-quarantine the session based on stale crash history. Read the RAW
	// SessionNameMetadata (not Info.SessionName, which falls back to
	// sessionNameFor(ID)) to preserve the skip-when-unset behavior. res.Info is
	// the typed WakeResult projection (WI-4), so no raw bead is cracked here.
	sessionName := res.Info.SessionNameMetadata
	if sessionName != "" {
		s.state.ClearCrashHistory(sessionName)
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "id": id})
}

// handleSessionRename updates a session's title.
func (s *Server) handleSessionRename(w http.ResponseWriter, r *http.Request) {
	store := s.state.SessionsBeadStore()
	if store.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no bead store configured")
		return
	}

	id, err := s.resolveSessionIDWithConfig(store.Store, r.PathValue("id"))
	if err != nil {
		writeResolveError(w, err)
		return
	}

	var body struct {
		Title string `json:"title"`
	}
	if decErr := decodeBody(r, &body); decErr != nil {
		writeError(w, http.StatusBadRequest, "invalid", decErr.Error())
		return
	}
	if body.Title == "" {
		writeError(w, http.StatusBadRequest, "invalid", "title is required")
		return
	}

	// Validate through the session front door (mirrors handleSessionPatch): the
	// codec stays confined inside Store.Get, and nothing downstream reads the raw
	// bead — rename operates by id. Present-but-non-session → the existing "not a
	// session" 400; absent → beads.ErrNotFound → 404.
	sessFront := session.NewStore(store)
	info, err := sessFront.Get(id)
	if err != nil {
		if errors.Is(err, session.ErrSessionNotFound) {
			writeError(w, http.StatusBadRequest, "invalid", id+" is not a session")
			return
		}
		writeStoreError(w, err)
		return
	}
	if info.Type == "" {
		sessFront.RepairTypeBestEffort(id)
	}

	handle, err := s.workerHandleForSession(store.Store, id)
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}
	if err := handle.Rename(r.Context(), body.Title); err != nil {
		writeSessionManagerError(w, err)
		return
	}

	// Re-fetch to return the updated session, consistent with PATCH.
	info, pr, err := sessionGetEnriched(session.NewStore(store), s.sessionManager(store.Store), id)
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}
	rresp := sessionResponseWithReason(info, pr, s.state.Config(), s.state.SessionProvider(), strings.TrimSpace(s.state.CityPath()) != "")
	writeJSON(w, http.StatusOK, rresp)
}

// defaultSessionPeekLines is the preview line count used when a caller
// requests peek=true without specifying peek_lines. Matches the long-standing
// 5-line dashboard preview.
const defaultSessionPeekLines = 5

// enrichSessionResponse populates runtime fields on a session response:
// running state, active bead, peek output, and model/context metadata.
//
// peekLines controls the line count for the preview when wantPeek is true.
// Zero means "use default" (defaultSessionPeekLines).
func (s *Server) enrichSessionResponse(resp *sessionResponse, info session.Info, cfg *config.City, runtimeHandle any, wantPeek, liveActiveBead, allowWorkdirTranscriptDiscovery bool, peekLines int) {
	s.enrichSessionResponseWithKeyedPaths(resp, info, cfg, runtimeHandle, wantPeek, liveActiveBead, allowWorkdirTranscriptDiscovery, peekLines, nil)
}

// enrichSessionResponseWithKeyedPaths accepts an optional page-level map of
// exact transcript paths. A non-nil map is authoritative, including misses,
// so list callers can batch Codex discovery once instead of scanning per row.
func (s *Server) enrichSessionResponseWithKeyedPaths(resp *sessionResponse, info session.Info, cfg *config.City, runtimeHandle any, wantPeek, liveActiveBead, allowWorkdirTranscriptDiscovery bool, peekLines int, keyedTranscriptPaths map[string]string) {
	if info.State != session.StateActive {
		return
	}
	var (
		stateHandle worker.StateHandle
		peekHandle  worker.PeekHandle
	)
	switch v := runtimeHandle.(type) {
	case worker.Handle:
		stateHandle = v
		peekHandle = v
	case sessionResponseHandle:
		stateHandle = v
		peekHandle = v
	case runtime.Provider:
		store := s.state.SessionsBeadStore()
		if store.Store == nil {
			return
		}
		resolved, err := s.workerHandleForSession(store.Store, info.ID)
		if err != nil {
			return
		}
		stateHandle = resolved
		peekHandle = resolved
	default:
		return
	}
	if stateHandle == nil {
		return
	}
	state, err := stateHandle.State(context.Background())
	if err != nil {
		return
	}
	resp.Running = workerPhaseHasLiveOutput(state.Phase)

	// Active bead: search rig stores for in_progress work assigned to the
	// concrete session first, then fall back to alias/runtime/session names.
	// Alias inclusion preserves compatibility with role flows that assign
	// by alias (e.g., mayor, sky, wolf) until all assigners migrate to the
	// concrete session ID.
	//
	// Search all rig stores for concrete session ownership first, then fall
	// back to alias/runtime/session names for older assigners.
	// A previous fix accidentally passed info.Alias as the first positional
	// (rig) argument, which silently narrowed the search to a rig named after
	// the alias — so alias-assigned work still disappeared from ActiveBead.
	if liveActiveBead {
		resp.ActiveBead = s.findLiveActiveBeadForAssignees("", info.ID, info.SessionName, info.Alias, info.Template)
	} else {
		resp.ActiveBead = s.findActiveBeadForAssignees("", info.ID, info.SessionName, info.Alias, info.Template)
	}

	// Peek preview (opt-in, only when running). peekLines=0 means "use
	// default" so existing callers that omit the query param keep the
	// historical 5-line preview.
	if wantPeek && resp.Running && peekHandle != nil {
		lines := peekLines
		if lines <= 0 {
			lines = defaultSessionPeekLines
		}
		if output, err := peekHandle.Peek(context.Background(), lines); err == nil {
			resp.LastOutput = output
		}
	}

	// Model + context usage (best-effort).
	if resp.Running && info.WorkDir != "" {
		workDir := info.WorkDir
		if abs, err := filepath.Abs(workDir); err == nil {
			workDir = abs
		}
		factory, err := s.workerFactory(s.state.SessionsBeadStore().Store)
		if err != nil {
			return
		}
		// Prefer session-key lookup to avoid cross-reading another session's transcript.
		provider := info.Provider
		if strings.TrimSpace(provider) == "" && cfg != nil {
			provider, _ = resolveProviderInfo(provider, cfg)
		}
		transcriptProvider := session.ProviderFamilyFromInfo(info, provider)
		var sessionFile string
		switch {
		case allowWorkdirTranscriptDiscovery:
			sessionFile = factory.DiscoverTranscript(transcriptProvider, workDir, info.SessionKey)
		case keyedTranscriptPaths != nil:
			sessionFile = keyedTranscriptPaths[info.ID]
		default:
			// List responses must attribute telemetry to one exact session. Use
			// the already-loaded Info projection so enrichment stays free of
			// per-row store reads, and never fall back to a same-workdir file.
			lookupInfo := info
			lookupInfo.WorkDir = workDir
			sessionFile = session.ResolveKeyedTranscriptPath(lookupInfo, s.sessionLogPaths())
		}
		if sessionFile != "" {
			if meta, err := factory.TailMetaForProvider(transcriptProvider, sessionFile); err == nil && meta != nil {
				resp.Model = meta.Model
				if meta.ContextUsage != nil {
					resp.ContextPct = &meta.ContextUsage.Percentage
					resp.ContextWindow = &meta.ContextUsage.ContextWindow
				}
				resp.Activity = meta.Activity
			}
		}
	}
}

// handleSessionPatch handles PATCH /v0/session/{id}. Title and alias are mutable.
func (s *Server) handleSessionPatch(w http.ResponseWriter, r *http.Request) {
	store := s.state.SessionsBeadStore()
	if store.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no bead store configured")
		return
	}

	id, err := s.resolveSessionIDWithConfig(store.Store, r.PathValue("id"))
	if err != nil {
		writeResolveError(w, err)
		return
	}

	var body map[string]any
	if decErr := decodeBody(r, &body); decErr != nil {
		writeError(w, http.StatusBadRequest, "invalid", decErr.Error())
		return
	}

	// Reject any field other than "title" or "alias".
	for key := range body {
		if key != "title" && key != "alias" {
			writeError(w, http.StatusForbidden, "forbidden",
				fmt.Sprintf("field %q is immutable on sessions; only 'title' and 'alias' can be patched", key))
			return
		}
	}

	var titlePtr *string
	if rawTitle, ok := body["title"]; ok {
		title, isString := rawTitle.(string)
		if !isString || title == "" {
			writeError(w, http.StatusBadRequest, "invalid", "title must be a non-empty string")
			return
		}
		titlePtr = &title
	}

	var aliasPtr *string
	if rawAlias, ok := body["alias"]; ok {
		alias, isString := rawAlias.(string)
		if !isString {
			writeError(w, http.StatusBadRequest, "invalid", "alias must be a string")
			return
		}
		aliasPtr = &alias
	}
	if titlePtr == nil && aliasPtr == nil {
		writeError(w, http.StatusBadRequest, "invalid", "at least one of 'title' or 'alias' is required")
		return
	}

	// Validate through the session front door: the codec stays confined inside
	// Store.Get, so no raw session bead is cracked in the handler. A present-but-
	// non-session bead yields ErrSessionNotFound → the existing "not a session"
	// 400; an absent id stays on the beads.ErrNotFound chain → 404.
	sessFront := session.NewStore(store)
	info, err := sessFront.Get(id)
	if err != nil {
		if errors.Is(err, session.ErrSessionNotFound) {
			writeError(w, http.StatusBadRequest, "invalid", id+" is not a session")
			return
		}
		writeStoreError(w, err)
		return
	}
	// Preserve the empty-type heal RepairEmptyType performed on the raw bead.
	if info.Type == "" {
		sessFront.RepairTypeBestEffort(id)
	}

	catalog, err := s.workerSessionCatalog(store.Store)
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}
	updateFn := func() error {
		return catalog.UpdatePresentation(id, titlePtr, aliasPtr)
	}
	if aliasPtr != nil {
		// agent_name comes off the persisted Info from the front door — the
		// controller-managed-alias gate; the codec projection of the persisted
		// agent_name field, with no raw bead in the handler's hands.
		if strings.TrimSpace(info.AgentName) != "" {
			writeError(w, http.StatusForbidden, "forbidden", "alias is controller-managed for this session")
			return
		}
		if err := session.WithCitySessionAliasLock(s.state.CityPath(), *aliasPtr, func() error {
			if err := session.EnsureAliasAvailableWithConfig(store.Store, s.state.Config(), *aliasPtr, id); err != nil {
				return err
			}
			return updateFn()
		}); err != nil {
			writeSessionManagerError(w, err)
			return
		}
	} else if err := updateFn(); err != nil {
		writeSessionManagerError(w, err)
		return
	}

	// Re-fetch to get updated state.
	info, pr, err := sessionGetEnriched(session.NewStore(store), s.sessionManager(store.Store), id)
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}
	presp := sessionResponseWithReason(info, pr, s.state.Config(), s.state.SessionProvider(), strings.TrimSpace(s.state.CityPath()) != "")
	writeJSON(w, http.StatusOK, presp)
}

func resolveProviderForSessionOptions(info session.Info, metadata map[string]string, cfg *config.City) (*config.ResolvedProvider, error) {
	if cfg == nil {
		return nil, fmt.Errorf("no config")
	}
	agent, agentFound := findAgent(cfg, info.Template)
	if session.UseAgentTemplateForProviderResolution(legacySessionKind(metadata), metadata, info.Provider, agent.Provider, agentFound) {
		if !agentFound {
			return nil, fmt.Errorf("agent template %q not found", info.Template)
		}
		return config.ResolveProvider(&agent, &cfg.Workspace, cfg.Providers, exec.LookPath)
	}
	var lastErr error
	for _, providerName := range []string{info.Provider, info.Template} {
		providerName = strings.TrimSpace(providerName)
		if providerName == "" {
			continue
		}
		rp, err := config.ResolveProvider(&config.Agent{Provider: providerName}, &cfg.Workspace, cfg.Providers, exec.LookPath)
		if err == nil {
			return rp, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("provider for session %q not found", info.ID)
}
