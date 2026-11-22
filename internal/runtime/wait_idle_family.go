package runtime

import "slices"

// WaitIdleLiveFamilies are the provider families whose sessions a wait-idle
// nudge can be delivered to live: the family's runtime exposes an idle
// boundary this build can wait on (claude's ready prompt, grok's composer and
// Ctrl+c:cancel turn footer). Every gate that decides whether a wait-idle
// delivery may run — the worker handle's, the session manager's, the CLI's —
// asks WaitIdleLiveOn, so the family list has one owner.
//
// Adding a family here is a promise about its runtimes: an idle wait on one of
// its sessions must not settle while its turn runs, or a wait-idle nudge would
// type into a working session. claude keeps that promise wherever a runtime
// waits: its busy indicator is what the family-blind pane scan matches. A
// family whose turn only a family-aware reading sees is listed in
// familyAwareWaitIdleFamilies too, and is live only on a runtime that says it
// reads that family (IdleWaitFamilyProvider); the tmux leaf must say so for
// every family here (the tmux package pins its list against this one).
var WaitIdleLiveFamilies = []string{"claude", "grok"}

// familyAwareWaitIdleFamilies are the WaitIdleLiveFamilies whose running turn
// a family-blind pane scan cannot see. grok's boxed composer stays on screen
// while it works, so only its Ctrl+c:cancel footer below that composer shows
// the turn, and an idle wait that classifies captures without the family (the
// exec runtime's) would settle in the middle of it.
var familyAwareWaitIdleFamilies = []string{"grok"}

// IdleWaitFamilyProvider is an optional extension for an [IdleWaitProvider]
// whose idle wait reads a provider family's own turn indicator.
// IdleWaitReadsFamily reports whether WaitForIdle on a session of family
// stays unsettled while that session's turn runs. It is a fixed trait of the
// runtime, read on the leaf that serves the session (WaitIdleLiveOn), so
// composites do not implement it. WaitIdleLiveOn asks it only about a family
// whose turn only a family-aware reading sees, and refuses such a family on a
// runtime that does not implement it.
type IdleWaitFamilyProvider interface {
	IdleWaitReadsFamily(family string) bool
}

// WaitIdleLiveFamily reports whether family is one of WaitIdleLiveFamilies.
func WaitIdleLiveFamily(family string) bool {
	return slices.Contains(WaitIdleLiveFamilies, family)
}

// WaitIdleLiveOn reports whether a wait-idle nudge to session name, whose
// provider family is family, may wait on p for an idle boundary and then be
// typed live: family is one of WaitIdleLiveFamilies, and a family whose turn
// only a family-aware reading sees (familyAwareWaitIdleFamilies) is live only
// when the leaf [ResolveBackend] names for name is an [IdleWaitFamilyProvider]
// that reads it. Any other runtime refuses such a family, so its wait never
// settles in the middle of a turn it cannot see. It takes no reading, so it
// costs no runtime call.
func WaitIdleLiveOn(p Provider, name, family string) bool {
	if !WaitIdleLiveFamily(family) {
		return false
	}
	if !slices.Contains(familyAwareWaitIdleFamilies, family) {
		return true
	}
	leaf, _, _ := ResolveBackend(p, name)
	reader, ok := leaf.(IdleWaitFamilyProvider)
	return ok && reader.IdleWaitReadsFamily(family)
}
