// Package configedit provides serialized, atomic mutations of city.toml.
//
// It extracts the load → mutate → validate → write-back pattern used
// throughout the CLI (cmd/gc) into a reusable package that the API layer
// can share. All mutations go through [Editor], which serializes access
// with a mutex and writes atomically via temp file + rename.
package configedit

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"

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
	if err := config.ValidateRigs(cfg.Rigs, cfg.Workspace.Name); err != nil {
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
		return err
	}

	if err := config.ValidateAgents(raw.Agents); err != nil {
		return fmt.Errorf("%w: agents: %w", ErrValidation, err)
	}
	if err := config.ValidateRigs(raw.Rigs, raw.Workspace.Name); err != nil {
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

// SuspendAgent suspends an agent, using inline edit or patch depending
// on provenance. This is the correct implementation that writes desired
// state to city.toml (not ephemeral session metadata).
func (e *Editor) SuspendAgent(name string) error {
	return e.EditExpanded(func(raw, expanded *config.City) error {
		switch AgentOrigin(raw, expanded, name) {
		case OriginInline:
			return SetAgentSuspended(raw, name, true)
		case OriginDerived:
			return AddOrUpdateAgentPatch(raw, name, func(p *config.AgentPatch) {
				p.Suspended = boolPtr(true)
			})
		default:
			return fmt.Errorf("%w: agent %q", ErrNotFound, name)
		}
	})
}

// ResumeAgent resumes a suspended agent, using inline edit or patch
// depending on provenance.
func (e *Editor) ResumeAgent(name string) error {
	return e.EditExpanded(func(raw, expanded *config.City) error {
		switch AgentOrigin(raw, expanded, name) {
		case OriginInline:
			return SetAgentSuspended(raw, name, false)
		case OriginDerived:
			return AddOrUpdateAgentPatch(raw, name, func(p *config.AgentPatch) {
				p.Suspended = boolPtr(false)
			})
		default:
			return fmt.Errorf("%w: agent %q", ErrNotFound, name)
		}
	})
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
	Path      string
	Prefix    string
	Suspended *bool
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
type ProviderUpdate struct {
	DisplayName  *string
	Command      *string
	Args         []string // nil = not set, non-nil = replace
	PromptMode   *string
	PromptFlag   *string
	ReadyDelayMs *int
	Env          map[string]string // nil = not set, non-nil = additive merge
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
		if patch.Command != nil {
			spec.Command = *patch.Command
		}
		if patch.Args != nil {
			spec.Args = make([]string, len(patch.Args))
			copy(spec.Args, patch.Args)
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

// SetOrderOverride creates or updates an order override in
// [orders.overrides]. Matches by name and rig.
func (e *Editor) SetOrderOverride(ov config.OrderOverride) error {
	return e.Edit(func(cfg *config.City) error {
		if ov.Name == "" {
			return fmt.Errorf("order override: name is required")
		}
		for i := range cfg.Orders.Overrides {
			if cfg.Orders.Overrides[i].Name == ov.Name && cfg.Orders.Overrides[i].Rig == ov.Rig {
				cfg.Orders.Overrides[i] = ov
				return nil
			}
		}
		cfg.Orders.Overrides = append(cfg.Orders.Overrides, ov)
		return nil
	})
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

// validateProviders checks that all city-level providers have a command set.
func validateProviders(providers map[string]config.ProviderSpec) error {
	for name, spec := range providers {
		if spec.Command == "" {
			return fmt.Errorf("provider %q: command is required", name)
		}
	}
	return nil
}
