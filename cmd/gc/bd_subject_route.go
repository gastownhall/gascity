package main

import (
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/pathutil"
	"github.com/gastownhall/gascity/internal/sling"
)

// bdSubjectPrefixTarget routes a by-id gc bd invocation by its subjects'
// prefixes alone. ok is false, and the caller keeps the existence-probe chain,
// unless argv is a by-id read or write whose subjects scan exactly, every
// subject's prefix belongs to the same single bound store, and argv names no
// -C / --directory.
//
// The existence probe in resolveBdScopeTarget exists to tell bead ids from
// id-shaped tokens that are not ids (a flag value, free text), and it costs a
// store open plus a read per candidate. An id a by-id verb ADDRESSES is not
// such a token: the verb's flag manifest says which positionals are subjects
// (bdByIDReadSubjects, bdMutationWriteIDs), and a subject's prefix names the
// store that mints it. A subject absent from that store is answered by bd
// there ("not found"), rather than by whichever store cwd or GC_RIG named.
//
// A -C directory outranks the prefix, because bd resolves its store from it
// over the BEADS_DIR the returned target pins: answered by prefix, the exact-ID
// guard would read one store while bd writes another, so -C keeps the chain.
func bdSubjectPrefixTarget(cfg *config.City, cityTarget execStoreTarget, cityPath string, args []string) (execStoreTarget, bool) {
	if extractBdDirectoryFlag(args) != "" {
		return execStoreTarget{}, false
	}
	ids := bdAddressedSubjects(args)
	if len(ids) == 0 {
		return execStoreTarget{}, false
	}
	var target execStoreTarget
	for i, id := range ids {
		owner, ok := bdPrefixOwner(cfg, cityTarget, cityPath, id)
		if !ok || (i > 0 && owner != target) {
			return execStoreTarget{}, false
		}
		target = owner
	}
	return target, true
}

// bdAddressedSubjects returns the bead ids a by-id read or write addresses,
// scanned exactly against the verb's flag manifest, or nil when argv is no
// such invocation or cannot be scanned without guessing.
//
// gc's own `release-if-current <issue-id> <assignee>` addresses the issue
// alone. Its assignee is an identity, often a pool session's bead id carrying
// the city's prefix, and must never pick the store the release runs in.
func bdAddressedSubjects(args []string) []string {
	if id, _, ok, err := parseBdReleaseIfCurrentArgs(args); ok {
		if err != nil {
			return nil
		}
		return []string{id}
	}
	if ids := bdByIDReadSubjects(args); len(ids) > 0 {
		return ids
	}
	return bdByIDMutationSubjects(args)
}

// bdPrefixOwner returns the one bound store whose prefix id carries. ok is
// false when no store owns the prefix, when two do (the city and a rig, or two
// rigs, configured with the same prefix), or when the owner is a rig with no
// path binding, which resolveBdScopeTarget deliberately never aliases to the
// city store.
//
// A configured prefix is asked first; an id none of them owns falls to the
// routes registration (bdRoutesOwner), which spells each store's real issue
// prefix. The two arms agree on one dash-append gate, so a configured prefix
// and a registered route cannot disagree about which IDs they select.
//
// It walks cfg.Rigs itself rather than calling sling.FindRigByPrefix, which
// returns only the first matching rig and so cannot see a second one.
func bdPrefixOwner(cfg *config.City, cityTarget execStoreTarget, cityPath, id string) (execStoreTarget, bool) {
	prefix := beadPrefix(cfg, id)
	if prefix == "" {
		return execStoreTarget{}, false
	}
	var owners []execStoreTarget
	if sling.IsHQPrefix(cfg, prefix) {
		owners = append(owners, cityTarget)
	}
	for _, rig := range cfg.Rigs {
		if !strings.EqualFold(rig.EffectivePrefix(), prefix) {
			continue
		}
		if strings.TrimSpace(rig.Path) == "" {
			return execStoreTarget{}, false
		}
		owners = append(owners, bdRigScopeTarget(cityPath, rig))
	}
	if len(owners) == 1 {
		return owners[0], true
	}
	if len(owners) > 1 {
		return execStoreTarget{}, false
	}
	return bdRoutesOwner(cfg, cityTarget, cityPath, id)
}

// bdRoutesOwner resolves an id no CONFIGURED prefix owns against the routes
// registration. writeAllRoutes spells each store's REAL beads issue prefix
// (collectRigRoutes reads it from the store), which under an adopted store
// differs from the config-derived one — DeriveBeadsPrefix truncates
// ("city" → "ci") where the store mints city-* — so before this arm every
// consumer that dash-appends (bd's lead-prefix match, the API plane's
// residency arm, this router) matched nothing and a by-id gc bd invocation
// from a rig answered from whichever store cwd or GC_RIG named (city-fr3t).
//
// Matching is sling.BeadIDMatchesPrefix, the same gate configured prefixes go
// through, and among matching entries the LONGEST prefix wins, mirroring
// matchConfiguredBeadPrefixBySuffix — overlapping registered prefixes ("be",
// "be-frontend") resolve exactly as configured ones do.
//
// The city's own routes.jsonl is read first, then each bound rig's, the API
// plane's residency order. An entry whose path resolves outside the city and
// the bound rigs is skipped: like the API plane's resolver, a route naming an
// unregistered store says nothing about this city's stores, and the caller
// keeps its probe chain. ok is false unless exactly one bound store owns the
// id — the same prefix registered by two stores naming different targets is
// registration telling the truth twice, and the existence probes must decide.
func bdRoutesOwner(cfg *config.City, cityTarget execStoreTarget, cityPath, id string) (execStoreTarget, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return execStoreTarget{}, false
	}
	cityRoot := resolveStoreScopeRoot(cityPath, cityPath)
	bestLen := 0
	var owners []execStoreTarget

	consider := func(ownerDir, prefix, path string) {
		if !sling.BeadIDMatchesPrefix(id, prefix) || len(prefix) < bestLen {
			return
		}
		resolved := path
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(ownerDir, resolved)
		}
		owner, ok := bdRouteStoreOwner(cfg, cityTarget, cityPath, cityRoot, resolved)
		if !ok {
			return
		}
		if len(prefix) > bestLen {
			bestLen = len(prefix)
			owners = []execStoreTarget{owner}
			return
		}
		for _, existing := range owners {
			if existing == owner {
				return
			}
		}
		owners = append(owners, owner)
	}

	if entries, ok := routeEntriesFor(cityRoot); ok {
		for _, entry := range entries {
			consider(cityRoot, entry.Prefix, entry.Path)
		}
	}
	for i := range cfg.Rigs {
		if strings.TrimSpace(cfg.Rigs[i].Path) == "" {
			continue
		}
		rigRoot := resolveStoreScopeRoot(cityPath, cfg.Rigs[i].Path)
		if entries, ok := routeEntriesFor(rigRoot); ok {
			for _, entry := range entries {
				consider(rigRoot, entry.Prefix, entry.Path)
			}
		}
	}
	if len(owners) == 1 {
		return owners[0], true
	}
	return execStoreTarget{}, false
}

// bdRouteStoreOwner maps a routes.jsonl entry's resolved store path to the
// bound store target it names. ok is false when it names neither the city
// store nor a bound rig. Paths are normalized the way resolveStoreScopeRoot
// normalizes scope roots, so a route written through a linked path resolves
// to the same store target bd would pin.
func bdRouteStoreOwner(cfg *config.City, cityTarget execStoreTarget, cityPath, cityRoot, resolved string) (execStoreTarget, bool) {
	if pathutil.NormalizePathForCompare(resolved) == cityRoot {
		return cityTarget, true
	}
	for i := range cfg.Rigs {
		if strings.TrimSpace(cfg.Rigs[i].Path) == "" {
			continue
		}
		if pathutil.NormalizePathForCompare(resolved) == resolveStoreScopeRoot(cityPath, cfg.Rigs[i].Path) {
			return bdRigScopeTarget(cityPath, cfg.Rigs[i]), true
		}
	}
	return execStoreTarget{}, false
}
