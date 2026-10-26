package main

import (
	"fmt"

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
		return fmt.Sprintf("[beads] conditional_writes does not resolve (%v), and v2 needs conditional writes", err)
	}
	switch mode := flags.BeadsConditionalWrites(); {
	case mode == rollout.Auto || mode == rollout.Require:
		return ""
	case flags.OriginOf(rollout.KeyBeadsConditionalWrites) == rollout.OriginEnv:
		return fmt.Sprintf("GC_BEADS_CONDITIONAL_WRITES=%s turns conditional writes off, and v2 needs them", mode)
	default:
		return fmt.Sprintf("[beads] conditional_writes = %q turns conditional writes off, and v2 needs them", mode)
	}
}

// v2DefaultStoreKindFallbacks is the store-kind half of C0.7 read from config:
// a fallback for each binding of the sessions and graph classes that is not a
// sqlite-beads binding, the one kind config shows to fence and to list with
// revisions. The work binding's store is decided when it opens (bd opens
// native Dolt only when preflight admits it), and a file, exec or bd store is
// one C0.7 refuses, so a class there falls back, naming the storage migration
// that moves it; so does a binding whose provider config cannot classify.
func v2DefaultStoreKindFallbacks(cfg *config.City) []string {
	storage := cfg.EffectiveStorage()
	var out []string
	sessions := storage.Classes.BindingFor(config.StorageClassSessions)
	graph := storage.Classes.BindingFor(config.StorageClassGraph)
	type classBinding struct{ classes, them, binding string }
	stores := []classBinding{{"sessions and graph class stores are", "them", sessions}}
	if sessions != graph {
		stores = []classBinding{{"sessions class store is", "it", sessions}, {"graph class store is", "it", graph}}
	}
	for _, c := range stores {
		var where, fix string
		switch binding, ok := storage.Bindings[c.binding]; {
		case c.binding == "" || c.binding == config.StorageWorkBinding:
			where = fmt.Sprintf("the %q binding", config.StorageWorkBinding)
			fix = fmt.Sprintf(": moving %s there is a storage migration (configure [storage], then run %s)", c.them, storageMigrationCommand)
		case !ok:
			where = fmt.Sprintf("binding %q, which [storage.bindings] does not define", c.binding)
		case binding.Provider == config.StorageProviderSQLiteBeads:
			continue
		default:
			where = fmt.Sprintf("binding %q (provider %q)", c.binding, binding.Provider)
		}
		out = append(out, fmt.Sprintf("the %s on %s, and v2 needs %s on a %s binding%s", c.classes, where, c.them, config.StorageProviderSQLiteBeads, fix))
	}
	return out
}
