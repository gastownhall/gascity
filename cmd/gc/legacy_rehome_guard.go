package main

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storeref"
)

// The legacy re-home repairs (canonicalizeLegacyBoundAssignedWork,
// canonicalizeLegacyBoundUnassignedRoutedWork) rewrite persisted identities
// that the permissive legacy resolver reads as old spellings of a local agent.
// That resolver consults only this city's config, and a rig bead store may be
// attached by several cities: city A's unbound "qcore/omp" reads city B's
// canonical "qcore/pool.omp" as a legacy bound form, and B's bound
// "qcore/pool.omp" reads A's canonical "qcore/omp" as a legacy unbound form.
// Unguarded, the two cities rewrite each other's rows every tick (#5846).
//
// The guard below is scoped by leg. On a leg only this city reads (the city
// work store, a relocated class binding) every legacy rewrite is still made.
// On a shared rig leg a rewrite is made only when local evidence says the row
// is this city's own:
//
//   - its config: the rewrite strips a binding this city mints on that rig
//     (legacyRehomeAllowed, through cityMintsBindingOnRig, the predicate the
//     orphan sweeper already trusts for the same question);
//   - its session records: this city's own session store shows it running
//     the legacy identity at or after the row's work began
//     (legacyRehomeProvenance): the row, or the molecule root it belongs to,
//     was created no later than the latest creation of a session record of
//     the identity, open or closed. That holds for this city's own
//     pre-migration rows after it migrated that agent itself (#3378 dropped a
//     binding, #5293 added one), and for the retries and ralph iterations of
//     its pre-migration molecules, which the dispatcher creates under the
//     legacy spelling frozen in the molecule's recipe however late it creates
//     them. It does not hold for a row and root created after that, which are
//     most likely a neighbor's, since this city's own new rows and molecules
//     carry its canonical identity. A record is dated by its creation, not by
//     a later write or its close, because the city keeps a pre-migration
//     session as a seat of the migrated agent, and that seat's later writes,
//     its close included, come from running the canonical identity. A
//     neighbor's session records live in its own session store and never
//     vouch here.
//
// Every other rewrite is refused and reported. A bare "dir/name" carries no
// mark of the city that wrote it, so adding a binding to one on a shared leg
// always needs the session-record evidence. On the config evidence alone a row
// changes at most once on a shared leg (only a strip is made), so no two
// cities' guards authorize opposite rewrites. The session-record evidence has
// two residuals. The first is the shared-identity residual the orphan sweeper
// documents too (foreignClaimGate): among the rows of work begun while both
// cities ran an identity, it cannot tell this city's from its neighbor's. A
// city whose record of its neighbor's current identity covers a row re-homes
// the neighbor's row under it, an in-flight one included, and with it every
// later row of the neighbor's molecules rooted in that window. The neighbor
// rewrites it back only when its own records cover the row under the new
// spelling too, which takes two cities that both migrated, in opposite
// directions, after the work began (a fleet-wide binding change one city later
// rolled back): such a row is rewritten back and forth while it stays open. The
// second is this city's own: a legacy row whose creation and root both come
// after the creation of its last session record of the identity stays
// refused, such as work queued while its last legacy session ran or after it
// closed, or while a seat it kept after the migration ran. The refusal summary
// and the v2-demand-migrations doctor check name such rows for a hand re-home.
//
// The pool-demand pass applies the config half of the same rule
// (legacyRehomeUnproven): a row assigned under a legacy spelling the config
// does not prove is this city's is not demand, so no canonical session is
// woken for work it could not claim.

// legacyRehomeFieldAssignee names the Assignee column in a legacyRehomeChange;
// the route is named by its metadata key, beadmeta.RoutedToMetadataKey.
const legacyRehomeFieldAssignee = "assignee"

// legacyRehomeChange is one identity rewrite on one field of a work row.
type legacyRehomeChange struct {
	Field string
	From  string
	To    string
}

// legacyRehomeRow is one work row a re-home pass rewrote or refused, on the
// leg (store ref) it was collected from.
type legacyRehomeRow struct {
	ID      string
	Leg     string
	Changes []legacyRehomeChange
}

// legacyRehomeReport is what one canonicalizeLegacyBound* pass did: the rows
// it rewrote and the rows the shared-leg guard refused.
type legacyRehomeReport struct {
	Rehomed []legacyRehomeRow
	Refused []legacyRehomeRow
	// LookupErr is the pass's first provenance lookup error, a session-record
	// listing or a molecule-root read, on the report of the arm that met it:
	// the rewrites it left unproven were refused for the pass.
	LookupErr error
}

// legacyRehomeChangeFor resolves a persisted identity through the permissive
// legacy resolver (findAgentByTemplate) and reports the rewrite to the
// resolved agent's canonical identity together with that agent, or a nil agent
// when there is nothing to rewrite: the identity is empty, resolves to no
// agent, or is already canonical.
func legacyRehomeChangeFor(cfg *config.City, field, from string) (legacyRehomeChange, *config.Agent) {
	from = strings.TrimSpace(from)
	if from == "" {
		return legacyRehomeChange{}, nil
	}
	agent := findAgentByTemplate(cfg, from)
	if agent == nil {
		return legacyRehomeChange{}, nil
	}
	to := agent.QualifiedName()
	if to == "" || to == from {
		return legacyRehomeChange{}, nil
	}
	return legacyRehomeChange{Field: field, From: from, To: to}, agent
}

// legacyRehomeLegIsOwn reports whether a work leg is read by this city alone:
// the city work store under either census vocabulary ("" or "city:<name>") or
// a relocated class binding ("class:<token>"), which serves only this city's
// classes and is never attached as another city's rig. Every other ref, a rig
// leg ("<rig>", "rig:<rig>") or a spelling this does not recognize, is treated
// as shared, so an unknown ref fails closed.
func legacyRehomeLegIsOwn(ref string) bool {
	return ref == "" || strings.HasPrefix(ref, "city:") || storeref.IsClassRef(ref)
}

// legacyRehomeAllowed is the config half of the guard: whether a legacy
// rewrite of from onto agent (the agent the permissive resolver returned,
// whose canonical identity differs from from) may be written on a leg on the
// config's evidence alone. An own leg allows every rewrite. On a shared leg
// the agent's binding gives the direction:
//
//   - strip (agent unbound, from "dir/<b>.name"): allowed only when this city
//     mints binding b on rig dir, b cut at the first "." exactly as
//     legacyBoundTemplateMatchesUnboundAgent cuts it; an empty b is refused.
//   - add (agent bound, from a bare identity): refused, since a bare identity
//     carries no mark of the city that wrote it.
//
// legacyRehomeGuard.permits adds the session-record half.
func legacyRehomeAllowed(cfg *config.City, own bool, from string, agent *config.Agent) bool {
	if own {
		return true
	}
	if agent == nil || strings.TrimSpace(agent.BindingName) != "" {
		return false
	}
	rig, local := config.ParseQualifiedName(strings.TrimSpace(from))
	binding, _, ok := strings.Cut(local, ".")
	if !ok {
		return false
	}
	return cityMintsBindingOnRig(cfg, rig, binding)
}

// legacyRehomeGuard is the shared-leg guard as one re-home pass applies it:
// the config's evidence, then this city's session records.
type legacyRehomeGuard struct {
	cfg        *config.City
	provenance *legacyRehomeProvenance
}

// permits reports whether the rewrite of from onto agent may be written on
// row, collected from store on leg: the config allows it (legacyRehomeAllowed,
// on the leg's legacyRehomeLegIsOwn), or this city's session records show it
// running from at or after the row's work began (legacyRehomeProvenance),
// which are looked up only for a rewrite the config refuses.
func (g legacyRehomeGuard) permits(leg string, store beads.StoreReader, row beads.Bead, from string, agent *config.Agent) bool {
	if legacyRehomeAllowed(g.cfg, legacyRehomeLegIsOwn(leg), from, agent) {
		return true
	}
	return agent != nil && g.provenance.covers(from, leg, store, row)
}

// legacyRehomeProvenance is the session-record half of the guard for one
// re-home pass: whether this city's own session store shows it running a
// legacy identity at or after a row's work began (session.TemplateTenureIn,
// TemplateTenure.Covers). That is positive local provenance: this city ran
// that identity's sessions once the work existed, so the row may be its own
// pre-migration work. A row's work began when the row was created or, for a
// row of a molecule, when the molecule's root was created (rootCreated): the
// dispatcher creates a molecule's retries and ralph iterations under the
// target frozen in its recipe, so a pre-migration molecule's new rows carry
// the legacy spelling. A row and root created after the city's last record of
// the identity was created are not taken as its own: the city's own new
// rows and molecules carry its canonical identity (sling stamps the resolved
// agent's; graph routing resolves the agent or fails). Only the store the
// pass leads with is read for records, the city's own session store that its
// pool sessions are created in, never a rig store, because a store other
// cities share is exactly where a neighbor's records can sit.
//
// Each identity is looked up at most once per pass, and only for a rewrite the
// config refuses on a shared leg, so a city whose rows are canonical reads
// nothing. A lookup error leaves the identity unproven for the pass (fail
// closed), and the report of the arm that met it carries it; a hard error also
// ends the pass's lookups, so an unreachable store costs one failed listing
// per pass. Each lookup is one Live, closed-inclusive listing, a scan of the
// store on bd/Dolt. A root is read at most once per pass, and only for a row
// whose own creation the city's dated records of the identity leave
// uncovered; a failed read dates the row by its own creation (fail closed)
// and is reported the same way. The memo lasts one pass, so while a
// neighbor's rows stand refused on a shared rig their identities, and the
// roots of its molecule rows, are looked up again every pass, as the orphan
// sweeper's identities are (foreignClaimGate, which also gives the ages at
// which closed session records are purged: provenance lasts as long as one
// survives).
//
// A nil provenance looks nothing up and proves nothing.
type legacyRehomeProvenance struct {
	sessions beads.StoreReader
	// tenures memoizes the lookup per identity for the pass.
	tenures map[string]sessionpkg.TemplateTenure
	// roots memoizes the creation time of each molecule root read for the
	// pass, zero for a root that is not in its row's store or failed to read.
	roots map[legacyRehomeRootKey]time.Time
	// err is the pass's first lookup error; reported is set once an arm's
	// report took it.
	err      error
	reported bool
	// stopped is set by a hard session-record lookup error: every identity
	// not yet looked up stays unproven for the rest of the pass.
	stopped bool
}

// legacyRehomeRootKey names a molecule root a pass reads: its ID on the leg of
// the row that names it (spelled by demandMigrationLeg, so both arms' spellings
// of one leg share a read), since two legs' stores may mint the same ID.
type legacyRehomeRootKey struct {
	leg string
	id  string
}

// newLegacyRehomeProvenance returns the session-record test of one re-home
// pass over sessions, the city's own session store.
func newLegacyRehomeProvenance(sessions beads.StoreReader) *legacyRehomeProvenance {
	return &legacyRehomeProvenance{sessions: sessions}
}

// tenure returns what this city's session store shows about when it ran
// identity (session.TemplateTenureIn); the zero tenure, which covers no row,
// when the pass's lookups have stopped or p is nil.
func (p *legacyRehomeProvenance) tenure(identity string) sessionpkg.TemplateTenure {
	if p == nil {
		return sessionpkg.TemplateTenure{}
	}
	identity = strings.TrimSpace(identity)
	if tenure, ok := p.tenures[identity]; ok {
		return tenure
	}
	if p.stopped {
		return sessionpkg.TemplateTenure{}
	}
	tenure, err := sessionpkg.TemplateTenureIn(p.sessions, identity)
	if err != nil {
		p.note(err)
		p.stopped = !beads.IsPartialResult(err)
	}
	if p.tenures == nil {
		p.tenures = make(map[string]sessionpkg.TemplateTenure)
	}
	p.tenures[identity] = tenure
	return tenure
}

// covers reports whether this city's session records show it running
// identity at or after row's work began: the row's creation, or, when the
// city holds a dated record of identity that leaves the row's creation
// uncovered, its molecule root's (rootCreated, read from store on leg).
func (p *legacyRehomeProvenance) covers(identity, leg string, store beads.StoreReader, row beads.Bead) bool {
	tenure := p.tenure(identity)
	if tenure.Covers(row.CreatedAt) {
		return true
	}
	if tenure.Until.IsZero() {
		return false
	}
	return tenure.Covers(p.rootCreated(leg, store, row))
}

// rootCreated returns when row's molecule root was created: the bead its
// gc.root_bead_id names (legacyRehomeRootID), read from store, the store the
// pass collected row from on leg, since a molecule's steps, retries and ralph
// iterations are created in its root's store. It is zero when row names no
// root to read, or the root is not in store or fails to read; a failed read
// is kept as the pass's lookup error. Each root is read at most once per pass.
func (p *legacyRehomeProvenance) rootCreated(leg string, store beads.StoreReader, row beads.Bead) time.Time {
	rootID := legacyRehomeRootID(row)
	if p == nil || store == nil || rootID == "" {
		return time.Time{}
	}
	key := legacyRehomeRootKey{leg: demandMigrationLeg(leg), id: rootID}
	if created, ok := p.roots[key]; ok {
		return created
	}
	var created time.Time
	root, err := store.Get(rootID)
	switch {
	case err == nil:
		created = root.CreatedAt
	case !errors.Is(err, beads.ErrNotFound):
		p.note(fmt.Errorf("reading molecule root %q of %s: %w", rootID, row.ID, err))
	}
	if p.roots == nil {
		p.roots = make(map[legacyRehomeRootKey]time.Time)
	}
	p.roots[key] = created
	return created
}

// legacyRehomeRootID returns the molecule root row names for dating, its
// gc.root_bead_id, or "" when that is empty, is row itself, or is shaped like
// no bead ID: whitespace, or a leading "-" that a CLI-backed store would read
// as a flag. The ID is untrusted text, read off a row another city may have
// written.
func legacyRehomeRootID(row beads.Bead) string {
	id := strings.TrimSpace(row.Metadata[beadmeta.RootBeadIDMetadataKey])
	if id == row.ID || strings.HasPrefix(id, "-") || strings.ContainsAny(id, " \t\r\n") {
		return ""
	}
	return id
}

// note keeps err as the pass's lookup error unless one is already kept.
func (p *legacyRehomeProvenance) note(err error) {
	if p.err == nil {
		p.err = err
	}
}

// takeLookupErr returns the pass's first lookup error the first time it is
// asked after one occurred, and nil otherwise, so the arms sharing a pass's
// provenance report it once.
func (p *legacyRehomeProvenance) takeLookupErr() error {
	if p == nil || p.err == nil || p.reported {
		return nil
	}
	p.reported = true
	return p.err
}

// legacyRehomeUnproven reports whether the pool-demand pass must leave out a
// row that leg ref holds assigned to identity: identity is a legacy spelling
// of a local pool agent that the config alone does not let this city re-home
// on that leg (legacyRehomeAllowed refuses it on a shared rig leg). Claims
// match the assignee by raw string, so the canonical session the
// wake-known-identity tier would wake for the row could not claim it. A
// re-home pass rewrites the row once this city's session records prove the
// row is its own (legacyRehomeProvenance), and the pass after that wakes for
// it; without that proof the row is most likely a neighbor's canonical work
// and is not this city's demand. It is pure config, as the pool-demand pass
// must be. Callers apply it only to a row no live session of this city answers
// to: such a row belongs to the resume tier, and the re-home passes leave it
// alone too.
func legacyRehomeUnproven(cfg *config.City, ref, identity string) bool {
	if legacyRehomeLegIsOwn(ref) {
		return false
	}
	change, agent := legacyRehomeChangeFor(cfg, legacyRehomeFieldAssignee, identity)
	if agent == nil || legacyRehomeAllowed(cfg, false, change.From, agent) {
		return false
	}
	return isKnownPoolTemplate(change.From, cfg)
}

// log writes the report to w under the repair's name: one line per rewritten
// row (each row is rewritten at most once, so this stays bounded), at most
// one summary of the refused rows, in the format of the orphan sweeper's
// protected-identity summary, and the provenance lookup error, if the report
// carries one. An empty report writes nothing.
func (r legacyRehomeReport) log(w io.Writer, repair string) {
	if w == nil {
		return
	}
	for _, row := range r.Rehomed {
		changes := make([]string, 0, len(row.Changes))
		for _, c := range row.Changes {
			changes = append(changes, fmt.Sprintf("%s %q -> %q", c.Field, c.From, c.To))
		}
		fmt.Fprintf(w, "%s: rehomed %s on %s: %s\n", repair, row.ID, demandMigrationLeg(row.Leg), strings.Join(changes, ", ")) //nolint:errcheck
	}
	if summary := r.refusalSummary(); summary != "" {
		fmt.Fprintf(w, "%s: %s\n", repair, summary) //nolint:errcheck
	}
	if r.LookupErr != nil {
		fmt.Fprintf(w, "%s: provenance lookup failed, so this pass refused the shared rig re-homes it could not prove: %v\n", repair, r.LookupErr) //nolint:errcheck
	}
}

// refusalSummary renders the refused rows grouped by rewrite (from -> to),
// sorted so consecutive passes produce comparable lines, with an exact row
// count and a bounded sample of IDs per rewrite; "" when nothing was refused.
// Identities are %q-quoted: they are untrusted text read off rows another city
// may have written, and an unquoted comma or newline would forge structure.
// A refused row stays as it is: it is most likely a neighboring city's
// canonical work, which must be left alone. A row an operator knows is this
// city's own is re-homed by hand
// (gc bd update <id> --assignee <to> --set-metadata gc.routed_to=<to>).
func (r legacyRehomeReport) refusalSummary() string {
	type rewrite struct{ from, to string }
	byRewrite := map[rewrite][]string{}
	for _, row := range r.Refused {
		for _, c := range row.Changes {
			key := rewrite{c.From, c.To}
			if ids := byRewrite[key]; len(ids) == 0 || ids[len(ids)-1] != row.ID {
				byRewrite[key] = append(ids, row.ID)
			}
		}
	}
	if len(byRewrite) == 0 {
		return ""
	}
	keys := make([]rewrite, 0, len(byRewrite))
	for key := range byRewrite {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].from != keys[j].from {
			return keys[i].from < keys[j].from
		}
		return keys[i].to < keys[j].to
	})
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		ids := byRewrite[key]
		sample, suffix := ids, ""
		if len(sample) > protectedForeignAssigneeIDSampleLimit {
			suffix = fmt.Sprintf(" +%d more", len(sample)-protectedForeignAssigneeIDSampleLimit)
			sample = sample[:protectedForeignAssigneeIDSampleLimit]
		}
		parts = append(parts, fmt.Sprintf("%q -> %q (%d: %s%s)", key.from, key.to, len(ids), strings.Join(sample, ", "), suffix))
	}
	return fmt.Sprintf("refused %d legacy re-home(s) on shared rig legs this pass (%d rows): %s",
		len(keys), len(r.Refused), strings.Join(parts, ", "))
}
