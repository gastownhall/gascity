package api

import (
	"context"
	"encoding/json"
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

// sessionResponseWithReason builds a session response that includes the
// reason field derived from bead metadata. If the bead is nil (not found
// in the index), the reason is omitted.
func sessionResponseWithReason(info session.Info, b *beads.Bead, cfg *config.City, hasDeferredQueue bool) sessionResponse {
	r := sessionToResponse(info, cfg)
	// Expose effective options: provider EffectiveDefaults merged with
	// per-session template_overrides. The dashboard uses this to display
	// the actual permission mode and other settings.
	if b != nil && cfg != nil {
		rp, _ := resolveProviderForTemplate(info.Template, cfg)
		if rp != nil && len(rp.EffectiveDefaults) > 0 {
			merged := make(map[string]string, len(rp.EffectiveDefaults))
			for k, v := range rp.EffectiveDefaults {
				merged[k] = v
			}
			if raw := b.Metadata["template_overrides"]; raw != "" {
				var overrides map[string]string
				if err := json.Unmarshal([]byte(raw), &overrides); err == nil {
					for k, v := range overrides {
						if k != "initial_message" {
							merged[k] = v
						}
					}
				}
			}
			r.Options = merged
		}
	}
	if b == nil || info.Closed {
		return r
	}
	// Populate kind from persisted metadata.
	if k := b.Metadata["real_world_app_session_kind"]; k != "" {
		r.Kind = k
	}
	r.Reason = session.LifecycleDisplayReason(b.Status, b.Metadata, time.Now().UTC())
	r.ConfiguredNamedSession = strings.TrimSpace(b.Metadata[apiNamedSessionMetadataKey]) == "true"
	r.SubmissionCapabilities = session.SubmissionCapabilitiesForMetadata(b.Metadata, hasDeferredQueue)
	// Expose only real_world_app_* prefixed metadata keys to API consumers.
	// Internal fields (session_key, command, work_dir, etc.) are redacted.
	r.Metadata = filterMetadata(b.Metadata)
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
	store := s.state.CityBeadStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no bead store configured")
		return
	}
	catalog, err := s.workerSessionCatalog(store)
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}
	cfg := s.state.Config()

	q := r.URL.Query()
	stateFilter := q.Get("state")
	templateFilter := q.Get("template")
	wantPeek := q.Get("peek") == "true"

	all, partialErrors, err := sessionReadModelRows(store)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	listResult := catalog.ListFullFromBeads(all, stateFilter, templateFilter)
	sessions := listResult.Sessions

	// Build bead index for reason enrichment.
	beadIndex := make(map[string]*beads.Bead)
	for i := range listResult.Beads {
		beadIndex[listResult.Beads[i].ID] = &listResult.Beads[i]
	}

	items := make([]sessionResponse, len(sessions))
	hasDeferredQueue := strings.TrimSpace(s.state.CityPath()) != ""
	for i, sess := range sessions {
		items[i] = sessionResponseWithReason(sess, beadIndex[sess.ID], cfg, hasDeferredQueue)
		s.enrichSessionResponse(&items[i], sess, cfg, s.runtimeSessionResponseHandle(sess), wantPeek, false, false)
	}

	pp := parsePagination(r, maxPaginationLimit)
	if !pp.IsPaging {
		if pp.Limit < len(items) {
			items = items[:pp.Limit]
		}
		writeJSON(w, http.StatusOK, listResponse{
			Items:         items,
			Total:         len(items),
			Partial:       len(partialErrors) > 0,
			PartialErrors: partialErrors,
		})
		return
	}
	page, total, nextCursor := paginate(items, pp)
	if page == nil {
		page = []sessionResponse{}
	}
	writeJSON(w, http.StatusOK, listResponse{
		Items:         page,
		Total:         total,
		NextCursor:    nextCursor,
		Partial:       len(partialErrors) > 0,
		PartialErrors: partialErrors,
	})
}

func (s *Server) handleSessionGet(w http.ResponseWriter, r *http.Request) {
	store := s.state.CityBeadStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no bead store configured")
		return
	}
	catalog, err := s.workerSessionCatalog(store)
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}
	cfg := s.state.Config()

	id, err := s.resolveSessionIDAllowClosedWithConfig(store, r.PathValue("id"))
	if err != nil {
		writeResolveError(w, err)
		return
	}
	info, err := catalog.Get(id)
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}
	b, _ := store.Get(id)
	wantPeek := r.URL.Query().Get("peek") == "true"
	resp := sessionResponseWithReason(info, &b, cfg, strings.TrimSpace(s.state.CityPath()) != "")
	handle, err := s.workerHandleForSession(store, id)
	if err == nil {
		s.enrichSessionResponse(&resp, info, cfg, handle, wantPeek, true, true)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleSessionSuspend(w http.ResponseWriter, r *http.Request) {
	store := s.state.CityBeadStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no bead store configured")
		return
	}

	id, err := s.resolveSessionIDMaterializingNamedWithContext(r.Context(), store, r.PathValue("id"))
	if err != nil {
		writeResolveError(w, err)
		return
	}
	handle, err := s.workerHandleForSession(store, id)
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
	store := s.state.CityBeadStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no bead store configured")
		return
	}
	id, err := s.resolveSessionIDWithConfig(store, r.PathValue("id"))
	if err != nil {
		writeResolveError(w, err)
		return
	}
	handle, err := s.workerHandleForSession(store, id)
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}
	nudgeIDs, err := session.WaitNudgeIDs(store, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if err := handle.Close(r.Context()); err != nil {
		writeSessionManagerError(w, err)
		return
	}
	if err := withdrawQueuedWaitNudges(store, s.state.CityPath(), nudgeIDs); err != nil {
		log.Printf("gc api: withdrawing queued wait nudges after close %s: %v", id, err)
	}

	// Optional: permanently delete the bead after closing.
	if r.URL.Query().Get("delete") == "true" {
		if err := deleteSessionBeadAfterClose(store, id); err != nil {
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

// handleSessionWake clears hold and quarantine on a session.
func (s *Server) handleSessionWake(w http.ResponseWriter, r *http.Request) {
	store := s.state.CityBeadStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no bead store configured")
		return
	}

	id, err := s.resolveSessionIDMaterializingNamedWithContext(r.Context(), store, r.PathValue("id"))
	if err != nil {
		writeResolveError(w, err)
		return
	}

	b, err := store.Get(id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if !session.IsSessionBeadOrRepairable(b) {
		writeError(w, http.StatusBadRequest, "invalid", id+" is not a session")
		return
	}
	session.RepairEmptyType(store, &b)
	nudgeIDs, err := session.WakeSession(store, b, time.Now().UTC())
	if err != nil {
		if state, conflict := session.WakeConflictState(err); conflict {
			writeError(w, http.StatusConflict, "conflict", "session "+id+" is "+state)
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if err := withdrawQueuedWaitNudges(store, s.state.CityPath(), nudgeIDs); err != nil {
		log.Printf("gc api: withdrawing queued wait nudges after wake %s: %v", id, err)
	}
	// Clear in-memory crash tracker so the reconciler doesn't immediately
	// re-quarantine the session based on stale crash history.
	sessionName := b.Metadata["session_name"]
	if sessionName != "" {
		s.state.ClearCrashHistory(sessionName)
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "id": id})
}

// handleSessionRename updates a session's title.
func (s *Server) handleSessionRename(w http.ResponseWriter, r *http.Request) {
	store := s.state.CityBeadStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no bead store configured")
		return
	}

	id, err := s.resolveSessionIDWithConfig(store, r.PathValue("id"))
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

	b, err := store.Get(id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if !session.IsSessionBeadOrRepairable(b) {
		writeError(w, http.StatusBadRequest, "invalid", id+" is not a session")
		return
	}
	session.RepairEmptyType(store, &b)

	handle, err := s.workerHandleForSession(store, id)
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}
	if err := handle.Rename(r.Context(), body.Title); err != nil {
		writeSessionManagerError(w, err)
		return
	}

	// Re-fetch to return the updated session, consistent with PATCH.
	catalog, err := s.workerSessionCatalog(store)
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}
	info, err := catalog.Get(id)
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}
	updated, _ := store.Get(id)
	rresp := sessionResponseWithReason(info, &updated, s.state.Config(), strings.TrimSpace(s.state.CityPath()) != "")
	writeJSON(w, http.StatusOK, rresp)
}

// enrichSessionResponse populates runtime fields on a session response:
// running state, active bead, peek output, and model/context metadata.
func (s *Server) enrichSessionResponse(resp *sessionResponse, info session.Info, cfg *config.City, runtimeHandle any, wantPeek, liveActiveBead, allowWorkdirTranscriptDiscovery bool) {
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
		store := s.state.CityBeadStore()
		if store == nil {
			return
		}
		resolved, err := s.workerHandleForSession(store, info.ID)
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

	// Peek preview (opt-in, only when running).
	if wantPeek && resp.Running && peekHandle != nil {
		if output, err := peekHandle.Peek(context.Background(), 5); err == nil {
			resp.LastOutput = output
		}
	}

	// Model + context usage (best-effort).
	if resp.Running && info.WorkDir != "" {
		workDir := info.WorkDir
		if abs, err := filepath.Abs(workDir); err == nil {
			workDir = abs
		}
		factory, err := s.workerFactory(s.state.CityBeadStore())
		if err != nil {
			return
		}
		// Prefer session-key lookup to avoid cross-reading another session's transcript.
		// Cache the resolved file path — session files don't move once created.
		provider := info.Provider
		if strings.TrimSpace(provider) == "" && cfg != nil {
			provider, _ = resolveProviderInfo(provider, cfg)
		}
		if !allowWorkdirTranscriptDiscovery && !canUseCheapTranscriptLookup(provider, info.SessionKey) {
			return
		}
		sessionFile := factory.DiscoverTranscript(provider, workDir, info.SessionKey)
		if sessionFile != "" {
			if meta, err := factory.TailMeta(sessionFile); err == nil && meta != nil {
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

func canUseCheapTranscriptLookup(provider, sessionKey string) bool {
	if strings.TrimSpace(sessionKey) == "" {
		return false
	}
	p := strings.ToLower(strings.TrimSpace(provider))
	if strings.Contains(p, "codex") || strings.Contains(p, "gemini") {
		return false
	}
	return true
}

// handleSessionPatch handles PATCH /v0/session/{id}. Title and alias are mutable.
func (s *Server) handleSessionPatch(w http.ResponseWriter, r *http.Request) {
	store := s.state.CityBeadStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no bead store configured")
		return
	}

	id, err := s.resolveSessionIDWithConfig(store, r.PathValue("id"))
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

	b, err := store.Get(id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if !session.IsSessionBeadOrRepairable(b) {
		writeError(w, http.StatusBadRequest, "invalid", id+" is not a session")
		return
	}
	session.RepairEmptyType(store, &b)

	catalog, err := s.workerSessionCatalog(store)
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}
	updateFn := func() error {
		return catalog.UpdatePresentation(id, titlePtr, aliasPtr)
	}
	if aliasPtr != nil {
		if strings.TrimSpace(b.Metadata["agent_name"]) != "" {
			writeError(w, http.StatusForbidden, "forbidden", "alias is controller-managed for this session")
			return
		}
		if err := session.WithCitySessionAliasLock(s.state.CityPath(), *aliasPtr, func() error {
			if err := session.EnsureAliasAvailableWithConfig(store, s.state.Config(), *aliasPtr, id); err != nil {
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
	info, err := catalog.Get(id)
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}
	updated, _ := store.Get(id)
	presp := sessionResponseWithReason(info, &updated, s.state.Config(), strings.TrimSpace(s.state.CityPath()) != "")
	writeJSON(w, http.StatusOK, presp)
}

// resolveProviderForTemplate resolves the provider for an agent template,
// returning the full ResolvedProvider with EffectiveDefaults and OptionsSchema.
func resolveProviderForTemplate(template string, cfg *config.City) (*config.ResolvedProvider, error) {
	if cfg == nil {
		return nil, fmt.Errorf("no config")
	}
	agent, ok := findAgent(cfg, template)
	if !ok {
		return nil, fmt.Errorf("agent %q not found", template)
	}
	return config.ResolveProvider(&agent, &cfg.Workspace, cfg.Providers, exec.LookPath)
}
