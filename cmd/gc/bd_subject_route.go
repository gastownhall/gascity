package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/bdflags"
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
	return bdStoreOwnerForPrefix(cfg, cityTarget, cityPath, prefix)
}

// bdStoreOwnerForPrefix is bdPrefixOwner's walk for an already-extracted
// prefix: the city store when the prefix is the HQ's, every rig configured
// with it otherwise, one owner or none.
func bdStoreOwnerForPrefix(cfg *config.City, cityTarget execStoreTarget, cityPath, prefix string) (execStoreTarget, bool) {
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

// bdSelectorStoreVerbs are the read-only selector verbs
// bdSelectorPrefixTarget routes. Unlike the by-id verbs bdSubjectPrefixTarget
// serves, a selector does not address one bead — it asks a store for every row
// matching its filters, and bd answers [] and exit 0 from whichever store it
// was pointed at. The verbs are read-only because a mint or a mutation acts in
// the store the operator chose, not in the store its filters name.
var bdSelectorStoreVerbs = map[string]bool{"list": true, "ready": true, "search": true}

// bdSelectorStoreFlags are the selector flags whose value NAMES the rows the
// query selects — a bead id (`--id`, `--parent`, `--filter-parent`, `--mol`,
// `--spec`) or an agent identity (`--assignee`) — and with it the store those
// rows live in. The rest of a selector's flags (`--label`, `--title`,
// `--status`, free text) describe rows without naming one, and the value they
// carry is prose a prefix must never route on
// (TestGcBdDoesNotAutoRouteHyphenatedFlagValue).
var bdSelectorStoreFlags = map[string]bool{
	"--id": true, "--parent": true, "--filter-parent": true,
	"--mol": true, "--spec": true, "--assignee": true, "-a": true,
}

// bdSelectorPrefixTarget routes a selector read (list, ready, search) by its
// store-naming filter values' prefixes. ok is false, and the caller keeps the
// existence-probe chain, unless the argv is one of those verbs, every such
// value present resolves to the same single bound store, and argv names no
// -C / --directory.
//
// The by-id reads above route because their verb ADDRESSES the id; a selector
// QUOTES its values. But a quoted prefix still names the store whose rows the
// query is about, and the same query answered from another store is a
// confident [] — the queue-starvation scan's `gc bd list --status=open
// --assignee=gk/…` reported 0 assigned for every rig agent for as long as it
// ran against the city store, while `bd show gk-…` on the same beads routed
// fine. Values that resolve to no store (an unprefixed identity, prose, an
// unknown flag's argument) leave the store to the existing chain, so every
// answer this does not change is byte-identical to the old one.
func bdSelectorPrefixTarget(cfg *config.City, cityTarget execStoreTarget, cityPath string, args []string) (execStoreTarget, bool) {
	if extractBdDirectoryFlag(args) != "" {
		return execStoreTarget{}, false
	}
	verb, rest, resolved := bdByIDSubcommand(args)
	if !resolved || !bdSelectorStoreVerbs[verb] {
		return execStoreTarget{}, false
	}
	values := bdSelectorStoreValues(verb, rest)
	if len(values) == 0 {
		return execStoreTarget{}, false
	}
	var target execStoreTarget
	for i, value := range values {
		owner, ok := bdSelectorValueOwner(cfg, cityTarget, cityPath, value)
		if !ok || (i > 0 && owner != target) {
			return execStoreTarget{}, false
		}
		target = owner
	}
	return target, true
}

// bdSelectorStoreValues returns the store-naming filter values one selector
// argv carries, splitting the comma lists `--id` accepts, or nil when the
// argv carries none or cannot be scanned without guessing. The walk is the
// bdByIDSubcommand discipline against the verb's own flag manifest: `--`
// ends it, a positional is skipped (search's free-text query), a bool or
// inline flag is passed, an unknown flag fails the whole scan closed — it
// may or may not consume the next token, and a guess here would route on a
// value that is not a value.
func bdSelectorStoreValues(verb string, rest []string) []string {
	valueFlags := bdflags.ValueFlags(verb)
	boolFlags := bdflags.BoolFlags(verb)
	var values []string
	for i := 0; i < len(rest); i++ {
		arg := rest[i]
		if arg == "--" {
			break
		}
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		name, inline, hasInline := strings.Cut(arg, "=")
		if bdSelectorStoreFlags[name] {
			if !hasInline {
				if i+1 >= len(rest) {
					return nil
				}
				inline = rest[i+1]
				i++
			}
			for _, part := range strings.Split(inline, ",") {
				if part = strings.TrimSpace(part); part != "" {
					values = append(values, part)
				}
			}
			continue
		}
		if hasInline || boolFlags[name] {
			continue
		}
		if valueFlags[name] {
			i++
			continue
		}
		return nil
	}
	return values
}

// bdSelectorValueOwner resolves the store one selector filter value names. A
// value carrying "/" is a qualified agent identity ("<rig-dir>/<binding.name>",
// Agent.QualifiedName's shape; a bead id and a session_name never contain
// "/"): its left side resolves by rig name first, then through the prefix
// walk, which is also what carries a city whose rig routes beads under a
// prefix distinct from the rig's name. Any other value resolves as a bead id.
func bdSelectorValueOwner(cfg *config.City, cityTarget execStoreTarget, cityPath, value string) (execStoreTarget, bool) {
	if dir, rest, ok := strings.Cut(value, "/"); ok && dir != "" && rest != "" {
		if rig, found := rigByName(cfg, dir); found {
			if strings.TrimSpace(rig.Path) == "" {
				return execStoreTarget{}, false
			}
			return bdRigScopeTarget(cityPath, rig), true
		}
		return bdStoreOwnerForPrefix(cfg, cityTarget, cityPath, dir)
	}
	return bdPrefixOwner(cfg, cityTarget, cityPath, value)
}
