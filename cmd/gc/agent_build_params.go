package main

import (
	"io"
	"os/exec"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
)

// agentBuildParams holds shared, per-city parameters for building agents.
// These are constant across all agents in a single buildDesiredState call.
type agentBuildParams struct {
	cityName        string
	cityPath        string
	workspace       *config.Workspace
	agents          []config.Agent
	providers       map[string]config.ProviderSpec
	lookPath        config.LookPathFunc
	fs              fsys.FS
	sp              runtime.Provider
	rigs            []config.Rig
	sessionTemplate string
	beaconTime      time.Time
	packDirs        []string
	packOverlayDirs []string
	rigOverlayDirs  map[string][]string
	globalFragments []string
	stderr          io.Writer

	// beadStore is the city-level bead store for session bead lookups.
	// When non-nil, session names are derived from bead IDs ("s-{beadID}")
	// instead of the legacy SessionNameFor function.
	beadStore beads.Store

	// sessionBeads caches the open session-bead snapshot for the current
	// desired-state build so per-agent resolution does not rescan the store.
	sessionBeads *sessionBeadSnapshot

	// beadNames caches qualifiedName → session_name mappings resolved
	// during this build cycle. Populated lazily by resolveSessionName.
	beadNames map[string]string
}

// newAgentBuildParams constructs agentBuildParams from the common startup values.
func newAgentBuildParams(cityName, cityPath string, cfg *config.City, sp runtime.Provider, beaconTime time.Time, store beads.Store, stderr io.Writer) *agentBuildParams {
	return &agentBuildParams{
		cityName:        cityName,
		cityPath:        cityPath,
		workspace:       &cfg.Workspace,
		agents:          append([]config.Agent(nil), cfg.Agents...),
		providers:       cfg.Providers,
		lookPath:        exec.LookPath,
		fs:              fsys.OSFS{},
		sp:              sp,
		rigs:            cfg.Rigs,
		sessionTemplate: cfg.Workspace.SessionTemplate,
		beaconTime:      beaconTime,
		packDirs:        cfg.PackDirs,
		packOverlayDirs: cfg.PackOverlayDirs,
		rigOverlayDirs:  cfg.RigOverlayDirs,
		globalFragments: cfg.Workspace.GlobalFragments,
		beadStore:       store,
		beadNames:       make(map[string]string),
		stderr:          stderr,
	}
}

// effectiveOverlayDirs merges city-level and rig-level pack overlay dirs.
// City dirs come first (lower priority), then rig-specific dirs.
func effectiveOverlayDirs(cityDirs []string, rigDirs map[string][]string, rigName string) []string {
	rigSpecific := rigDirs[rigName]
	if len(rigSpecific) == 0 {
		return cityDirs
	}
	if len(cityDirs) == 0 {
		return rigSpecific
	}
	merged := make([]string, 0, len(cityDirs)+len(rigSpecific))
	merged = append(merged, cityDirs...)
	merged = append(merged, rigSpecific...)
	return merged
}

// templateNameFor returns the configuration template name for an agent.
// For pool instances, this is the original template name (PoolName).
// For regular agents, it's the qualified name.
func templateNameFor(cfgAgent *config.Agent, qualifiedName string) string {
	if cfgAgent.PoolName != "" {
		return cfgAgent.PoolName
	}
	return qualifiedName
}
