package configedit

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// ErrRevisionConflict means the patch changed since the caller read it.
var ErrRevisionConflict = errors.New("agent patch revision conflict")

// ErrPatchScope means an override cannot be confined to one session identity.
var ErrPatchScope = errors.New("agent patch target is not a single session")

// AgentPatchMerge changes only provider and option defaults on an exact agent
// patch. Revision is mandatory; use AgentPatchRevision(nil) for an absent patch.
// Delete restores absence, conditional on that same revision. Clearing an option
// removes the patch key, exposing any underlying configured default again.
type AgentPatchMerge struct {
	Dir            string            `json:"dir,omitempty"`
	Rig            string            `json:"rig,omitempty"`
	Name           string            `json:"name" minLength:"1" doc:"Exact singleton agent template name; qualify its rig with dir or rig."`
	Revision       string            `json:"revision" minLength:"1" doc:"Expected X-GC-Patch-Revision from GET, or absent for a missing patch."`
	Provider       *string           `json:"provider,omitempty" doc:"Provider override for the next start."`
	OptionDefaults map[string]string `json:"option_defaults,omitempty" doc:"Merge these option defaults, including model, preserving other keys."`
	ClearProvider  bool              `json:"clear_provider,omitempty" doc:"Remove this patch's provider field, exposing the underlying configuration."`
	RemoveOptions  []string          `json:"remove_options,omitempty" doc:"Remove these option keys from this patch, exposing underlying defaults."`
	Delete         bool              `json:"delete,omitempty" doc:"Remove the entire patch under the expected revision, restoring prior absence."`
}

// AgentPatchMergeResult returns the old patch for exact restoration and the
// revision of the resulting patch. A nil Before or After means absent.
type AgentPatchMergeResult struct {
	Before   *config.AgentPatch `json:"before,omitempty"`
	After    *config.AgentPatch `json:"after,omitempty"`
	Revision string             `json:"revision"`
}

// AgentPatchRevision is an opaque content revision of the complete raw patch.
// It includes pointer presence and every field, so an unrelated edit conflicts.
// The literal "absent" is the revision for a patch that does not exist.
func AgentPatchRevision(patch *config.AgentPatch) string {
	if patch == nil {
		return "absent"
	}
	data, _ := json.Marshal(patch) // AgentPatch contains only JSON-encodable fields.
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

// MergeAgentPatch merges a revision-guarded override under the Editor lock,
// preserving all other fields. Multi-session templates and wildcard targets
// are refused before writing; session-specific patches are not config surface.
func (e *Editor) MergeAgentPatch(change AgentPatchMerge) (AgentPatchMergeResult, error) {
	var result AgentPatchMergeResult
	err := e.EditExpanded(func(raw, expanded *config.City) error {
		target := config.AgentPatch{Dir: change.Dir, Rig: change.Rig, Name: change.Name}
		if err := target.Validate(); err != nil {
			return fmt.Errorf("%w: %w", ErrValidation, err)
		}
		if change.Revision == "" || change.Provider != nil && change.ClearProvider || change.Delete && (change.Provider != nil || change.ClearProvider || len(change.OptionDefaults)+len(change.RemoveOptions) != 0) {
			return fmt.Errorf("%w: revision required; set, clear and delete operations must not overlap", ErrValidation)
		}
		for _, key := range change.RemoveOptions {
			if _, set := change.OptionDefaults[key]; set || strings.TrimSpace(key) == "" {
				return fmt.Errorf("%w: invalid or overlapping option %q", ErrValidation, key)
			}
		}
		name := target.TargetQualifiedName()
		if target.Rig == "*" {
			return fmt.Errorf("%w: wildcard %q", ErrPatchScope, name)
		}
		var agent *config.Agent
		for i := range expanded.Agents {
			if expanded.Agents[i].QualifiedName() == name {
				agent = &expanded.Agents[i]
				break
			}
		}
		if agent == nil {
			return fmt.Errorf("%w: agent %q", ErrNotFound, name)
		}
		named := 0
		for _, ns := range expanded.NamedSessions {
			if ns.TemplateQualifiedName() == name {
				named++
			}
		}
		if agent.SupportsMultipleSessions() || named > 1 {
			return fmt.Errorf("%w: %q requires max_active_sessions=1 and at most one named identity", ErrPatchScope, name)
		}
		index := -1
		for i := range raw.Patches.Agents {
			if raw.Patches.Agents[i].TargetQualifiedName() == name {
				if index >= 0 {
					return fmt.Errorf("%w: %q has multiple raw patch entries", ErrPatchScope, name)
				}
				index = i
				prior := raw.Patches.Agents[i]
				result.Before = &prior
			}
		}
		if AgentPatchRevision(result.Before) != change.Revision {
			return fmt.Errorf("%w: %q; read the patch again", ErrRevisionConflict, name)
		}
		if change.Delete {
			if index >= 0 {
				raw.Patches.Agents = append(raw.Patches.Agents[:index], raw.Patches.Agents[index+1:]...)
			}
			result.Revision = AgentPatchRevision(nil)
			return nil
		}
		merged := target
		if result.Before != nil {
			merged = *result.Before
		}
		if change.Provider != nil {
			merged.Provider = change.Provider
		}
		if change.ClearProvider {
			merged.Provider = nil
		}
		merged.OptionDefaults = maps.Clone(merged.OptionDefaults)
		for key, value := range change.OptionDefaults {
			if merged.OptionDefaults == nil {
				merged.OptionDefaults = make(map[string]string)
			}
			merged.OptionDefaults[key] = value
		}
		for _, key := range change.RemoveOptions {
			delete(merged.OptionDefaults, key)
		}
		if len(merged.OptionDefaults) == 0 {
			merged.OptionDefaults = nil
		}
		if index < 0 {
			raw.Patches.Agents = append(raw.Patches.Agents, merged)
		} else {
			raw.Patches.Agents[index] = merged
		}
		result.After = &merged
		result.Revision = AgentPatchRevision(&merged)
		return nil
	})
	if err != nil {
		return AgentPatchMergeResult{}, err
	}
	return result, nil
}
