package main

import (
	"encoding/json"
	"fmt"
	"os"
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
//   - A write naming another database (--db, --database, create --repo) is
//     refused: the check reads this scope's store, not that one.
//   - relate and unrelate pass (a documented residual): they record a
//     symmetric "relates-to" link, which blocks nothing and changes no row's
//     owner, status or routing. link is not one of them: `link <a> <b>` is
//     `dep add <a> <b>` (blocks by default, or a re-parent with
//     -t parent-child), so it is checked on <a>'s row like dep add.
type bdRigNamespaceCheck struct {
	ns *rigNamespace
	// get reads a row for classification through the scope's bd front door.
	get func(id string) (beads.Bead, error)
	// actor is the claim actor when the argv names none (--actor): the
	// BEADS_ACTOR bd inherits, or "" when unset (bd then picks a default gc
	// cannot predict, which is never admitted).
	actor string
}

// newBdRigNamespaceCheck returns the check for a gc bd invocation whose
// scope is scopeRoot, or nil when no guarded rig owns that scope.
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
	if ns == nil {
		return nil
	}
	return &bdRigNamespaceCheck{ns: ns, get: get, actor: bdEffectiveActorFromEnv(os.Getenv)}
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
	verb, rest := bdflags.SplitGlobalFlags(bdArgs)
	if verb == "new" {
		verb = "create"
	}
	if verb == "" {
		return bdArgs, "", false
	}
	if bdRigNamespaceReadVerbs[verb] {
		if flag, claims := bdflags.ClaimFlag(verb, rest); claims {
			return nil, c.unvettedClaim(verb, flag), true
		}
		return bdArgs, "", false
	}
	sub := ""
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		sub = rest[0]
	}
	if bdRigNamespaceCompoundReads[verb][sub] {
		return bdArgs, "", false
	}
	switch verb {
	case "relate", "unrelate":
		return bdArgs, "", false
	}
	// pflag reads "-reason" as -r with the attached value "eason", while the
	// id scan reads it as --reason taking the next token: the next token
	// would then be an id bd writes that the guard never checked. A write
	// carrying a shorthand cluster or an attached shorthand value is
	// refused as unreadable (no shipped form uses one); "-x=value" is fine.
	if tok, ok := bdShorthandCluster(bdArgs); ok {
		return nil, fmt.Sprintf("gc bd: refusing %s: cannot read %q exactly (a shorthand cluster or attached value), so rig %s (federation = \"rig\") cannot check what it writes; spell flags out (--flag value)\n", verb, tok, rigNamespaceLabel(c.ns)), true
	}
	if bdRigNamespaceHelpOn(verb, sub, rest) {
		// cobra prints help and runs nothing.
		return bdArgs, "", false
	}
	// Everything below writes: it must write this scope's store.
	for _, f := range []string{"--db", "--database"} {
		if bdRigNamespaceFlagSet(verb, sub, bdArgs, f) {
			return nil, fmt.Sprintf("gc bd: refusing %s %s: rig %s (federation = \"rig\") checks writes against its own store only; use raw bd for another database\n", verb, f, rigNamespaceLabel(c.ns)), true
		}
	}
	switch verb {
	case "create":
		return c.checkCreate(bdArgs, rest)
	case "update", "close", "reopen", "delete", "heartbeat":
		return c.checkByID(verb, bdArgs, rest)
	case "release-if-current":
		if len(rest) == 0 {
			return bdArgs, "", false // doBd reports the usage error
		}
		return c.checkRows(verb, bdArgs, rest[:1])
	case "assign":
		return c.checkAssign(bdArgs, rest)
	case "unclaim":
		return c.checkUnclaim(bdArgs, rest)
	case "defer", "undefer", "duplicate", "supersede":
		return c.checkSubjects(verb, bdArgs, rest, bdSubjectsAll)
	case "todo", "human":
		if (verb == "todo" && sub == "done") || (verb == "human" && sub == "dismiss") {
			return c.checkSubjects(verb+" "+sub, bdArgs, rest[1:], bdSubjectsAll)
		}
	case "link":
		pos, ok := c.scanVerbArgs("link", rest)
		if !ok || len(pos) < 2 {
			return nil, c.unreadable("link"), true
		}
		// `link <a> <b>` is `dep add <a> <b>`: <a>'s readiness or parent
		// changes.
		return c.checkRows("link", bdArgs, pos[:1])
	case "set-state", "comment", "note":
		return c.checkSubjects(verb, bdArgs, rest, bdSubjectsFirst)
	case "tag":
		return c.checkSubjects(verb, bdArgs, rest, bdSubjectsAllButLast)
	case "label":
		if sub == "add" || sub == "remove" {
			return c.checkSubjects("label "+sub, bdArgs, rest[1:], bdSubjectsAllButLast)
		}
	case "comments":
		if sub == "add" {
			return c.checkSubjects("comments add", bdArgs, rest[1:], bdSubjectsFirst)
		}
		switch sub {
		case "delete", "edit", "remove", "rm", "update":
		default:
			// `comments <id>` lists a bead's comments.
			return bdArgs, "", false
		}
	case "dep":
		if sub == "add" || sub == "remove" {
			ids, ambiguous := bdScanPositionalIDs(rest[1:], bdSubcmdValueFlags("dep "+sub), bdSubcmdBoolFlags("dep "+sub), nil)
			if ambiguous || len(ids) == 0 {
				return nil, c.unreadable("dep " + sub), true
			}
			// The dependent's side: the row whose readiness changes.
			return c.checkRows("dep "+sub, bdArgs, ids[:1])
		}
	}
	name := verb
	if sub != "" {
		name += " " + sub
	}
	return nil, fmt.Sprintf("gc bd: refusing %s: rig %s (federation = \"rig\") lets gc bd run only the writes it can check first; use raw bd\n", name, rigNamespaceLabel(c.ns)), true
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
func bdRigNamespaceHelpOn(verb, sub string, rest []string) bool {
	key, args := verb, rest
	if sub != "" {
		if v, _ := bdRigNamespaceManifest(verb + " " + sub); v != nil {
			key, args = verb+" "+sub, rest[1:]
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

// bdRigNamespaceFlagSet reports whether flag is given in an argv read the
// way pflag reads it with the verb's manifest (a "--" consumed as a value
// hides nothing): as a value flag with a value, or as a bool flag that is
// on or carries a value ParseBool rejects (fail closed). A verb without a
// manifest is read with the global flags only.
func bdRigNamespaceFlagSet(verb, sub string, args []string, flag string) bool {
	key := verb
	if sub != "" {
		if v, _ := bdRigNamespaceManifest(verb + " " + sub); v != nil {
			key = verb + " " + sub
		}
	}
	value, _ := bdRigNamespaceManifest(key)
	if value == nil {
		value = bdflags.GlobalValueFlags()
	}
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
	actor := c.claimActor(bdArgs)
	for _, id := range pos {
		row, err := c.get(id)
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

func (c *bdRigNamespaceCheck) checkCreate(bdArgs, rest []string) ([]string, string, bool) {
	stamp, ok := c.ns.ownerStamp()
	if !ok {
		return nil, fmt.Sprintf("gc bd: refusing create: rig %s (federation = \"rig\") cannot stamp the rows it creates\n", rigNamespaceLabel(c.ns)), true
	}
	if _, ambiguous := bdScanPositionalIDs(rest, bdSubcmdValueFlags("create"), bdSubcmdBoolFlags("create"), nil); ambiguous {
		return nil, c.unreadable("create"), true
	}
	for _, flag := range []string{"--file", "-f", "--graph"} {
		if bdRigNamespaceFlagSet("create", "", rest, flag) {
			return nil, fmt.Sprintf("gc bd: refusing create %s: rig %s (federation = \"rig\") stamps every row gc creates, and a batch create cannot be stamped\n", flag, rigNamespaceLabel(c.ns)), true
		}
	}
	for _, flag := range []string{"--deps", "--repo"} {
		if bdRigNamespaceFlagSet("create", "", rest, flag) {
			return nil, fmt.Sprintf("gc bd: refusing create %s: rig %s (federation = \"rig\") cannot check what it writes outside the new row; use raw bd\n", flag, rigNamespaceLabel(c.ns)), true
		}
	}
	if n := len(bdflags.FlagValues(rest, bdSubcmdValueFlags("create"), "--metadata")); n > 1 {
		return nil, fmt.Sprintf("gc bd: refusing create with %d --metadata flags: bd keeps only the last, so the rig's creation stamp could be lost\n", n), true
	}
	writes, msg, refused := bdMetadataWrites("create", rest)
	if refused {
		return nil, msg, true
	}
	if msg, refused := c.checkStampKey("create", writes); refused {
		return nil, msg, true
	}
	for _, a := range bdflags.FlagValues(rest, bdSubcmdValueFlags("create"), "--assignee", "-a") {
		if strings.TrimSpace(a) != "" && !rigNamespacePinAdmitted(c.ns, a) {
			return nil, c.actorRefusal("create", "a new bead", a), true
		}
	}
	for _, w := range writes {
		if w.unset || strings.TrimSpace(w.value) == "" {
			continue
		}
		if isBdRigNamespaceAssigneeKey(w.key) && !rigNamespacePinAdmitted(c.ns, w.value) {
			return nil, c.actorRefusal("create", "a new bead", w.value), true
		}
		if isRigNamespaceRouteKey(w.key) && !rigNamespaceRouteAdmitted(c.ns, w.value) {
			return nil, c.routeRefusal("create", "a new bead", w), true
		}
	}
	out, err := bdArgsWithOwnerStamp(bdArgs, stamp)
	if err != nil {
		return nil, fmt.Sprintf("gc bd: refusing create: %v\n", err), true
	}
	return out, "", false
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
	subject := strings.Join(ids, ", ")
	if _, claims := bdflags.ClaimFlag(verb, rest); claims {
		if actor := c.claimActor(bdArgs); !rigNamespacePinAdmitted(c.ns, actor) {
			return nil, c.actorRefusal(verb, subject, actor), true
		}
	}
	for _, a := range bdflags.FlagValues(rest, bdSubcmdValueFlags(verb), "--assignee", "-a") {
		if strings.TrimSpace(a) != "" && !rigNamespacePinAdmitted(c.ns, a) {
			return nil, c.actorRefusal(verb, subject, a), true
		}
	}
	for _, w := range writes {
		if w.unset || strings.TrimSpace(w.value) == "" {
			continue
		}
		if isBdRigNamespaceAssigneeKey(w.key) && !rigNamespacePinAdmitted(c.ns, w.value) {
			return nil, c.actorRefusal(verb, subject, w.value), true
		}
		if !isRigNamespaceRouteKey(w.key) {
			continue
		}
		if !config.RigRouteQualified(w.value) {
			return nil, c.routeRefusal(verb, subject, w), true
		}
		if rigNamespaceRouteAdmitted(c.ns, w.value) {
			continue
		}
		// A route outside the namespace hands this city's own work off; on
		// any other row it would claim the row for an outside identity.
		for _, id := range ids {
			row, err := c.get(id)
			if err != nil || c.ns.classify(row) != rowOwned {
				return nil, c.routeRefusal(verb, id, w), true
			}
		}
	}
	return args, "", false
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
		row, err := c.get(id)
		if err != nil {
			return nil, c.unreadableRow(verb, id, err), true
		}
		if msg, foreign := c.foreignRow(verb, id, row); foreign {
			return nil, msg, true
		}
	}
	return bdArgs, "", false
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

// claimActor is the identity bd runs the command as: the last --actor, else
// the inherited BEADS_ACTOR.
func (c *bdRigNamespaceCheck) claimActor(bdArgs []string) string {
	verb, _ := bdflags.SplitGlobalFlags(bdArgs)
	value, _ := bdRigNamespaceManifest(verb)
	if value == nil {
		value = bdflags.GlobalValueFlags()
	}
	if vs := bdflags.FlagValues(bdArgs, value, "--actor"); len(vs) > 0 {
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
		switch name {
		case "--set-metadata", "--metadata", "--unset-metadata":
		default:
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
		switch name {
		case "--unset-metadata":
			out = append(out, bdMetadataWrite{key: value, unset: true})
		case "--set-metadata":
			k, v, ok := strings.Cut(value, "=")
			if !ok || strings.TrimSpace(k) == "" {
				return nil, fmt.Sprintf("gc bd: refusing malformed --set-metadata value %q before write\n", value), true
			}
			out = append(out, bdMetadataWrite{key: k, value: v})
		case "--metadata":
			raw := strings.TrimSpace(value)
			if strings.HasPrefix(raw, "@") {
				return nil, fmt.Sprintf("gc bd: refusing --metadata %q: @file input cannot be checked before write\n", value), true
			}
			var m map[string]json.RawMessage
			if err := json.Unmarshal([]byte(raw), &m); err != nil {
				return nil, fmt.Sprintf("gc bd: refusing malformed --metadata value before write: %v\n", err), true
			}
			for k, rv := range m {
				var v string
				if err := json.Unmarshal(rv, &v); err != nil {
					if strings.TrimSpace(k) == rigNamespaceOwnerStampKey || isRigNamespaceRouteKey(k) || isBdRigNamespaceAssigneeKey(k) {
						return nil, fmt.Sprintf("gc bd: refusing non-string %s before write\n", k), true
					}
					continue
				}
				out = append(out, bdMetadataWrite{key: k, value: v})
			}
		}
	}
	return out, "", false
}

// bdArgsWithOwnerStamp returns a create argv whose metadata carries the
// creation stamp: merged into its --metadata object when it has one,
// otherwise as a new --metadata flag right after the verb (so a "--"
// terminator never turns it into a positional).
func bdArgsWithOwnerStamp(bdArgs []string, stamp string) ([]string, error) {
	verbAt := -1
	globals := bdflags.GlobalValueFlags()
	for i := 0; i < len(bdArgs); i++ {
		a := bdArgs[i]
		if !strings.HasPrefix(a, "-") {
			verbAt = i
			break
		}
		if !strings.Contains(a, "=") && globals[a] && i+1 < len(bdArgs) {
			i++
		}
	}
	if verbAt < 0 {
		return nil, fmt.Errorf("cannot find the create verb")
	}
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
