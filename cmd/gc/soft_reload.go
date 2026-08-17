package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

type softReloadAcceptanceResult struct {
	Updated        int
	Failed         int
	FailedSessions []string
	CanceledDrains int
	OpenSessions   int
	DesiredEmpty   bool
}

func (r softReloadAcceptanceResult) warnings() []string {
	var warnings []string
	if r.Failed > 0 {
		detail := formatSoftReloadFailedSessions(r.FailedSessions)
		if detail != "" {
			detail = " (" + detail + ")"
		}
		warnings = append(warnings, fmt.Sprintf("soft reload: failed to accept config drift on %d session(s)%s; affected sessions may still drain", r.Failed, detail))
	}
	if r.DesiredEmpty && r.OpenSessions > 0 {
		warnings = append(warnings, fmt.Sprintf("soft reload: desired state is empty; %d open session(s) will not have config drift accepted", r.OpenSessions))
	}
	return warnings
}

func formatSoftReloadFailedSessions(names []string) string {
	if len(names) == 0 {
		return ""
	}
	const limit = 5
	shown := names
	if len(shown) > limit {
		shown = shown[:limit]
	}
	detail := strings.Join(shown, ", ")
	if extra := len(names) - len(shown); extra > 0 {
		detail = fmt.Sprintf("%s, and %d more", detail, extra)
	}
	return detail
}

// acceptConfigDriftAcrossSessions writes the current per-session config
// hash into every open session bead's started_config_hash metadata
// whose desired-state entry produces a different hash than the one
// recorded on the bead. After the function returns, the reconciler's
// drift-detection check (storedHash != currentHash) sees no drift for
// any updated session, so the immediately-following reconcile tick
// proceeds without firing config-drift drains for those sessions.
//
// Used by `gc reload --soft` so an operator editing a running city's
// .gc/settings.json doesn't drain every drifted session — the new
// config is accepted in-place instead. The caller is expected to have
// just rebuilt desired state from the freshly reloaded config.
//
// Sessions are skipped (no metadata write) when:
//   - the session is closed
//   - the session has no session_name metadata
//   - the session has no started_config_hash yet (the existing drift
//     check already skips these — the next first-start path will
//     stamp the right value)
//   - the session's name has no entry in desired (orphaned by the
//     config edit; normal orphan/suspended drain handles them on the
//     next tick)
//   - the recomputed current hash already equals the stored hash
//
// The hash computation uses sessionCoreConfigForHash, the same canonical
// reconciler drift-hash helper used by live and asleep drift detection.
//
// Returns accepted-session, failed-session, stale-drain-cancellation, and
// empty-desired-state diagnostics for the controller reply.
func acceptConfigDriftAcrossSessions(
	sessFront *session.Store,
	desired map[string]TemplateParams,
	sessionBeads *sessionBeadSnapshot,
	sp runtime.Provider,
	dt *drainTracker,
	stderr io.Writer,
) softReloadAcceptanceResult {
	result := softReloadAcceptanceResult{DesiredEmpty: len(desired) == 0}
	if !sessFront.Backed() {
		return result
	}
	if sessionBeads == nil {
		var err error
		sessionBeads, err = loadSessionBeadSnapshot(sessFront.Store().Store)
		if err != nil {
			fmt.Fprintf(stderr, "soft reload: listing session beads: %v\n", err) //nolint:errcheck // best-effort stderr
			return result
		}
	}
	openInfos := sessionBeads.OpenInfos()
	result.OpenSessions = len(openInfos)
	if len(openInfos) == 0 {
		return result
	}
	if len(desired) == 0 {
		return result
	}

	for i := range openInfos {
		info := openInfos[i]
		if info.Closed {
			continue
		}
		name := strings.TrimSpace(info.SessionNameMetadata)
		if name == "" {
			continue
		}
		storedHash := strings.TrimSpace(info.StartedConfigHash)
		if storedHash == "" {
			continue
		}
		tp, ok := desired[name]
		if !ok {
			continue
		}
		agentCfg := sessionCoreConfigForHashInfo(tp, info)
		currentHash := runtime.CoreFingerprint(agentCfg)
		if storedHash == currentHash {
			continue
		}
		if err := clearSoftReloadConfigDriftDrainAckInfo(info, sp, dt); err != nil {
			result.Failed++
			result.FailedSessions = append(result.FailedSessions, name)
			fmt.Fprintf(stderr, "soft reload: clearing config-drift drain ack metadata for %s: %v; leaving config hash unchanged\n", name, err) //nolint:errcheck // best-effort stderr
			continue
		}
		metadata, err := softReloadAcceptedHashMetadata(agentCfg, currentHash)
		if err != nil {
			result.Failed++
			result.FailedSessions = append(result.FailedSessions, name)
			fmt.Fprintf(stderr, "soft reload: preparing config hash metadata for %s: %v\n", name, err) //nolint:errcheck // best-effort stderr
			continue
		}
		if err := sessFront.ApplyPatch(info.ID, metadata); err != nil {
			result.Failed++
			result.FailedSessions = append(result.FailedSessions, name)
			fmt.Fprintf(stderr, "soft reload: updating config hash metadata for %s: %v\n", name, err) //nolint:errcheck // best-effort stderr
			continue
		}
		if cancelSoftReloadConfigDriftDrainInfo(info, sp, dt) {
			result.CanceledDrains++
		}
		result.Updated++
	}
	return result
}

func softReloadAcceptedHashMetadata(agentCfg runtime.Config, currentHash string) (map[string]string, error) {
	breakdown, err := json.Marshal(runtime.CoreFingerprintBreakdown(agentCfg))
	if err != nil {
		return nil, err
	}
	// Stamp the partition sub-hashes (B2) alongside started_config_hash: every
	// writer of started_config_hash must keep started_provision_hash/
	// started_launch_hash consistent with it, or a later launch-only drift check
	// would compare against a stale baseline.
	return map[string]string{
		"started_config_hash":    currentHash,
		"started_provision_hash": runtime.ProvisionFingerprint(agentCfg),
		"started_launch_hash":    runtime.LaunchFingerprint(agentCfg),
		"core_hash_breakdown":    string(breakdown),
	}, nil
}

func cancelSoftReloadConfigDriftDrainInfo(info session.Info, sp runtime.Provider, dt *drainTracker) bool {
	if dt == nil {
		return false
	}
	ds := dt.get(info.ID)
	if ds == nil || ds.reason != "config-drift" {
		return false
	}
	return cancelSessionConfigDriftDrainInfo(info, sp, dt)
}

func clearSoftReloadConfigDriftDrainAckInfo(info session.Info, sp runtime.Provider, dt *drainTracker) error {
	if dt == nil {
		return nil
	}
	ds := dt.get(info.ID)
	if ds == nil || ds.reason != "config-drift" || !ds.ackSet {
		return nil
	}
	name := strings.TrimSpace(info.SessionNameMetadata)
	if err := clearReconcilerDrainAckMetadata(sp, name); err != nil {
		return err
	}
	ds.ackSet = false
	return nil
}
