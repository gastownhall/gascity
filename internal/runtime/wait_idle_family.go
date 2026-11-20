package runtime

import "slices"

// WaitIdleLiveFamilies are the provider families whose sessions a wait-idle
// nudge can be delivered to live: the family's runtime exposes an idle
// boundary this build can wait on (claude's ready prompt, grok's composer and
// Ctrl+c:cancel turn footer). Every gate that decides whether a wait-idle
// delivery may run — the worker handle's, the session manager's, the CLI's —
// asks this predicate, so the family list has one owner.
//
// Adding a family here is a promise about its runtime: the tmux leaf must
// trust the family's busy reading (the tmux package's busy-trust policy pins
// its list against this one), or a wait-idle nudge would wait out a turn it
// cannot see and type into a working session.
var WaitIdleLiveFamilies = []string{"claude", "grok"}

// WaitIdleLiveFamily reports whether family is one of WaitIdleLiveFamilies.
func WaitIdleLiveFamily(family string) bool {
	return slices.Contains(WaitIdleLiveFamilies, family)
}
