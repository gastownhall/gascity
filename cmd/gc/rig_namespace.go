package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sling"
)

// rigNamespace decides, for one rig with federation = "rig", which rows of the
// rig's bead store this city may act on and which identities it may act as.
// Several cities can attach the same store; identity strings are unique only
// inside one city, so a row is this city's only when its identities are inside
// the rig's namespace ("<rig>/" under the bead store's actor fold) or name one
// of this city's own pool sessions of the rig's agents.
//
// A rigNamespace is pure decision logic over a config snapshot and the city's
// session resolver: it never reads the rig's store. It is safe for concurrent
// use when its resolver is. Every answer fails closed: a nil rigNamespace, a
// rig without a usable name, and a session lookup that fails all admit
// nothing, and a row they cannot vouch for is Foreign.
type rigNamespace struct {
	rig config.Rig
	ns  string
	// sessions resolves this city's own session records. It belongs to one
	// city and is never shared with another, so provenance can only admit
	// that city's ids. A nil resolver admits no session id.
	sessions ownSessionResolver
}

// rowClass is the ownership class of a row on a guarded rig.
type rowClass uint8

const (
	// rowForeign: the row belongs to something outside this city's share of
	// the rig. The zero value, so an unset class fails closed.
	rowForeign rowClass = iota
	// rowOwned: every owner identity on the row is admitted, or the row's
	// claim or creation stamp is this city's.
	rowOwned
	// rowUnattributed: the row carries no owner key, claim stamp or creation
	// stamp (a writer other than gc made it). Callers keep today's behavior.
	rowUnattributed
)

// String returns the class's lower-case name.
func (c rowClass) String() string {
	switch c {
	case rowOwned:
		return "owned"
	case rowUnattributed:
		return "unattributed"
	default:
		return "foreign"
	}
}

// rowRule names the classification rule that decided a row's class.
type rowRule uint8

const (
	// rowRuleNone: no key was present, or the namespace was nil.
	rowRuleNone rowRule = iota
	// rowRuleOwnerKeys: K_P (assignee, routes, deferred and stranded owners,
	// and a session row's template and alias) decided.
	rowRuleOwnerKeys
	// rowRuleClaimStamps: K_P was empty and K_S (session claim stamps)
	// decided.
	rowRuleClaimStamps
	// rowRuleCreationStamp: K_P and K_S were empty and the creation stamp
	// (gc.owner_namespace) decided.
	rowRuleCreationStamp
)

// String returns the rule's name as messages and traces print it.
func (r rowRule) String() string {
	switch r {
	case rowRuleOwnerKeys:
		return "owner_keys"
	case rowRuleClaimStamps:
		return "claim_stamps"
	case rowRuleCreationStamp:
		return "creation_stamp"
	default:
		return "none"
	}
}

// rowClassification is classifyRow's verdict with the evidence behind it, for
// refusal messages ("its owner %q is outside it") and traces.
type rowClassification struct {
	Class rowClass
	Rule  rowRule
	// Key and Value name the deciding entry: for a Foreign row under the owner
	// keys, the first owner value that is not admitted; for an Owned row under
	// the claim stamps, the admitted stamp, and for a Foreign one, the first
	// stamp present (none is admitted); for the creation stamp, the stamp.
	// Both are empty for an Unattributed row and for an Owned row under the
	// owner keys.
	Key   string
	Value string
	// Err is a session lookup failure that kept a value from being admitted.
	// The verdict already accounts for it (fail closed: Foreign for this
	// pass); it is reported so callers can log it rather than act on it.
	Err error
}

// admitVia names the arm of admission that admitted an identity.
type admitVia uint8

const (
	// admitNone: the identity is not admitted.
	admitNone admitVia = iota
	// admitLexical (L): the identity's fold is inside the rig's namespace.
	admitLexical
	// admitSessionRecord (P): one of this city's own session records of
	// the rig's agents answers to the identity: a pool session by its bead
	// id or recorded session name, or (for name-holding claim stamps only)
	// any session by its recorded session name.
	admitSessionRecord
)

// String returns the arm's name as messages and JSON print it.
func (v admitVia) String() string {
	switch v {
	case admitLexical:
		return "lexical"
	case admitSessionRecord:
		return "session_record"
	default:
		return "none"
	}
}

// rigAdmission is the full answer of rigNamespace.admission.
type rigAdmission struct {
	Via admitVia
	// Record is the session record that admitted the identity under
	// admitSessionRecord.
	Record ownSessionRecord
	// Err is a session lookup failure. The identity is then not admitted
	// (fail closed); a later pass may admit it.
	Err error
}

// Admitted reports whether the identity is admitted.
func (a rigAdmission) Admitted() bool { return a.Via != admitNone }

// Owner keys (K_P): every non-empty value must be admitted for the row to be
// this city's. The assignee is the bead field, not a metadata key, and is
// checked first.
var rigNamespaceOwnerMetadataKeys = []string{
	beadmeta.RoutedToMetadataKey,
	beadmeta.ExecutionRoutedToMetadataKey,
	beadmeta.DeferredAssigneeMetadataKey,
	beadmeta.DeferredRoutedToMetadataKey,
	beadmeta.DeferredExecutionRoutedToMetadataKey,
	beadmeta.StrandedAssigneeMetadataKey,
}

// Session rows also name their owner in their template and alias (bare
// session-record keys, not gc.* metadata).
var rigNamespaceSessionRowOwnerKeys = []string{rowSessionTemplateKey, "alias"}

// Claim stamps (K_S): judged only when no owner key is set. One admitted
// stamp makes the row this city's. The name-holding stamps
// (rigNamespaceNameStampKeys) need a name inside the namespace and a record
// of it in this city's sessions.
var rigNamespaceClaimStampKeys = []string{
	beadmeta.SessionIDMetadataKey,
	beadmeta.SessionIDCamelMetadataKey,
	beadmeta.SessionNameMetadataKey,
	beadmeta.SessionNameCamelMetadataKey,
	beadmeta.StrandedSessionMetadataKey,
}

// rigNamespaceNameStampKeys are the claim stamps that hold a session name
// (gc.stranded_session holds the closed session's id, or a name when that is
// what a writer stamped). A runtime session name is derived from a template
// and may fold into any namespace, or lie outside the rig's namespace for
// one of its own agents, so these are admitted only when the name is inside
// the namespace and a record in this city's sessions store carries it and
// belongs to the rig.
var rigNamespaceNameStampKeys = []string{
	beadmeta.SessionNameMetadataKey,
	beadmeta.SessionNameCamelMetadataKey,
	beadmeta.StrandedSessionMetadataKey,
}

func isRigNamespaceNameStampKey(key string) bool {
	for _, k := range rigNamespaceNameStampKeys {
		if key == k {
			return true
		}
	}
	return false
}

// rigNamespaceRouteKeys are the routing keys, a subset of the owner keys. A
// guarded store accepts only a rig-qualified value in them
// (rigRouteQualified), because a bare route in a shared store is served by
// every city that defines it.
var rigNamespaceRouteKeys = []string{
	beadmeta.RoutedToMetadataKey,
	beadmeta.ExecutionRoutedToMetadataKey,
	beadmeta.DeferredRoutedToMetadataKey,
	beadmeta.DeferredExecutionRoutedToMetadataKey,
}

// rigNamespaceAssigneeMetadataKeys are the metadata keys that hold an
// assignee rather than a route.
var rigNamespaceAssigneeMetadataKeys = []string{
	beadmeta.DeferredAssigneeMetadataKey,
	beadmeta.StrandedAssigneeMetadataKey,
}

// rigNamespaceOwnerStampKey is the creation stamp (K_O). The guarded store
// writes it, set to the rig's namespace, on every row it creates. Only the
// guard may write it: a store that let a caller set or change it would let
// one city mark a row as another's.
const rigNamespaceOwnerStampKey = beadmeta.OwnerNamespaceMetadataKey

// isRigNamespaceIdentityPredicateKey reports whether a metadata equality on
// key selects rows by identity, so a guarded read must admit the value
// before reading: the owner keys and gc.run_target (served by name on the
// legacy workflow tier, though it is not an owner key).
func isRigNamespaceIdentityPredicateKey(key string) bool {
	key = strings.TrimSpace(key)
	if key == beadmeta.RunTargetMetadataKey {
		return true
	}
	for _, k := range rigNamespaceOwnerMetadataKeys {
		if key == k {
			return true
		}
	}
	return false
}

// isRigNamespaceRouteKey reports whether key is one of the routing keys.
func isRigNamespaceRouteKey(key string) bool {
	key = strings.TrimSpace(key)
	for _, k := range rigNamespaceRouteKeys {
		if key == k {
			return true
		}
	}
	return false
}

// rigRouteQualified reports whether a routing value names a rig-scoped
// identity: its fold contains "/" and no segment of it is empty ("/x",
// "--x", "x/" and "a//b" are not qualified). A bare value is city-scoped.
// Every comparison here uses config.FoldIdentity, the normalization config
// validation uses.
func rigRouteQualified(v string) bool {
	f := config.FoldIdentity(v)
	if !strings.Contains(f, "/") {
		return false
	}
	for _, seg := range strings.Split(f, "/") {
		if seg == "" {
			return false
		}
	}
	return true
}

// newRigNamespace returns the namespace of rig, or (nil, false) when the rig
// is not guarded. A guarded rig without a usable name still returns a
// namespace; it admits nothing.
func newRigNamespace(rig config.Rig, sessions ownSessionResolver) (*rigNamespace, bool) {
	if !rig.FederationGuarded() {
		return nil, false
	}
	return &rigNamespace{rig: rig, ns: rig.IdentityNamespace(), sessions: sessions}, true
}

// denyAllRigNamespace is the namespace of an ambiguous guarded scope (two
// guarded rigs answer to one store directory or prefix): it admits nothing.
func denyAllRigNamespace(sessions ownSessionResolver) *rigNamespace {
	return &rigNamespace{sessions: sessions}
}

// rigNamespaceForRigName looks up the configured rig called name (exact,
// case-sensitive match). known reports whether such a rig exists; guarded
// whether it is guarded, in which case ns is its namespace. Callers must
// treat an unknown rig as an error, never as unguarded.
func rigNamespaceForRigName(cfg *config.City, name string, sessions ownSessionResolver) (ns *rigNamespace, guarded, known bool) {
	if cfg == nil || name == "" {
		return nil, false, false
	}
	match := func(r *config.Rig) bool { return r.Name == name }
	for i := range cfg.Rigs {
		if match(&cfg.Rigs[i]) {
			known = true
			break
		}
	}
	if !known {
		return nil, false, false
	}
	ns, guarded = pickGuardedRig(cfg.Rigs, sessions, match)
	return ns, guarded, true
}

// rigNamespaceForBeadID returns the namespace of the guarded rig whose bead
// prefix beadID carries (resolved the way sling resolves a bead's rig,
// prefixes compared case-insensitively), or (nil, false) when the prefix maps
// to no guarded rig.
func rigNamespaceForBeadID(cfg *config.City, beadID string, sessions ownSessionResolver) (*rigNamespace, bool) {
	if cfg == nil {
		return nil, false
	}
	prefix := sling.BeadPrefixForCity(cfg, strings.TrimSpace(beadID))
	if prefix == "" {
		return nil, false
	}
	return pickGuardedRig(cfg.Rigs, sessions, func(r *config.Rig) bool {
		return strings.EqualFold(r.EffectivePrefix(), prefix)
	})
}

func pickGuardedRig(rigs []config.Rig, sessions ownSessionResolver, match func(*config.Rig) bool) (*rigNamespace, bool) {
	var found *config.Rig
	for i := range rigs {
		r := &rigs[i]
		if !r.FederationGuarded() || !match(r) {
			continue
		}
		if found != nil && found.Name != r.Name {
			return denyAllRigNamespace(sessions), true
		}
		found = r
	}
	if found == nil {
		return nil, false
	}
	return newRigNamespace(*found, sessions)
}

// rigName returns the guarded rig's name ("" for a deny-all namespace).
func (n *rigNamespace) rigName() string {
	if n == nil {
		return ""
	}
	return n.rig.Name
}

// namespace returns the rig's identity namespace ("<rig>/" folded), or ""
// when it admits nothing lexically.
func (n *rigNamespace) namespace() string {
	if n == nil {
		return ""
	}
	return n.ns
}

// ownerStamp returns the value of the creation stamp
// (rigNamespaceOwnerStampKey) the guarded store writes on every row it
// creates: the namespace. ok is false when the namespace is unusable, and
// then the store must refuse the create.
func (n *rigNamespace) ownerStamp() (value string, ok bool) {
	if n == nil || n.ns == "" {
		return "", false
	}
	return n.ns, true
}

// admitsLexically is the lexical arm (L): config's Rig.AdmitsLexically (v's
// fold is inside the namespace and is not the bare namespace), except for a
// value that embeds a pool session bead id. A pool session's runtime name is
// its template's last segment plus its id, so another city's agent
// "alpha/beta-" runs sessions named "beta--<id>", which fold into "beta/";
// such values are admitted only through this city's own session records
// (admission's P arm).
//
// Inside the namespace, a spelling is admitted whatever wrote it. Config
// validation refuses the city's own spellings that would land there from
// outside the rig: another agent's qualified name, its first pool slot
// ("<name>-1") and the session name its base name yields, its namepool
// instance names, and named sessions' qualified names. It does not see names
// rendered at runtime from a session template or tmux alias, nor other
// cities' agents.
func (n *rigNamespace) admitsLexically(v string) bool {
	if n == nil || n.ns == "" {
		return false
	}
	return n.rig.AdmitsLexically(v) && !embedsPoolSessionID(v)
}

// templateInRig reports whether a session template is one of this rig's own
// agents. A rig agent's qualified name is "<rig name>/<agent>" verbatim
// (validation refuses a guarded rig's agents attached by path), so the
// template must carry that literal prefix as well as fold inside the
// namespace. That keeps a city-scoped agent whose name merely folds into the
// namespace ("beta--crew" reads as "beta/crew") from vouching for its
// sessions here, even if validation were bypassed.
func (n *rigNamespace) templateInRig(template string) bool {
	if n == nil || n.ns == "" || n.rig.Name == "" {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(template), n.rig.Name+"/") && n.rig.AdmitsLexically(template)
}

// admits reports whether v may own a row or act on one in the rig's store:
// lexically (L) or as one of this city's pool sessions of the rig (P).
func (n *rigNamespace) admits(v string) bool {
	return n.admission(v).Admitted()
}

// admission decides v under L, then P. P admits v only when this city's
// session resolver returns a record that answers to v (its bead id or
// recorded session name), whose id has the unguessable pool shape, and whose
// template is one of the rig's agents (templateInRig). A short, store-minted
// session id never reaches the resolver. A resolver failure leaves v
// unadmitted, with the error reported.
func (n *rigNamespace) admission(v string) rigAdmission {
	if n == nil || n.ns == "" || config.FoldIdentity(v) == "" {
		return rigAdmission{}
	}
	if n.admitsLexically(v) {
		return rigAdmission{Via: admitLexical}
	}
	// Lookups key on the exact (trimmed) spelling: a pool session's id and
	// name are what the city stamped, so another spelling of them is never
	// looked up and stays unadmitted.
	key := strings.TrimSpace(v)
	if n.sessions == nil || !embedsPoolSessionID(key) {
		return rigAdmission{}
	}
	rec, ok, err := n.sessions.lookupOwnSession(key)
	if err != nil {
		return rigAdmission{Err: err}
	}
	if !ok || !n.poolRecordAdmits(rec, key) {
		return rigAdmission{}
	}
	return rigAdmission{Via: admitSessionRecord, Record: rec}
}

// nameStampAdmission decides a name-holding claim stamp. A pool session name
// goes through admission's P arm. Any other name is admitted only when it is
// inside the namespace (the lexical arm) and a record in this city's
// sessions carries it and belongs to one of the rig's agents.
func (n *rigNamespace) nameStampAdmission(v string) rigAdmission {
	if n == nil || n.ns == "" || config.FoldIdentity(v) == "" || n.sessions == nil {
		return rigAdmission{}
	}
	key := strings.TrimSpace(v)
	if embedsPoolSessionID(key) {
		return n.admission(key)
	}
	// The name must be inside the namespace as well as recorded: a rig
	// agent's session can run under a name outside it (a session template
	// of "{{.Name}}", or a tmux alias), and another city importing the same
	// pack runs its own sessions under that same name.
	if !n.admitsLexically(key) {
		return rigAdmission{}
	}
	admits := func(rec ownSessionRecord) bool {
		return ownSessionNameIs(rec, key) && n.templateInRig(rec.Template)
	}
	recs, err := n.sessions.lookupOwnSessionsByName(key, admits)
	if err != nil {
		return rigAdmission{Err: err}
	}
	for _, rec := range recs {
		if admits(rec) {
			return rigAdmission{Via: admitSessionRecord, Record: rec}
		}
	}
	return rigAdmission{}
}

// poolRecordAdmits re-checks a resolver answer rather than trusting it: the
// record must carry an unguessable pool session id, answer to v, and belong
// to an agent of this rig.
func (n *rigNamespace) poolRecordAdmits(rec ownSessionRecord, v string) bool {
	if !isPoolSessionBeadID(strings.TrimSpace(rec.ID)) {
		return false
	}
	return ownSessionAnswersTo(rec, v) && n.templateInRig(rec.Template)
}

// ownSessionAnswersTo reports whether v names the record: its fold equals the
// fold of the record's bead id or of its recorded session name.
func ownSessionAnswersTo(rec ownSessionRecord, v string) bool {
	fv := config.FoldIdentity(v)
	if fv == "" {
		return false
	}
	return fv == config.FoldIdentity(rec.ID) || ownSessionNameIs(rec, v)
}

// ownSessionNameIs reports whether v is the record's recorded session name
// under the fold.
func ownSessionNameIs(rec ownSessionRecord, v string) bool {
	name := config.FoldIdentity(rec.SessionName)
	return name != "" && config.FoldIdentity(v) == name
}

// classify returns the row's class on the guarded rig.
func (n *rigNamespace) classify(b beads.Bead) rowClass {
	return n.classifyRow(b).Class
}

// classifyRow classifies b:
//
//  1. Any owner key (K_P) set: Foreign iff some non-empty owner value is not
//     admitted, else Owned.
//  2. Otherwise, any claim stamp (K_S) set: Owned iff some stamp is
//     admitted, else Foreign.
//  3. Otherwise, a creation stamp (K_O) set: Owned iff its fold equals the
//     namespace, else Foreign.
//  4. Otherwise Unattributed.
//
// gc.run_target and gc.session_affinity are not keys. A nil, nameless or
// deny-all namespace classifies every row Foreign.
func (n *rigNamespace) classifyRow(b beads.Bead) rowClassification {
	if n == nil || n.ns == "" {
		return rowClassification{Class: rowForeign}
	}
	owners := rowOwnerValues(b)
	if len(owners) > 0 {
		return n.classifyByOwners(owners)
	}
	if stamps := rowClaimStampValues(b); len(stamps) > 0 {
		return n.classifyByStamps(stamps)
	}
	if v := b.Metadata[rigNamespaceOwnerStampKey]; config.FoldIdentity(v) != "" {
		c := rowClassification{Class: rowForeign, Rule: rowRuleCreationStamp, Key: rigNamespaceOwnerStampKey, Value: v}
		if n.ns != "" && config.FoldIdentity(v) == n.ns {
			c.Class = rowOwned
		}
		return c
	}
	return rowClassification{Class: rowUnattributed}
}

// keyValue is one non-empty identity entry on a row.
type keyValue struct {
	Key   string
	Value string
}

// rowAssigneeKey is the Key a rowClassification reports for the bead's
// assignee field.
const rowAssigneeKey = "assignee"

func rowOwnerValues(b beads.Bead) []keyValue {
	var out []keyValue
	add := func(k, v string) {
		if config.FoldIdentity(v) != "" {
			out = append(out, keyValue{Key: k, Value: v})
		}
	}
	add(rowAssigneeKey, b.Assignee)
	for _, k := range rigNamespaceOwnerMetadataKeys {
		add(k, b.Metadata[k])
	}
	if sessionpkg.IsSessionBeadOrRepairable(b) {
		for _, k := range rigNamespaceSessionRowOwnerKeys {
			add(k, b.Metadata[k])
		}
	}
	return out
}

func rowClaimStampValues(b beads.Bead) []keyValue {
	var out []keyValue
	for _, k := range rigNamespaceClaimStampKeys {
		if v := b.Metadata[k]; config.FoldIdentity(v) != "" {
			out = append(out, keyValue{Key: k, Value: v})
		}
	}
	return out
}

// rowSessionTemplateKey is the owner key a session row names its template
// under. A template is admitted only when it is one of the rig's agents
// verbatim (templateInRig), not merely by its fold.
const rowSessionTemplateKey = "template"

// ownerAdmission decides one owner value: a session row's template by
// templateInRig, anything else by admission (L or P).
func (n *rigNamespace) ownerAdmission(kv keyValue) rigAdmission {
	if kv.Key == rowSessionTemplateKey {
		if n.templateInRig(kv.Value) {
			return rigAdmission{Via: admitLexical}
		}
		return rigAdmission{}
	}
	return n.admission(kv.Value)
}

func (n *rigNamespace) classifyByOwners(owners []keyValue) rowClassification {
	// Values that need no session lookup decide first: one that is not
	// lexically admitted and embeds no pool session id can never be
	// admitted, so the row is Foreign before any lookup runs.
	for _, kv := range owners {
		if kv.Key == rowSessionTemplateKey || embedsPoolSessionID(kv.Value) {
			continue
		}
		if !n.admitsLexically(kv.Value) {
			return rowClassification{Class: rowForeign, Rule: rowRuleOwnerKeys, Key: kv.Key, Value: kv.Value}
		}
	}
	for _, kv := range owners {
		if a := n.ownerAdmission(kv); !a.Admitted() {
			return rowClassification{Class: rowForeign, Rule: rowRuleOwnerKeys, Key: kv.Key, Value: kv.Value, Err: a.Err}
		}
	}
	return rowClassification{Class: rowOwned, Rule: rowRuleOwnerKeys}
}

// stampAdmission decides one claim stamp: a name-holding stamp only through
// this city's session records, a session id stamp by admission (L or P).
func (n *rigNamespace) stampAdmission(kv keyValue) rigAdmission {
	if isRigNamespaceNameStampKey(kv.Key) {
		return n.nameStampAdmission(kv.Value)
	}
	return n.admission(kv.Value)
}

func (n *rigNamespace) classifyByStamps(stamps []keyValue) rowClassification {
	// A lexically admitted id stamp needs no lookup.
	for _, kv := range stamps {
		if !isRigNamespaceNameStampKey(kv.Key) && n.admitsLexically(kv.Value) {
			return rowClassification{Class: rowOwned, Rule: rowRuleClaimStamps, Key: kv.Key, Value: kv.Value}
		}
	}
	var firstErr error
	for _, kv := range stamps {
		a := n.stampAdmission(kv)
		if a.Admitted() {
			return rowClassification{Class: rowOwned, Rule: rowRuleClaimStamps, Key: kv.Key, Value: kv.Value}
		}
		if firstErr == nil {
			firstErr = a.Err
		}
	}
	return rowClassification{Class: rowForeign, Rule: rowRuleClaimStamps, Key: stamps[0].Key, Value: stamps[0].Value, Err: firstErr}
}
