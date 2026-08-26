package main

import (
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// The pool orphan sweeper validates a claim it can SEE against liveness
// evidence it may not be able to see. A work bead in a SHARED rig store is
// visible to every city attached to that rig, but the session bead proving the
// claim is alive lives in the claiming city's own hq — unfederated. So when a
// foreign city's agent holds a claim, this city asks its own hq for that
// session, does not find it, and concludes the claim is dead. Both cities do
// it to each other: theirs cleared two of our P0s and a P1 in an 8-second
// batch; ours cleared fifteen of theirs in 34 seconds on 2026-08-21.
//
// The gate below separates "no session bead exists" from "no session bead is
// OBSERVABLE from here":
//
//	foreign / unknown  -> unobservable -> PROTECTED (skip, do not release)
//	locally configured -> observable   -> today's behaviour, UNCHANGED
//
// The unchanged-for-local property is what makes this shippable onto a box
// running live reviews, so every helper here is a pure read of local config.
//
// TWO THINGS THIS GATE DELIBERATELY DOES NOT DO.
//
// It does not test the RIG PREFIX. qcore is precisely the SHARED rig, so a
// prefix test protects nothing — the discriminator has to be the agent
// identity against the local roster.
//
// It does not test pool CAPACITY. "<local agent>-<slot>" is an instance
// identity of a local agent family whatever the current max_active_sessions
// says; measured against this city's own release history, live assignees run up
// to qcore/gc.run-operator-36 against a pool configured for 4. Gating on the
// slot ceiling would make every claim above the ceiling permanently unreapable
// the moment a pool is shrunk.
//
// KNOWN RESIDUAL (tracked separately): two cities importing the same agent pack
// share an identity space, so a foreign POOL instance ("qcore/gastown.furiosa")
// is byte-identical to ours and this gate cannot protect it. It protects
// foreign NAMED identities, which is the shape both observed incidents took.
// Closing the residual needs a claiming-city discriminator on the claim itself.

// poolAssigneeIsLocallyObservable reports whether assignee names an identity
// whose liveness this city is in a position to answer.
//
// Anything that is not a well-formed <rig>/<name> identity returns true: bare
// aliases, session bead IDs and runtime session names ("gastown__dog-ga-up143",
// "claude-mc-xyz") are this city's own naming and were never the cross-city
// hazard. Keeping them on the existing path is what preserves current
// behaviour for every local assignee shape.
func poolAssigneeIsLocallyObservable(cfg *config.City, cityName, assignee string) bool {
	assignee = strings.TrimSpace(assignee)
	if cfg == nil || assignee == "" {
		return true
	}
	rig, local := config.ParseQualifiedName(assignee)
	if strings.TrimSpace(rig) == "" || strings.TrimSpace(local) == "" {
		return true
	}
	return poolIdentityInLocalRoster(cfg, cityName, assignee)
}

// poolIdentityInLocalRoster resolves a <rig>/<name> identity against local
// config: configured named sessions, configured agent templates (including the
// legacy bound form that persisted assignees still carry), namepool-themed pool
// instances, and the instance identities gc mints for a local agent (numeric
// slots and adhoc tokens).
func poolIdentityInLocalRoster(cfg *config.City, cityName, identity string) bool {
	for _, candidate := range poolIdentityLocalCandidates(cfg, identity) {
		// A candidate still carrying a binding this city does not mint cannot
		// name a local agent, whatever the resolvers say (ga-8yi7ne). This is
		// the SECOND route into the collision and it does not go through the
		// stripped candidate above: poolIdentityIsInstanceOfLocalAgent reduces
		// "qcore/pool.omp-1" to base "qcore/pool.omp", which
		// findAgentByTemplate then matches against our unbound "qcore/omp" via
		// legacyBoundTemplateMatchesUnboundAgent — that helper accepts ANY
		// binding, by design, because it also serves bound->unbound MIGRATION
		// RECOVERY in cities where the binding was genuinely removed. So the
		// discriminator cannot live there without breaking that recovery; it
		// belongs here, where the question is "is this claim FOREIGN?" rather
		// than "what agent did this identity used to name?".
		//
		// Gating candidates rather than individual checks is deliberate: all
		// five resolvers below inherit it, so a sixth added later cannot
		// silently reopen the hole.
		if !poolCandidateBindingIsLocal(cfg, candidate) {
			continue
		}
		if _, ok := findNamedSessionSpec(cfg, cityName, candidate); ok {
			return true
		}
		if findAgentByTemplate(cfg, candidate) != nil {
			return true
		}
		if config.FindAgent(cfg, candidate) != nil {
			return true
		}
		if poolIdentityIsThemedInstance(cfg, candidate) {
			return true
		}
		if poolIdentityIsInstanceOfLocalAgent(cfg, candidate) {
			return true
		}
	}
	return false
}

// poolIdentityLocalCandidates returns the identity itself plus, when its local
// part carries a binding prefix THIS CITY MINTS, the binding-stripped form.
//
// Persisted assignees outlive the config that minted them: this city's own
// release history is full of "qcore/gastown.polecat" and
// "qcore/gc.run-operator-1" while the resolved config now holds those agents
// UNBOUND as "qcore/polecat" and "qcore/run-operator". findAgentByTemplate
// already carries that bound->unbound fallback for exact template lookups
// (legacyBoundTemplateMatchesUnboundAgent); the instance forms below need the
// same tolerance, so the stripping happens once, here, for every check.
// Without it this gate reads 45 of this city's 46 historically-reaped pool
// identities as foreign and protects them forever — a sweeper that has been
// disabled rather than fixed.
//
// THE BINDING GATE (ga-8yi7ne). Stripping ANY binding is what turned this gate
// from a protection into a cross-city hazard: another city's canonical
// "qcore/pool.omp-1" strips to "qcore/omp-1", which matches OUR configured
// "qcore/omp", so their live claim reads as our dead instance and is released.
// Measured in the field 2026-08-25 — the bead oscillated, released and
// re-claimed twice, and only their post-hoc quarantine rule settled it.
//
// The fix is to strip only bindings this city could itself have minted. A
// binding names an import, and the city's own agents record which import
// carried them (config.Agent.BindingName), so the set is exact and needs no
// heuristic. Measured on this city: {bd, cherub-law, core, gastown, gc,
// oversight} — which covers every historically-reaped local identity — while
// the neighbouring city's "pool" and "review" are absent. Their naming is
// linter-certified canonical on their side and cannot be changed, so the
// discriminator has to live here.
func poolIdentityLocalCandidates(cfg *config.City, identity string) []string {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return nil
	}
	candidates := []string{identity}
	rig, local := config.ParseQualifiedName(identity)
	if binding, unbound, ok := strings.Cut(local, "."); ok && binding != "" && unbound != "" {
		if cityMintsBinding(cfg, binding) {
			stripped := unbound
			if rig != "" {
				stripped = rig + "/" + unbound
			}
			candidates = append(candidates, stripped)
		}
	}
	return candidates
}

// cityMintsBinding reports whether binding names an import THIS city binds, and
// so whether this city could itself have minted a "<binding>.<agent>" identity.
//
// The DECLARED import tables are the primary source, deliberately ahead of
// anything inferred from the agents themselves. An import binding is a
// declaration; whether some agent currently carries it is a side effect, and a
// city whose only agent from an import has migrated bound->unbound would lose
// the binding entirely if this read agents alone. That would strand exactly the
// legacy identities the bound->unbound tolerance exists to keep resolvable.
//
//	cfg.Imports            city-level [imports.<binding>]
//	cfg.DefaultRigImports  [defaults.rig.imports], for imports whose agents are
//	                       not currently instantiated
//	rig.Imports            rig-level [rigs.<rig>.imports]; a rig-scoped import
//	                       mints "<rig>/<binding>.<agent>" just the same
//	Agent.BindingName      backstop for agents composed through a path none of
//	                       the tables above record
//
// An empty or unknown binding is NOT ours, and that refusal is the whole point.
// It fails in the safe direction: declining to resolve only ever PROTECTS a
// claim. Stranding this city's own stale work is repairable and, in the pool
// sweeper, reported in the per-sweep protected-identity summary; releasing a
// neighbouring city's live claim is neither.
func cityMintsBinding(cfg *config.City, binding string) bool {
	binding = strings.TrimSpace(binding)
	if cfg == nil || binding == "" {
		return false
	}
	if _, ok := cfg.Imports[binding]; ok {
		return true
	}
	if _, ok := cfg.DefaultRigImports[binding]; ok {
		return true
	}
	for i := range cfg.Rigs {
		if _, ok := cfg.Rigs[i].Imports[binding]; ok {
			return true
		}
	}
	for i := range cfg.Agents {
		if strings.TrimSpace(cfg.Agents[i].BindingName) == binding {
			return true
		}
	}
	return false
}

// poolCandidateBindingIsLocal reports whether a roster candidate is free of a
// FOREIGN binding prefix. A candidate with no binding at all is local as far as
// this predicate is concerned — bare names are this city's own naming and were
// never the cross-city hazard.
func poolCandidateBindingIsLocal(cfg *config.City, candidate string) bool {
	_, local := config.ParseQualifiedName(strings.TrimSpace(candidate))
	binding, unbound, ok := strings.Cut(local, ".")
	if !ok || strings.TrimSpace(binding) == "" || strings.TrimSpace(unbound) == "" {
		return true
	}
	return cityMintsBinding(cfg, binding)
}

// poolIdentityIsThemedInstance matches a namepool display name, e.g.
// "qcore/furiosa" against agent qcore/polecat whose namepool lists "furiosa".
//
// A generic rig-scoped template (Dir empty, Scope "rig") applies to every
// configured rig, so its instances are addressed under a concrete rig this
// template never names. config.FindAgent already synthesizes the rig-bound copy
// for exact lookups; the themed comparison has to do the same or a legitimate
// local "qcore/furiosa" reads as foreign.
func poolIdentityIsThemedInstance(cfg *config.City, identity string) bool {
	if cfg == nil {
		return false
	}
	rig, _ := config.ParseQualifiedName(identity)
	rigConfigured := rig != "" && cityHasRigNamed(cfg, rig)
	for i := range cfg.Agents {
		agentCfg := cfg.Agents[i]
		variants := []config.Agent{agentCfg}
		if rigConfigured && strings.TrimSpace(agentCfg.Dir) == "" && agentCfg.Scope == "rig" {
			rigBound := agentCfg
			rigBound.Dir = rig
			variants = append(variants, rigBound)
		}
		for _, themed := range agentCfg.NamepoolNames {
			if themed = strings.TrimSpace(themed); themed == "" {
				continue
			}
			for v := range variants {
				if variants[v].QualifiedInstanceName(themed) == identity {
					return true
				}
			}
		}
	}
	return false
}

func cityHasRigNamed(cfg *config.City, name string) bool {
	if cfg == nil {
		return false
	}
	for i := range cfg.Rigs {
		if cfg.Rigs[i].Name == name {
			return true
		}
	}
	return false
}

// poolIdentityIsInstanceOfLocalAgent matches an instance identity minted for a
// locally-configured agent: "<local agent>-<slot>" or
// "<local agent>-adhoc-<token>".
//
// Those are the two instance-name generators in the tree — poolInstanceName's
// numeric slot form and session.GenerateAdhocIdentity, which cmd_hook.go writes
// straight into the claim assignee for an aliasless pool worker
// ("rig/polecat-adhoc-<hash>"). Both are recognised WITHOUT consulting the
// pool's configured ceiling: see the capacity note at the top of this file.
//
// The grammar is closed on purpose rather than accepting any suffix, so this
// stays a statement about identities gc actually mints. A THIRD generator added
// later and not added here would have its claims protected — visibly, in the
// per-sweep summary, rather than silently — which is the failure direction this
// whole change is built around. Add new generators here.
func poolIdentityIsInstanceOfLocalAgent(cfg *config.City, identity string) bool {
	rig, local := config.ParseQualifiedName(identity)
	for _, base := range poolInstanceBaseNames(local) {
		baseIdentity := base
		if rig != "" {
			baseIdentity = rig + "/" + base
		}
		if findAgentByTemplate(cfg, baseIdentity) != nil {
			return true
		}
		if config.FindAgent(cfg, baseIdentity) != nil {
			return true
		}
	}
	return false
}

// poolInstanceBaseNames strips a recognised instance suffix off an identity's
// local part and returns the candidate agent names it could have been minted
// from.
func poolInstanceBaseNames(local string) []string {
	var bases []string
	if base, suffix, ok := cutLastDash(local); ok && isPoolSlotNumber(suffix) {
		bases = append(bases, base)
	}
	if idx := strings.LastIndex(local, "-adhoc-"); idx > 0 {
		if token := local[idx+len("-adhoc-"):]; token != "" {
			bases = append(bases, local[:idx])
		}
	}
	return bases
}

// isPoolSlotNumber accepts only the digits poolInstanceName emits. strconv.Atoi
// would also accept "+1" and "0007", which no generator produces; letting them
// through would make this predicate a claim about arbitrary strings rather than
// about minted identities.
func isPoolSlotNumber(s string) bool {
	if s == "" || s == "0" || s[0] == '0' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func cutLastDash(s string) (string, string, bool) {
	idx := strings.LastIndex(s, "-")
	if idx <= 0 || idx == len(s)-1 {
		return "", "", false
	}
	return s[:idx], s[idx+1:], true
}

// protectedForeignAssignees accumulates one sweep's protected claims so the
// skip is reported once per pass instead of once per candidate.
//
// The count is "candidates this gate skipped", not "beads proven still
// claimed": the gate returns before the owner-store lookup and the
// release-time revalidation, so a bead that a concurrent writer has since
// completed is still counted here. That is deliberate — establishing the
// stronger claim costs one store read per protected candidate every 30s, and
// over-reporting is the safe direction for a leak detector.
//
// This exists because the gate above is otherwise INVISIBLE, and a silent skip
// would be a fresh instance of the class it was written to fix. A local agent
// removed from config is also "not in the local roster", so its claims become
// protected too — correct as a default, but a permanent silent leak if nobody
// can see it. Fifty protected claims for an identity nobody recognises has to
// read as a decommissioned agent leaking, from the log alone, without knowing
// to look for it.
type protectedForeignAssignees struct {
	order      []string
	byIdentity map[string][]string
}

// protectedForeignAssigneeIDSampleLimit bounds the per-identity work-bead ID
// sample so one leaking identity cannot turn the summary into an unreadable
// line. The count is always exact; only the ID list is sampled.
const protectedForeignAssigneeIDSampleLimit = 5

func (p *protectedForeignAssignees) add(identity, workID string) {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return
	}
	if p.byIdentity == nil {
		p.byIdentity = make(map[string][]string, 4)
	}
	if _, seen := p.byIdentity[identity]; !seen {
		p.order = append(p.order, identity)
	}
	p.byIdentity[identity] = append(p.byIdentity[identity], strings.TrimSpace(workID))
}

func (p *protectedForeignAssignees) claims() int {
	total := 0
	for _, ids := range p.byIdentity {
		total += len(ids)
	}
	return total
}

// summary renders the once-per-sweep line, or "" when nothing was protected.
// Identities are sorted so consecutive sweeps produce comparable lines.
func (p *protectedForeignAssignees) summary() string {
	if len(p.byIdentity) == 0 {
		return ""
	}
	identities := make([]string, len(p.order))
	copy(identities, p.order)
	sort.Strings(identities)
	parts := make([]string, 0, len(identities))
	for _, identity := range identities {
		ids := p.byIdentity[identity]
		sample := ids
		suffix := ""
		if len(sample) > protectedForeignAssigneeIDSampleLimit {
			suffix = fmt.Sprintf(" +%d more", len(sample)-protectedForeignAssigneeIDSampleLimit)
			sample = sample[:protectedForeignAssigneeIDSampleLimit]
		}
		// %q on the identity: it is untrusted text read off a bead written by
		// another city, and an unquoted comma or newline would forge structure
		// in a line an operator reads as a leak report.
		parts = append(parts, fmt.Sprintf("%q (%d: %s%s)", identity, len(ids), strings.Join(sample, ", "), suffix))
	}
	return fmt.Sprintf("releaseOrphanedPoolAssignments: protected %d foreign/unknown identities this pass (%d claims): %s",
		len(identities), p.claims(), strings.Join(parts, ", "))
}

func (p *protectedForeignAssignees) log() {
	if summary := p.summary(); summary != "" {
		log.Print(summary)
	}
}
