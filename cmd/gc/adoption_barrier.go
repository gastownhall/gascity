package main

import (
	"fmt"
	"io"
	"regexp"
	"strconv"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
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
	store beads.Store,
	sp runtime.Provider,
	cfg *config.City,
	cityName string,
	clk clock.Clock,
	stderr io.Writer,
	dryRun bool,
) (adoptionResult, bool) {
	var result adoptionResult

	if store == nil {
		return result, false
	}

	// Step 1: List all running sessions.
	running, err := sp.ListRunning("")
	if err != nil {
		fmt.Fprintf(stderr, "adoption barrier: listing running sessions: %v\n", err) //nolint:errcheck
		return result, false
	}
	result.Total = len(running)
	if len(running) == 0 {
		return result, true // nothing to adopt
	}

	// Step 2: Load existing open session beads, indexed by session_name.
	existing, err := store.List(beads.ListQuery{
		Label: sessionBeadLabel,
		Type:  sessionBeadType,
	})
	if err != nil {
		fmt.Fprintf(stderr, "adoption barrier: listing beads: %v\n", err) //nolint:errcheck
		return result, false
	}
	bySessionName := make(map[string]bool, len(existing))
	for _, b := range existing {
		if b.Status == "closed" {
			continue // closed beads don't count for dedup
		}
		if sn := b.Metadata["session_name"]; sn != "" {
			bySessionName[sn] = true
		}
	}

	// Build config agent lookup: session_name -> agent config.
	// Also build a reverse lookup by qualified name for pool instance resolution.
	// Uses the already-loaded session beads to avoid N store queries.
	st := cfg.Workspace.SessionTemplate
	snapshot := &sessionBeadSnapshot{}
	for _, b := range existing {
		if b.Status != "closed" {
			snapshot.add(b)
		}
	}
	agentBySession := make(map[string]*config.Agent, len(cfg.Agents))
	agentByQN := make(map[string]*config.Agent, len(cfg.Agents))
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		sn := snapshot.FindSessionNameByTemplate(a.QualifiedName())
		if sn == "" {
			sn = agent.SessionNameFor(cityName, a.QualifiedName(), st)
		}
		agentBySession[sn] = a
		agentByQN[a.QualifiedName()] = a
	}

	now := clk.Now().UTC()

	// Step 3: For each running session, adopt if no open bead exists.
	for _, sessionName := range running {
		if bySessionName[sessionName] {
			result.AlreadyHadBead++
			result.Details = append(result.Details, adoptionDetail{
				SessionName: sessionName,
				HasBead:     true,
			})
			continue
		}

		// Find matching config agent.
		// First try exact session name match, then try resolving pool
		// instances by stripping the numeric suffix and matching the
		// base template name (e.g., "city-worker-3" -> "worker").
		cfgAgent, isConfigAgent := agentBySession[sessionName]
		isPoolInstance := false
		if !isConfigAgent {
			if base := resolvePoolBase(sessionName, store, cityName, st, agentByQN); base != nil {
				cfgAgent = base
				isConfigAgent = true
				isPoolInstance = true
			}
		}

		// Build bead metadata. Config/live hashes are left empty —
		// syncSessionBeads populates them from built agent objects.
		meta := map[string]string{
			"session_name":       sessionName,
			"state":              "active",
			"generation":         strconv.Itoa(sessionpkg.DefaultGeneration),
			"continuation_epoch": strconv.Itoa(sessionpkg.DefaultContinuationEpoch),
			"instance_token":     sessionpkg.NewInstanceToken(),
			"synced_at":          now.Format("2006-01-02T15:04:05Z07:00"),
		}

		detail := adoptionDetail{SessionName: sessionName}

		if isConfigAgent {
			if isPoolInstance {
				// For pool instances, reconstruct the instance name
				// (e.g., "worker-3") to match what syncSessionBeads uses.
				slot := parsePoolSlot(sessionName)
				instanceName := fmt.Sprintf("%s-%d", cfgAgent.QualifiedName(), slot)
				detail.AgentName = instanceName
				meta["agent_name"] = instanceName
			} else {
				detail.AgentName = cfgAgent.QualifiedName()
				meta["agent_name"] = cfgAgent.QualifiedName()
			}
		} else {
			detail.AgentName = sessionName
			meta["agent_name"] = sessionName
		}

		// Detect pool instances from session name suffix.
		// Only set pool_slot metadata when the agent is actually a pool agent,
		// to avoid false positives on singleton agents whose names end in numbers.
		if slot := parsePoolSlot(sessionName); slot > 0 && isConfigAgent && isMultiSessionCfgAgent(cfgAgent) {
			detail.PoolSlot = slot
			meta["pool_slot"] = strconv.Itoa(slot)
			if maxSess := cfgAgent.EffectiveMaxActiveSessions(); maxSess != nil && *maxSess >= 0 && slot > *maxSess {
				detail.OutOfBounds = true
				fmt.Fprintf(stderr, "adoption barrier: %s pool slot %d exceeds max %d (adopt-then-drain)\n", //nolint:errcheck
					sessionName, slot, *maxSess)
			}
		}

		if dryRun {
			result.Adopted++
			result.Details = append(result.Details, detail)
			continue
		}

		_, createErr := store.Create(beads.Bead{
			Title:    detail.AgentName,
			Type:     sessionBeadType,
			Labels:   []string{sessionBeadLabel, "agent:" + detail.AgentName},
			Metadata: meta,
		})
		if createErr != nil {
			fmt.Fprintf(stderr, "adoption barrier: creating bead for %s: %v\n", sessionName, createErr) //nolint:errcheck
			result.Skipped++
			continue
		}
		result.Adopted++
		result.Details = append(result.Details, detail)
	}

	// Step 4: Barrier gate — all running sessions must have beads.
	passed := result.Skipped == 0
	return result, passed
}

// resolvePoolBase attempts to match a pool instance session name back to its
// base template agent. It strips the numeric suffix (e.g., "worker-3" -> "worker")
// and checks whether the resulting base name corresponds to a configured agent.
// Returns nil if no match is found.
func resolvePoolBase(sessionName string, store beads.Store, cityName, sessionTemplate string, agentByQN map[string]*config.Agent) *config.Agent {
	slot := parsePoolSlot(sessionName)
	if slot == 0 {
		return nil
	}
	// Strip the "-N" suffix from the session name to get the base session name.
	suffix := fmt.Sprintf("-%d", slot)
	baseSessName := sessionName[:len(sessionName)-len(suffix)]
	// Check each config agent to see if its session name matches the base.
	for _, a := range agentByQN {
		if !isMultiSessionCfgAgent(a) {
			continue
		}
		sn := lookupSessionNameOrLegacy(store, cityName, a.QualifiedName(), sessionTemplate)
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
