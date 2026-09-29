package main

import (
	"regexp"
	"strings"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/config"
)

// assigneeRoster answers one question: can this assignee name ever be routed to?
//
// The city has always had this information — build_desired_state.go consults it
// on every reconcile — but it only ever asked the question in the direction
// "which beads does this spec own?". A bead whose assignee matches no spec is
// never visited by that loop, so an unroutable owner has never been
// representable as a finding. Everything below exists to ask it the other way.
type assigneeRoster struct {
	// exact holds every name that is directly slingable: named-session
	// identities (short and qualified) and configured agent names.
	exact map[string]struct{}
	// poolStems holds agent names that materialize numbered pool instances,
	// so "polecat-4" resolves via the stem "polecat".
	poolStems map[string]struct{}
	// idPrefixes holds the bead ID prefixes this city actually issues, taken
	// from the HQ store and every rig. A runtime session identity is named
	// after its session bead, so these are the only prefixes an opaque
	// identity can legitimately carry.
	idPrefixes map[string]struct{}
}

// poolInstanceSuffix matches the numbered suffix poolInstanceName appends to a
// pool agent name when it materializes an instance (`%s-%d`). The optional
// trailing letter is not something that function produces; it is tolerated so
// a hand-written variant resolves rather than reading as a phantom owner.
var poolInstanceSuffix = regexp.MustCompile(`-\d+[a-z]?$`)

// generatedSessionShape matches the session names the runtime composes rather
// than reads from config. These exist only at runtime, so a static roster
// cannot confirm them and must not reject them.
var generatedSessionShape = regexp.MustCompile(`^.*-(?:adhoc-[0-9a-f]{6,}|auto-\d+)$`)

// beadIDSuffix matches the generated half of a bead ID, after the prefix.
var beadIDSuffix = regexp.MustCompile(`^[0-9a-z]{4,}$`)

// reservedAssignees are meaningful owners that can never appear in city
// config, because they name a category of actor rather than a routable
// target: a person, not an agent. Anything that IS a configured routing
// target — including a seat like "mayor" — must resolve through the roster
// built from config below, not through a literal comparison here. Keying
// this map on a role name would violate the project's zero-hardcoded-roles
// invariant (AGENTS.md); "human" is not a role, it is the fixed sentinel
// for "not city-routable by construction".
var reservedAssignees = map[string]struct{}{
	"human": {},
}

func newAssigneeRoster(cfg *config.City) *assigneeRoster {
	r := &assigneeRoster{
		exact:      map[string]struct{}{},
		poolStems:  map[string]struct{}{},
		idPrefixes: map[string]struct{}{},
	}
	if cfg == nil {
		return r
	}
	if p := strings.TrimSpace(config.EffectiveHQPrefix(cfg)); p != "" {
		r.idPrefixes[p] = struct{}{}
	}
	for i := range cfg.Rigs {
		if p := strings.TrimSpace(cfg.Rigs[i].EffectivePrefix()); p != "" {
			r.idPrefixes[p] = struct{}{}
		}
	}
	for i := range cfg.NamedSessions {
		r.addExact(cfg.NamedSessions[i].IdentityName())
		r.addExact(cfg.NamedSessions[i].QualifiedName())
	}
	for i := range cfg.Agents {
		name := strings.TrimSpace(cfg.Agents[i].Name)
		r.addExact(name)
		r.addExact(cfg.Agents[i].QualifiedName())
		if name != "" {
			r.poolStems[name] = struct{}{}
		}
		// A pool instance is not always named `<agent>-<slot>`: when the agent
		// declares a namepool, poolInstanceName returns the declared name
		// instead, which shares nothing with the stem. Those names are
		// routable and would otherwise read as phantom owners.
		for _, pooled := range cfg.Agents[i].NamepoolNames {
			r.addExact(pooled)
		}
	}
	return r
}

// Empty reports whether the roster knows of no routable target at all. A roster
// built from a config that failed to expand its packs looks exactly like a city
// with no agents, and would then declare every assignee in the fleet
// unroutable. Callers must treat an empty roster as "cannot answer" rather than
// "answer is no" — the same failure this whole change exists to prevent, one
// layer up.
func (r *assigneeRoster) Empty() bool {
	return len(r.exact) == 0 && len(r.poolStems) == 0
}

func (r *assigneeRoster) addExact(name string) {
	name = strings.TrimSpace(name)
	if name != "" {
		r.exact[name] = struct{}{}
	}
}

// Resolves reports whether assignee names something the city could route work
// to. An empty assignee resolves: unowned is a valid state, distinct from
// owned-by-nobody, and is reported separately by the reconciler.
func (r *assigneeRoster) Resolves(assignee string) bool {
	assignee = strings.TrimSpace(assignee)
	if assignee == "" {
		return true
	}
	if _, ok := reservedAssignees[assignee]; ok {
		return true
	}
	// The identity checks run over the path spellings only. The file store
	// records an owner path-qualified ("research/dr-huhn") where the bd store
	// records it bare, and a runtime identity written the long way is still a
	// runtime identity. They deliberately do not run over the dot spelling:
	// those checks match on shape rather than on a configured name, so any
	// typo carrying a bead-ID-shaped tail after a dot ("sjarmak.gc-818bx")
	// would pass as a session that never existed.
	for _, candidate := range pathCandidates(assignee) {
		// A runtime identity cannot be confirmed against static config.
		// Treating an unconfirmable name as a finding would bury the real ones.
		if generatedSessionShape.MatchString(candidate) {
			return true
		}
		if r.looksLikeBeadID(candidate) {
			return true
		}
	}
	// The name lookups match against names the config actually declares, so a
	// wider set of spellings costs nothing: a spelling that is not a declared
	// name still does not resolve.
	for _, candidate := range assigneeCandidates(assignee) {
		if _, ok := r.exact[candidate]; ok {
			return true
		}
		if _, ok := r.poolStems[poolInstanceSuffix.ReplaceAllString(candidate, "")]; ok {
			return true
		}
	}
	return false
}

// looksLikeBeadID reports whether assignee is a session identity named after a
// bead this city could have issued. The prefix is checked against the ones the
// config declares rather than against a generic letter-run: a shape like
// `[a-z]{2,4}-[0-9a-z]{4,}` also matches an ordinary misspelling of an agent
// name ("poly-cat1" for "polecat-1"), which would wave through the exact class
// of typo this roster exists to catch.
//
// When the city declares no prefixes at all there is nothing to check against,
// so any prefix is accepted. That is the same cannot-answer posture as Empty().
func (r *assigneeRoster) looksLikeBeadID(assignee string) bool {
	idx := strings.Index(assignee, "-")
	if idx <= 0 || idx+1 >= len(assignee) {
		return false
	}
	if !beadIDSuffix.MatchString(assignee[idx+1:]) {
		return false
	}
	if len(r.idPrefixes) == 0 {
		return true
	}
	_, ok := r.idPrefixes[assignee[:idx]]
	return ok
}

// assigneeCandidates expands the spellings the same owner is written in across
// the fleet: bare, qualified with a rig or directory prefix, and absolute paths
// (the file store records `/home/ds/gas-city/goal-5-temporal` for what the bd
// store records as `goal-5-temporal`).
//
// Work is also assigned under the runtime session-name spelling, which encodes
// "/" as "--" and "." as "__" (internal/agent.SessionNameFor). That decode is
// fed only into these NAME lookups, never into the shape heuristics in
// Resolves (generatedSessionShape / looksLikeBeadID): those match on shape
// rather than on a configured name, so a decoded string carrying a
// bead-ID-shaped tail would pass as a session that never existed, the same
// reason the dot spelling is already excluded from them.
func assigneeCandidates(assignee string) []string {
	candidates := pathCandidates(assignee)
	if idx := strings.LastIndex(assignee, "."); idx >= 0 && idx+1 < len(assignee) {
		candidates = append(candidates, assignee[idx+1:])
	}
	if unsanitized := agent.UnsanitizeQualifiedNameFromSession(assignee); unsanitized != assignee {
		candidates = append(candidates, pathCandidates(unsanitized)...)
	}
	return candidates
}

// pathCandidates expands only the path spellings of an owner: the name as
// written, and the trailing segment of a rig-qualified or absolute-path form.
func pathCandidates(assignee string) []string {
	candidates := []string{assignee}
	if idx := strings.LastIndex(assignee, "/"); idx >= 0 && idx+1 < len(assignee) {
		candidates = append(candidates, assignee[idx+1:])
	}
	return candidates
}
