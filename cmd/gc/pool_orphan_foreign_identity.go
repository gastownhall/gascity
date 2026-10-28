package main

import (
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
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
// OBSERVABLE from here". An identity is observable when this city could have
// minted it (it resolves against the local roster) or when this city's own
// session store holds a session record, open or closed, that answers to it:
//
//	in the local roster           -> observable   -> today's behavior, UNCHANGED
//	held by a local session bead  -> observable   -> today's behavior, UNCHANGED
//	neither (foreign / unknown)   -> unobservable -> PROTECTED (skip, do not release)
//
// The unchanged-for-local property is what makes this shippable onto a box
// running live reviews. The roster test is a pure read of local config. The
// session-record test runs only for an identity that misses the roster, at
// most once per identity per sweep, as exact-metadata lookups in this city's
// session store. On SQLite and in memory their cost follows the records that
// answer to the identity, not the size of the session ledger; on bd/Dolt each
// lookup scans the store (see foreignClaimGate).
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
// the moment a pool is shrunk. Trimming a NAMEPOOL is the same move by name:
// "qcore/nux" leaves the roster the moment "nux" leaves namepool_names, and the
// seat's session record is what keeps that claim reapable.
//
// The gate is scoped to the pool dead-assignee sweep
// (releaseOrphanedPoolAssignments), the one Go lane that releases a claim
// under an identity read off the WORK bead. The killed-seat, drain-ack,
// close-cascade, stranded-repair and retired-session lanes release only under
// the departing seat's own identifiers, read off its session bead or derived
// from config for it. That is this city's own provenance, so they are exempt
// and not gated. Outside Go, the core pack's orphan-sweep.sh order makes the
// same release decision in shell and asks `gc agent is-foreign`, which applies
// this gate to every identity shape without the shape exemption, because the
// order has no route filter (claimIdentityProvenance, #5955).
//
// The orphan-close release (releaseConfirmedOrphanSessionWork) has no gate of
// its own, deliberately. It releases only under identifiers read off this
// city's own orphaned session bead or derived from its roster, so every claim
// it touches already carries the provenance this gate looks for. A roster test
// there would refuse exactly the seats that site exists for (an orphaned seat
// is one whose config is gone) and re-wedge them (ga-jrnou).
//
// KNOWN RESIDUAL (ga-dk24ao): two cities importing the same agent pack share an
// identity space, so a foreign POOL instance ("qcore/gastown.furiosa") is
// byte-identical to ours and this gate cannot protect it. It protects foreign
// NAMED identities, which is the shape both observed incidents took.
// Provenance widens the residual to any identity both cities have held, for as
// long as this city's record of holding it survives the closed-session purge:
// a <rig>/<name> identity moved to another city on the shared rig store keeps
// being released here until this city's closed session records for it are
// purged or re-aliased. Closing the residual needs a claiming-city
// discriminator on the claim itself.

// foreignClaimGate is the gate as one releaseOrphanedPoolAssignments pass uses
// it: the roster test, the session-record test behind it, and the summary of
// the claims they protected.
//
// The session-record test is POSITIVE LOCAL PROVENANCE. An identity that fails
// the roster is still this city's own when a session bead in this city's
// session store, open or closed, answers to it under its alias, its configured
// named identity, or its alias history (session.HasRecordAnsweringTo). That
// keeps the claims of a seat whose name left a namepool, or of a named session
// dropped from config, on the release path, and it does not reach a neighbor's
// claim: another city's session beads live in its own unfederated hq, not here
// (the shared-identity residual above aside). Only this city's session store
// is read, never a work bead's owner store, because a shared rig store is
// exactly where a neighbor's records can sit.
//
// Only the keys that can hold a <rig>/<name> identity are looked up: a session
// bead ID or session_name never contains "/". alias_history is stored
// comma-joined, so a prior alias is found only when it is the record's sole
// history entry. Once a seat has been renamed more than once, claims under its
// earlier aliases stay protected and named in the summary unless another
// record answers to them, which is the safe direction.
//
// COST. Each distinct identity that misses the roster is looked up at most once
// per pass, as up to three exact-metadata, closed-inclusive listings of the
// session store, one per key; a pass whose candidates all pass the roster reads
// nothing new. What a listing costs depends on the backend. On SQLite, which
// indexes the metadata filter, and in memory, it follows the records that
// answer to the identity, not the size of the session ledger. bd/Dolt filters
// metadata without an index, so there each listing scans the store: about 0.6s
// measured on an 11.3k-bead store, up to about 1.8s per identity off the
// roster per pass. A claim under an identity off the roster misses it on every
// pass, so while one is outstanding its lookups recur every sweep.
//
// The lookups are Live reads, like the per-assignee probe. A CachingStore reads
// closed history from the store under it either way, but a non-Live read that
// fails there comes back as a partial result over the cached open rows, which
// would not end the pass's lookups, and records a cache problem that postpones
// the cache's reconcile retry; recurring every pass, that can keep the cache
// from ever degrading. A Live read returns the failure as the hard error it is.
// A successful one refreshes the cache with the rows it returns, as any Live
// read does; open-bead reads still exclude the closed ones.
//
// A lookup error leaves its identity protected and is logged once per pass. A
// hard error also ends the pass's lookups, so an unreachable store costs one
// failed listing per pass, not one per identity; the rows a partial result
// returns still count.
//
// Provenance lasts as long as the store retains a closed session bead that
// answers to the identity. Closed session beads in a relocated SQLite sessions
// ledger are purged after GC_INFRA_SESSION_PURGE_AGE (default 72h,
// infraSessionPurgeAgeDefault), and only when wisp_ttl is set. In the city
// store the reaper order prunes them after GC_REAPER_SESSION_PURGE_AGE (default
// 720h), and only when GC_REAPER_SESSION_BEAD_PATTERN (default "gm-*") matches
// the city's session bead IDs or is set to "". Anywhere else they are never
// purged and provenance never expires.
type foreignClaimGate struct {
	cfg          *config.City
	cityName     string
	sessionStore beads.SessionStore
	// records memoizes the session-record test per identity for the pass.
	records map[string]bool
	// lookupErr is the pass's first lookup error, logged once by log.
	lookupErr error
	// lookupsStopped is set by a hard lookup failure: every identity not yet
	// looked up stays protected for the rest of the pass.
	lookupsStopped bool
	protected      protectedForeignAssignees
}

func newForeignClaimGate(cfg *config.City, sessionStore beads.SessionStore) *foreignClaimGate {
	return &foreignClaimGate{cfg: cfg, cityName: cfg.EffectiveCityName(), sessionStore: sessionStore}
}

// claimMayStillBeHeld reports whether the sweep must keep assignee's claim on
// workID: either this city cannot observe the identity, and the claim is
// recorded as protected, or a live open session answers to it.
// liveAssignee is the sweep's per-assignee memo for that live probe.
func (g *foreignClaimGate) claimMayStillBeHeld(assignee, workID string, liveAssignee map[string]bool) bool {
	if !g.observable(assignee) {
		g.protected.add(assignee, workID)
		return true
	}
	return memoizedLiveOpenSessionAssignmentExists(liveAssignee, assignee, g.sessionStore.Store, g.cfg, assignee)
}

func (g *foreignClaimGate) observable(assignee string) bool {
	if poolAssigneeIsLocallyObservable(g.cfg, g.cityName, assignee) {
		return true
	}
	return g.hasLocalRecord(strings.TrimSpace(assignee))
}

// hasLocalRecord is the session-record test, looked up at most once per
// identity per pass. A lookup error leaves the identity protected and is
// logged by log; a hard one also ends the pass's lookups.
func (g *foreignClaimGate) hasLocalRecord(identity string) bool {
	if found, ok := g.records[identity]; ok {
		return found
	}
	if g.lookupsStopped {
		return false
	}
	found, err := session.HasRecordAnsweringTo(g.sessionStore.Store, identity)
	if err != nil {
		if g.lookupErr == nil {
			g.lookupErr = err
		}
		g.lookupsStopped = !beads.IsPartialResult(err)
	}
	if g.records == nil {
		g.records = make(map[string]bool)
	}
	g.records[identity] = found
	return found
}

func (g *foreignClaimGate) log() {
	if g.lookupErr != nil {
		log.Printf("releaseOrphanedPoolAssignments: looking up session records for claim provenance: %v (identities outside the roster with no record found stay protected this pass)", g.lookupErr)
	}
	g.protected.log()
}

// poolAssigneeIsLocallyObservable reports whether local configuration alone
// puts this city in a position to answer for assignee's liveness: the roster
// half of the gate. A false here is not yet a protection; foreignClaimGate
// adds the session-record half, and it is the gate a release must consult.
//
// Anything that is not a well-formed <rig>/<name> identity returns true: bare
// aliases, session bead IDs and runtime session names ("gastown__dog-ga-up143",
// "claude-mc-xyz") are this city's own naming and were never the cross-city
// hazard. Keeping them on the existing path is what preserves current
// behavior for every local assignee shape.
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
// The fix is to strip only bindings this city could itself have minted on the
// identity's rig (cityMintsBindingOnRig). A binding names an import, and the
// city's own agents record which import carried them
// (config.Agent.BindingName), so the set is exact and needs no heuristic.
// Measured on this city: {bd, cherub-law, core, gastown, gc, oversight} —
// which covers every historically-reaped local identity — while the
// neighboring city's "pool" and "review" are absent. Their naming is
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
		if cityMintsBindingOnRig(cfg, rig, binding) {
			stripped := unbound
			if rig != "" {
				stripped = rig + "/" + unbound
			}
			candidates = append(candidates, stripped)
		}
	}
	return candidates
}

// cityMintsBindingOnRig reports whether binding names an import THIS city binds
// on rig, and so whether this city could itself have minted a
// "<rig>/<binding>.<agent>" identity there.
//
// The DECLARED import tables are the primary source, deliberately ahead of
// anything inferred from the agents themselves. An import binding is a
// declaration; whether some agent currently carries it is a side effect, and a
// city whose only agent from an import has migrated bound->unbound would lose
// the binding entirely if this read agents alone. That would strand exactly the
// legacy identities the bound->unbound tolerance exists to keep resolvable.
//
//	cfg.Imports        city-level [imports.<binding>], on every rig
//	rig.Imports        [rigs.<rig>.imports], on THAT rig only
//	Agent.BindingName  backstop for agents composed through a path neither
//	                   table records, on the rig the agent runs on
//	                   (agentRunsOnRig)
//
// [defaults.rig.imports] is not a source: gc copies it into a rig's own import
// table when the rig is added, so on its own it binds nothing on any rig that
// exists. Named sessions need no row either: import expansion stamps a named
// session's BindingName from the same city or rig import entry it stamps that
// import's agents from, so the tables already hold every binding one carries.
//
// The rig scoping is the gate, not bookkeeping. A binding this city imports on
// one rig says nothing about the shared rig next to it: importing "pool" on
// "alpha" must not let a neighbor's canonical "qcore/pool.omp-1" strip to
// "qcore/omp-1" and resolve onto our unbound "qcore/omp" — the ga-8yi7ne
// release, reopened one rig over.
//
// An empty or unknown binding is NOT ours, and that refusal is the whole point.
// It fails in the safe direction: declining to resolve only ever PROTECTS a
// claim. Stranding this city's own stale work is repairable and, in the pool
// sweeper, reported in the per-sweep protected-identity summary; releasing a
// neighboring city's live claim is neither.
func cityMintsBindingOnRig(cfg *config.City, rig, binding string) bool {
	binding = strings.TrimSpace(binding)
	if cfg == nil || binding == "" {
		return false
	}
	if _, ok := cfg.Imports[binding]; ok {
		return true
	}
	for i := range cfg.Rigs {
		if _, ok := cfg.Rigs[i].Imports[binding]; ok && cfg.Rigs[i].Name == rig {
			return true
		}
	}
	for i := range cfg.Agents {
		if strings.TrimSpace(cfg.Agents[i].BindingName) == binding && agentRunsOnRig(cfg, &cfg.Agents[i], rig) {
			return true
		}
	}
	return false
}

// agentRunsOnRig reports whether a configured agent answers to identities under
// rig: the rig its Dir names, or, for a generic rig-scoped template (Dir empty,
// Scope "rig"), every configured rig — the copy config.FindAgent synthesizes.
//
// A Dir-less agent of any other scope runs on no rig. A city-scoped agent
// hoisted out of a rig's import keeps that rig's binding but answers only to
// "<binding>.<name>", so counting it on every rig would reopen ga-8yi7ne for
// a binding this city imports on one rig.
func agentRunsOnRig(cfg *config.City, agentCfg *config.Agent, rig string) bool {
	if dir := strings.TrimSpace(agentCfg.Dir); dir != "" {
		return dir == rig
	}
	return agentCfg.Scope == "rig" && rigConfigHasRigNamed(cfg, rig)
}

// poolCandidateBindingIsLocal reports whether a roster candidate is free of a
// FOREIGN binding prefix: one this city does not mint on the candidate's own
// rig. A candidate with no binding at all is local as far as this predicate is
// concerned — bare names are this city's own naming and were never the
// cross-city hazard.
func poolCandidateBindingIsLocal(cfg *config.City, candidate string) bool {
	rig, local := config.ParseQualifiedName(strings.TrimSpace(candidate))
	binding, unbound, ok := strings.Cut(local, ".")
	if !ok || strings.TrimSpace(binding) == "" || strings.TrimSpace(unbound) == "" {
		return true
	}
	return cityMintsBindingOnRig(cfg, rig, binding)
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
	rigConfigured := rig != "" && rigConfigHasRigNamed(cfg, rig)
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

// poolIdentityIsInstanceOfLocalAgent matches an instance identity minted for a
// locally-configured agent: "<local agent>-<slot>" or
// "<local agent>-adhoc-<token>".
//
// Those are the two instance-name shapes in the tree. The numeric slot form is
// poolInstanceName's. The adhoc suffix is the one session.GenerateAdhocExplicitName
// appends (sessionExplicitNameForNewSession in cmd_session.go), but today's
// generators never emit it qualified: that one builds on the bare agent name
// and session.GenerateAdhocIdentity sanitizes "/" to "--", so a freshly minted
// adhoc name reaches the gate through the bare-name arm of
// poolAssigneeIsLocallyObservable, unchanged. The QUALIFIED
// "rig/polecat-adhoc-<hash>" spelling this arm recognizes is a persisted legacy
// assignee shape, the per-instance GC_AGENT / GC_ALIAS form the
// resolveAgentIdentity retry in cmd_hook.go still documents. Both shapes are
// recognized WITHOUT consulting the pool's configured ceiling: see the capacity
// note at the top of this file.
//
// The grammar is closed on purpose rather than accepting any suffix, so this
// stays a statement about identities gc actually mints. A THIRD shape added
// later and not added here would have its claims protected — visibly, in the
// per-sweep summary, rather than silently — which is the failure direction this
// whole change is built around. Add new instance shapes here.
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

// poolInstanceBaseNames strips a recognized instance suffix off an identity's
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
//
// Deliberately STRICTER than routeHasSlotSuffixShape
// (demand_serve_predicate.go), which accepts any digit run ("007") because it
// is a fail-open pre-filter on ROUTE shape. This one is a fail-closed claim
// about minted IDENTITIES, and on "polecat-007" the two disagree by design:
// here the unminted shape is PROTECTED AND NAMED in the sweep summary rather
// than silently stripped to a base name — the safe direction for a suffix no
// generator produces. Unifying them would force one predicate to answer the
// other's question.
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
// removed from config is also "not in the local roster". Provenance keeps its
// claims on the release path while one of its session beads still answers to
// it (see foreignClaimGate), but once the closed-session purge has taken the
// last one its claims become protected too — correct as a default, but a
// permanent silent leak if nobody can see it. Fifty protected claims for an
// identity nobody recognizes has to read as a decommissioned agent leaking,
// from the log alone, without knowing to look for it.
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
