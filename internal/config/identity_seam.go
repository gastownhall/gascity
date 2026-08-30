package config

import "strings"

// bindingQualifiedIdentity returns name qualified by an import binding when
// one is present. Empty and otherwise unusual values are preserved verbatim.
func bindingQualifiedIdentity(binding, name string) string {
	if binding == "" {
		return name
	}
	return binding + "." + name
}

// qualifiedIdentity returns the canonical identity with an optional directory
// prefix and import binding. It intentionally performs no validation or
// normalization; callers rely on the historical string representation.
func qualifiedIdentity(dir, binding, name string) string {
	identity := bindingQualifiedIdentity(binding, name)
	if dir == "" {
		return identity
	}
	return dir + "/" + identity
}

// parseQualifiedIdentity splits an identity at its final slash. Identities
// without a slash have an empty directory, and all other bytes are preserved.
func parseQualifiedIdentity(identity string) (dir, name string) {
	if i := strings.LastIndex(identity, "/"); i >= 0 {
		return identity[:i], identity[i+1:]
	}
	return "", identity
}

// agentIdentityMatches reports whether the supplied identity names an agent.
// A V1 directory/name fallback is retained only for agents without an import
// binding; bound agents must use their binding-qualified identity.
func agentIdentityMatches(dir, name, binding, identity string) bool {
	if qualifiedIdentity(dir, binding, name) == identity {
		return true
	}
	if binding == "" {
		identityDir, identityName := parseQualifiedIdentity(identity)
		return dir == identityDir && name == identityName
	}
	return false
}
