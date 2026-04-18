package api

import (
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
	"github.com/gastownhall/gascity/internal/sessionlog"
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
	Attached    bool   `json:"attached"`

	// Classification fields derived from config (for dashboard grouping).
	Rig  string `json:"rig,omitempty"`
	Pool string `json:"pool,omitempty"`

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

	// ConfiguredNamedSession marks canonical configured sessions materialized
	// from [[named_session]] configuration.
	ConfiguredNamedSession bool `json:"configured_named_session,omitempty"`

	// Options contains the effective per-session option overrides from
	// template_overrides bead metadata (e.g., {"permission_mode":"unrestricted"}).
	Options map[string]string `json:"options,omitempty"`

	// Metadata exposes mc_-prefixed bead metadata for external consumers.
	Metadata map[string]string `json:"metadata,omitempty"`
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
	// Populate pool from config lookup. The pool field is the agent's
	// base name (e.g., "polecat"), useful for dashboard type classification.
	if cfg != nil {
		if agent, ok := findAgent(cfg, info.Template); ok && isMultiSessionAgent(agent) {
			r.Pool = agent.Name
		}
	}
	if !info.LastActive.IsZero() {
		r.LastActive = info.LastActive.Format(time.RFC3339)
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
	if k := b.Metadata["mc_session_kind"]; k != "" {
		r.Kind = k
	}
	// Surface bead-persisted sleep/hold/quarantine reason.
	if reason := session.LifecycleDisplayReason(b.Status, b.Metadata, time.Now().UTC()); reason != "" {
		r.Reason = reason
	}
	r.ConfiguredNamedSession = strings.TrimSpace(b.Metadata[apiNamedSessionMetadataKey]) == "true"
	r.SubmissionCapabilities = session.SubmissionCapabilitiesForMetadata(b.Metadata, hasDeferredQueue)
	// Expose only mc_* prefixed metadata keys to API consumers.
	// Internal fields (session_key, command, work_dir, etc.) are redacted.
	r.Metadata = filterMetadata(b.Metadata)
	return r
}

// filterMetadataAllowedKeys lists non-mc_ metadata keys that are safe to expose.
var filterMetadataAllowedKeys = map[string]bool{
	"template_overrides": true,
}

// filterMetadata returns only metadata keys with the "mc_" prefix plus
// explicitly allowlisted keys. This prevents leaking internal bead fields
// (session_key, command, work_dir, quarantine state) to API consumers.
func filterMetadata(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	filtered := make(map[string]string)
	for k, v := range m {
		if strings.HasPrefix(k, "mc_") || filterMetadataAllowedKeys[k] {
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
	mgr := s.sessionManager(store)
	cfg := s.state.Config()
	sp := s.state.SessionProvider()

	q := r.URL.Query()
	stateFilter := q.Get("state")
	templateFilter := q.Get("template")
	wantPeek := q.Get("peek") == "true"

	sessions, err := mgr.List(stateFilter, templateFilter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	// Build bead index for reason enrichment.
	beadIndex := make(map[string]*beads.Bead)
	if all, err := store.List(beads.ListQuery{Label: session.LabelSession}); err == nil {
		for i := range all {
			beadIndex[all[i].ID] = &all[i]
		}
	}

	items := make([]sessionResponse, len(sessions))
	hasDeferredQueue := strings.TrimSpace(s.state.CityPath()) != ""
	for i, sess := range sessions {
		items[i] = sessionResponseWithReason(sess, beadIndex[sess.ID], cfg, hasDeferredQueue)
		s.enrichSessionResponse(&items[i], sess, cfg, sp, wantPeek)
	}

	pp := parsePagination(r, maxPaginationLimit)
	if !pp.IsPaging {
		if pp.Limit < len(items) {
			items = items[:pp.Limit]
		}
		writeJSON(w, http.StatusOK, listResponse{Items: items, Total: len(items)})
		return
	}
	page, total, nextCursor := paginate(items, pp)
	if page == nil {
		page = []sessionResponse{}
	}
	writeJSON(w, http.StatusOK, listResponse{Items: page, Total: total, NextCursor: nextCursor})
}

func (s *Server) handleSessionGet(w http.ResponseWriter, r *http.Request) {
	store := s.state.CityBeadStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no bead store configured")
		return
	}
	mgr := s.sessionManager(store)
	cfg := s.state.Config()
	sp := s.state.SessionProvider()

	id, err := s.resolveSessionIDAllowClosedWithConfig(store, r.PathValue("id"))
	if err != nil {
		writeResolveError(w, err)
		return
	}
	info, err := mgr.Get(id)
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}
	b, _ := store.Get(id)
	wantPeek := r.URL.Query().Get("peek") == "true"
	resp := sessionResponseWithReason(info, &b, cfg, strings.TrimSpace(s.state.CityPath()) != "")
	s.enrichSessionResponse(&resp, info, cfg, sp, wantPeek)
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleSessionSuspend(w http.ResponseWriter, r *http.Request) {
	store := s.state.CityBeadStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no bead store configured")
		return
	}
	mgr := s.sessionManager(store)

	id, err := s.resolveSessionIDMaterializingNamedWithContext(r.Context(), store, r.PathValue("id"))
	if err != nil {
		writeResolveError(w, err)
		return
	}
	if err := mgr.Suspend(id); err != nil {
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
	mgr := s.sessionManager(store)

	id, err := s.resolveSessionIDWithConfig(store, r.PathValue("id"))
	if err != nil {
		writeResolveError(w, err)
		return
	}
	nudgeIDs, err := session.WaitNudgeIDs(store, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if err := mgr.Close(id); err != nil {
		writeSessionManagerError(w, err)
		return
	}
	if err := withdrawQueuedWaitNudges(store, s.state.CityPath(), nudgeIDs); err != nil {
		log.Printf("gc api: withdrawing queued wait nudges after close %s: %v", id, err)
	}

	// Optional: permanently delete the bead after closing.
	if r.URL.Query().Get("delete") == "true" {
		if err := store.Delete(id); err != nil {
			log.Printf("gc api: deleting bead after close %s: %v", id, err)
			writeError(w, http.StatusInternalServerError, "internal", "closed but delete failed: "+err.Error())
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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

	mgr := s.sessionManager(store)
	if err := mgr.Rename(id, body.Title); err != nil {
		writeSessionManagerError(w, err)
		return
	}

	// Re-fetch to return the updated session, consistent with PATCH.
	info, err := mgr.Get(id)
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
func (s *Server) enrichSessionResponse(resp *sessionResponse, info session.Info, cfg *config.City, sp runtime.Provider, wantPeek bool) {
	if info.State != session.StateActive {
		return
	}

	resp.Running = sp.IsRunning(info.SessionName)

	// Active bead: prefer canonical session ownership, with legacy
	// session_name/alias/template fallbacks for old in-progress records.
	resp.ActiveBead = s.findActiveBeadForAssignees("", info.ID, info.SessionName, info.Alias, info.Template)

	// Peek preview (opt-in, only when running).
	if wantPeek && resp.Running {
		if output, err := sp.Peek(info.SessionName, 5); err == nil {
			resp.LastOutput = output
		}
	}

	// Model + context usage (best-effort).
	if resp.Running && info.WorkDir != "" {
		workDir := info.WorkDir
		if abs, err := filepath.Abs(workDir); err == nil {
			workDir = abs
		}
		searchPaths := s.sessionLogSearchPaths
		if searchPaths == nil && cfg != nil {
			searchPaths = sessionlog.MergeSearchPaths(cfg.Daemon.ObservePaths)
		}
		if searchPaths == nil {
			searchPaths = sessionlog.DefaultSearchPaths()
		}
		// Prefer session-key lookup to avoid cross-reading another session's transcript.
		// Cache the resolved file path — session files don't move once created.
		var sessionFile string
		if info.SessionKey != "" {
			sessionFile = sessionlog.FindSessionFileByID(searchPaths, workDir, info.SessionKey)
		} else {
			sessionFile = sessionlog.FindSessionFileForProvider(searchPaths, info.Provider, workDir)
		}
		if sessionFile != "" {
			if meta, err := sessionlog.ExtractTailMetaFromSearchPaths(searchPaths, sessionFile); err == nil && meta != nil {
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

	mgr := s.sessionManager(store)
	updateFn := func() error {
		return mgr.UpdatePresentation(id, titlePtr, aliasPtr)
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
	info, err := mgr.Get(id)
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
