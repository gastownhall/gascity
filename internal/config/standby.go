package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
)

// StandbyFile is the record of the agents the controller has moved onto their
// standby_provider: .gc/runtime/standby.toml. The controller's claims-stall
// guard writes it when an agent's seats stop claiming their routed work on
// the agent's own provider, and rewrites it when the move ends; nothing else
// should. Every gc process composes it into the effective config
// (LoadWithIncludes), so the controller, the API and a session's own gc
// resolve the same provider for the agent, and config.Revision covers it, so
// a change to it is a config change. It is not watched: its writer requests
// the reload. Delete an entry, or the file, and run `gc reload` to put an
// agent back on its own provider at once.
//
// A move changes only the agent's provider: never its caps, sessions or
// anything else a config check could refuse, so no record can keep a city's
// config from loading.
type StandbyFile struct {
	Agents []StandbyEntry `toml:"agent,omitempty"`
}

// StandbyEntry is one agent on its standby provider.
type StandbyEntry struct {
	// Name is the agent's qualified name.
	Name string `toml:"name"`
	// Provider is the standby in effect: the agent's standby_provider.
	Provider string `toml:"provider"`
	// Replaced is the agent's own provider, which stalled.
	Replaced string `toml:"replaced"`
	// Since is when the move was recorded; Until is when the controller
	// returns the agent to Replaced to probe it. Until is the controller's
	// to act on: composition ignores it, so the config never changes with
	// the clock.
	Since time.Time `toml:"since"`
	Until time.Time `toml:"until"`
	// Engagements counts the moves in this stall; each doubles the hold.
	Engagements int `toml:"engagements"`
	// Episode names the stall (agent.claims_stalled's episode).
	Episode string `toml:"episode,omitempty"`
	// Cause is what made the move: failed seats ("seats", "probe") or the
	// city's provider-health record ("provider_health").
	Cause string `toml:"cause,omitempty"`
	// Reason is why the agent was moved, for the people reading the file.
	Reason string `toml:"reason,omitempty"`
}

// standbyWarningMarker opens every warning the standby layer raises, after
// its source, so strict mode can tell them apart (IsStandbyWarning): a stale
// or inert standby is guidance, never a reason to refuse a config. It is not
// the key's name, which an unknown-key or typo warning can carry.
const standbyWarningMarker = ": standby: "

// IsStandbyWarning reports whether warning is one the standby layer raised:
// a recorded move that no longer applies, an unreadable record, or a
// standby_provider the agent cannot use. None is fatal, in strict mode
// either.
func IsStandbyWarning(warning string) bool {
	return strings.Contains(warning, standbyWarningMarker)
}

// StandbyPath returns the standby record for a city.
func StandbyPath(cityRoot string) string {
	return citylayout.StandbyFile(cityRoot)
}

// LoadStandby reads the standby record. A missing file is an empty record;
// the raw bytes are returned for the revision hash.
func LoadStandby(fs fsys.FS, cityRoot string) (StandbyFile, []byte, error) {
	path := StandbyPath(cityRoot)
	data, err := fs.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return StandbyFile{}, nil, nil
		}
		return StandbyFile{}, nil, fmt.Errorf("reading standby record %q: %w", path, err)
	}
	var f StandbyFile
	if _, err := toml.Decode(string(data), &f); err != nil {
		return StandbyFile{}, data, fmt.Errorf("parsing standby record %q: %w", path, err)
	}
	return f, data, nil
}

// standbyFileHeader opens every record the controller writes.
const standbyFileHeader = `# Written by the gc controller's claims-stall guard. Each [[agent]] below runs
# on its standby_provider in place of its own provider (replaced) until
# "until", when the controller puts it back on its own provider to probe it.
# Delete an entry, or the file, and run "gc reload" to end a move at once.

`

// WriteStandby replaces the standby record atomically, in name order, and
// removes it when f holds no agent.
func WriteStandby(fs fsys.FS, cityRoot string, f StandbyFile) error {
	path := StandbyPath(cityRoot)
	if len(f.Agents) == 0 {
		if err := fs.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("removing standby record %q: %w", path, err)
		}
		return nil
	}
	sorted := append([]StandbyEntry(nil), f.Agents...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	var buf bytes.Buffer
	buf.WriteString(standbyFileHeader)
	if err := toml.NewEncoder(&buf).Encode(StandbyFile{Agents: sorted}); err != nil {
		return fmt.Errorf("encoding standby record: %w", err)
	}
	if err := fs.MkdirAll(citylayout.RuntimeDataDir(cityRoot), 0o755); err != nil {
		return fmt.Errorf("creating %q: %w", citylayout.RuntimeDataDir(cityRoot), err)
	}
	if err := fsys.WriteFileIfChangedAtomic(fs, path, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("writing standby record %q: %w", path, err)
	}
	return nil
}

// AgentOwnProvider is the provider the agent's config gives it, whether or
// not the standby record has moved it: its own provider, else the
// workspace's, for an agent that runs a provider at all ("" for a
// start_command agent).
func AgentOwnProvider(a *Agent, workspace *Workspace) string {
	if a == nil || strings.TrimSpace(a.StartCommand) != "" {
		return ""
	}
	if own := strings.TrimSpace(a.StandbyFor); own != "" {
		return own
	}
	if p := strings.TrimSpace(a.Provider); p != "" {
		return p
	}
	if workspace != nil {
		return strings.TrimSpace(workspace.Provider)
	}
	return ""
}

// StandbyUsable reports why an agent's standby cannot be used, or "" when it
// can: a provider other than its own, on an agent that runs a provider and
// leaves the process names to it (an agent's own process_names would name the
// own provider's process on the standby too, and its seats there would read
// dead).
func StandbyUsable(a *Agent, workspace *Workspace) string {
	switch {
	case a == nil || strings.TrimSpace(a.StandbyProvider) == "":
		return "no standby_provider"
	case strings.TrimSpace(a.StartCommand) != "":
		return "a start_command agent runs no provider"
	case strings.TrimSpace(a.StandbyProvider) == AgentOwnProvider(a, workspace):
		return "standby_provider is the agent's own provider"
	case len(a.ProcessNames) > 0:
		return "an agent that sets its own process_names cannot change provider"
	}
	return ""
}

// applyStandby moves each agent an entry names onto its standby provider,
// when the entry still matches the agent's config: its own provider is the
// one the entry replaced, and its standby_provider the entry's provider. An
// entry that no longer matches (the config changed under it) is ignored with
// a warning; the controller drops it on its next write.
func applyStandby(cfg *City, f StandbyFile, source string) []string {
	if cfg == nil || len(f.Agents) == 0 {
		return nil
	}
	index := make(map[string]int, len(cfg.Agents))
	for i := range cfg.Agents {
		index[cfg.Agents[i].QualifiedName()] = i
	}
	var warnings []string
	for _, e := range f.Agents {
		i, ok := index[e.Name]
		if !ok {
			warnings = append(warnings, fmt.Sprintf("%s"+standbyWarningMarker+"agent %q is not in the config; its recorded move is ignored", source, e.Name))
			continue
		}
		a := &cfg.Agents[i]
		own := AgentOwnProvider(a, &cfg.Workspace)
		if a.StandbyFor != "" || strings.TrimSpace(e.Provider) != strings.TrimSpace(a.StandbyProvider) || strings.TrimSpace(e.Replaced) != own {
			warnings = append(warnings, fmt.Sprintf("%s"+standbyWarningMarker+"agent %q now runs provider %q with standby_provider %q; the recorded move (%q in place of %q) is ignored",
				source, e.Name, own, a.StandbyProvider, e.Provider, e.Replaced))
			continue
		}
		if why := StandbyUsable(a, &cfg.Workspace); why != "" {
			warnings = append(warnings, fmt.Sprintf("%s"+standbyWarningMarker+"agent %q: %s; its recorded move is ignored", source, e.Name, why))
			continue
		}
		a.StandbyFor = own
		a.Provider = strings.TrimSpace(a.StandbyProvider)
	}
	return warnings
}

// applyStandbyLayer composes the city's standby record into cfg and keeps its
// bytes on prov for the revision. A record that cannot be read or parsed is
// ignored with a warning: it must never keep the city's config from loading.
func applyStandbyLayer(fs fsys.FS, cityRoot string, cfg *City, prov *Provenance) []string {
	f, data, err := LoadStandby(fs, cityRoot)
	if prov != nil {
		prov.standby = data
	}
	if err != nil {
		return []string{fmt.Sprintf("%s"+standbyWarningMarker+"%v; no agent is moved to its standby_provider", StandbyPath(cityRoot), err)}
	}
	return applyStandby(cfg, f, StandbyPath(cityRoot))
}
