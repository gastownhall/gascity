package config

import (
	"fmt"
	"sort"
	"strings"
)

// ProviderReference describes a provider name referenced by config.
type ProviderReference struct {
	Kind     string
	Agent    string
	Provider string
}

// MissingProviderReferences returns provider references that are not present
// in the composed provider catalog.
func MissingProviderReferences(cfg *City) []ProviderReference {
	if cfg == nil {
		return nil
	}
	seen := make(map[string]bool)
	var refs []ProviderReference
	add := func(ref ProviderReference) {
		ref.Provider = strings.TrimSpace(ref.Provider)
		if ref.Provider == "" {
			return
		}
		if _, ok := cfg.Providers[ref.Provider]; ok {
			return
		}
		key := ref.Kind + "\x00" + ref.Agent + "\x00" + ref.Provider
		if seen[key] {
			return
		}
		seen[key] = true
		refs = append(refs, ref)
	}

	add(ProviderReference{Kind: "workspace", Provider: cfg.Workspace.Provider})
	for _, agent := range cfg.Agents {
		if agent.StartCommand != "" {
			continue
		}
		add(ProviderReference{
			Kind:     "agent",
			Agent:    agent.QualifiedName(),
			Provider: agent.Provider,
		})
	}

	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Kind != refs[j].Kind {
			return refs[i].Kind == "workspace"
		}
		if refs[i].Agent != refs[j].Agent {
			return refs[i].Agent < refs[j].Agent
		}
		return refs[i].Provider < refs[j].Provider
	})
	return refs
}

// ProviderCatalogError reports provider references missing from the explicit
// provider catalog.
type ProviderCatalogError struct {
	References []ProviderReference
}

// Error formats the missing provider catalog references with repair hints.
func (e *ProviderCatalogError) Error() string {
	if e == nil || len(e.References) == 0 {
		return "provider catalog is missing referenced providers"
	}
	var b strings.Builder
	b.WriteString("provider catalog is missing referenced providers:")
	for _, ref := range e.References {
		b.WriteString("\n- ")
		b.WriteString(providerReferenceLabel(ref))
		b.WriteString(": ")
		b.WriteString(providerReferenceFixHint(ref.Provider))
	}
	b.WriteString("\nRun `gc doctor --fix` to add missing builtin provider aliases.")
	return b.String()
}

// ValidateProviderReferences returns an error when any provider reference is
// missing from the composed provider catalog.
func ValidateProviderReferences(cfg *City) error {
	refs := MissingProviderReferences(cfg)
	if len(refs) == 0 {
		return nil
	}
	return &ProviderCatalogError{References: refs}
}

func providerReferenceLabel(ref ProviderReference) string {
	switch ref.Kind {
	case "workspace":
		return fmt.Sprintf("workspace.provider %q", ref.Provider)
	case "agent":
		return fmt.Sprintf("agent %q: provider %q", ref.Agent, ref.Provider)
	default:
		return fmt.Sprintf("%s provider %q", ref.Kind, ref.Provider)
	}
}

func providerReferenceFixHint(provider string) string {
	if _, ok := BuiltinProviders()[provider]; ok {
		return fmt.Sprintf("add [providers.%s] base = %q", provider, BasePrefixBuiltin+provider)
	}
	return fmt.Sprintf("add [providers.%s] with command = ...", provider)
}
