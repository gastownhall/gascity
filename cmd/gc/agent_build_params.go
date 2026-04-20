package main

import (
	"fmt"
	"io"
	"os/exec"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/materialize"
	"github.com/gastownhall/gascity/internal/runtime"
)

// agentBuildParams holds shared, per-city parameters for building agents.
// These are constant across all agents in a single buildDesiredState call.
type agentBuildParams struct {
	city            *config.City
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
	appendFragments []string // V2: city-level [agents].append_fragments / [agent_defaults].append_fragments
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

	// skillCatalog is the shared skill catalog for this city (union of
	// city pack's skills/ and every bootstrap implicit-import pack's
	// skills/). Loaded once per build cycle and reused across every
	// agent. Nil when LoadCityCatalog returned an error — the build
	// continues without skill materialization participation in
	// fingerprints or PreStart injection. The load error is logged to
	// stderr at params-construction time.
	skillCatalog *materialize.CityCatalog
	// skillCatalogFromCache reports whether skillCatalog came from the
	// last-good cache rather than the current LoadCityCatalog result.
	skillCatalogFromCache bool
	// rigSkillCatalogs caches rig-specific shared catalogs. Each entry
	// includes city-shared skills plus any rig-import shared catalogs.
	rigSkillCatalogs map[string]*materialize.CityCatalog
	// rigSkillCatalogsFromCache reports which rig entries came from the
	// last-good cache rather than the current LoadCityCatalog result.
	rigSkillCatalogsFromCache map[string]bool
	// failedRigSkillCatalogs tracks rig scopes whose shared catalog
	// failed to load for this build. Agents in those rigs must not
	// fall back to the city catalog or they will inject stage-2 skill
	// hooks that reload the broken rig catalog and fail at runtime.
	failedRigSkillCatalogs map[string]bool

	// sessionProvider is cfg.Session.Provider (the city-level session
	// runtime selector: "" / "tmux" / "subprocess" / "acp" / "k8s" /
	// etc.). Used by the skill materialization integration to decide
	// stage-2 eligibility.
	sessionProvider string
}

// newAgentBuildParams constructs agentBuildParams from the common startup values.
func newAgentBuildParams(cityName, cityPath string, cfg *config.City, sp runtime.Provider, beaconTime time.Time, store beads.Store, stderr io.Writer) *agentBuildParams {
	params := &agentBuildParams{
		city:            cfg,
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
		appendFragments: mergeFragmentLists(cfg.AgentDefaults.AppendFragments, cfg.AgentsDefaults.AppendFragments),
		beadStore:       store,
		beadNames:       make(map[string]string),
		stderr:          stderr,
		sessionProvider: cfg.Session.Provider,
	}
	// Load the shared skill catalog once per build cycle. Transient load
	// failures (filesystem race during dolt sync / heavy I/O) used to
	// silently set skillCatalog = nil for that tick, which dropped every
	// `skills:*` entry from FingerprintExtra and flipped CoreFingerprint
	// for every live session → config-drift drain storm. Fall back to the
	// last successfully cached catalog for this exact input set so the
	// fingerprint stays stable across transient failures. A truly empty
	// catalog still propagates; bootstrap-backed empty successes get one
	// grace tick before the empty result replaces the cache so stale skill
	// sources do not stick around forever.
	cityCatalog := loadSharedSkillCatalogWithFallback(cityPath, cfg, "")
	if cityCatalog.Err != nil {
		if cityCatalog.Mode == sharedCatalogLoadCachedOnError {
			catCopy := cityCatalog.Catalog
			params.skillCatalog = &catCopy
			params.skillCatalogFromCache = true
			if stderr != nil {
				fmt.Fprintf(stderr, "buildDesiredState: LoadCityCatalog %v (using cached catalog to avoid drift)\n", cityCatalog.Err) //nolint:errcheck // best-effort stderr
			}
		} else if stderr != nil {
			fmt.Fprintf(stderr, "buildDesiredState: LoadCityCatalog %v (no cached catalog; skills will not contribute to fingerprints this tick)\n", cityCatalog.Err) //nolint:errcheck // best-effort stderr
		}
	} else {
		catCopy := cityCatalog.Catalog
		params.skillCatalog = &catCopy
		if cityCatalog.Mode == sharedCatalogLoadCachedOnEmptyGrace {
			params.skillCatalogFromCache = true
			if stderr != nil {
				fmt.Fprintf(stderr, "buildDesiredState: LoadCityCatalog returned empty while bootstrap skills were unavailable (using cached catalog to avoid drift)\n") //nolint:errcheck // best-effort stderr
			}
		}
	}
	for rigName := range cfg.RigPackSkills {
		rigCatalog := loadSharedSkillCatalogWithFallback(cityPath, cfg, rigName)
		if rigCatalog.Err != nil {
			if rigCatalog.Mode == sharedCatalogLoadCachedOnError {
				if params.rigSkillCatalogs == nil {
					params.rigSkillCatalogs = make(map[string]*materialize.CityCatalog)
				}
				if params.rigSkillCatalogsFromCache == nil {
					params.rigSkillCatalogsFromCache = make(map[string]bool)
				}
				catCopy := rigCatalog.Catalog
				params.rigSkillCatalogs[rigName] = &catCopy
				params.rigSkillCatalogsFromCache[rigName] = true
				if stderr != nil {
					fmt.Fprintf(stderr, "buildDesiredState: LoadCityCatalog rig %q %v (using cached catalog to avoid drift)\n", rigName, rigCatalog.Err) //nolint:errcheck // best-effort stderr
				}
				continue
			}
			if params.failedRigSkillCatalogs == nil {
				params.failedRigSkillCatalogs = make(map[string]bool)
			}
			params.failedRigSkillCatalogs[rigName] = true
			if stderr != nil {
				fmt.Fprintf(stderr, "buildDesiredState: LoadCityCatalog rig %q %v (no cached catalog; skills will not contribute to fingerprints this tick)\n", rigName, rigCatalog.Err) //nolint:errcheck // best-effort stderr
			}
			continue
		}
		if params.rigSkillCatalogs == nil {
			params.rigSkillCatalogs = make(map[string]*materialize.CityCatalog)
		}
		catCopy := rigCatalog.Catalog
		params.rigSkillCatalogs[rigName] = &catCopy
		if rigCatalog.Mode == sharedCatalogLoadCachedOnEmptyGrace {
			if params.rigSkillCatalogsFromCache == nil {
				params.rigSkillCatalogsFromCache = make(map[string]bool)
			}
			params.rigSkillCatalogsFromCache[rigName] = true
			if stderr != nil {
				fmt.Fprintf(stderr, "buildDesiredState: LoadCityCatalog rig %q returned empty while bootstrap skills were unavailable (using cached catalog to avoid drift)\n", rigName) //nolint:errcheck // best-effort stderr
			}
		}
	}
	return params
}

func (p *agentBuildParams) sharedSkillCatalogForAgent(agent *config.Agent) *materialize.CityCatalog {
	return p.sharedSkillCatalogSnapshotForAgent(agent)
}

func (p *agentBuildParams) sharedSkillCatalogSnapshotForAgent(agent *config.Agent) *materialize.CityCatalog {
	if p == nil || agent == nil {
		return nil
	}
	rigName := agentRigScopeName(agent, p.rigs)
	if rigName != "" && p.failedRigSkillCatalogs != nil && p.failedRigSkillCatalogs[rigName] {
		return nil
	}
	if p.rigSkillCatalogs != nil && rigName != "" {
		if cat := p.rigSkillCatalogs[rigName]; cat != nil {
			return cat
		}
	}
	return p.skillCatalog
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
