package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/bdflags"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// bdRigNamespaceCheck is the rig namespace guard of one `gc bd`
// invocation. `gc bd` hands its argv to a bd subprocess, which writes the
// store without passing a guarded Go store, so when the scope is a rig with
// federation = "rig" the passthrough checks the command itself before bd
// runs. It is an allowlist: a verb it does not know is refused, and the
// operator keeps raw bd for it.
//
//   - Reads pass (show, list, ready without a claim, search, dep list, ...).
//   - By-id writes (update, close, reopen, delete, heartbeat,
//     release-if-current, assign, unclaim, defer, undefer, duplicate,
//     supersede, set-state, comment, note, todo done, human dismiss,
//     label/tag add and remove, comments add, and dep add/remove and link on
//     the dependent's side) are refused when a row they name is Foreign or
//     cannot be read to tell, or when they name no row at all.
//   - A help request passes only when the argv, read the way pflag reads it,
//     really turns help on (the last --help/-h, outside a value position,
//     with a value strconv.ParseBool reads as true); `--help=false` runs the
//     command, so it is checked like any other.
//   - A claim (update --claim) needs an admitted actor; claims gc cannot vet
//     first (ready --claim, close --claim-next, close --continue without
//     --no-auto) are refused. unclaim needs the actor to be the holder.
//   - An assignee written (--assignee, assign, gc.deferred_assignee,
//     gc.stranded_assignee) must be admitted; a routing value must be
//     rig-qualified, and admitted unless the row is already this city's (a
//     hand-off of its own work).
//   - The creation stamp (gc.owner_namespace) is never set or unset through
//     gc bd, and a create is stamped with the rig's namespace. A create whose
//     argv cannot be read exactly, or that cannot be stamped (a batch file or
//     graph, two --metadata flags, an ambiguous guarded scope), is refused, as
//     is one carrying --deps.
//   - An argv whose command cannot be read the way bd's cobra resolves it (a
//     flag other than bd's global ones before the verb, or before a compound
//     verb's sub-verb) is refused: cobra may take the token after such a flag
//     as its value, and run another command than the one checked.
//   - A write aimed at another store (--db, --database, --server-url,
//     --global, --profile, create --repo, or a -C that opens another store)
//     is refused: the check reads this scope's store, not that one.
//   - relate and unrelate pass (a documented residual): they record a
//     symmetric "relates-to" link, which blocks nothing and changes no row's
//     owner, status or routing. link is not one of them: `link <a> <b>` is
//     `dep add <a> <b>` (blocks by default, or a re-parent with
//     -t parent-child), so it is checked on <a>'s row like dep add.
type bdRigNamespaceCheck struct {
	// ns is the namespace of the guarded rig whose store the scope opens, or
	// nil when the scope is no guarded rig's: then only storeRefusal runs.
	ns *rigNamespace
	// cfg is the city's config, for the per-id prefix check; nil skips it.
	cfg *config.City
	// cityPath and scopeRoot locate the store the scope opens, which a -C
	// in the argv must not move bd off (directoryRefusal). scopeRoot is the
	// directory bd runs in; "" when the caller runs bd elsewhere.
	cityPath, scopeRoot string
	// get reads a row for classification through the scope's bd front door.
	get func(id string) (beads.Bead, error)
	// rows holds the rows one check has read (row), so a row it judges
	// twice costs one bd process.
	rows map[string]beads.Bead
	// actor is the claim actor when the argv names none (--actor): the
	// BEADS_ACTOR bd inherits, or "" when unset (bd then picks a default gc
	// cannot predict, which is never admitted).
	actor string
}

// newBdRigNamespaceCheck returns the check for a gc bd invocation whose
// scope is scopeRoot, or nil when the city has no guarded rig. When no
// guarded rig owns the scope, the check only refuses a write that lands
// outside the store it judged (storeRefusal).
func newBdRigNamespaceCheck(cfg *config.City, cityPath, scopeRoot string, get func(string) (beads.Bead, error)) *bdRigNamespaceCheck {
	if !cityHasGuardedRig(cfg) {
		return nil
	}
	sessions := newCityCLISessionResolver(cityPath, cfg, func() (beads.SessionStore, error) {
		cityStore, err := openCityStoreAtWithConfig(cityPath, cfg)
		if err != nil {
			return beads.SessionStore{}, err
		}
		return cliStores(cityPath, cityStore).Sessions(), nil
	})
	ns := rigNamespaceForScopeOrDeny(cfg, cityPath, scopeRoot, sessions)
	return &bdRigNamespaceCheck{ns: ns, cfg: cfg, cityPath: cityPath, scopeRoot: scopeRoot, get: get, actor: bdEffectiveActorFromEnv(os.Getenv)}
}

// bdEffectiveActorFromEnv is the BEADS_ACTOR the bd child inherits from
// this process (bdCommandEnv passes the process environment through), or ""
// when it is unset.
func bdEffectiveActorFromEnv(getenv func(string) string) string {
	return strings.TrimSpace(getenv("BEADS_ACTOR"))
}

// bdRigNamespaceReadVerbs are the single-word bd verbs that only read.
var bdRigNamespaceReadVerbs = map[string]bool{
	"show": true, "list": true, "ready": true, "search": true, "count": true,
	"stats": true, "status": true, "blocked": true, "info": true, "version": true,
	"help": true, "where": true, "prime": true, "query": true, "history": true,
	"export": true, "stale": true, "children": true, "state": true,
}

// bdRigNamespaceCompoundReads are the read sub-verbs of compound bd verbs.
var bdRigNamespaceCompoundReads = map[string]map[string]bool{
	"dep":     {"list": true, "tree": true},
	"label":   {"list": true, "list-all": true},
	"mol":     {"current": true, "show": true, "progress": true},
	"gate":    {"list": true},
	"config":  {"get": true, "list": true},
	"human":   {"list": true},
	"formula": {"list": true, "show": true},
}

// bdRigNamespaceVerbFlags are the flags the by-id write verbs without a
// bdflags manifest accept here, beyond the global ones. Any other flag
// without an inline value makes the argv unreadable, and it is refused.
var bdRigNamespaceVerbFlags = map[string]struct{ value, bools []string }{
	"assign":        {bools: []string{"--force", "-f"}},
	"unclaim":       {value: []string{"--reason", "-r", "--if-assignee"}, bools: []string{"--force", "-f"}},
	"todo done":     {value: []string{"--reason", "-r"}},
	"human dismiss": {value: []string{"--reason", "-r"}},
	"defer":         {value: []string{"--until", "--reason", "-r"}},
	"link":          {value: []string{"--type", "-t"}},
	"undefer":       {},
	"duplicate":     {value: []string{"--of"}},
	"supersede":     {value: []string{"--with"}},
	"set-state":     {value: []string{"--reason"}},
	"comment":       {value: []string{"--author", "--file", "-f"}, bools: []string{"--stdin"}},
	"note":          {value: []string{"--file", "-f"}, bools: []string{"--stdin"}},
	"tag":           {},
	"label add":     {},
	"label remove":  {},
	"comments add":  {value: []string{"--author", "-a", "--file", "-f"}},
}

// bdRigNamespaceAssigneeKeys are the metadata keys that hold an assignee: a
// value written there must be admitted like --assignee.
var bdRigNamespaceAssigneeKeys = rigNamespaceAssigneeMetadataKeys

// check returns the argv bd must run with (a create gains the creation
// stamp) or a refusal message ending in a newline.
func (c *bdRigNamespaceCheck) check(bdArgs []string) ([]string, string, bool) {
	if c == nil {
		return bdArgs, "", false
	}
	c.rows = nil
	w := splitBdVerbCall(bdArgs)
	if msg, refused := c.storeRefusal(w); refused {
		return nil, msg, true
	}
	if c.ns == nil {
		return bdArgs, "", false
	}
	if !w.readable {
		return nil, c.unreadableCommand(w), true
	}
	if w.verb == "" {
		return bdArgs, "", false
	}
	if bdRigNamespaceReadVerbs[w.verb] {
		if flag, claims := bdflags.ClaimFlag(w.verb, w.rest); claims {
			return nil, c.unvettedClaim(w.verb, flag), true
		}
		return bdArgs, "", false
	}
	if bdRigNamespaceCompoundReads[w.verb][w.sub] || w.verb == "relate" || w.verb == "unrelate" {
		return bdArgs, "", false
	}
	if args, msg, refused, done := c.checkWritePreconditions(w); done {
		return args, msg, refused
	}
	if verbCheck, ok := bdRigNamespaceWriteChecks[w.verb]; ok {
		if args, msg, refused, handled := verbCheck(c, w); handled {
			return args, msg, refused
		}
	}
	name := w.verb
	if w.sub != "" {
		name += " " + w.sub
	}
	return nil, fmt.Sprintf("gc bd: refusing %s: rig %s (federation = \"rig\") lets gc bd run only the writes it can check first; use raw bd\n", name, rigNamespaceLabel(c.ns)), true
}

// bdVerbCall is one gc bd argv as the guard reads it: the verb ("new" read
// as "create"), the sub-verb of a compound verb (any other verb's first
// positional), the argv after the verb, the argv a compound verb's sub-verb
// parses (the whole argv without the verb and the sub-verb), and the whole
// argv.
//
// readable is false when the verb, or a compound verb's sub-verb, cannot be
// read the way bd's cobra resolves it (bdflags.SplitGlobalFlagsStrict): a
// flag other than bd's global ones, a "--" or an empty token comes before
// it, and cobra may take the token after such a flag as that flag's value,
// so bd could run another command than the one the guard reads.
type bdVerbCall struct {
	verb, sub             string
	rest, subArgs, bdArgs []string
	readable              bool
}

// bdRigNamespaceCompoundVerbs are the verbs whose sub-verb the guard reads
// (cobra resolves `<verb> <sub>` as a command of its own), so it is read as
// strictly as the verb.
var bdRigNamespaceCompoundVerbs = map[string]bool{
	"comments": true, "config": true, "dep": true, "formula": true, "gate": true,
	"human": true, "label": true, "mol": true, "todo": true,
}

// splitBdVerbCall reads bdArgs' verb, and a compound verb's sub-verb, the way
// bd's cobra resolves them.
func splitBdVerbCall(bdArgs []string) bdVerbCall {
	w := bdVerbCall{bdArgs: bdArgs}
	w.verb, w.rest, w.readable = bdflags.SplitGlobalFlagsStrict(bdArgs)
	if w.verb == "new" {
		w.verb = "create"
	}
	switch {
	case !w.readable:
	case bdRigNamespaceCompoundVerbs[w.verb]:
		at := w.verbAt()
		w.sub, w.subArgs, w.readable = bdflags.SplitSubcommandStrict(append(bdArgs[:at:at], bdArgs[at+1:]...))
	case len(w.rest) > 0 && !strings.HasPrefix(w.rest[0], "-"):
		w.sub = w.rest[0]
	}
	return w
}

// verbAt is the index of the verb in the whole argv; w must have a verb.
func (w bdVerbCall) verbAt() int {
	return len(w.bdArgs) - len(w.rest) - 1
}

// storeRefusal is the check every scope of a city with a guarded rig runs,
// guarded or not: a write must land in the store the guard judged. It
// refuses a write naming a guarded rig's bead outside that rig's scope
// (bdCrossRigSubjectRefusal) and one whose -C opens another store
// (directoryRefusal). Reads pass; a claim (ready --claim) is a write.
func (c *bdRigNamespaceCheck) storeRefusal(w bdVerbCall) (string, bool) {
	verb, ids, write := bdWriteSubjects(w)
	if !write {
		return "", false
	}
	if msg, refused := bdCrossRigSubjectRefusal(c.cfg, c.ns, verb, ids); refused {
		return msg, true
	}
	return c.directoryRefusal(w)
}

// directoryRefusal refuses a write whose -C (--directory) makes bd open a
// store other than the one the guard judged. bd changes to that directory,
// read from the scope root it runs in, and opens the store it finds there,
// while gc picked the scope by --rig, --city or a bead's prefix first: gc bd
// -C <guarded rig> update <another rig's bead> would write the guarded rig's
// store unchecked. -C is read in every spelling pflag accepts, attached and
// in a shorthand run too (-Cdir, -qC dir; bdflags.FlagValues). Stores are
// compared by identity (rigNamespaceForScopeOrDeny), so the scope's own rig, a
// directory inside it and a worktree redirected into it pass, and so does any
// store no guarded rig owns from a scope no guarded rig owns.
func (c *bdRigNamespaceCheck) directoryRefusal(w bdVerbCall) (string, bool) {
	for _, dir := range bdflags.FlagValues(w.bdArgs, bdValueFlagsFor(w.verb, w.sub), "-C", "--directory") {
		if !filepath.IsAbs(dir) {
			if c.scopeRoot == "" {
				return fmt.Sprintf("gc bd: refusing -C %s: gc bd cannot tell which directory a relative -C names here, so it cannot check the store bd would write; give an absolute path\n", dir), true
			}
			dir = filepath.Join(c.scopeRoot, dir)
		}
		opened := rigNamespaceForScopeOrDeny(c.cfg, c.cityPath, dir, nil)
		if sameGuardedStore(opened, c.ns) {
			continue
		}
		return fmt.Sprintf("gc bd: refusing -C %s: bd would write %s there, but gc bd checked this command against %s; drop -C and choose the scope with gc bd --rig\n", dir, bdStoreLabel(opened), bdStoreLabel(c.ns)), true
	}
	return "", false
}

// sameGuardedStore reports whether two stores, named by the guarded
// namespace that owns each (nil for none), are one store as far as the guard
// is concerned: both owned by no guarded rig, or both by the same one with a
// usable namespace.
func sameGuardedStore(a, b *rigNamespace) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.namespace() != "" && a.rigName() == b.rigName()
}

// bdStoreLabel names, for a refusal, the store a guarded namespace owns.
func bdStoreLabel(ns *rigNamespace) string {
	if ns == nil {
		return "a store that is no guarded rig's"
	}
	return "rig " + rigNamespaceLabel(ns) + "'s store (federation = \"rig\")"
}

// bdRigNamespaceStoreSelectors are the global flags that point bd at another
// store than the scope's: another database, another bd serve endpoint, the
// global shared-server database, or a named storage profile.
var bdRigNamespaceStoreSelectors = []string{"--db", "--database", "--server-url", "--global", "--profile"}

// checkWritePreconditions applies the rules every write verb shares. done
// reports that the check ends here, with args, msg and refused its answer:
// an argv the guard cannot read exactly, a help request (bd runs nothing),
// or a write aimed at another store.
func (c *bdRigNamespaceCheck) checkWritePreconditions(w bdVerbCall) (args []string, msg string, refused, done bool) {
	// pflag reads "-reason" as -r with the attached value "eason", while the
	// id scan reads it as --reason taking the next token: the next token
	// would then be an id bd writes that the guard never checked. A write
	// carrying a shorthand cluster or an attached shorthand value is
	// refused as unreadable (no shipped form uses one); "-x=value" is fine.
	if tok, ok := bdShorthandCluster(w.bdArgs); ok {
		return nil, fmt.Sprintf("gc bd: refusing %s: cannot read %q exactly (a shorthand cluster or attached value), so rig %s (federation = \"rig\") cannot check what it writes; spell flags out (--flag value)\n", w.verb, tok, rigNamespaceLabel(c.ns)), true, true
	}
	if bdRigNamespaceHelpOn(w) {
		// cobra prints help and runs nothing.
		return w.bdArgs, "", false, true
	}
	// Everything below writes: it must write this scope's store (a -C is
	// storeRefusal's).
	for _, f := range bdRigNamespaceStoreSelectors {
		if bdRigNamespaceFlagSet(w.verb, w.sub, w.bdArgs, f) {
			return nil, fmt.Sprintf("gc bd: refusing %s %s: rig %s (federation = \"rig\") checks writes against its own store only; use raw bd for another store\n", w.verb, f, rigNamespaceLabel(c.ns)), true, true
		}
	}
	return nil, "", false, false
}

// bdVerbCheck checks one write verb of a guarded scope. handled=false sends
// the write to the refusal of every write gc bd cannot check first.
type bdVerbCheck func(c *bdRigNamespaceCheck, w bdVerbCall) (args []string, msg string, refused, handled bool)

// bdRigNamespaceWriteChecks is the allowlist of write verbs gc bd runs on a
// guarded scope, each with the check it passes first. Every other verb is
// refused.
var bdRigNamespaceWriteChecks = map[string]bdVerbCheck{
	"create":             (*bdRigNamespaceCheck).verbCreate,
	"update":             (*bdRigNamespaceCheck).verbByID,
	"close":              (*bdRigNamespaceCheck).verbByID,
	"reopen":             (*bdRigNamespaceCheck).verbByID,
	"delete":             (*bdRigNamespaceCheck).verbByID,
	"heartbeat":          (*bdRigNamespaceCheck).verbByID,
	"release-if-current": (*bdRigNamespaceCheck).verbReleaseIfCurrent,
	"assign":             (*bdRigNamespaceCheck).verbAssign,
	"unclaim":            (*bdRigNamespaceCheck).verbUnclaim,
	"defer":              bdVerbSubjectsCheck(bdSubjectsAll),
	"undefer":            bdVerbSubjectsCheck(bdSubjectsAll),
	"duplicate":          bdVerbSubjectsCheck(bdSubjectsAll),
	"supersede":          bdVerbSubjectsCheck(bdSubjectsAll),
	"todo":               bdSubVerbSubjectsCheck(bdSubjectsAll, "done"),
	"human":              bdSubVerbSubjectsCheck(bdSubjectsAll, "dismiss"),
	"link":               (*bdRigNamespaceCheck).verbLink,
	"set-state":          bdVerbSubjectsCheck(bdSubjectsFirst),
	"comment":            bdVerbSubjectsCheck(bdSubjectsFirst),
	"note":               bdVerbSubjectsCheck(bdSubjectsFirst),
	"tag":                bdVerbSubjectsCheck(bdSubjectsAllButLast),
	"label":              bdSubVerbSubjectsCheck(bdSubjectsAllButLast, "add", "remove"),
	"comments":           (*bdRigNamespaceCheck).verbComments,
	"dep":                (*bdRigNamespaceCheck).verbDep,
}

// handledCheck is a verb check's answer that ends the check.
func handledCheck(args []string, msg string, refused bool) ([]string, string, bool, bool) {
	return args, msg, refused, true
}

func (c *bdRigNamespaceCheck) verbCreate(w bdVerbCall) ([]string, string, bool, bool) {
	return handledCheck(c.checkCreate(w))
}

func (c *bdRigNamespaceCheck) verbByID(w bdVerbCall) ([]string, string, bool, bool) {
	return handledCheck(c.checkByID(w.verb, w.bdArgs, w.rest))
}

func (c *bdRigNamespaceCheck) verbReleaseIfCurrent(w bdVerbCall) ([]string, string, bool, bool) {
	if len(w.rest) == 0 {
		return handledCheck(w.bdArgs, "", false) // doBd reports the usage error
	}
	return handledCheck(c.checkRows(w.verb, w.bdArgs, w.rest[:1]))
}

func (c *bdRigNamespaceCheck) verbAssign(w bdVerbCall) ([]string, string, bool, bool) {
	return handledCheck(c.checkAssign(w.bdArgs, w.rest))
}

func (c *bdRigNamespaceCheck) verbUnclaim(w bdVerbCall) ([]string, string, bool, bool) {
	return handledCheck(c.checkUnclaim(w.bdArgs, w.rest))
}

// verbLink checks `link <a> <b>`, which is `dep add <a> <b>`: <a>'s
// readiness or parent changes.
func (c *bdRigNamespaceCheck) verbLink(w bdVerbCall) ([]string, string, bool, bool) {
	pos, ok := c.scanVerbArgs("link", w.rest)
	if !ok || len(pos) < 2 {
		return handledCheck(nil, c.unreadable("link"), true)
	}
	return handledCheck(c.checkRows("link", w.bdArgs, pos[:1]))
}

// verbComments checks `comments add`; `comments <id>` lists a bead's
// comments, and the other sub-verbs are refused.
func (c *bdRigNamespaceCheck) verbComments(w bdVerbCall) ([]string, string, bool, bool) {
	switch w.sub {
	case "add":
		return handledCheck(c.checkSubjects("comments add", w.bdArgs, w.subArgs, bdSubjectsFirst))
	case "delete", "edit", "remove", "rm", "update":
		return nil, "", false, false
	}
	return handledCheck(w.bdArgs, "", false)
}

// verbDep checks `dep add|remove` on the dependent's side: the row whose
// readiness changes.
func (c *bdRigNamespaceCheck) verbDep(w bdVerbCall) ([]string, string, bool, bool) {
	if w.sub != "add" && w.sub != "remove" {
		return nil, "", false, false
	}
	ids, ambiguous := bdScanPositionalIDs(w.subArgs, bdSubcmdValueFlags("dep "+w.sub), bdSubcmdBoolFlags("dep "+w.sub), nil)
	if ambiguous || len(ids) == 0 {
		return handledCheck(nil, c.unreadable("dep "+w.sub), true)
	}
	return handledCheck(c.checkRows("dep "+w.sub, w.bdArgs, ids[:1]))
}

// bdVerbSubjectsCheck checks a manifest-less verb on the rows its
// positionals name under rule.
func bdVerbSubjectsCheck(rule bdSubjectRule) bdVerbCheck {
	return func(c *bdRigNamespaceCheck, w bdVerbCall) ([]string, string, bool, bool) {
		return handledCheck(c.checkSubjects(w.verb, w.bdArgs, w.rest, rule))
	}
}

// bdSubVerbSubjectsCheck checks `<verb> <sub> ...` for the sub-verbs subs,
// on the rows the positionals after sub name under rule. Any other sub-verb
// is not handled, and is refused.
func bdSubVerbSubjectsCheck(rule bdSubjectRule, subs ...string) bdVerbCheck {
	return func(c *bdRigNamespaceCheck, w bdVerbCall) ([]string, string, bool, bool) {
		if !slices.Contains(subs, w.sub) {
			return nil, "", false, false
		}
		return handledCheck(c.checkSubjects(w.verb+" "+w.sub, w.bdArgs, w.subArgs, rule))
	}
}

type bdSubjectRule uint8

const (
	bdSubjectsAll bdSubjectRule = iota
	bdSubjectsFirst
	bdSubjectsAllButLast
)

// checkSubjects reads a manifest-less verb's positionals strictly and
// checks the rows they name.
func (c *bdRigNamespaceCheck) checkSubjects(verb string, bdArgs, args []string, rule bdSubjectRule) ([]string, string, bool) {
	pos, ok := c.scanVerbArgs(verb, args)
	if !ok {
		return nil, c.unreadable(verb), true
	}
	switch rule {
	case bdSubjectsFirst:
		if len(pos) > 0 {
			pos = pos[:1]
		}
	case bdSubjectsAllButLast:
		if len(pos) < 2 {
			return nil, c.unreadable(verb), true
		}
		pos = pos[:len(pos)-1]
	}
	if len(pos) == 0 {
		return nil, c.unreadable(verb), true
	}
	return c.checkRows(verb, bdArgs, pos)
}

// scanVerbArgs returns a manifest-less verb's positionals, or ok=false when
// a flag it does not know could consume one.
func (c *bdRigNamespaceCheck) scanVerbArgs(verb string, args []string) ([]string, bool) {
	value, bools := bdRigNamespaceManifest(verb)
	pos, ambiguous := bdScanPositionalIDs(args, value, bools, nil)
	return pos, !ambiguous
}

// bdRigNamespaceManifest returns the value and bool flags of a verb key: the
// bdflags manifest when it has one, else this file's table (with the global
// flags), else nil maps.
func bdRigNamespaceManifest(key string) (value, bools map[string]bool) {
	if v := bdflags.ValueFlags(key); v != nil {
		return v, bdflags.BoolFlags(key)
	}
	spec, ok := bdRigNamespaceVerbFlags[key]
	if !ok {
		return nil, nil
	}
	value = bdflags.GlobalValueFlags()
	for _, f := range spec.value {
		value[f] = true
	}
	bools = bdflags.GlobalBoolFlags()
	for _, f := range spec.bools {
		bools[f] = true
	}
	return value, bools
}

// bdRigNamespaceHelpOn reports whether a write verb's argv really asks for
// help, read the way pflag reads it. A verb without a known manifest never
// counts as a help request: a value flag could be hiding the token.
func bdRigNamespaceHelpOn(w bdVerbCall) bool {
	key, args := w.verb, w.rest
	if w.sub != "" {
		if v, _ := bdRigNamespaceManifest(w.verb + " " + w.sub); v != nil {
			key, args = w.verb+" "+w.sub, w.subArgs
		}
	}
	value, bools := bdRigNamespaceManifest(key)
	if value == nil {
		return false
	}
	// A flag the manifest does not know could be a value flag that takes the
	// help token as its value (bd has hidden aliases), and then bd runs the
	// command: help counts only when the whole argv reads cleanly.
	if _, ambiguous := bdScanPositionalIDs(args, value, bools, nil); ambiguous {
		return false
	}
	present, on, valid := bdflags.BoolFlagValue(args, value, "--help", "-h")
	return present && on && valid
}

// bdShorthandCluster returns the first token of a write's argv that is a
// single-dash token longer than "-x" and not "-x=value": a shorthand
// cluster ("-fv") or an attached shorthand value ("-reason" is -r with
// "eason"). Every token is examined, past a "--" too: a "--" may be a
// flag's value, and refusing a positional that merely looks like one fails
// closed.
func bdShorthandCluster(args []string) (string, bool) {
	for _, a := range args {
		if len(a) <= 2 || a[0] != '-' || a[1] == '-' {
			continue
		}
		if a[2] == '=' {
			continue
		}
		return a, true
	}
	return "", false
}

// bdValueFlagsFor returns the value flags pflag reads a verb's argv with:
// the manifest of its sub-verb, else of the verb, else the global flags.
func bdValueFlagsFor(verb, sub string) map[string]bool {
	if sub != "" {
		if v, _ := bdRigNamespaceManifest(verb + " " + sub); v != nil {
			return v
		}
	}
	if v, _ := bdRigNamespaceManifest(verb); v != nil {
		return v
	}
	return bdflags.GlobalValueFlags()
}

// bdRigNamespaceFlagSet reports whether flag is given in an argv read the
// way pflag reads it with the verb's manifest (a "--" consumed as a value
// hides nothing): as a value flag with a value, or as a bool flag that is
// on or carries a value ParseBool rejects (fail closed). A verb without a
// manifest is read with the global flags only.
func bdRigNamespaceFlagSet(verb, sub string, args []string, flag string) bool {
	value := bdValueFlagsFor(verb, sub)
	if value[flag] {
		return len(bdflags.FlagValues(args, value, flag)) > 0
	}
	present, on, valid := bdflags.BoolFlagValue(args, value, flag)
	return present && (on || !valid)
}

func (c *bdRigNamespaceCheck) checkAssign(bdArgs, rest []string) ([]string, string, bool) {
	pos, ok := c.scanVerbArgs("assign", rest)
	if !ok || len(pos) == 0 || len(pos) > 2 {
		return nil, c.unreadable("assign"), true
	}
	if args, msg, refused := c.checkRows("assign", bdArgs, pos[:1]); refused {
		return args, msg, refused
	}
	if len(pos) == 2 && strings.TrimSpace(pos[1]) != "" && !rigNamespacePinAdmitted(c.ns, pos[1]) {
		return nil, c.actorRefusal("assign", pos[0], pos[1]), true
	}
	return bdArgs, "", false
}

func (c *bdRigNamespaceCheck) checkUnclaim(bdArgs, rest []string) ([]string, string, bool) {
	pos, ok := c.scanVerbArgs("unclaim", rest)
	if !ok || len(pos) == 0 {
		return nil, c.unreadable("unclaim"), true
	}
	actor := c.claimActor("unclaim", bdArgs)
	for _, id := range pos {
		row, err := c.row(id)
		if err != nil {
			return nil, c.unreadableRow("unclaim", id, err), true
		}
		if msg, foreign := c.foreignRow("unclaim", id, row); foreign {
			return nil, msg, true
		}
		holder := strings.TrimSpace(row.Assignee)
		if holder == "" {
			continue
		}
		if actor != holder || !rigNamespacePinAdmitted(c.ns, actor) {
			return nil, fmt.Sprintf("gc bd: refusing unclaim on %s: rig %s (federation = \"rig\") lets only the holder %q release it; actor %q is not\n", id, rigNamespaceLabel(c.ns), holder, actor), true
		}
	}
	return bdArgs, "", false
}

func (c *bdRigNamespaceCheck) checkCreate(w bdVerbCall) ([]string, string, bool) {
	stamp, ok := c.ns.ownerStamp()
	if !ok {
		return nil, fmt.Sprintf("gc bd: refusing create: rig %s (federation = \"rig\") cannot stamp the rows it creates\n", rigNamespaceLabel(c.ns)), true
	}
	if msg, refused := c.createArgvRefusal(w.rest); refused {
		return nil, msg, true
	}
	writes, msg, refused := bdMetadataWrites("create", w.rest)
	if refused {
		return nil, msg, true
	}
	if msg, refused := c.checkStampKey("create", writes); refused {
		return nil, msg, true
	}
	if msg, refused := c.assigneeFlagRefusal("create", "a new bead", w.rest); refused {
		return nil, msg, true
	}
	if msg, refused := c.createWritesRefusal(writes); refused {
		return nil, msg, true
	}
	assignee, _ := bdLastFlagValue("create", w.rest, "--assignee", "-a")
	if msg, refused := c.claimStampWritesRefusal("create", "a new bead", bdRowAfter(beads.Bead{}, assignee, writes), writes); refused {
		return nil, msg, true
	}
	out, err := bdArgsWithOwnerStamp(w.bdArgs, w.verbAt(), stamp)
	if err != nil {
		return nil, fmt.Sprintf("gc bd: refusing create: %v\n", err), true
	}
	return out, "", false
}

// createArgvRefusal refuses a create argv the guard cannot check whole: one
// it cannot read exactly, a batch create (which cannot be stamped), a write
// outside the new row, or more than one --metadata flag.
func (c *bdRigNamespaceCheck) createArgvRefusal(rest []string) (string, bool) {
	if _, ambiguous := bdScanPositionalIDs(rest, bdSubcmdValueFlags("create"), bdSubcmdBoolFlags("create"), nil); ambiguous {
		return c.unreadable("create"), true
	}
	for _, flag := range []string{"--file", "-f", "--graph"} {
		if bdRigNamespaceFlagSet("create", "", rest, flag) {
			return fmt.Sprintf("gc bd: refusing create %s: rig %s (federation = \"rig\") stamps every row gc creates, and a batch create cannot be stamped\n", flag, rigNamespaceLabel(c.ns)), true
		}
	}
	for _, flag := range []string{"--deps", "--repo"} {
		if bdRigNamespaceFlagSet("create", "", rest, flag) {
			return fmt.Sprintf("gc bd: refusing create %s: rig %s (federation = \"rig\") cannot check what it writes outside the new row; use raw bd\n", flag, rigNamespaceLabel(c.ns)), true
		}
	}
	if n := len(bdflags.FlagValues(rest, bdSubcmdValueFlags("create"), "--metadata")); n > 1 {
		return fmt.Sprintf("gc bd: refusing create with %d --metadata flags: bd keeps only the last, so the rig's creation stamp could be lost\n", n), true
	}
	return "", false
}

// assigneeFlagRefusal refuses a --assignee (-a) value of verb that the
// namespace does not admit.
func (c *bdRigNamespaceCheck) assigneeFlagRefusal(verb, subject string, rest []string) (string, bool) {
	for _, a := range bdflags.FlagValues(rest, bdSubcmdValueFlags(verb), "--assignee", "-a") {
		if strings.TrimSpace(a) != "" && !rigNamespacePinAdmitted(c.ns, a) {
			return c.actorRefusal(verb, subject, a), true
		}
	}
	return "", false
}

// createWritesRefusal refuses a create's metadata write of an assignee the
// namespace does not admit or of a route outside it.
func (c *bdRigNamespaceCheck) createWritesRefusal(writes []bdMetadataWrite) (string, bool) {
	for _, w := range writes {
		if w.unset || strings.TrimSpace(w.value) == "" {
			continue
		}
		if isBdRigNamespaceAssigneeKey(w.key) && !rigNamespacePinAdmitted(c.ns, w.value) {
			return c.actorRefusal("create", "a new bead", w.value), true
		}
		// Create-time route rule (one rule, two layers; see "Routes on a new
		// bead" in docs/guides/sharing-a-rig-store.md): gc bd create, like
		// gc sling, admits only a route to one of the rig's own agents. The
		// guarded store underneath (rigNamespaceStore.stampMetadata) also
		// accepts a rig-qualified route outside the namespace on create, as
		// a hand-off of this city's own new row. gc bd is stricter by design:
		// a hand-off goes through an update of an existing Owned row (see
		// checkByID), never through a create.
		if isRigNamespaceRouteKey(w.key) && !rigNamespaceRouteAdmitted(c.ns, w.value) {
			return c.routeRefusal("create", "a new bead", w), true
		}
	}
	return "", false
}

func (c *bdRigNamespaceCheck) checkByID(verb string, bdArgs, rest []string) ([]string, string, bool) {
	if flag, claims := bdflags.ClaimFlag(verb, rest); claims && verb != "update" {
		return nil, c.unvettedClaim(verb, flag), true
	}
	if verb == "delete" && (bdRigNamespaceFlagSet(verb, "", rest, "--cascade") || bdRigNamespaceFlagSet(verb, "", rest, "--from-file")) {
		return nil, fmt.Sprintf("gc bd: refusing delete --cascade/--from-file: rig %s (federation = \"rig\") deletes only rows it can check first\n", rigNamespaceLabel(c.ns)), true
	}
	ids, ambiguous := bdScanPositionalIDs(rest, bdSubcmdValueFlags(verb), bdSubcmdBoolFlags(verb), nil)
	if ambiguous || len(ids) == 0 {
		return nil, c.unreadable(verb), true
	}
	var writes []bdMetadataWrite
	if verb == "update" {
		var msg string
		var refused bool
		if writes, msg, refused = bdMetadataWrites(verb, rest); refused {
			return nil, msg, true
		}
		if msg, refused := c.checkStampKey(verb, writes); refused {
			return nil, msg, true
		}
	}
	args, msg, refused := c.checkRows(verb, bdArgs, ids)
	if refused || verb != "update" {
		return args, msg, refused
	}
	if msg, refused := c.updateValueRefusal(verb, bdArgs, rest, ids, writes); refused {
		return nil, msg, true
	}
	return args, "", false
}

// updateValueRefusal refuses an update of rows the guard admits that sets a
// value the namespace does not admit: a claim or an assignee for an
// identity outside it, or a route that would hand a row that is not this
// city's own to an outside identity.
func (c *bdRigNamespaceCheck) updateValueRefusal(verb string, bdArgs, rest, ids []string, writes []bdMetadataWrite) (string, bool) {
	subject := strings.Join(ids, ", ")
	if _, claims := bdflags.ClaimFlag(verb, rest); claims {
		if actor := c.claimActor(verb, bdArgs); !rigNamespacePinAdmitted(c.ns, actor) {
			return c.actorRefusal(verb, subject, actor), true
		}
	}
	if msg, refused := c.assigneeFlagRefusal(verb, subject, rest); refused {
		return msg, true
	}
	for _, w := range writes {
		if w.unset || strings.TrimSpace(w.value) == "" {
			continue
		}
		if isBdRigNamespaceAssigneeKey(w.key) && !rigNamespacePinAdmitted(c.ns, w.value) {
			return c.actorRefusal(verb, subject, w.value), true
		}
		if msg, refused := c.updateRouteRefusal(verb, subject, ids, w); refused {
			return msg, true
		}
	}
	return c.updateClaimStampRefusal(verb, bdArgs, rest, ids, writes)
}

// updateClaimStampRefusal applies the store guard's claim-stamp rule to an
// update of the rows ids: a stamp is checked when it names a pool session,
// or when the row carries no owner key once the update lands
// (claimStampDecides), since the stamp then decides whose row it is.
func (c *bdRigNamespaceCheck) updateClaimStampRefusal(verb string, bdArgs, rest, ids []string, writes []bdMetadataWrite) (string, bool) {
	if !bdWritesClaimStamp(writes) {
		return "", false
	}
	// The assignee the update leaves: a claim's actor, else the last
	// --assignee, else the row's own.
	assignee, assigned := bdLastFlagValue(verb, rest, "--assignee", "-a")
	if _, claims := bdflags.ClaimFlag(verb, rest); claims {
		assignee, assigned = c.claimActor(verb, bdArgs), true
	}
	for _, id := range ids {
		row, err := c.row(id)
		if err != nil {
			return c.unreadableRow(verb, id, err), true
		}
		after := row.Assignee
		if assigned {
			after = assignee
		}
		if msg, refused := c.claimStampWritesRefusal(verb, id, bdRowAfter(row, after, writes), writes); refused {
			return msg, true
		}
	}
	return "", false
}

// claimStampWritesRefusal refuses a claim stamp in writes that the store
// guard would refuse on a row that reads as after once written: the shared
// rule (claimStampDecides, rigNamespace.claimStampRefusal).
func (c *bdRigNamespaceCheck) claimStampWritesRefusal(verb, subject string, after beads.Bead, writes []bdMetadataWrite) (string, bool) {
	ownersAfter := len(rowOwnerValues(after)) > 0
	for _, w := range writes {
		if w.unset || !isRigNamespaceClaimStampKey(w.key) || !claimStampDecides(w.value, ownersAfter) {
			continue
		}
		if reason, err := c.ns.claimStampRefusal(w.key, w.value); reason != "" {
			if err != nil {
				reason += fmt.Sprintf(" (%v)", err)
			}
			return fmt.Sprintf("gc bd: refusing %s on %s: rig %s (federation = \"rig\") refuses it: %s\n", verb, subject, rigNamespaceLabel(c.ns), reason), true
		}
	}
	return "", false
}

// bdWritesClaimStamp reports whether writes set a claim stamp.
func bdWritesClaimStamp(writes []bdMetadataWrite) bool {
	for _, w := range writes {
		if !w.unset && isRigNamespaceClaimStampKey(w.key) {
			return true
		}
	}
	return false
}

// bdRowAfter is row as a write leaves it: assignee as its assignee and the
// metadata writes applied.
func bdRowAfter(row beads.Bead, assignee string, writes []bdMetadataWrite) beads.Bead {
	after := row
	after.Assignee = assignee
	after.Metadata = make(map[string]string, len(row.Metadata)+len(writes))
	for k, v := range row.Metadata {
		after.Metadata[k] = v
	}
	for _, w := range writes {
		if w.unset {
			delete(after.Metadata, strings.TrimSpace(w.key))
			continue
		}
		after.Metadata[strings.TrimSpace(w.key)] = w.value
	}
	return after
}

// bdLastFlagValue is the value pflag keeps for a value flag of verb's argv
// (the last one given), and whether it is given at all.
func bdLastFlagValue(verb string, args []string, names ...string) (string, bool) {
	vs := bdflags.FlagValues(args, bdValueFlagsFor(verb, ""), names...)
	if len(vs) == 0 {
		return "", false
	}
	return vs[len(vs)-1], true
}

// updateRouteRefusal refuses an update's route write w that is not
// rig-qualified, or that is outside the namespace on a row that is not this
// city's own: a route outside the namespace hands this city's own work off;
// on any other row it would claim the row for an outside identity.
func (c *bdRigNamespaceCheck) updateRouteRefusal(verb, subject string, ids []string, w bdMetadataWrite) (string, bool) {
	if !isRigNamespaceRouteKey(w.key) {
		return "", false
	}
	if !config.RigRouteQualified(w.value) {
		return c.routeRefusal(verb, subject, w), true
	}
	if rigNamespaceRouteAdmitted(c.ns, w.value) {
		return "", false
	}
	for _, id := range ids {
		row, err := c.row(id)
		if err != nil || c.ns.classify(row) != rowOwned {
			return c.routeRefusal(verb, id, w), true
		}
	}
	return "", false
}

// checkStampKey refuses a write that sets or unsets the creation stamp.
func (c *bdRigNamespaceCheck) checkStampKey(verb string, writes []bdMetadataWrite) (string, bool) {
	for _, w := range writes {
		if strings.TrimSpace(w.key) == rigNamespaceOwnerStampKey {
			return fmt.Sprintf("gc bd: refusing %s: %s is written only by the rig namespace guard (rig %s, federation = \"rig\")\n", verb, rigNamespaceOwnerStampKey, rigNamespaceLabel(c.ns)), true
		}
	}
	return "", false
}

// checkRows refuses the write when any row it names is Foreign or cannot be
// read to tell.
func (c *bdRigNamespaceCheck) checkRows(verb string, bdArgs, ids []string) ([]string, string, bool) {
	for _, id := range ids {
		row, err := c.row(id)
		if err != nil {
			return nil, c.unreadableRow(verb, id, err), true
		}
		if msg, foreign := c.foreignRow(verb, id, row); foreign {
			return nil, msg, true
		}
	}
	return bdArgs, "", false
}

// row reads id through get, once per check.
func (c *bdRigNamespaceCheck) row(id string) (beads.Bead, error) {
	if b, ok := c.rows[id]; ok {
		return b, nil
	}
	b, err := c.get(id)
	if err != nil {
		return beads.Bead{}, err
	}
	if c.rows == nil {
		c.rows = map[string]beads.Bead{}
	}
	c.rows[id] = b
	return b, nil
}

func (c *bdRigNamespaceCheck) foreignRow(verb, id string, row beads.Bead) (string, bool) {
	cls := c.ns.classifyRow(row)
	if cls.Class != rowForeign {
		return "", false
	}
	if c.ns.namespace() == "" {
		return fmt.Sprintf("gc bd: refusing %s on %s: rig %s (federation = \"rig\") admits no row in this scope\n", verb, id, rigNamespaceLabel(c.ns)), true
	}
	if cls.Err != nil {
		return fmt.Sprintf("gc bd: refusing %s on %s: rig %s (federation = \"rig\") does not own it (owner %q could not be checked: %v)\n", verb, id, rigNamespaceLabel(c.ns), cls.Value, cls.Err), true
	}
	return fmt.Sprintf("gc bd: refusing %s on %s: rig %s (federation = \"rig\") does not own it (owner %q)\n", verb, id, rigNamespaceLabel(c.ns), cls.Value), true
}

// claimActor is the identity bd runs verb as: the last --actor, else the
// inherited BEADS_ACTOR.
func (c *bdRigNamespaceCheck) claimActor(verb string, bdArgs []string) string {
	if vs := bdflags.FlagValues(bdArgs, bdValueFlagsFor(verb, ""), "--actor"); len(vs) > 0 {
		return strings.TrimSpace(vs[len(vs)-1])
	}
	return c.actor
}

func (c *bdRigNamespaceCheck) unvettedClaim(verb, flag string) string {
	return fmt.Sprintf("gc bd: refusing %s %s: rig %s (federation = \"rig\") lets gc claim only rows it can check first; claim a named bead instead\n", verb, flag, rigNamespaceLabel(c.ns))
}

func (c *bdRigNamespaceCheck) unreadable(verb string) string {
	return fmt.Sprintf("gc bd: refusing %s: cannot tell which beads it writes, so rig %s (federation = \"rig\") cannot check them (name the beads, and give flag values inline as --flag=value)\n", verb, rigNamespaceLabel(c.ns))
}

// unreadableCommand is the refusal of an argv whose command the guard
// cannot read the way bd's cobra resolves it (bdVerbCall.readable).
func (c *bdRigNamespaceCheck) unreadableCommand(w bdVerbCall) string {
	what := "this argv"
	if w.verb != "" {
		what = w.verb
	}
	return fmt.Sprintf("gc bd: refusing %s: cannot tell which command bd would run (bd reads the token after a flag it does not know yet as that flag's value), so rig %s (federation = \"rig\") cannot check it; put only bd's global flags before the command and its sub-command, and the command's own flags after them\n", what, rigNamespaceLabel(c.ns))
}

func (c *bdRigNamespaceCheck) unreadableRow(verb, id string, err error) string {
	return fmt.Sprintf("gc bd: refusing %s on %s: rig %s (federation = \"rig\") could not read it to check its owner: %v\n", verb, id, rigNamespaceLabel(c.ns), err)
}

func (c *bdRigNamespaceCheck) actorRefusal(verb, subject, actor string) string {
	return fmt.Sprintf("gc bd: refusing %s on %s: rig %s (federation = \"rig\") serves only %s and this city's pool sessions; actor %q is outside it%s\n", verb, subject, rigNamespaceLabel(c.ns), rigNamespaceShown(c.ns), strings.TrimSpace(actor), rigNamespaceActorHint(c.ns, actor))
}

func (c *bdRigNamespaceCheck) routeRefusal(verb, subject string, w bdMetadataWrite) string {
	return fmt.Sprintf("gc bd: refusing %s on %s: rig %s (federation = \"rig\") takes only routes to its own agents (%s); %s=%q is not one\n", verb, subject, rigNamespaceLabel(c.ns), rigNamespaceShown(c.ns), strings.TrimSpace(w.key), w.value)
}

func isBdRigNamespaceAssigneeKey(key string) bool {
	key = strings.TrimSpace(key)
	for _, k := range bdRigNamespaceAssigneeKeys {
		if key == k {
			return true
		}
	}
	return false
}

// bdMetadataWrite is one metadata key a create or update sets or unsets.
type bdMetadataWrite struct {
	key   string
	value string
	unset bool
}

// bdMetadataWrites returns every metadata write in a create or update argv:
// --set-metadata k=v, --metadata <json object> and --unset-metadata k, in
// both the separate and the inline spelling. An argv it cannot read exactly
// (a missing or malformed value, @file input, a non-string value of a key
// the guard checks) is refused, since the write could then carry anything.
func bdMetadataWrites(verb string, args []string) ([]bdMetadataWrite, string, bool) {
	valueFlags := bdflags.ValueFlags(verb)
	var out []bdMetadataWrite
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		name, value, inline := strings.Cut(arg, "=")
		if name != "--set-metadata" && name != "--metadata" && name != "--unset-metadata" {
			if !inline && valueFlags[arg] && i+1 < len(args) {
				i++
			}
			continue
		}
		if !inline {
			if i+1 >= len(args) {
				return nil, fmt.Sprintf("gc bd: refusing %s without a value before write\n", name), true
			}
			i++
			value = args[i]
		}
		writes, msg, refused := bdMetadataFlagWrites(name, value)
		if refused {
			return nil, msg, true
		}
		out = append(out, writes...)
	}
	return out, "", false
}

// bdMetadataFlagWrites returns the writes of one metadata flag name
// (--set-metadata, --metadata or --unset-metadata) with its value, or a
// refusal when the value cannot be read exactly.
func bdMetadataFlagWrites(name, value string) ([]bdMetadataWrite, string, bool) {
	switch name {
	case "--unset-metadata":
		return []bdMetadataWrite{{key: value, unset: true}}, "", false
	case "--set-metadata":
		k, v, ok := strings.Cut(value, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, fmt.Sprintf("gc bd: refusing malformed --set-metadata value %q before write\n", value), true
		}
		return []bdMetadataWrite{{key: k, value: v}}, "", false
	}
	return bdMetadataJSONWrites(value)
}

// bdMetadataJSONWrites returns the writes of a --metadata JSON object, in key
// order. A non-string value is skipped, unless its key is one the guard
// checks.
func bdMetadataJSONWrites(value string) ([]bdMetadataWrite, string, bool) {
	raw := strings.TrimSpace(value)
	if strings.HasPrefix(raw, "@") {
		return nil, fmt.Sprintf("gc bd: refusing --metadata %q: @file input cannot be checked before write\n", value), true
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Sprintf("gc bd: refusing malformed --metadata value before write: %v\n", err), true
	}
	var out []bdMetadataWrite
	// Keys in order, so a refusal names the same key on every run and the
	// writes are checked in one order.
	for _, k := range slices.Sorted(maps.Keys(m)) {
		rv := m[k]
		var v string
		if err := json.Unmarshal(rv, &v); err != nil {
			if strings.TrimSpace(k) == rigNamespaceOwnerStampKey || isRigNamespaceRouteKey(k) || isBdRigNamespaceAssigneeKey(k) {
				return nil, fmt.Sprintf("gc bd: refusing non-string %s before write\n", k), true
			}
			continue
		}
		out = append(out, bdMetadataWrite{key: k, value: v})
	}
	return out, "", false
}

// bdArgsWithOwnerStamp returns a create argv, its verb at verbAt, whose
// metadata carries the creation stamp: merged into its --metadata object
// when it has one, otherwise as a new --metadata flag right after the verb
// (so a "--" terminator never turns it into a positional).
func bdArgsWithOwnerStamp(bdArgs []string, verbAt int, stamp string) ([]string, error) {
	out := append([]string(nil), bdArgs...)
	valueFlags := bdflags.ValueFlags("create")
	for i := verbAt + 1; i < len(out); i++ {
		a := out[i]
		if a == "--" {
			break
		}
		name, value, inline := strings.Cut(a, "=")
		if name != "--metadata" {
			if !inline && valueFlags[a] && i+1 < len(out) {
				i++
			}
			continue
		}
		at := i
		if !inline {
			if i+1 >= len(out) {
				return nil, fmt.Errorf("--metadata has no value")
			}
			at = i + 1
			value = out[at]
		}
		merged, err := mergeOwnerStampJSON(value, stamp)
		if err != nil {
			return nil, err
		}
		if inline {
			out[at] = "--metadata=" + merged
		} else {
			out[at] = merged
		}
		return out, nil
	}
	fresh, err := mergeOwnerStampJSON("{}", stamp)
	if err != nil {
		return nil, err
	}
	res := make([]string, 0, len(out)+2)
	res = append(res, out[:verbAt+1]...)
	res = append(res, "--metadata", fresh)
	res = append(res, out[verbAt+1:]...)
	return res, nil
}

func mergeOwnerStampJSON(raw, stamp string) (string, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &m); err != nil {
		return "", fmt.Errorf("malformed --metadata value: %w", err)
	}
	if m == nil {
		m = map[string]json.RawMessage{}
	}
	v, err := json.Marshal(stamp)
	if err != nil {
		return "", err
	}
	m[beadmeta.OwnerNamespaceMetadataKey] = v
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// bdCrossRigSubjectRefusal is gc bd's per-id prefix check, the one the hook
// claim and agent-script apply (rigNamespaceForLeg): the guard above checks
// the scope gc bd resolved, but a write's ids need not live there (an
// explicit --rig or --city, -C, or ids from two stores), and bd may follow
// an id to another store. So a write verb naming the bead ids ids
// (bdWriteSubjects) whose prefix maps to a guarded rig other than the
// scope's (or to any guarded rig, when the scope is no guarded rig's) is
// refused before bd runs; a prefix two guarded rigs share is refused too.
// scope is the scope's guarded namespace, or nil. verb is "" for an argv
// whose command cannot be read.
func bdCrossRigSubjectRefusal(cfg *config.City, scope *rigNamespace, verb string, ids []string) (string, bool) {
	if !cityHasGuardedRig(cfg) {
		return "", false
	}
	if verb == "" {
		verb = "this command"
	}
	scopeName := ""
	scopeLabel := "a scope that is no guarded rig's store"
	if scope != nil {
		scopeName = scope.rigName()
		scopeLabel = "rig " + rigNamespaceLabel(scope) + "'s scope"
	}
	for _, id := range ids {
		owner, guarded := rigNamespaceForBeadID(cfg, id, nil)
		if !guarded {
			continue
		}
		name := owner.rigName()
		if name == "" {
			return fmt.Sprintf("gc bd: refusing %s on %s: its prefix names more than one rig with federation = \"rig\", so gc bd cannot tell whose store it is in; use raw bd\n", verb, id), true
		}
		if scope != nil && name == scopeName {
			continue
		}
		return fmt.Sprintf("gc bd: refusing %s on %s: it is rig %q's bead (federation = \"rig\"), and this command runs in %s, where that rig cannot check it; run it in rig %q's scope (gc bd --rig %s ...)\n", verb, id, name, scopeLabel, name, name), true
	}
	return "", false
}

// bdWriteSubjects names the beads a gc bd write addresses, the way the
// guard's dispatch reads them: verb is its name, write reports whether it
// writes at all (reads, relate/unrelate and real help requests do not). A
// read verb that claims (ready --claim) writes the row it picks, and names
// none. A write whose subjects cannot be read exactly (an unknown verb, an
// argv a flag it does not know could hide an id in, a shorthand cluster)
// returns every positional token, so a guarded rig's id anywhere in it is
// seen; an argv whose command cannot be read is such a write, and every
// positional token of the whole argv counts.
func bdWriteSubjects(w bdVerbCall) (verb string, ids []string, write bool) {
	verb = w.verb
	if !w.readable {
		return verb, bdPositionalTokens(w.bdArgs), true
	}
	if verb == "" {
		return verb, nil, false
	}
	if bdRigNamespaceReadVerbs[verb] {
		_, claims := bdflags.ClaimFlag(verb, w.rest)
		return verb, nil, claims
	}
	if bdRigNamespaceCompoundReads[verb][w.sub] || verb == "relate" || verb == "unrelate" {
		return verb, nil, false
	}
	_, cluster := bdShorthandCluster(w.bdArgs)
	if !cluster && bdRigNamespaceHelpOn(w) {
		return verb, nil, false
	}
	if cluster {
		return verb, bdPositionalTokens(w.rest), true
	}
	if verb == "create" {
		return verb, nil, true
	}
	scan, known := bdWriteSubjectScans[verb]
	if !known {
		return verb, bdPositionalTokens(w.rest), true
	}
	pos, ok, list := scan(w)
	switch {
	case list:
		return verb, nil, false
	case !ok:
		return verb, bdPositionalTokens(w.rest), true
	}
	return verb, pos, true
}

// bdSubjectScan reads the bead ids a write verb names. ok=false means they
// cannot be read exactly; list=true means the form only reads.
type bdSubjectScan func(w bdVerbCall) (pos []string, ok, list bool)

// bdWriteSubjectScans reads each write verb of the guard's allowlist
// (bdRigNamespaceWriteChecks) the way its check does. A verb not here
// cannot be read exactly.
var bdWriteSubjectScans = map[string]bdSubjectScan{
	"update":             bdVerbSubjectScan(bdSubjectsAll),
	"close":              bdVerbSubjectScan(bdSubjectsAll),
	"reopen":             bdVerbSubjectScan(bdSubjectsAll),
	"delete":             bdVerbSubjectScan(bdSubjectsAll),
	"heartbeat":          bdVerbSubjectScan(bdSubjectsAll),
	"release-if-current": bdReleaseIfCurrentSubjects,
	"assign":             bdVerbSubjectScan(bdSubjectsFirst),
	"unclaim":            bdVerbSubjectScan(bdSubjectsAll),
	"defer":              bdVerbSubjectScan(bdSubjectsAll),
	"undefer":            bdVerbSubjectScan(bdSubjectsAll),
	"duplicate":          bdVerbSubjectScan(bdSubjectsAll),
	"supersede":          bdVerbSubjectScan(bdSubjectsAll),
	"todo":               bdSubVerbSubjectScan(bdSubjectsAll, "done"),
	"human":              bdSubVerbSubjectScan(bdSubjectsAll, "dismiss"),
	"link":               bdVerbSubjectScan(bdSubjectsFirst),
	"set-state":          bdVerbSubjectScan(bdSubjectsFirst),
	"comment":            bdVerbSubjectScan(bdSubjectsFirst),
	"note":               bdVerbSubjectScan(bdSubjectsFirst),
	// A label is no bead id, even one that starts like a guarded rig's: tag
	// and label add/remove drop it, as the guard's own rule for them does.
	"tag":      bdVerbSubjectScan(bdSubjectsAllButLast),
	"label":    bdSubVerbSubjectScan(bdSubjectsAllButLast, "add", "remove"),
	"comments": bdCommentsSubjects,
	"dep":      bdSubVerbSubjectScan(bdSubjectsFirst, "add", "remove"),
}

// bdPositionalTokens is every token of args that is not a flag.
func bdPositionalTokens(args []string) []string {
	var out []string
	for _, a := range args {
		if a != "" && !strings.HasPrefix(a, "-") {
			out = append(out, a)
		}
	}
	return out
}

// bdScanManifestPositionals reads args' positionals with the manifest of
// key; ok=false when key has none or a flag it does not know could hide one.
func bdScanManifestPositionals(key string, args []string) ([]string, bool) {
	value, bools := bdRigNamespaceManifest(key)
	if value == nil {
		return nil, false
	}
	pos, ambiguous := bdScanPositionalIDs(args, value, bools, nil)
	return pos, !ambiguous
}

// bdPickSubjects keeps the positionals rule names.
func bdPickSubjects(rule bdSubjectRule, pos []string, ok bool) ([]string, bool) {
	switch rule {
	case bdSubjectsFirst:
		if len(pos) > 1 {
			pos = pos[:1]
		}
	case bdSubjectsAllButLast:
		if !ok || len(pos) < 2 {
			return nil, false
		}
		pos = pos[:len(pos)-1]
	}
	return pos, ok
}

// bdVerbSubjectScan reads a verb's subjects from its positionals under rule.
func bdVerbSubjectScan(rule bdSubjectRule) bdSubjectScan {
	return func(w bdVerbCall) ([]string, bool, bool) {
		pos, ok := bdScanManifestPositionals(w.verb, w.rest)
		pos, ok = bdPickSubjects(rule, pos, ok)
		return pos, ok, false
	}
}

// bdSubVerbSubjectScan reads `<verb> <sub> ...` for the sub-verbs subs from
// the positionals after sub, under rule; any other sub-verb cannot be read.
func bdSubVerbSubjectScan(rule bdSubjectRule, subs ...string) bdSubjectScan {
	return func(w bdVerbCall) ([]string, bool, bool) {
		if !slices.Contains(subs, w.sub) {
			return nil, false, false
		}
		pos, ok := bdScanManifestPositionals(w.verb+" "+w.sub, w.subArgs)
		pos, ok = bdPickSubjects(rule, pos, ok)
		return pos, ok, false
	}
}

// bdReleaseIfCurrentSubjects is release-if-current's first positional.
func bdReleaseIfCurrentSubjects(w bdVerbCall) ([]string, bool, bool) {
	pos, ok := bdPickSubjects(bdSubjectsFirst, w.rest, true)
	return pos, ok, false
}

// bdCommentsSubjects reads `comments add <id>`; `comments <id>` lists a
// bead's comments, and the other sub-verbs cannot be read.
func bdCommentsSubjects(w bdVerbCall) ([]string, bool, bool) {
	switch w.sub {
	case "add":
		return bdSubVerbSubjectScan(bdSubjectsFirst, "add")(w)
	case "delete", "edit", "remove", "rm", "update":
		return nil, false, false
	}
	return nil, true, true
}
