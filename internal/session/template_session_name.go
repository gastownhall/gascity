package session

import (
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// FindSessionNameByTemplate returns the session_name of the open session row
// that answers for template, a configured agent's qualified name, in store, or
// "" when no row does or store is the zero handle. Routing reads it to name an
// agent's session by the name its row records, which may be a custom name
// rather than the computed default.
//
// It asks four targeted lookups in turn, and the first that answers wins: the
// rows whose agent_name is template, the rows carrying the agent:<template>
// label, the rows whose template is template, and the rows whose common_name is
// template. It never lists every session row. Within a lookup, closed rows,
// rows with no session_name and pool capacity (a pool_managed, pool slot,
// ephemeral or dependency-only row, or one named like a pool member of its
// template) are passed over: a template query must not answer with one of the
// pool's instances. The two agent lookups keep pool capacity whose agent name
// is not its template (a legacy pool instance answering for its own name) and
// template's canonical singleton pool seat. A configured named session answers
// at once; otherwise the lookup's first remaining row answers, and the
// canonical seat only when no other row does. A lookup whose listing fails
// answers nothing, so the next lookup is asked. The lookup writes nothing: a
// repairable row (empty type, gc:session label) answers unrepaired.
//
// cmd/gc's findSessionNameByTemplate applies the same rule to the controller's
// stores; TestSessionNameLookupAgreesWithTheSessionPackage holds the two to the
// same answers.
func FindSessionNameByTemplate(store beads.SessionStore, template string) string {
	template = strings.TrimSpace(template)
	if store == (beads.SessionStore{}) || template == "" {
		return ""
	}
	if sn := templateSessionNameByMetadata(store, "agent_name", template, true); sn != "" {
		return sn
	}
	if sn := templateSessionNameByAgentLabel(store, template); sn != "" {
		return sn
	}
	if sn := templateSessionNameByMetadata(store, "template", template, false); sn != "" {
		return sn
	}
	return templateSessionNameByMetadata(store, "common_name", template, false)
}

// templateSessionNameByMetadata is FindSessionNameByTemplate's lookup of the
// open rows whose key metadata is template.
func templateSessionNameByMetadata(store beads.SessionStore, key, template string, agentLookup bool) string {
	rows, err := store.List(beads.ListQuery{Metadata: map[string]string{key: template}})
	if err != nil {
		return ""
	}
	return chooseTemplateSessionName(rows, agentLookup, key, template)
}

// templateSessionNameByAgentLabel is FindSessionNameByTemplate's lookup of the
// open rows carrying the agent:<template> label.
func templateSessionNameByAgentLabel(store beads.SessionStore, template string) string {
	rows, err := store.List(beads.ListQuery{Label: "agent:" + template})
	if err != nil {
		return ""
	}
	return chooseTemplateSessionName(rows, true, "", template)
}

// chooseTemplateSessionName picks one lookup's answer among rows, as
// FindSessionNameByTemplate describes. A non-empty key is the metadata the
// lookup selected on, rechecked trimmed against template.
func chooseTemplateSessionName(rows []beads.Bead, agentLookup bool, key, template string) string {
	var fallback, canonicalSeat string
	for _, b := range rows {
		if !IsSessionBeadOrRepairable(b) || b.Status == "closed" {
			continue
		}
		if key != "" && strings.TrimSpace(b.Metadata[key]) != template {
			continue
		}
		seat := isCanonicalSingletonPoolSeat(b, template)
		pool := isPoolCapacitySessionRow(b)
		if agentLookup && pool && sessionRowAgentName(b) == b.Metadata["template"] && !seat {
			continue
		}
		if !agentLookup && pool {
			continue
		}
		sessionName := strings.TrimSpace(b.Metadata["session_name"])
		if sessionName == "" {
			continue
		}
		if NamedSessionIdentity(b) != "" {
			return sessionName
		}
		if seat {
			if canonicalSeat == "" {
				canonicalSeat = sessionName
			}
			continue
		}
		if fallback == "" {
			fallback = sessionName
		}
	}
	if fallback == "" {
		return canonicalSeat
	}
	return fallback
}

// isPoolCapacitySessionRow reports whether b is pool capacity rather than a
// session of its own: it carries the pool_managed marker or a pool slot, or
// its origin is ephemeral. An explicit session_origin decides the origin;
// without one, a named or manual session is not ephemeral, and a
// dependency-only row, or one whose agent or session name is a pool member
// name of its template, is.
func isPoolCapacitySessionRow(b beads.Bead) bool {
	if strings.TrimSpace(b.Metadata["pool_managed"]) == "true" || strings.TrimSpace(b.Metadata["pool_slot"]) != "" {
		return true
	}
	if origin := strings.TrimSpace(b.Metadata["session_origin"]); origin != "" {
		return origin == "ephemeral"
	}
	if IsNamedSessionBead(b) || b.Metadata["manual_session"] == "true" {
		return false
	}
	if strings.TrimSpace(b.Metadata["dependency_only"]) == "true" {
		return true
	}
	template := strings.TrimSpace(b.Metadata["template"])
	if template == "" {
		return false
	}
	return PoolSlotFromName(strings.TrimSpace(sessionRowAgentName(b)), template) > 0 ||
		PoolSlotFromName(strings.TrimSpace(b.Metadata["session_name"]), template) > 0
}

// isCanonicalSingletonPoolSeat reports whether b is template's canonical
// singleton pool seat: pool capacity with no pool slot that identifies as
// template by its agent name, alias, title or agent:<template> label.
func isCanonicalSingletonPoolSeat(b beads.Bead, template string) bool {
	template = strings.TrimSpace(template)
	if template == "" || !isPoolCapacitySessionRow(b) || strings.TrimSpace(b.Metadata["pool_slot"]) != "" {
		return false
	}
	return strings.TrimSpace(b.Metadata["agent_name"]) == template ||
		strings.TrimSpace(b.Metadata["alias"]) == template ||
		strings.TrimSpace(b.Title) == template ||
		slices.Contains(b.Labels, "agent:"+template)
}

// sessionRowAgentName returns b's agent name: its agent_name metadata, else
// the value of its agent: label.
func sessionRowAgentName(b beads.Bead) string {
	if name := b.Metadata["agent_name"]; name != "" {
		return name
	}
	for _, label := range b.Labels {
		if name, ok := strings.CutPrefix(label, "agent:"); ok {
			return name
		}
	}
	return ""
}
