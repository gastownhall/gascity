package main

import (
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// adoptionResult holds the outcome of an adoption barrier run.
type adoptionResult struct {
	Adopted        int
	AlreadyHadBead int
	Skipped        int // sessions that failed bead creation
	Total          int // total running sessions
	// Details records per-session info for dry-run display.
	Details []adoptionDetail
}

// adoptionDetail describes what would happen for a single session.
type adoptionDetail struct {
	SessionName string
	AgentName   string
	PoolSlot    int  // 0 if not a pool instance
	OutOfBounds bool // pool slot exceeds max
	HasBead     bool // already has an open bead
}

// poolSlotPattern extracts the numeric suffix from pool instance session names.
// e.g., "s-worker-3" -> "3"
var poolSlotPattern = regexp.MustCompile(`-(\d+)$`)

// runAdoptionBarrier ensures every running session has a corresponding open
// session bead. This is rerunnable and crash-safe: if the controller crashes
// mid-adoption, the next startup re-runs it. The per-instance dedup key
// (session_name) prevents duplicate beads.
//
// Config hashes are NOT set by the adoption barrier — the subsequent
// syncSessionBeads call populates them from the built agent objects.
//
// When dryRun is true, no beads are created — the function only reports
// what would happen. This powers the `gc migration plan` command.
//
// Returns the adoption result and whether the barrier passed (all running
// sessions have beads).
func runAdoptionBarrier(
	cityPath string,
	sessFront *sessionpkg.Store,
	sp runtime.Provider,
	cfg *config.City,
	cityName string,
	clk clock.Clock,
	stderr io.Writer,
	dryRun bool,
) (adoptionResult, bool) {
	var result adoptionResult

	if sessFront == nil {
		return result, false
	}
	// Session-bead list queries below go through the typed session front door
	// (sessFront.ListAll); creates go through the front door too. Same underlying
	// store, so behavior is unchanged.

	// Step 1: List all running sessions.
	running, err := sp.ListRunning("")
	partialList := runtime.IsPartialListError(err)
	if err != nil && !partialList {
		fmt.Fprintf(stderr, "adoption barrier: listing running sessions: %v\n", err) //nolint:errcheck
		return result, false
	}
	if partialList {
		fmt.Fprintf(stderr, "adoption barrier: listing running sessions partially failed: %v\n", err) //nolint:errcheck
	}
	result.Total = len(running)
	if len(running) == 0 {
		return result, !partialList // nothing visible to adopt
	}

	// Step 2: Load existing open session beads, indexed by session_name.
	// The helper unions Type and Label queries so canonical beads that
	// lost their gc:session label (after a crash or partial write) still
	// participate in adoption dedup. Without the union, those beads would
	// be invisible here and adoption would re-create duplicates.
	existing, err := sessFront.ListAll(sessionpkg.ListAllOptions{})
	if err != nil {
		fmt.Fprintf(stderr, "adoption barrier: listing beads: %v\n", err) //nolint:errcheck
		return result, false
	}
	bySessionName := make(map[string]bool, len(existing))
	for _, info := range existing {
		// ListAll already filters via IsSessionBeadOrRepairable and excludes closed.
		if info.Closed {
			continue // closed beads don't count for dedup
		}
		if sn := info.SessionNameMetadata; sn != "" {
			bySessionName[sn] = true
		}
	}

	// Build config agent lookup: session_name -> agent config.
	// Also build a reverse lookup by qualified name for pool instance resolution.
	// Uses the already-loaded session Infos to avoid N store queries.
	st := cfg.Workspace.SessionTemplate
	snapshot := newSessionBeadSnapshotFromInfos(existing)
	agentBySession := make(map[string]*config.Agent, len(cfg.Agents))
	agentByQN := make(map[string]*config.Agent, len(cfg.Agents))
	agentBaseSessionName := make(map[string]string, len(cfg.Agents))
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		sn := snapshot.FindSessionNameByTemplate(a.QualifiedName())
		if sn == "" {
			sn = agent.SessionNameFor(cityName, a.QualifiedName(), st)
		}
		agentBySession[sn] = a
		agentByQN[a.QualifiedName()] = a
		agentBaseSessionName[a.QualifiedName()] = sn
	}

	// Step 3: For each running session, adopt if no open bead exists.
	for _, sessionName := range running {
		// Find matching config agent.
		// First try exact session name match, then try resolving pool
		// instances by stripping the numeric suffix and matching the
		// base template name (e.g., "city-worker-3" -> "worker").
		cfgAgent, isConfigAgent := agentBySession[sessionName]
		isPoolInstance := false
		staleSingletonSuffix := false
		if !isConfigAgent {
			if base := resolveCanonicalSingletonSuffixBase(sessionName, agentBaseSessionName, agentByQN); base != nil {
				cfgAgent = base
				isConfigAgent = true
				staleSingletonSuffix = true
			} else if base := resolvePoolBase(sessionName, agentBaseSessionName, agentByQN); base != nil {
				cfgAgent = base
				isConfigAgent = true
				isPoolInstance = true
			}
		}
		processNames := processHints(cfg, cfgAgent)
		alive, err := workerSessionTargetAliveWithConfig(nil, sp, nil, sessionName, processNames)
		if err != nil || !alive {
			result.Total--
			continue
		}
		if bySessionName[sessionName] {
			result.AlreadyHadBead++
			result.Details = append(result.Details, adoptionDetail{
				SessionName: sessionName,
				HasBead:     true,
			})
			continue
		}

		detail := adoptionDetail{SessionName: sessionName}

		// Resolve the canonical agent_name and pool slot BEFORE deriving identity
		// metadata, so desiredSessionIdentity emits agent_name/pool_slot (and, for
		// config-resolved agents, the durable canonical record) instead of the
		// former hand-stamps. resolvedAgentName / resolvedSlot hold exactly the
		// values the old hand-stamps used; the orphan arm resolves to
		// agent_name=sessionName but is NOT config-resolved, so it mints no
		// canonical record (S19 S2-3).
		var (
			resolvedAgentName string
			resolvedSlot      int
		)

		if isConfigAgent {
			if isPoolInstance {
				// For pool instances, reconstruct the instance name
				// (e.g., "worker-3") to match what syncSessionBeads uses.
				slot := parsePoolSlot(sessionName)
				instanceName := fmt.Sprintf("%s-%d", cfgAgent.QualifiedName(), slot)
				detail.AgentName = instanceName
				resolvedAgentName = instanceName
			} else {
				detail.AgentName = cfgAgent.QualifiedName()
				resolvedAgentName = cfgAgent.QualifiedName()
			}
		} else {
			detail.AgentName = sessionName
			resolvedAgentName = sessionName
		}

		// Detect pool instances from session name suffix.
		// Only set pool_slot metadata when the agent actually supports
		// instance expansion, to avoid false positives on direct session
		// names that end in numbers.
		slot := parsePoolSlot(sessionName)
		switch {
		case slot > 0 && staleSingletonSuffix:
			fmt.Fprintf(stderr, "adoption barrier: adopting stale singleton suffix session %s as canonical agent %s without pool_slot metadata\n", //nolint:errcheck
				sessionName, cfgAgent.QualifiedName())
		case slot > 0 && isConfigAgent && cfgAgent.SupportsInstanceExpansion():
			detail.PoolSlot = slot
			resolvedSlot = slot
			if maxSess := cfgAgent.EffectiveMaxActiveSessions(); maxSess != nil && *maxSess >= 0 && slot > *maxSess {
				detail.OutOfBounds = true
				fmt.Fprintf(stderr, "adoption barrier: %s pool slot %d exceeds max %d (adopt-then-drain)\n", //nolint:errcheck
					sessionName, slot, *maxSess)
			}
		case slot > 0 && !isConfigAgent:
			// Defensive log (ga-fiw): a session ending in "-N" did not match
			// any configured agent — either by exact session name or by pool
			// base resolution. This is the orphan shape that produced the
			// "cashmaster/gastown.refinery-1" phantom: the canonical refinery
			// agent has max_active_sessions=1, so resolvePoolBase rejected the
			// "-1" suffix, and adoption fell through to creating a bead with
			// agent_name=session_name. The log makes that leak visible.
			fmt.Fprintf(stderr, "adoption barrier: %s ends in -%d but no configured agent (after pool-base resolution) claims it; adopting under sessionName=agent_name (orphan?)\n", //nolint:errcheck
				sessionName, slot)
		}

		// Build bead metadata. Config/live hashes are left empty —
		// syncSessionBeads populates them from built agent objects.
		meta := desiredSessionIdentity(sessionIdentityInputs{
			AgentName:         resolvedAgentName,
			SessionName:       sessionName,
			State:             "active",
			Generation:        sessionpkg.DefaultGeneration,
			ContinuationEpoch: sessionpkg.DefaultContinuationEpoch,
			InstanceToken:     sessionpkg.NewInstanceToken(),
			PoolSlot:          resolvedSlot,
			ConfigResolved:    isConfigAgent,
		})

		if dryRun {
			result.Adopted++
			result.Details = append(result.Details, detail)
			continue
		}

		alreadyHadBead := false
		createSessionBead := func() error {
			meta["synced_at"] = clk.Now().UTC().Format("2006-01-02T15:04:05Z07:00")
			beadID, err := sessFront.CreateSession(sessionpkg.CreateSpec{
				Title:     detail.AgentName,
				AgentName: detail.AgentName,
				Metadata:  meta,
			})
			if err != nil {
				return fmt.Errorf("creating session bead for %q: %w", sessionName, err)
			}
			// S19 Stage 3 shadow: record the legacy canonical-identity stamp built
			// by desiredSessionIdentity for this adopted bead (no-op unless the
			// shadow harness is enabled).
			recordLegacyCompareWrites(beadID, "adoptionBarrier.create", meta)
			return nil
		}
		createErr := sessionpkg.WithCitySessionIdentifierLocks(cityPath, []string{sessionName, detail.AgentName}, func() error {
			hasBead, err := openSessionBeadExists(sessFront, sessionName)
			if err != nil {
				return err
			}
			if hasBead {
				alreadyHadBead = true
				return nil
			}
			return createSessionBead()
		})
		if alreadyHadBead {
			result.AlreadyHadBead++
			detail.HasBead = true
			result.Details = append(result.Details, detail)
			continue
		}
		if createErr != nil {
			fmt.Fprintf(stderr, "adoption barrier: %v\n", createErr) //nolint:errcheck
			result.Skipped++
			continue
		}
		result.Adopted++
		result.Details = append(result.Details, detail)
	}

	// Step 4: Barrier gate — all running sessions must have beads.
	passed := result.Skipped == 0 && !partialList
	return result, passed
}

func openSessionBeadExists(sessFront *sessionpkg.Store, sessionName string) (bool, error) {
	// HasOpenSessionNamed is the Live-tier existence probe: a session_name-filtered,
	// CachingStore-bypassing union scan so the adoption barrier observes just-created
	// beads immediately. It is byte-equivalent to the prior inline Live ListAll +
	// closed filter this wrapped. The Live bypass is pinned by
	// TestHasOpenSessionNamed in internal/session.
	return sessFront.HasOpenSessionNamed(sessionName)
}

// resolvePoolBase attempts to match a pool instance session name back to its
// base template agent. It strips the numeric suffix (e.g., "worker-3" -> "worker")
// and checks whether the resulting base name corresponds to a configured agent.
// Returns nil if no match is found.
func resolvePoolBase(sessionName string, agentBaseSessionName map[string]string, agentByQN map[string]*config.Agent) *config.Agent {
	slot := parsePoolSlot(sessionName)
	if slot == 0 {
		return nil
	}
	// Strip the "-N" suffix from the session name to get the base session name.
	suffix := fmt.Sprintf("-%d", slot)
	baseSessName := sessionName[:len(sessionName)-len(suffix)]
	// Check each config agent to see if its session name matches the base.
	for _, a := range agentByQN {
		if !a.SupportsInstanceExpansion() {
			continue
		}
		sn := strings.TrimSpace(agentBaseSessionName[a.QualifiedName()])
		if sn == baseSessName {
			return a
		}
	}
	return nil
}

func resolveCanonicalSingletonSuffixBase(sessionName string, agentBaseSessionName map[string]string, agentByQN map[string]*config.Agent) *config.Agent {
	slot := parsePoolSlot(sessionName)
	if slot == 0 {
		return nil
	}
	suffix := fmt.Sprintf("-%d", slot)
	baseSessName := sessionName[:len(sessionName)-len(suffix)]
	for _, a := range agentByQN {
		if !a.UsesCanonicalSingletonPoolIdentity() {
			continue
		}
		sn := strings.TrimSpace(agentBaseSessionName[a.QualifiedName()])
		if sn == baseSessName {
			return a
		}
	}
	return nil
}

// parsePoolSlot extracts the numeric pool slot from a session name suffix.
// Returns 0 if no slot suffix is found.
func parsePoolSlot(sessionName string) int {
	matches := poolSlotPattern.FindStringSubmatch(sessionName)
	if len(matches) < 2 {
		return 0
	}
	slot, err := strconv.Atoi(matches[1])
	if err != nil {
		return 0
	}
	return slot
}

func processHints(cfg *config.City, a *config.Agent) []string {
	if a == nil {
		return nil
	}
	return config.AgentProcessNames(cfg, *a, exec.LookPath)
}
