package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/config"
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
func bdAddressedSubjects(args []string) []string {
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
	if len(owners) != 1 {
		return execStoreTarget{}, false
	}
	return owners[0], true
}
