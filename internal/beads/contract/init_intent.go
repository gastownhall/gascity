package contract

import (
	"fmt"
	"strings"
)

// InitIntent is the provider-neutral initialization topology requested by a
// caller. Empty values mean that no intent was supplied at that layer.
type InitIntent struct {
	Transport string
	Target    string
}

// InitScopeState is the durable portion of a scope's provider state used when
// resolving initialization. Initialized state is authoritative over policy.
type InitScopeState struct {
	Initialized bool
	Backend     string
	DoltMode    string
	Target      string
}

// InitIntentResolution is the result of resolving initialization policy.
// PreserveBackend reports that an initialized non-Dolt backend must remain
// untouched.
type InitIntentResolution struct {
	Intent          InitIntent
	Source          string
	PreserveBackend bool
}

// ResolveInitIntent applies the provider-neutral initialization precedence:
// persisted state, CLI, explicitly admitted policy environment, city policy,
// then provider default. It never reads ambient BEADS_DOLT_* variables.
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
		if !isInitDoltBackend(persisted.Backend) {
			if cliIntent != (InitIntent{}) || envIntent != (InitIntent{}) || configIntent != (InitIntent{}) {
				return InitIntentResolution{}, fmt.Errorf("initialized backend %q is authoritative; initialization topology cannot be changed", persisted.Backend)
			}
			return InitIntentResolution{PreserveBackend: true, Source: "persisted-backend"}, nil
		}
		if mode := strings.TrimSpace(persisted.DoltMode); mode != "" && persistedTransport(mode) == "" {
			return InitIntentResolution{}, fmt.Errorf("persisted dolt mode %q is unsupported", mode)
		}
		persistedIntent := InitIntent{Transport: persistedTransport(persisted.DoltMode), Target: normalizeInitTarget(persisted.Target)}
		if persistedIntent.Transport != "" && persistedIntent.Target == "" {
			persistedIntent.Target = "local"
		}
		for _, item := range []struct {
			name   string
			intent InitIntent
		}{
			{"cli", cliIntent}, {"environment", envIntent}, {"city config", configIntent},
		} {
			if item.intent != (InitIntent{}) && item.intent != persistedIntent {
				return InitIntentResolution{}, fmt.Errorf("%s initialization intent %s conflicts with persisted topology %s", item.name, formatInitIntent(item.intent), formatInitIntent(persistedIntent))
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

func isInitDoltBackend(backend string) bool {
	b := strings.ToLower(strings.TrimSpace(backend))
	return b == "" || b == "dolt" || b == "bd"
}

func persistedTransport(mode string) string {
	if strings.EqualFold(strings.TrimSpace(mode), "proxied-server") {
		return "proxied"
	}
	if strings.EqualFold(strings.TrimSpace(mode), "server") {
		return "direct"
	}
	return ""
}

func normalizeInitTarget(target string) string {
	target = strings.ToLower(strings.TrimSpace(target))
	if target == "local" || target == "external" {
		return target
	}
	return ""
}

func formatInitIntent(intent InitIntent) string {
	return fmt.Sprintf("%s/%s", strings.TrimSpace(intent.Transport), strings.TrimSpace(intent.Target))
}
