// Package configedit provides serialized, atomic mutations of city.toml.
//
// It extracts the load → mutate → validate → write-back pattern used
// throughout the CLI (cmd/gc) into a reusable package that the API layer
// can share. All mutations go through [Editor], which serializes access
// with a mutex and writes atomically via temp file + rename.
package configedit

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/workspacesvc"
)

// Sentinel errors for typed error matching. API handlers use errors.Is() to
// map these to appropriate HTTP status codes without string matching.
var (
	// ErrNotFound is returned when a named resource (agent, rig, provider,
	// patch) doesn't exist in the config. Maps to HTTP 404.
	ErrNotFound = errors.New("resource not found")

	// ErrAlreadyExists is returned when creating a resource whose name
	// collides with an existing one. Maps to HTTP 409.
	ErrAlreadyExists = errors.New("resource already exists")

	// ErrPackDerived is returned when attempting to mutate a resource that
	// originates from an imported pack (must go through the patches API
	// instead). Maps to HTTP 409.
	ErrPackDerived = errors.New("resource is pack-derived")

	// ErrValidation is returned when a mutation would produce an invalid
	// config (duplicate names, missing required fields, etc.). Maps to
	// HTTP 400.
	ErrValidation = errors.New("validation failed")

	// ErrUnmodified signals that an [Editor.EditExpanded] callback
	// completed successfully without mutating the raw config, and the
	// writeback should be skipped. The Editor still releases its lock
	// and returns nil to the caller. Use this when a mutation lives
	// entirely outside city.toml (e.g., a write to
	// agents/<name>/agent.toml) so that we don't churn city.toml's
	// mtime or risk losing comments on a no-op rewrite.
	ErrUnmodified = errors.New("configedit: raw config unmodified")
)

// Origin describes where an agent or rig is defined in the config.
type Origin int

const (
	// OriginInline means the resource is defined directly in city.toml
	// (or a merged fragment) and can be edited in place.
	OriginInline Origin = iota
	// OriginDerived means the resource comes from pack expansion and
	// must be modified via [[patches.agent]] or [[patches.rigs]].
	OriginDerived
	// OriginNotFound means the resource was not found in any config.
	OriginNotFound
)

// Editor provides serialized, atomic mutations of a city.toml file.
// It is safe for concurrent use from multiple goroutines.
type Editor struct {
	mu       sync.Mutex
	tomlPath string
	fs       fsys.FS
}

// NewEditor creates an Editor for the city.toml at the given path.
func NewEditor(fs fsys.FS, tomlPath string) *Editor {
	return &Editor{
		tomlPath: tomlPath,
		fs:       fs,
	}
}

// Edit loads the raw config (no pack expansion), calls fn to mutate it,
// validates the result, and writes it back atomically. The mutex ensures
// only one mutation runs at a time.
func (e *Editor) Edit(fn func(cfg *config.City) error) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	cfg, err := e.loadForEdit()
	if err != nil {
		return err
	}

	if err := fn(cfg); err != nil {
		return err
	}

	if err := config.ValidateAgents(cfg.Agents); err != nil {
		return fmt.Errorf("%w: agents: %w", ErrValidation, err)
	}
	if err := config.ValidateRigs(cfg.Rigs, config.EffectiveHQPrefix(cfg)); err != nil {
		return fmt.Errorf("%w: rigs: %w", ErrValidation, err)
	}
	if err := config.ValidateServices(cfg.Services); err != nil {
		return fmt.Errorf("%w: services: %w", ErrValidation, err)
	}
	if err := workspacesvc.ValidateRuntimeSupport(cfg.Services); err != nil {
		return fmt.Errorf("%w: services: %w", ErrValidation, err)
	}
	if err := validateProviders(cfg.Providers); err != nil {
		return fmt.Errorf("%w: providers: %w", ErrValidation, err)
	}

	return e.write(cfg)
}

// EditExpanded loads both raw and expanded configs, calls fn with both,
// then validates and writes back the raw config. Use this when the
// mutation needs provenance detection (e.g., to decide whether to edit
// an inline agent or add a patch for a pack-derived agent).
//
// The fn receives the raw config (which will be written back) and the
// expanded config (read-only, for provenance checks). Only mutations
// to raw are persisted.
func (e *Editor) EditExpanded(fn func(raw, expanded *config.City) error) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	raw, err := e.loadForEdit()
	if err != nil {
		return fmt.Errorf("loading raw config: %w", err)
	}

	expanded, _, err := config.LoadWithIncludes(e.fs, e.tomlPath)
	if err != nil {
		return fmt.Errorf("loading expanded config: %w", err)
	}

	if err := fn(raw, expanded); err != nil {
		if errors.Is(err, ErrUnmodified) {
			return nil
		}
		return err
	}

	if err := config.ValidateAgents(raw.Agents); err != nil {
		return fmt.Errorf("%w: agents: %w", ErrValidation, err)
	}
	if err := config.ValidateRigs(raw.Rigs, config.EffectiveHQPrefix(raw)); err != nil {
		return fmt.Errorf("%w: rigs: %w", ErrValidation, err)
	}
	if err := config.ValidateServices(raw.Services); err != nil {
		return fmt.Errorf("%w: services: %w", ErrValidation, err)
	}
	if err := workspacesvc.ValidateRuntimeSupport(raw.Services); err != nil {
		return fmt.Errorf("%w: services: %w", ErrValidation, err)
	}
	if err := validateProviders(raw.Providers); err != nil {
		return fmt.Errorf("%w: providers: %w", ErrValidation, err)
	}

	return e.write(raw)
}

func (e *Editor) loadForEdit() (*config.City, error) {
	cfg, err := config.Load(e.fs, e.tomlPath)
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}
	if _, err := config.ApplySiteBindingsForEdit(e.fs, filepath.Dir(e.tomlPath), cfg); err != nil {
		return nil, fmt.Errorf("loading site binding: %w", err)
	}
	return cfg, nil
}

// write persists city.toml first, then .gc/site.toml. A crash between the
// two writes leaves city.toml with rig paths stripped while .gc/site.toml
// retains its previous state — producing an orphan legacy/unbound rig
// that the loader surfaces via warnings rather than the silent
// site-wins-over-stale-city state the reverse order would create.
//
// The city.toml write is skipped when on-disk content already matches,
// matching the idempotency guarantee documented on
// writeCityConfigForEditFS so repeated no-op mutations don't churn
// watcher mtime or break debounce.
func (e *Editor) write(cfg *config.City) error {
	cityPath := filepath.Dir(e.tomlPath)
	content, err := cfg.MarshalForWrite()
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	if err := fsys.WriteFileIfChangedAtomic(e.fs, e.tomlPath, content, 0o644); err != nil {
		return err
	}
	if err := config.PersistRigSiteBindings(e.fs, cityPath, cfg.Rigs); err != nil {
		// Surface the half-migrated state: city.toml has been rewritten
		// without rig paths, but the site binding wasn't persisted, so
		// any previously-bound rigs whose path came only from city.toml
		// are now unbound.
		return fmt.Errorf("writing .gc/site.toml failed after city.toml was rewritten — rigs may be unbound; re-run the command or `gc doctor --fix` to retry: %w", err)
	}
	return nil
}

// AgentOrigin determines whether an agent is defined inline in the raw
// config or derived from pack expansion. This is the two-phase detection
// pattern extracted from the CLI's doAgentSuspend/doAgentResume.
func AgentOrigin(raw, expanded *config.City, name string) Origin {
	// Check raw config first.
	for _, a := range raw.Agents {
		if config.AgentMatchesIdentity(&a, name) {
			return OriginInline
		}
	}
	// Check expanded config for pack-derived agents.
	for _, a := range expanded.Agents {
		if config.AgentMatchesIdentity(&a, name) {
			return OriginDerived
		}
	}
	return OriginNotFound
}

// RigOrigin determines whether a rig is defined inline in the raw config.
// Rigs cannot currently be pack-derived, so this is simpler than agents.
func RigOrigin(raw *config.City, name string) Origin {
	for _, r := range raw.Rigs {
		if r.Name == name {
			return OriginInline
		}
	}
	return OriginNotFound
}

// SetAgentSuspended sets the suspended field on an inline agent.
// Returns an error if the agent is not found in the config.
func SetAgentSuspended(cfg *config.City, name string, suspended bool) error {
	for i := range cfg.Agents {
		if config.AgentMatchesIdentity(&cfg.Agents[i], name) {
			cfg.Agents[i].Suspended = suspended
			return nil
		}
	}
	return fmt.Errorf("%w: agent %q", ErrNotFound, name)
}

// SetRigSuspended sets the suspended field on an inline rig.
// Returns an error if the rig is not found in the config.
func SetRigSuspended(cfg *config.City, name string, suspended bool) error {
	for i := range cfg.Rigs {
		if cfg.Rigs[i].Name == name {
			cfg.Rigs[i].Suspended = suspended
			return nil
		}
	}
	return fmt.Errorf("%w: rig %q", ErrNotFound, name)
}

// AddOrUpdateAgentPatch adds or updates an agent patch in the config's
// [[patches.agent]] section. If a patch for the given agent already
// exists, fn is called on it. Otherwise a new patch is created.
func AddOrUpdateAgentPatch(cfg *config.City, name string, fn func(p *config.AgentPatch)) error {
	dir, base := config.ParseQualifiedName(name)
	for i := range cfg.Patches.Agents {
		if cfg.Patches.Agents[i].Dir == dir && cfg.Patches.Agents[i].Name == base {
			fn(&cfg.Patches.Agents[i])
			return nil
		}
	}
	p := config.AgentPatch{Dir: dir, Name: base}
	fn(&p)
	cfg.Patches.Agents = append(cfg.Patches.Agents, p)
	return nil
}

// AddOrUpdateRigPatch adds or updates a rig patch in the config's
// [[patches.rigs]] section. If a patch for the given rig already exists,
// fn is called on it. Otherwise a new patch is created.
func AddOrUpdateRigPatch(cfg *config.City, name string, fn func(p *config.RigPatch)) error {
	for i := range cfg.Patches.Rigs {
		if cfg.Patches.Rigs[i].Name == name {
			fn(&cfg.Patches.Rigs[i])
			return nil
		}
	}
	p := config.RigPatch{Name: name}
	fn(&p)
	cfg.Patches.Rigs = append(cfg.Patches.Rigs, p)
	return nil
}

// boolPtr returns a pointer to a bool value.
func boolPtr(b bool) *bool { return &b }

// SuspendAgent suspends an agent, using inline edit, agent.toml write,
// or [[patches.agent]] depending on provenance. Writes desired state
// to durable config (not ephemeral session metadata).
func (e *Editor) SuspendAgent(name string) error {
	return e.EditExpanded(func(raw, expanded *config.City) error {
		return mutateAgentSuspended(e.fs, filepath.Dir(e.tomlPath), raw, expanded, name, true)
	})
}

// ResumeAgent resumes a suspended agent, mirroring [Editor.SuspendAgent].
func (e *Editor) ResumeAgent(name string) error {
	return e.EditExpanded(func(raw, expanded *config.City) error {
		return mutateAgentSuspended(e.fs, filepath.Dir(e.tomlPath), raw, expanded, name, false)
	})
}

// mutateAgentSuspended is the shared dispatch for SuspendAgent and
// ResumeAgent. Branches on agent provenance:
//   - OriginInline (city.toml [[agent]]): edit the raw struct.
//   - OriginDerived + convention-discovered (agents/<name>/): write
//     agents/<name>/agent.toml; also strip any legacy [[patches.agent]]
//     suspended override so it can't shadow the new value.
//   - OriginDerived + pack-declared: add or update [[patches.agent]].
//
// Returns [ErrUnmodified] when the change lives entirely in agent.toml
// and raw was not touched, so EditExpanded skips the city.toml writeback.
func mutateAgentSuspended(fs fsys.FS, cityRoot string, raw, expanded *config.City, name string, suspended bool) error {
	switch AgentOrigin(raw, expanded, name) {
	case OriginInline:
		return SetAgentSuspended(raw, name, suspended)
	case OriginDerived:
		if agent, ok := findLocalDiscoveredAgent(fs, expanded, cityRoot, name); ok {
			if err := WriteLocalDiscoveredAgentSuspended(fs, cityRoot, agent, suspended); err != nil {
				return err
			}
			// A pre-existing [[patches.agent]] suspended override would
			// silently shadow the agent.toml write (patch precedence).
			// Strip it here so the durable agent.toml value wins. Use
			// the discovered agent's full (Dir, Name) identity so we
			// only strip the matching patch, not a same-named entry
			// targeting a different rig.
			if StripAgentPatchSuspended(raw, agent.QualifiedName()) {
				return nil
			}
			return ErrUnmodified
		}
		return AddOrUpdateAgentPatch(raw, name, func(p *config.AgentPatch) {
			p.Suspended = boolPtr(suspended)
		})
	case OriginNotFound:
		return fmt.Errorf("%w: agent %q", ErrNotFound, name)
	}
	return fmt.Errorf("agent %q: unknown origin", name)
}

func findLocalDiscoveredAgent(fs fsys.FS, expanded *config.City, cityRoot, name string) (config.Agent, bool) {
	cityRoot = filepath.Clean(cityRoot)
	for _, a := range expanded.Agents {
		if !config.AgentMatchesIdentity(&a, name) {
			continue
		}
		if !LocalDiscoveredAgent(fs, cityRoot, a) {
			continue
		}
		return a, true
	}
	return config.Agent{}, false
}

// LocalDiscoveredAgent reports whether an agent's durable configuration
// lives in agents/<name>/agent.toml. Such agents are scaffolded purely by
// the convention layout (a prompt file under agents/<name>/) and are not
// declared in either city.toml [[agent]] or the city's pack.toml [[agent]].
//
// Pack-declared [[agent]] entries that happen to point at a conventional
// prompt template are intentionally excluded — for those, [[patches.agent]]
// is the correct mutation surface, since pack.toml [[agent]] takes
// precedence over agent.toml during composition. The pack-declared check
// matches on the agent's full (Dir, Name) identity so that a city-scoped
// discovered agent and a pack rig-scoped agent that happen to share a
// bare Name remain distinct.
func LocalDiscoveredAgent(fs fsys.FS, cityRoot string, agent config.Agent) bool {
	if agent.BindingName != "" {
		return false
	}
	// Convention discovery scans <cityRoot>/agents/<Name>/, which is
	// strictly city-scoped (Agent.Dir == ""). A rig-scoped agent that
	// happens to point its prompt_template at the city's agents/<name>/
	// prompt template is a different identity and must NOT be classified
	// as local-discovered — writing agent.toml there would corrupt the
	// city agent's durable state.
	if agent.Dir != "" {
		return false
	}
	cityRoot = filepath.Clean(cityRoot)
	agentDir := filepath.Join(cityRoot, "agents", agent.Name)
	switch filepath.Clean(agent.PromptTemplate) {
	case filepath.Join(agentDir, "prompt.template.md"),
		filepath.Join(agentDir, "prompt.md.tmpl"),
		filepath.Join(agentDir, "prompt.md"):
		// Conventional layout — eligible unless explicitly declared.
	default:
		return false
	}
	return !agentDeclaredInCityPack(fs, cityRoot, agent.Dir, agent.Name)
}

// agentDeclaredInCityPack reports whether (dir, name) appears as an
// explicit [[agent]] entry in <cityRoot>/pack.toml. Convention-discovered
// agents from agents/<name>/ are not [[agent]] entries and return false.
// Matching uses the full (Dir, Name) identity so that, for example, a
// rig-scoped pack agent (dir="rig", name="worker") does not shadow a
// city-scoped discovered agent of the same bare name.
func agentDeclaredInCityPack(fs fsys.FS, cityRoot, dir, name string) bool {
	packPath := filepath.Join(cityRoot, "pack.toml")
	data, err := fs.ReadFile(packPath)
	if err != nil {
		return false
	}
	var pc struct {
		Agents []struct {
			Dir  string `toml:"dir"`
			Name string `toml:"name"`
		} `toml:"agent"`
	}
	if _, err := toml.Decode(string(data), &pc); err != nil {
		return false
	}
	for _, a := range pc.Agents {
		if a.Dir == dir && a.Name == name {
			return true
		}
	}
	return false
}

// StripAgentPatchSuspended clears the Suspended override from any
// matching [[patches.agent]] entry so it can't shadow a durable
// agent.toml write. If a patch had only Suspended set (the shape produced
// by older suspend/resume code), the entire entry is dropped to avoid
// leaving an identity-only [[patches.agent]] block in city.toml.
// Returns true if any patch was modified.
func StripAgentPatchSuspended(cfg *config.City, name string) bool {
	dir, base := config.ParseQualifiedName(name)
	modified := false
	kept := cfg.Patches.Agents[:0:0]
	for _, p := range cfg.Patches.Agents {
		if p.Dir == dir && p.Name == base && p.Suspended != nil {
			p.Suspended = nil
			modified = true
			if isAgentPatchOnlyIdentity(p) {
				continue
			}
		}
		kept = append(kept, p)
	}
	if modified {
		cfg.Patches.Agents = kept
	}
	return modified
}

// isAgentPatchOnlyIdentity reports whether every field of p other than
// Dir and Name is the zero value — i.e., the patch carries no overrides.
// Reflection avoids drift as new fields are added to AgentPatch.
func isAgentPatchOnlyIdentity(p config.AgentPatch) bool {
	v := reflect.ValueOf(p)
	t := v.Type()
	for i := 0; i < v.NumField(); i++ {
		switch t.Field(i).Name {
		case "Dir", "Name":
			continue
		}
		if !v.Field(i).IsZero() {
			return false
		}
	}
	return true
}

// WriteLocalDiscoveredAgentSuspended writes the suspended state to
// agents/<name>/agent.toml using an atomic temp-file rename. When
// suspended is false and the file would become empty (no other fields),
// it is removed instead.
//
// Decoding into map[string]any (rather than a typed struct) preserves
// any user-set fields the caller didn't ask about. TOML comments and
// key ordering are not preserved — that is a limitation of the
// underlying decode/encode round trip, not this helper.
func WriteLocalDiscoveredAgentSuspended(fs fsys.FS, cityRoot string, agent config.Agent, suspended bool) error {
	agentTomlPath := filepath.Join(cityRoot, "agents", agent.Name, "agent.toml")

	values := make(map[string]any)
	data, err := fs.ReadFile(agentTomlPath)
	switch {
	case err == nil:
		if len(bytes.TrimSpace(data)) > 0 {
			if _, decodeErr := toml.Decode(string(data), &values); decodeErr != nil {
				return fmt.Errorf("reading agents/%s/agent.toml: %w", agent.Name, decodeErr)
			}
		}
	case os.IsNotExist(err):
		// Start from an empty config; suspend=true may create the file.
	default:
		return fmt.Errorf("reading agents/%s/agent.toml: %w", agent.Name, err)
	}

	if suspended {
		values["suspended"] = true
	} else {
		delete(values, "suspended")
	}

	if len(values) == 0 {
		if err := fs.Remove(agentTomlPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing agents/%s/agent.toml: %w", agent.Name, err)
		}
		return nil
	}

	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(values); err != nil {
		return fmt.Errorf("encoding agents/%s/agent.toml: %w", agent.Name, err)
	}
	if err := fsys.WriteFileAtomic(fs, agentTomlPath, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("writing agents/%s/agent.toml: %w", agent.Name, err)
	}
	return nil
}

// SuspendRig suspends a rig by setting suspended=true in city.toml.
func (e *Editor) SuspendRig(name string) error {
	return e.Edit(func(cfg *config.City) error {
		return SetRigSuspended(cfg, name, true)
	})
}

// ResumeRig resumes a rig by clearing suspended in city.toml.
func (e *Editor) ResumeRig(name string) error {
	return e.Edit(func(cfg *config.City) error {
		return SetRigSuspended(cfg, name, false)
	})
}

// SuspendCity sets workspace.suspended = true.
func (e *Editor) SuspendCity() error {
	return e.Edit(func(cfg *config.City) error {
		cfg.Workspace.Suspended = true
		return nil
	})
}

// ResumeCity sets workspace.suspended = false.
func (e *Editor) ResumeCity() error {
	return e.Edit(func(cfg *config.City) error {
		cfg.Workspace.Suspended = false
		return nil
	})
}

// CreateAgent adds a new agent to the config. Returns an error if an
// agent with the same qualified name already exists.
func (e *Editor) CreateAgent(a config.Agent) error {
	return e.Edit(func(cfg *config.City) error {
		qn := a.QualifiedName()
		for _, existing := range cfg.Agents {
			if existing.QualifiedName() == qn {
				return fmt.Errorf("%w: agent %q", ErrAlreadyExists, qn)
			}
		}
		cfg.Agents = append(cfg.Agents, a)
		return nil
	})
}

// AgentUpdate holds optional fields for a partial agent update.
type AgentUpdate struct {
	Provider  string
	Scope     string
	Suspended *bool
}

// UpdateAgent partially updates an existing agent. Uses EditExpanded for
// provenance detection — pack-derived agents return a clear error.
func (e *Editor) UpdateAgent(name string, patch AgentUpdate) error {
	return e.EditExpanded(func(raw, expanded *config.City) error {
		origin := AgentOrigin(raw, expanded, name)
		switch origin {
		case OriginDerived:
			return fmt.Errorf("%w: agent %q cannot be updated directly (use patches)", ErrPackDerived, name)
		case OriginNotFound:
			return fmt.Errorf("%w: agent %q", ErrNotFound, name)
		}
		for i := range raw.Agents {
			if config.AgentMatchesIdentity(&raw.Agents[i], name) {
				if patch.Provider != "" {
					raw.Agents[i].Provider = patch.Provider
				}
				if patch.Scope != "" {
					raw.Agents[i].Scope = patch.Scope
				}
				if patch.Suspended != nil {
					raw.Agents[i].Suspended = *patch.Suspended
				}
				return nil
			}
		}
		return fmt.Errorf("%w: agent %q", ErrNotFound, name)
	})
}

// DeleteAgent removes an inline agent from the config.
// Returns an error if the agent is not found.
func (e *Editor) DeleteAgent(name string) error {
	return e.EditExpanded(func(raw, expanded *config.City) error {
		origin := AgentOrigin(raw, expanded, name)
		switch origin {
		case OriginDerived:
			return fmt.Errorf("%w: agent %q cannot be deleted (use patches to override)", ErrPackDerived, name)
		case OriginNotFound:
			return fmt.Errorf("%w: agent %q", ErrNotFound, name)
		}
		for i := range raw.Agents {
			if config.AgentMatchesIdentity(&raw.Agents[i], name) {
				raw.Agents = append(raw.Agents[:i], raw.Agents[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("%w: agent %q", ErrNotFound, name)
	})
}

// CreateRig adds a new rig to the config. Returns an error if a rig with
// the same name already exists.
func (e *Editor) CreateRig(r config.Rig) error {
	return e.Edit(func(cfg *config.City) error {
		for _, existing := range cfg.Rigs {
			if existing.Name == r.Name {
				return fmt.Errorf("%w: rig %q", ErrAlreadyExists, r.Name)
			}
		}
		cfg.Rigs = append(cfg.Rigs, r)
		return nil
	})
}

// RigUpdate holds optional fields for a partial rig update. Pointer fields
// distinguish "not set" from "set to zero value" to avoid the PATCH
// zero-value trap (e.g., omitting suspended must not reset it to false).
type RigUpdate struct {
	Path          string
	Prefix        string
	DefaultBranch string
	Suspended     *bool
}

// UpdateRig partially updates an existing rig. Only non-nil/non-empty
// fields are applied. Returns an error if the rig is not found.
func (e *Editor) UpdateRig(name string, patch RigUpdate) error {
	return e.Edit(func(cfg *config.City) error {
		for i := range cfg.Rigs {
			if cfg.Rigs[i].Name == name {
				if patch.Path != "" {
					cfg.Rigs[i].Path = patch.Path
				}
				if patch.Prefix != "" {
					cfg.Rigs[i].Prefix = patch.Prefix
				}
				if patch.DefaultBranch != "" {
					cfg.Rigs[i].DefaultBranch = patch.DefaultBranch
				}
				if patch.Suspended != nil {
					cfg.Rigs[i].Suspended = *patch.Suspended
				}
				return nil
			}
		}
		return fmt.Errorf("%w: rig %q", ErrNotFound, name)
	})
}

// DeleteRig removes a rig and all its scoped agents from the config.
// Returns an error if the rig is not found.
func (e *Editor) DeleteRig(name string) error {
	return e.Edit(func(cfg *config.City) error {
		found := false
		for i := range cfg.Rigs {
			if cfg.Rigs[i].Name == name {
				cfg.Rigs = append(cfg.Rigs[:i], cfg.Rigs[i+1:]...)
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%w: rig %q", ErrNotFound, name)
		}
		// Remove rig-scoped agents.
		var kept []config.Agent
		for _, a := range cfg.Agents {
			if a.Dir != name {
				kept = append(kept, a)
			}
		}
		cfg.Agents = kept
		return nil
	})
}

// ProviderUpdate holds optional fields for a partial provider update.
// Pointer fields distinguish "not set" from "set to zero value."
//
// Base uses **string so callers can distinguish four cases:
//   - nil              → no-op (don't touch Base)
//   - &(*string)(nil)  → clear Base declaration (remove the TOML key)
//   - &(&"")           → set explicit empty (standalone opt-out)
//   - &(&"<name>")     → set concrete value
type ProviderUpdate struct {
	DisplayName        *string
	Base               **string
	Command            *string
	ACPCommand         *string
	Args               []string // nil = not set, non-nil = replace
	ACPArgs            []string // nil = not set, non-nil = replace
	ArgsAppend         []string // nil = not set, non-nil = replace
	PromptMode         *string
	PromptFlag         *string
	ReadyDelayMs       *int
	Env                map[string]string // nil = not set, non-nil = additive merge
	OptionsSchemaMerge *string
	OptionsSchema      []config.ProviderOption // nil = not set, non-nil = replace
}

// CreateProvider adds a new city-level provider to the config.
// Returns an error if a provider with the same name already exists.
func (e *Editor) CreateProvider(name string, spec config.ProviderSpec) error {
	return e.Edit(func(cfg *config.City) error {
		if cfg.Providers == nil {
			cfg.Providers = make(map[string]config.ProviderSpec)
		}
		if _, exists := cfg.Providers[name]; exists {
			return fmt.Errorf("%w: provider %q", ErrAlreadyExists, name)
		}
		cfg.Providers[name] = spec
		return nil
	})
}

// UpdateProvider partially updates an existing city-level provider.
// Returns an error if the provider is not found in the raw config
// (builtin-only providers cannot be updated directly — use patches).
func (e *Editor) UpdateProvider(name string, patch ProviderUpdate) error {
	return e.Edit(func(cfg *config.City) error {
		if cfg.Providers == nil {
			return fmt.Errorf("%w: provider %q", ErrNotFound, name)
		}
		spec, ok := cfg.Providers[name]
		if !ok {
			return fmt.Errorf("%w: provider %q", ErrNotFound, name)
		}
		if patch.DisplayName != nil {
			spec.DisplayName = *patch.DisplayName
		}
		if patch.Base != nil {
			// Outer non-nil: patch touches Base. Inner may be nil (clear
			// to absent/inherit) or a pointer to a string ("" opt-out
			// or concrete).
			spec.Base = *patch.Base
		}
		if patch.Command != nil {
			spec.Command = *patch.Command
		}
		if patch.ACPCommand != nil {
			spec.ACPCommand = *patch.ACPCommand
		}
		if patch.Args != nil {
			spec.Args = make([]string, len(patch.Args))
			copy(spec.Args, patch.Args)
		}
		if patch.ACPArgs != nil {
			spec.ACPArgs = make([]string, len(patch.ACPArgs))
			copy(spec.ACPArgs, patch.ACPArgs)
		}
		if patch.ArgsAppend != nil {
			spec.ArgsAppend = make([]string, len(patch.ArgsAppend))
			copy(spec.ArgsAppend, patch.ArgsAppend)
		}
		if patch.PromptMode != nil {
			spec.PromptMode = *patch.PromptMode
		}
		if patch.PromptFlag != nil {
			spec.PromptFlag = *patch.PromptFlag
		}
		if patch.ReadyDelayMs != nil {
			spec.ReadyDelayMs = *patch.ReadyDelayMs
		}
		if len(patch.Env) > 0 {
			if spec.Env == nil {
				spec.Env = make(map[string]string, len(patch.Env))
			}
			for k, v := range patch.Env {
				spec.Env[k] = v
			}
		}
		if patch.OptionsSchemaMerge != nil {
			spec.OptionsSchemaMerge = *patch.OptionsSchemaMerge
		}
		if patch.OptionsSchema != nil {
			spec.OptionsSchema = append([]config.ProviderOption(nil), patch.OptionsSchema...)
		}
		cfg.Providers[name] = spec
		return nil
	})
}

// DeleteProvider removes a city-level provider from the config.
// Returns an error if the provider is not found.
func (e *Editor) DeleteProvider(name string) error {
	return e.Edit(func(cfg *config.City) error {
		if cfg.Providers == nil {
			return fmt.Errorf("%w: provider %q", ErrNotFound, name)
		}
		if _, ok := cfg.Providers[name]; !ok {
			return fmt.Errorf("%w: provider %q", ErrNotFound, name)
		}
		delete(cfg.Providers, name)
		return nil
	})
}

// --- Patch resource mutations ---

// SetAgentPatch creates or replaces an agent patch in [[patches.agent]].
func (e *Editor) SetAgentPatch(patch config.AgentPatch) error {
	return e.Edit(func(cfg *config.City) error {
		if patch.Name == "" {
			return fmt.Errorf("agent patch: name is required")
		}
		for i := range cfg.Patches.Agents {
			if cfg.Patches.Agents[i].Dir == patch.Dir && cfg.Patches.Agents[i].Name == patch.Name {
				cfg.Patches.Agents[i] = patch
				return nil
			}
		}
		cfg.Patches.Agents = append(cfg.Patches.Agents, patch)
		return nil
	})
}

// DeleteAgentPatch removes an agent patch from [[patches.agent]].
func (e *Editor) DeleteAgentPatch(name string) error {
	return e.Edit(func(cfg *config.City) error {
		dir, base := config.ParseQualifiedName(name)
		for i := range cfg.Patches.Agents {
			if cfg.Patches.Agents[i].Dir == dir && cfg.Patches.Agents[i].Name == base {
				cfg.Patches.Agents = append(cfg.Patches.Agents[:i], cfg.Patches.Agents[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("%w: agent patch %q", ErrNotFound, name)
	})
}

// SetRigPatch creates or replaces a rig patch in [[patches.rigs]].
func (e *Editor) SetRigPatch(patch config.RigPatch) error {
	return e.Edit(func(cfg *config.City) error {
		if patch.Name == "" {
			return fmt.Errorf("rig patch: name is required")
		}
		for i := range cfg.Patches.Rigs {
			if cfg.Patches.Rigs[i].Name == patch.Name {
				cfg.Patches.Rigs[i] = patch
				return nil
			}
		}
		cfg.Patches.Rigs = append(cfg.Patches.Rigs, patch)
		return nil
	})
}

// DeleteRigPatch removes a rig patch from [[patches.rigs]].
func (e *Editor) DeleteRigPatch(name string) error {
	return e.Edit(func(cfg *config.City) error {
		for i := range cfg.Patches.Rigs {
			if cfg.Patches.Rigs[i].Name == name {
				cfg.Patches.Rigs = append(cfg.Patches.Rigs[:i], cfg.Patches.Rigs[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("%w: rig patch %q", ErrNotFound, name)
	})
}

// SetProviderPatch creates or replaces a provider patch in [[patches.providers]].
func (e *Editor) SetProviderPatch(patch config.ProviderPatch) error {
	return e.Edit(func(cfg *config.City) error {
		if patch.Name == "" {
			return fmt.Errorf("provider patch: name is required")
		}
		for i := range cfg.Patches.Providers {
			if cfg.Patches.Providers[i].Name == patch.Name {
				cfg.Patches.Providers[i] = patch
				return nil
			}
		}
		cfg.Patches.Providers = append(cfg.Patches.Providers, patch)
		return nil
	})
}

// DeleteProviderPatch removes a provider patch from [[patches.providers]].
func (e *Editor) DeleteProviderPatch(name string) error {
	return e.Edit(func(cfg *config.City) error {
		for i := range cfg.Patches.Providers {
			if cfg.Patches.Providers[i].Name == name {
				cfg.Patches.Providers = append(cfg.Patches.Providers[:i], cfg.Patches.Providers[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("%w: provider patch %q", ErrNotFound, name)
	})
}

// SetOrderOverride creates or replaces an order override in
// [orders.overrides]. Matches by name and rig.
func (e *Editor) SetOrderOverride(ov config.OrderOverride) error {
	return e.setOrderOverride(ov, false)
}

// MergeOrderOverride creates or updates an order override in
// [orders.overrides], preserving existing fields when the incoming
// override leaves them unset. Matches by name and rig.
func (e *Editor) MergeOrderOverride(ov config.OrderOverride) error {
	return e.setOrderOverride(ov, true)
}

func (e *Editor) setOrderOverride(ov config.OrderOverride, merge bool) error {
	return e.Edit(func(cfg *config.City) error {
		if ov.Name == "" {
			return fmt.Errorf("order override: name is required")
		}
		normalizeOrderOverrideForWrite(&ov)
		for i := range cfg.Orders.Overrides {
			if cfg.Orders.Overrides[i].Name == ov.Name && cfg.Orders.Overrides[i].Rig == ov.Rig {
				if merge {
					mergeOrderOverride(&cfg.Orders.Overrides[i], ov)
				} else {
					cfg.Orders.Overrides[i] = ov
				}
				return nil
			}
		}
		cfg.Orders.Overrides = append(cfg.Orders.Overrides, ov)
		return nil
	})
}

func normalizeOrderOverrideForWrite(ov *config.OrderOverride) {
	if ov == nil {
		return
	}
	if ov.Trigger == nil {
		ov.Trigger = ov.Gate
	}
	ov.Gate = nil
}

func mergeOrderOverride(dst *config.OrderOverride, src config.OrderOverride) {
	if dst == nil {
		return
	}
	if src.Enabled != nil {
		dst.Enabled = src.Enabled
	}
	if src.Trigger != nil {
		dst.Trigger = src.Trigger
	}
	if src.Interval != nil {
		dst.Interval = src.Interval
	}
	if src.Schedule != nil {
		dst.Schedule = src.Schedule
	}
	if src.Check != nil {
		dst.Check = src.Check
	}
	if src.On != nil {
		dst.On = src.On
	}
	if src.Pool != nil {
		dst.Pool = src.Pool
	}
	if src.Timeout != nil {
		dst.Timeout = src.Timeout
	}
}

// DeleteOrderOverride removes an order override by name and rig.
func (e *Editor) DeleteOrderOverride(name, rig string) error {
	return e.Edit(func(cfg *config.City) error {
		for i := range cfg.Orders.Overrides {
			if cfg.Orders.Overrides[i].Name == name && cfg.Orders.Overrides[i].Rig == rig {
				cfg.Orders.Overrides = append(cfg.Orders.Overrides[:i], cfg.Orders.Overrides[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("%w: order override %q", ErrNotFound, name)
	})
}

// validateProviders checks that every city-level provider is authorable:
// either it declares a Command directly, or it has a Base set (in which
// case Command can be inherited via the chain walk). A provider with
// neither a Command nor a Base is rejected.
//
// Base presence is presence-aware (*string): any non-nil pointer counts
// as "base declared" — including the explicit-empty opt-out `base = ""`.
// The chain walker later resolves whether the declared base actually
// produces a Command; that's a load-time concern, not a CRUD one.
func validateProviders(providers map[string]config.ProviderSpec) error {
	for name, spec := range providers {
		if spec.Command != "" {
			continue
		}
		if spec.Base != nil {
			continue
		}
		return fmt.Errorf("provider %q: command is required (or set base to inherit)", name)
	}
	if err := config.ValidateCustomProviderOptions(providers); err != nil {
		return err
	}
	return nil
}
