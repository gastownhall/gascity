package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// rebaseSessionConfigHashes updates the stored config hash baseline for
// all active sessions to match the current resolved config. Called after
// a manual gc reload so that the reconciler does not treat the freshly
// resolved config as drift requiring session restarts.
//
// This implements the "gc reload = accept new baseline" semantics: the
// user is telling the system "this is the new normal — stop trying to
// converge on the old config."
//
// Returns the number of sessions whose hashes were updated.
func rebaseSessionConfigHashes(
	store beads.Store,
	cfg *config.City,
	desiredState map[string]TemplateParams,
	sessionBeads *sessionBeadSnapshot,
	stdout, stderr io.Writer,
) int {
	if store == nil || sessionBeads == nil {
		return 0
	}

	updated := 0
	for _, session := range sessionBeads.Open() {
		name := strings.TrimSpace(session.Metadata["session_name"])
		storedHash := session.Metadata["started_config_hash"]
		if name == "" || storedHash == "" {
			continue // startup window (no hash yet) or unnamed
		}

		tp, ok := desiredState[name]
		if !ok {
			continue // not in desired state (orphaned/suspended)
		}

		template := tp.TemplateName
		if template == "" {
			template = normalizedSessionTemplate(session, cfg)
		}
		if template == "" {
			continue
		}

		if findAgentByTemplate(cfg, template) == nil {
			continue
		}

		// Compute the effective config the same way the reconciler does
		// in session_reconciler.go (config-drift section), including
		// template_overrides application.
		agentCfg := templateParamsToConfig(tp)
		if rawOvr := session.Metadata["template_overrides"]; rawOvr != "" {
			if tp.ResolvedProvider != nil && len(tp.ResolvedProvider.OptionsSchema) > 0 {
				var ovr map[string]string
				if err := json.Unmarshal([]byte(rawOvr), &ovr); err == nil && len(ovr) > 0 {
					fullOptions := make(map[string]string)
					for k, v := range tp.ResolvedProvider.EffectiveDefaults {
						fullOptions[k] = v
					}
					for k, v := range ovr {
						if k == "initial_message" {
							continue
						}
						fullOptions[k] = v
					}
					if extra, rErr := config.ResolveExplicitOptions(tp.ResolvedProvider.OptionsSchema, fullOptions); rErr == nil && len(extra) > 0 {
						agentCfg.Command = replaceSchemaFlags(agentCfg.Command, tp.ResolvedProvider.OptionsSchema, extra)
					}
				}
			}
		}

		newCoreHash := runtime.CoreFingerprint(agentCfg)
		newLiveHash := runtime.LiveFingerprint(agentCfg)

		updates := make(map[string]string)
		if storedHash != newCoreHash {
			updates["started_config_hash"] = newCoreHash
			breakdown := runtime.CoreFingerprintBreakdown(agentCfg)
			breakdownJSON, _ := json.Marshal(breakdown)
			updates["core_hash_breakdown"] = string(breakdownJSON)
		}

		storedLive := session.Metadata["started_live_hash"]
		if storedLive != "" && storedLive != newLiveHash {
			updates["started_live_hash"] = newLiveHash
		}

		if len(updates) == 0 {
			continue
		}

		if err := store.SetMetadataBatch(session.ID, updates); err != nil {
			fmt.Fprintf(stderr, "rebase config hash %s: %v\n", name, err) //nolint:errcheck // best-effort stderr
			continue
		}
		fmt.Fprintf(stdout, "Rebased config hash for '%s'\n", name) //nolint:errcheck // best-effort stdout
		updated++
	}

	return updated
}
