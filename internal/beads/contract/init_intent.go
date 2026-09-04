package contract

import (
	"fmt"
	"strings"
)

// InitIntent is the provider-neutral initialization topology requested by a
// caller. Empty values mean that no intent was supplied at that layer.
// Transport is direct or proxied; Target is local or external.
type InitIntent struct {
	Transport string
	Target    string
}

// InitScopeState is the durable portion of a scope's provider state used when
// resolving initialization. Once Initialized is true, this state is
// authoritative and later policy cannot silently change it.
type InitScopeState struct {
	Initialized bool
	Backend     string
	DoltMode    string
	Target      string
}

// InitIntentResolution is the result of resolving initialization policy.
// PreserveBackend is true for an initialized non-Dolt backend (for example
// DoltLite); callers must leave that backend untouched.
type InitIntentResolution struct {
	Intent          InitIntent
	Source          string
	PreserveBackend bool
}

// ResolveInitIntent applies the initialization precedence contract:
// persisted state (when initialized), explicit CLI, allowed environment,
// city policy, then provider default. Ambient BEADS_DOLT_* variables must not
// be passed as envIntent; callers should provide only an explicitly allowed
// policy environment selection.
func ResolveInitIntent(persisted InitScopeState, cliIntent, envIntent, configIntent, providerDefault InitIntent) (InitIntentResolution, error) {
	var err error
	for _, item := range []struct {
		name   string
		intent *InitIntent
	}{
		{"cli", &cliIntent}, {"environment", &envIntent}, {"city config", &configIntent}, {"provider default", &providerDefault},
	} {
		*item.intent, err = normalizeInitIntent(item.name, *item.intent)
		if err != nil {
			return InitIntentResolution{}, err
		}
	}
	if persisted.Initialized {
		if !isDoltBackend(persisted.Backend) {
			if cliIntent != (InitIntent{}) || envIntent != (InitIntent{}) || configIntent != (InitIntent{}) {
				return InitIntentResolution{}, fmt.Errorf("initialized backend %q is authoritative; initialization topology cannot be changed", persisted.Backend)
			}
			return InitIntentResolution{PreserveBackend: true, Source: "persisted-backend"}, nil
		}
		transport, err := persistedTransport(persisted.DoltMode)
		if err != nil {
			return InitIntentResolution{}, err
		}
		persistedIntent := InitIntent{Transport: transport, Target: normalizeTarget(persisted.Target)}
		if persistedIntent.Transport != "" && persistedIntent.Target == "" {
			persistedIntent.Target = "local"
		}
		for _, item := range []struct {
			name   string
			intent InitIntent
		}{
			{"cli", cliIntent}, {"environment", envIntent}, {"city config", configIntent},
		} {
			name, intent := item.name, item.intent
			if intent != (InitIntent{}) && intent != persistedIntent {
				return InitIntentResolution{}, fmt.Errorf("%s initialization intent %s conflicts with persisted topology %s", name, formatIntent(intent), formatIntent(persistedIntent))
			}
		}
		return InitIntentResolution{Intent: persistedIntent, Source: "persisted"}, nil
	}
	if cliIntent != (InitIntent{}) {
		return InitIntentResolution{Intent: cliIntent, Source: "cli"}, nil
	}
	if envIntent != (InitIntent{}) {
		return InitIntentResolution{Intent: envIntent, Source: "environment"}, nil
	}
	if configIntent != (InitIntent{}) {
		return InitIntentResolution{Intent: configIntent, Source: "city-config"}, nil
	}
	return InitIntentResolution{Intent: providerDefault, Source: "provider-default"}, nil
}

func normalizeInitIntent(source string, intent InitIntent) (InitIntent, error) {
	intent.Transport = strings.ToLower(strings.TrimSpace(intent.Transport))
	intent.Target = strings.ToLower(strings.TrimSpace(intent.Target))
	if intent.Transport == "" && intent.Target == "" {
		return InitIntent{}, nil
	}
	if intent.Transport == "" || intent.Target == "" {
		return InitIntent{}, fmt.Errorf("%s initialization intent must specify both transport and target", source)
	}
	if intent.Transport != "direct" && intent.Transport != "proxied" {
		return InitIntent{}, fmt.Errorf("%s initialization intent has unsupported transport %q", source, intent.Transport)
	}
	if intent.Target != "local" && intent.Target != "external" {
		return InitIntent{}, fmt.Errorf("%s initialization intent has unsupported target %q", source, intent.Target)
	}
	return intent, nil
}

func isDoltBackend(backend string) bool {
	b := strings.ToLower(strings.TrimSpace(backend))
	return b == "" || b == "dolt" || b == "bd"
}

func persistedTransport(mode string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "":
		// Older initialized scopes did not persist dolt_mode. Preserve their
		// historical direct-server behavior rather than selecting a new default.
		return "direct", nil
	case "proxied-server":
		return "proxied", nil
	case "server":
		return "direct", nil
	default:
		return "", fmt.Errorf("persisted dolt mode %q is unsupported", mode)
	}
}

func normalizeTarget(target string) string {
	t := strings.ToLower(strings.TrimSpace(target))
	if t == "local" || t == "external" {
		return t
	}
	return ""
}

func formatIntent(intent InitIntent) string {
	return fmt.Sprintf("%s/%s", strings.TrimSpace(intent.Transport), strings.TrimSpace(intent.Target))
}
