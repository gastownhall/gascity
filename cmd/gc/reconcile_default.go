package main

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/rollout"
)

// v2DefaultFallbacks lists why an unset [daemon] session_reconciler resolves
// to legacy rather than v2 (mc-dpo2b), in a fixed order, or nil when v2 is the
// default. Like v2LatchRefusals it reads only cfg, lookupEnv and the builtin
// runtime registry, never a store, so the controller latch, gc start and
// doctor resolve the same mode. Beyond the latch refusals it mirrors C0.7
// (v2CapabilityRefusal) from config: conditional writes must resolve on, and
// the sessions and graph class stores must be a kind that fences and whose
// list reads carry revisions. Store-state refusals (the boot census) are not
// config: they stay hard boot refusals, which doctor reports ahead of time.
func v2DefaultFallbacks(cfg *config.City, lookupEnv func(string) (string, bool)) []string {
	var out []string
	for _, r := range v2LatchRefusals(cfg, lookupEnv) {
		out = append(out, r.String())
	}
	if reason := v2DefaultConditionalWritesFallback(cfg, lookupEnv); reason != "" {
		out = append(out, reason)
	}
	return append(out, v2DefaultStoreKindFallbacks(cfg)...)
}

// v2DefaultConditionalWritesFallback is the fallback when conditional writes
// do not resolve on (auto or require) for cfg under lookupEnv's
// GC_BEADS_CONDITIONAL_WRITES, or "". It resolves the conditional_writes gate
// alone, as noteRolloutDrift does, so a typo in a sibling gate is not read as
// this one's. A nil lookupEnv has no override.
func v2DefaultConditionalWritesFallback(cfg *config.City, lookupEnv func(string) (string, bool)) string {
	if lookupEnv == nil {
		lookupEnv = func(string) (string, bool) { return "", false }
	}
	cwOnly := &config.City{Beads: config.BeadsConfig{ConditionalWrites: cfg.Beads.ConditionalWrites}}
	flags, err := rollout.Resolve(cwOnly, rollout.ResolveOptions{LookupEnv: lookupEnv})
	if err != nil {
		return fmt.Sprintf("[beads] conditional_writes does not resolve (%v), and v2 never writes blind (C0.7)", err)
	}
	switch mode := flags.BeadsConditionalWrites(); {
	case mode == rollout.Auto || mode == rollout.Require:
		return ""
	case flags.OriginOf(rollout.KeyBeadsConditionalWrites) == rollout.OriginEnv:
		return fmt.Sprintf("GC_BEADS_CONDITIONAL_WRITES=%s turns conditional writes off, and v2 never writes blind (C0.7)", mode)
	default:
		return fmt.Sprintf("[beads] conditional_writes = %q turns conditional writes off, and v2 never writes blind (C0.7)", mode)
	}
}

// v2DefaultStoreKindFallbacks is the store-kind half of C0.7 read from config:
// a fallback for each binding of the sessions and graph classes whose config
// does not show a store that fences and whose list reads carry revisions.
// Only a sqlite-beads binding does. The work binding's store is decided when
// it opens (bd opens native Dolt only when preflight admits it), and a file,
// exec or bd store is one C0.7 refuses, so a class on it falls back, as does
// a binding whose provider config cannot classify.
func v2DefaultStoreKindFallbacks(cfg *config.City) []string {
	storage := cfg.EffectiveStorage()
	var out []string
	sessions := storage.Classes.BindingFor(config.StorageClassSessions)
	graph := storage.Classes.BindingFor(config.StorageClassGraph)
	type classBinding struct{ classes, binding string }
	stores := []classBinding{{"sessions and graph class stores are", sessions}}
	if sessions != graph {
		stores = []classBinding{{"sessions class store is", sessions}, {"graph class store is", graph}}
	}
	for _, c := range stores {
		var kind string
		switch binding, ok := storage.Bindings[c.binding]; {
		case c.binding == "" || c.binding == config.StorageWorkBinding:
			kind = fmt.Sprintf("the %q binding, %s", config.StorageWorkBinding, v2WorkStoreKind(cfg))
		case !ok:
			kind = fmt.Sprintf("binding %q, which [storage.bindings] does not define", c.binding)
		case binding.Provider == config.StorageProviderSQLiteBeads:
			continue
		default:
			kind = fmt.Sprintf("binding %q (provider %q), whose store kind config cannot classify", c.binding, binding.Provider)
		}
		out = append(out, fmt.Sprintf("the %s on %s, and v2 needs a store that fences and whose list reads carry revisions (C0.7): a %s binding", c.classes, kind, config.StorageProviderSQLiteBeads))
	}
	return out
}

// v2WorkStoreKind describes the work binding's store as config shows it:
// [beads] provider, which the factory opens (beads.OpenStoreAtForCity).
func v2WorkStoreKind(cfg *config.City) string {
	provider := strings.TrimSpace(cfg.Beads.Provider)
	if provider == "" {
		provider = "bd"
	}
	switch {
	case provider == "file":
		return fmt.Sprintf("a %s ([beads] provider %q)", beads.BeadsStoreNameFileStore, provider)
	case !beads.ProviderConsultsNativeTransport(provider):
		return fmt.Sprintf("an %s ([beads] provider %q)", beads.BeadsStoreNameExecStore, provider)
	case !contract.ProviderUsesBDContract(provider) || cfg.Beads.NormalizedNativeTransport() == "off":
		return fmt.Sprintf("a %s ([beads] provider %q)", beads.BeadsStoreNameBdStore, provider)
	default:
		return fmt.Sprintf("a %s or native Dolt store, which preflight decides at open ([beads] provider %q)", beads.BeadsStoreNameBdStore, provider)
	}
}
