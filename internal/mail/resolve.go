package mail

import (
	"fmt"
	"strings"
)

// AgentEntry represents a configured agent for recipient resolution.
type AgentEntry struct {
	Dir         string // rig directory (empty for city-scoped agents)
	Name        string // bare agent name
	BindingName string // V2 import binding (empty for city-local agents)
}

// QualifiedName returns the agent's qualified identity. For V2 agents
// with a binding, produces "dir/binding.name" or "binding.name".
// For V1 agents, produces "dir/name" or just "name".
func (a AgentEntry) QualifiedName() string {
	name := a.Name
	if a.BindingName != "" {
		name = a.BindingName + "." + a.Name
	}
	if a.Dir == "" {
		return name
	}
	return a.Dir + "/" + name
}

// ResolveRecipient resolves a mail recipient to a canonical qualified name.
//
// Resolution order:
//  1. "human" passes through unchanged (reserved recipient).
//  2. Qualified name ("rig/name") is matched literally.
//  3. Bare name ("name") is matched against all agents by Name field.
//     Succeeds only when exactly one agent matches; rejects ambiguous names.
//
// Returns the canonical qualified name or an error describing the failure.
func ResolveRecipient(to string, agents []AgentEntry) (string, error) {
	to = strings.TrimSpace(to)
	if to == "" {
		return "", fmt.Errorf("empty recipient")
	}
	if to == "human" {
		return "human", nil
	}

	// Qualified name: literal match.
	if strings.Contains(to, "/") {
		for _, a := range agents {
			if a.QualifiedName() == to {
				return to, nil
			}
		}
		return "", fmt.Errorf("unknown recipient %q", to)
	}

	// Bare name: find all agents with this Name.
	var matches []AgentEntry
	for _, a := range agents {
		if a.Name == to {
			matches = append(matches, a)
		}
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("unknown recipient %q", to)
	case 1:
		return matches[0].QualifiedName(), nil
	default:
		qualified := make([]string, len(matches))
		for i, m := range matches {
			qualified[i] = m.QualifiedName()
		}
		return "", fmt.Errorf("ambiguous recipient %q: matches %s", to, strings.Join(qualified, ", "))
	}
}

// AgentEntriesFromConfig builds an AgentEntry slice from agent qualified names.
// Each entry should have Dir and Name fields set.
func AgentEntriesFromConfig(agents []AgentEntry) []AgentEntry {
	return agents
}
