package api

import (
	"encoding/json"
	"fmt"
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

	// ConfiguredNamedSession marks canonical singleton sessions materialized from
	// [[named_session]] configuration.
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
	r.Reason = session.LifecycleDisplayReason(b.Status, b.Metadata, time.Now().UTC())
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

// enrichSessionResponse populates runtime fields on a session response:
// running state, active bead, peek output, and model/context metadata.
func (s *Server) enrichSessionResponse(resp *sessionResponse, info session.Info, cfg *config.City, sp runtime.Provider, wantPeek bool) {
	if info.State != session.StateActive {
		return
	}

	resp.Running = sp.IsRunning(info.SessionName)

	// Active bead: search rig stores for in_progress work assigned to the
	// concrete session first, then fall back to alias/runtime/session names.
	// Alias inclusion preserves compatibility with role flows that assign
	// by alias (e.g., mayor, sky, wolf) until all assigners migrate to the
	// concrete session ID.
	//
	// Signature is findActiveBeadForAssignees(rig string, assignees ...string);
	// pass "" for rig (search all rigs) and put info.Alias in the variadic.
	// A previous fix accidentally passed info.Alias as the first positional
	// (rig) argument, which silently narrowed the search to a rig named after
	// the alias — so alias-assigned work still disappeared from ActiveBead.
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
			if meta, err := sessionlog.ExtractTailMeta(sessionFile); err == nil && meta != nil {
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
