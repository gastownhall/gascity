package session

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// ReleaseStaleConfiguredNameClaims clears the reserved runtime session_name held
// by CLOSED session beads whose name is a configured named-session runtime name,
// returning the number of claims released. It is intended to run once at
// controller/supervisor startup so a freshly started process does not inherit
// pre-fix stuck name claims (ga-n2d Gap C).
//
// ga-841 and Gap A (configuredNamedIdentitySignalsMatch) already release such a
// closed bead's name lazily, when the configured identity reclaims it. This
// sweep additionally clears the claim eagerly at startup so the name index is
// clean for every path — including ownerless availability checks and
// `gc session list` — rather than carrying legacy stale claims forward. It
// specifically covers legacy entries created before this binary, whose identity
// is recorded only via an alias / agent_name / template (role) label.
//
// Only CLOSED beads are swept. A live or asleep bead may still legitimately own
// or resume its reserved name, so its claim is left untouched. A closed bead's
// claim is released only when its reserved session_name matches a configured
// named-session runtime name AND the bead is recognized as that configured named
// session — by the boolean flag, the recorded identity, or a legacy
// alias/agent_name/template signal that resolves to the configured identity.
func ReleaseStaleConfiguredNameClaims(store beads.Store, cfg *config.City, cityName string) (int, error) {
	if store == nil || cfg == nil {
		return 0, nil
	}
	cityName = strings.TrimSpace(cityName)
	if cityName == "" {
		cityName = cfg.EffectiveCityName()
	}

	// Map every configured runtime session name to its identity so a bead's
	// reserved session_name can be matched without per-bead config scans.
	runtimeToIdentity := make(map[string]string, len(cfg.NamedSessions))
	for i := range cfg.NamedSessions {
		identity := strings.TrimSpace(cfg.NamedSessions[i].QualifiedName())
		if identity == "" {
			continue
		}
		runtimeName := strings.TrimSpace(config.NamedSessionRuntimeName(cityName, cfg.Workspace, identity))
		if runtimeName == "" {
			continue
		}
		runtimeToIdentity[runtimeName] = identity
	}
	if len(runtimeToIdentity) == 0 {
		return 0, nil
	}

	items, err := store.List(beads.ListQuery{Label: LabelSession, IncludeClosed: true})
	if err != nil {
		return 0, fmt.Errorf("listing session beads for stale name-claim sweep: %w", err)
	}

	released := 0
	for _, b := range items {
		if b.Status != "closed" || !IsSessionBeadOrRepairable(b) {
			continue
		}
		name := strings.TrimSpace(b.Metadata["session_name"])
		if name == "" {
			continue
		}
		identity, ok := runtimeToIdentity[name]
		if !ok {
			continue
		}
		// Confirm the closed bead belongs to the configured named session that
		// OWNS this runtime name before releasing the claim, so an unrelated bead
		// that merely persisted the name is left alone (sjarmak review, #3373).
		//
		// Ownership must be scoped to identity — the configured identity whose
		// runtime name this bead reserves. wasConfiguredNamedSession(b) is owner-
		// AGNOSTIC (it only asks "is this bead SOME configured named session?"),
		// so a bead recorded for a DIFFERENT configured identity B that merely
		// persisted identity A's runtime session_name would wrongly clear A's
		// claim. Gate solely on the owner-scoped recognizer:
		// configuredNamedIdentitySignalsMatch already matches the recorded
		// identity, alias, agent_name, or template/role signal against THIS
		// identity, covering every legacy/pre-ga841 phantom shape this sweep
		// targets. The flag-only path is intentionally dropped — the boolean flag
		// alone cannot attribute the bead to a specific identity, and a
		// recorded-identity match is already one of the signals checked here.
		if !configuredNamedIdentitySignalsMatch(b, identity) {
			continue
		}
		update := beads.UpdateOpts{Metadata: map[string]string{"session_name": ""}}
		if err := store.Update(b.ID, update); err != nil {
			return released, fmt.Errorf("releasing stale name claim on %s: %w", b.ID, err)
		}
		released++
	}
	return released, nil
}
