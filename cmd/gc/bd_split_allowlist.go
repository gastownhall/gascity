package main

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/bdflags"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// On a split city `gc bd` is an ALLOWLIST. bd writes whatever it is told into
// the work store, and a bead that lands there with an infrastructure class is
// stranded: every later routed gc command refuses until the fleet is stopped
// and the row repaired. gc bd forwards only the command paths in
// bdSplitAllowlist, each with a vetted flag set; anything else is refused with
// a pointer to the gc-native command.
//
// The command gc judges is the command bd runs. gc matches the command tree of
// the bd binary it is about to run against the pinned inventories
// (internal/bdflags/inventory, dumped from bd's own cobra tree, hidden flags
// included; see bd_inventory_trust.go for the trust policy) and refuses a
// binary that matches none. It then resolves the argv with a
// mirror of that cobra tree under the cobra and pflag versions bd uses, so
// cobra's subcommand pre-scan and pflag's flag parse are bd's own. It also
// refuses an argv whose cobra reading and pflag reading disagree about which
// tokens name the command, and any short-flag cluster ending in a value flag
// that takes the next token (cobra's pre-scan and pflag read those
// differently).
//
// The mutating commands carry judges built on the one class-must-not-flip
// guard (coordclass.CheckFlip) and on the cross-store edge rule: an edge's
// every endpoint must be a bead of this work store that no class binding
// holds.
//
// Single-store cities never reach any of this: storeClassHolds answers nil
// there and gc bd forwards argv unchanged.

// bdSplitJudge decides an allowlisted command that can change a bead's class
// or write an edge.
type bdSplitJudge func(in bdClassGuardInput) bdClassGuard

// bdSplitCommand is one vetted command path.
type bdSplitCommand struct {
	// only, when non-nil, is the complete set of the command's own flags
	// (long names) that are vetted; every other flag of it is refused. nil
	// vets every flag bd declares for the command except those in exclude.
	only    []string
	exclude []string
	// judge, when set, decides the command; nil means it writes no field
	// coordclass.Classify reads and no edge.
	judge bdSplitJudge
	// why says what makes the command safe to forward.
	why string
}

const (
	bdSplitAltSling   = "gc sling <target> <formula> or gc formula cook <formula>, which put every bead in the store that holds its class"
	bdSplitAltRestore = "the split-city restore path in examples/bd/dolt/docs/journal-corruption-recovery.md, which ends in gc bd import <file> --skip-infra"
	bdSplitAltRequest = "if a workflow needs it, request an addition to the allowlist (docs/runbooks/split-storage-classes.md, \"gc bd on split cities\")"
	bdSplitAltBulk    = "this operation is not available through gc bd on a split city; follow the supported procedure in docs/runbooks/split-storage-classes.md (\"gc bd on split cities\") and run gc doctor afterwards"
)

// bdSplitAllowlist is every command path gc bd forwards on a split city.
var bdSplitAllowlist = map[string]bdSplitCommand{
	// Reads.
	"show":           {why: "read"},
	"list":           {why: "read (the relocated-class read guard runs first)"},
	"ready":          {why: "read (refused earlier by the relocated-class read guard unless overridden)"},
	"search":         {why: "read"},
	"query":          {why: "read (the relocated-class read guard runs first)"},
	"count":          {why: "read"},
	"children":       {why: "read"},
	"blocked":        {why: "read"},
	"stale":          {why: "read"},
	"status":         {why: "read"},
	"statuses":       {why: "read"},
	"types":          {why: "read"},
	"where":          {why: "read"},
	"info":           {why: "read"},
	"context":        {why: "read"},
	"version":        {why: "read"},
	"help":           {why: "help text"},
	"human":          {why: "help text"},
	"prime":          {why: "prints workflow context"},
	"history":        {why: "read"},
	"graph":          {why: "read"},
	"export":         {why: "writes JSONL out of the ledger"},
	"state":          {why: "read"},
	"comments":       {why: "lists comments"},
	"comments list":  {why: "lists comments"},
	"state list":     {why: "read"},
	"label list":     {why: "read"},
	"label list-all": {why: "read"},
	"dep list":       {why: "read"},
	"dep tree":       {why: "read"},
	"dep cycles":     {why: "read"},
	"mol current":    {why: "read (binding residents are refused by the by-id door)"},
	"mol show":       {why: "read"},
	"mol progress":   {why: "read"},
	"formula list":   {why: "read"},
	"formula show":   {why: "read"},
	"gate list":      {why: "read"},
	"config get":     {why: "read"},
	"config list":    {why: "read"},
	"config show":    {why: "read"},
	"backup status":  {why: "read"},
	"dolt status":    {why: "read"},
	"dolt show":      {why: "read"},

	// Writes that touch no field Classify reads.
	"close":        {why: "status and close reason"},
	"reopen":       {why: "status"},
	"delete":       {judge: judgeBdDelete, why: "removes beads; refused when a class-binding bead depends on one (--gc-allow-dangling-edges overrides)"},
	"assign":       {why: "assignee"},
	"priority":     {why: "priority"},
	"note":         {why: "appends notes"},
	"comment":      {why: "adds a comment row"},
	"comments add": {why: "adds a comment row"},
	"heartbeat":    {why: "pushes the claim lease forward"},
	"unclaim":      {why: "status and assignee"},
	"reclaim":      {why: "returns stale in_progress beads to ready"},
	"defer":        {why: "status and defer_until"},
	"undefer":      {why: "status and defer_until"},
	"link":         {judge: judgeBdEdgeVerb, why: "a dependency edge (its -t is the edge type); both endpoints must be work-store beads"},
	"dep add":      {judge: judgeBdEdgeVerb, why: "a dependency edge (its -t is the edge type); every endpoint must be a work-store bead; --file is refused"},
	"dep remove":   {why: "a dependency edge"},
	"dep relate":   {judge: judgeBdEdgeVerb, why: "a relates-to edge; both endpoints must be work-store beads"},
	"dep unrelate": {why: "a relates-to edge"},
	"promote":      {why: "moves a wisp row to the durable table with its type, labels and metadata unchanged"},
	"duplicate":    {judge: judgeBdEdgeVerb, why: "closes the bead and records an edge; both endpoints must be work-store beads"},
	"supersede":    {judge: judgeBdEdgeVerb, why: "closes the bead and records an edge; both endpoints must be work-store beads"},
	"purge":        {judge: judgeBdSweep, why: "deletes closed wisps (the core reaper); bd's dry run does not name them, so gc cannot check them against the class binding one by one, and a run that deletes must give an --older-than of at least a week, which keeps it clear of a bead an open cross-store drain gate still waits on"},
	"prune":        {judge: judgeBdSweep, why: "deletes old closed beads (the core reaper's session prune); held to age as purge, and --ignore-references is refused"},
	"gate create":  {judge: judgeBdEdgeVerb, why: "mints a type=gate bead (work class); its --type is the gate's await type; the bead it --blocks must be a work-store bead"},
	"gate resolve": {why: "closes a gate"},
	"gate check":   {why: "resolves and escalates gates (the core gate-sweep order)"},
	"mol wisp gc":  {judge: judgeBdMolWispGC, why: "deletes abandoned wisps (the deacon prompt); refused when a class-binding bead depends on one of the wisps bd's own dry run selects, and otherwise forwarded as bd's delete of exactly the wisps gc checked; --closed --force and an --age under an hour are refused"},
	"mol burn":     {judge: judgeBdMolBurn, why: "deletes a molecule (gastown patrols); every id must name exactly one work-store bead, and it is refused when a class-binding bead depends on a bead it could delete: the molecule's beads and, for a persistent molecule, every bead that depends on one of them; binding residents are refused by the by-id door"},
	"backup sync":  {why: "pushes the store to its backup; writes nothing into the store"},
	"backup init":  {why: "configures the backup destination (the dolt pack's backup order)"},
	"dolt start":   {why: "starts the scope's server"},
	"dolt stop":    {why: "stops the scope's server"},
	"dolt push":    {why: "pushes to the remote; writes nothing into the store"},
	"dolt commit":  {why: "commits the working set already written"},
	"config set":   {judge: judgeBdConfigSet, why: "configuration, not bead rows; only the keys in bdSplitConfigSetKeys"},
	"init": {
		judge: judgeBdInit,
		only:  []string{"--prefix", "--non-interactive", "--skip-hooks", "--skip-agents", "--init-if-missing", "--recreate-missing"},
		why:   "creates an empty store (the journal-corruption runbook); --from-jsonl, the backend/server/remote flags and the global --db are not vetted",
	},

	// Writes judged by the class-must-not-flip guard.
	"create":          {judge: judgeBdCreate, why: "judged: the bead's type, labels and metadata are classified"},
	"q":               {judge: judgeBdCreate, why: "judged: the bead's type and labels are classified"},
	"update":          {judge: judgeBdUpdate, why: "judged: type/label/metadata writes are classified against the bead they land on"},
	"label add":       {judge: judgeBdLabelAddRemove, why: "judged against the bead"},
	"label remove":    {judge: judgeBdLabelAddRemove, why: "judged against the bead"},
	"label propagate": {judge: judgeBdLabelPropagate, why: "judged: a class-marker label is refused"},
	"tag":             {judge: judgeBdTag, why: "judged against the bead"},
	"set-state":       {judge: judgeBdSetState, why: "judged against the bead"},
	"import":          {judge: judgeBdImport, why: "judged: rows of a class the store does not hold are refused or left out (--skip-infra)"},
	"batch":           {judge: judgeBdBatch, why: "judged: every create line is classified; update lines may set only class-free keys"},
	"sql":             {judge: judgeBdSQL, why: "judged: a single plain read statement only"},
	"mol wisp": {
		only:  []string{"--var", "--root-only"},
		judge: judgeBdMolWisp,
		why:   "gastown patrols; root-only only: the one bead it creates is judged in process",
	},
	"mol wisp create": {
		only:  []string{"--var", "--root-only"},
		judge: judgeBdMolWisp,
		why:   "as mol wisp",
	},
	"cook": {judge: judgeBdCook, why: "prints a resolved formula; --persist (which writes a proto) is refused"},
}

// bdSplitAllowedAliases are the aliases gc bd forwards on a split city, each
// mapped to the allowlisted path bd runs it as. Every other alias in the
// inventory is refused (bdSplitRefusedAliases).
var bdSplitAllowedAliases = map[string]string{
	"new":    "create",
	"done":   "close",
	"view":   "show",
	"hb":     "heartbeat",
	"stats":  "status",
	"dep rm": "dep remove",
}

// bdSplitRefusedAliases are the aliases refused on a split city, and why.
var bdSplitRefusedAliases = map[string]string{
	"protomolecule":      "an alias of mol; use mol",
	"mol fart":           "an alias of mol bond, which is refused",
	"find-dups":          "an alias of find-duplicates, which is not allowlisted",
	"formula primitives": "an alias of formula schema, which is not allowlisted",
	"backup add":         "an alias of backup init; use backup init",
	"backup rm":          "an alias of backup remove, which is not allowlisted",
}

// bdSplitRefusal is the decision for a command path that is not allowlisted.
type bdSplitRefusal struct {
	why string
	alt string
}

// bdSplitRefusals decides every command path that is not allowlisted. A key is
// a path ("backup restore") or a whole verb ("jira"), which decides every path
// under it that is neither allowlisted nor decided more specifically.
var bdSplitRefusals = map[string]bdSplitRefusal{
	"mol pour":          {"it instantiates every step of a formula or proto, and bd copies each step's type, labels and metadata onto the beads it mints", bdSplitAltSling},
	"mol bond":          {"it instantiates a formula or proto into an existing molecule", bdSplitAltSling},
	"mol":               {"molecule maintenance outside the patrol verbs is not vetted", bdSplitAltSling},
	"mol wisp list":     {"not vetted", bdSplitAltRequest},
	"backup restore":    {"it replaces the work store with the backup, and a backup taken before the split brings back every infrastructure row of that time; gc cannot verify a backup's age against the split", bdSplitAltRestore},
	"backup remove":     {"not vetted", bdSplitAltRequest},
	"backup":            {"not vetted", bdSplitAltRequest},
	"dolt pull":         {"it merges another revision into the work store wholesale, which gc cannot judge row by row", bdSplitAltBulk},
	"dolt":              {"server and remote administration beyond start/stop/status/show/push/commit is not vetted", bdSplitAltBulk},
	"vc":                {"a merge brings another revision into the work store wholesale", bdSplitAltBulk},
	"federation":        {"a federation sync merges a peer's rows into the work store wholesale", bdSplitAltBulk},
	"sync":              {"it pulls and merges another revision into the work store wholesale", bdSplitAltBulk},
	"branch":            {"switching or creating branches changes which revision the work store serves", bdSplitAltBulk},
	"conflicts":         {"resolving merge conflicts writes whichever side wins, unjudged", bdSplitAltBulk},
	"restore":           {"not vetted", bdSplitAltRequest},
	"migrate":           {"schema and backend migrations are not vetted", bdSplitAltBulk},
	"migrate-personal":  {"not vetted", bdSplitAltRequest},
	"admin":             {"database administration (reset, cleanup) is not vetted", bdSplitAltBulk},
	"compact":           {"not vetted", bdSplitAltBulk},
	"flatten":           {"not vetted", bdSplitAltBulk},
	"gc":                {"not vetted", bdSplitAltBulk},
	"rename":            {"not vetted", bdSplitAltRequest},
	"rename-prefix":     {"not vetted", bdSplitAltBulk},
	"recompute-blocked": {"not vetted", bdSplitAltRequest},
	"bootstrap":         {"not vetted", bdSplitAltBulk},
	"doctor":            {"its repairs write the store unjudged", bdSplitAltRequest},
	"serve":             {"bd serve exposes bd's write API to any HTTP client, and none of those writes passes through gc", "the gc API (gc dashboard / the supervisor API), which routes writes by class"},
	"connect":           {"it attaches the workspace to a bd serve gc does not route", bdSplitAltRequest},
	"create-form":       {"it asks for the bead's fields interactively, so gc cannot classify the bead before bd writes it", "gc bd create with flags"},
	"edit":              {"it edits through $EDITOR, which gc cannot judge", "gc bd update with flags"},
	"todo":              {"not vetted", "gc bd create / gc bd close"},
	"epic":              {"not vetted", bdSplitAltRequest},
	"swarm":             {"it creates a swarm molecule from an epic", bdSplitAltSling},
	"merge-slot":        {"not vetted", bdSplitAltRequest},
	"gate":              {"gate administration beyond create/resolve/check/list is not vetted", bdSplitAltRequest},
	"ship":              {"not vetted", bdSplitAltRequest},
	"provenance":        {"not vetted", bdSplitAltRequest},
	"events":            {"not vetted", bdSplitAltRequest},
	"audit":             {"not vetted", bdSplitAltRequest},
	"kv":                {"not vetted", bdSplitAltRequest},
	"config":            {"configuration changes beyond get/set/list/show are not vetted", bdSplitAltRequest},
	"remember":          {"not vetted", bdSplitAltRequest},
	"forget":            {"not vetted", bdSplitAltRequest},
	"memories":          {"not vetted", bdSplitAltRequest},
	"recall":            {"not vetted", bdSplitAltRequest},
	"formula":           {"not vetted", bdSplitAltRequest},
	"label":             {"not vetted", bdSplitAltRequest},
	"dep":               {"not vetted", bdSplitAltRequest},
	"graph check":       {"not vetted", bdSplitAltRequest},
	"human dismiss":     {"not vetted", bdSplitAltRequest},
	"human respond":     {"not vetted", bdSplitAltRequest},
	"human list":        {"not vetted", bdSplitAltRequest},
	"human stats":       {"not vetted", bdSplitAltRequest},
	"jira":              {"an external-tracker sync writes the tracker's labels and types onto work-store beads unjudged", bdSplitAltRequest},
	"linear":            {"an external-tracker sync writes the tracker's labels and types onto work-store beads unjudged", bdSplitAltRequest},
	"github":            {"an external-tracker sync writes the tracker's labels and types onto work-store beads unjudged", bdSplitAltRequest},
	"gitlab":            {"an external-tracker sync writes the tracker's labels and types onto work-store beads unjudged", bdSplitAltRequest},
	"ado":               {"an external-tracker sync writes the tracker's labels and types onto work-store beads unjudged", bdSplitAltRequest},
	"notion":            {"an external-tracker sync writes the tracker's labels and types onto work-store beads unjudged", bdSplitAltRequest},
	"mail":              {"it delegates to an external mail program", "gc mail"},
	"find-duplicates":   {"not vetted", bdSplitAltRequest},
	"duplicates":        {"it can merge duplicates", bdSplitAltRequest},
	"lint":              {"not vetted", bdSplitAltRequest},
	"orphans":           {"not vetted", bdSplitAltRequest},
	"preflight":         {"not vetted", bdSplitAltRequest},
	"diff":              {"not vetted", bdSplitAltRequest},
	"schema":            {"not vetted", bdSplitAltRequest},
	"hooks":             {"not vetted", bdSplitAltRequest},
	"setup":             {"not vetted", bdSplitAltRequest},
	"onboard":           {"not vetted", bdSplitAltRequest},
	"quickstart":        {"not vetted", bdSplitAltRequest},
	"init-safety":       {"not vetted", bdSplitAltRequest},
	"completion":        {"not vetted", bdSplitAltRequest},
	"metrics":           {"not vetted", bdSplitAltRequest},
	"upgrade":           {"not vetted", bdSplitAltRequest},
	"worktree":          {"not vetted", bdSplitAltRequest},
	"repo":              {"not vetted", bdSplitAltRequest},
	"rules":             {"not vetted", bdSplitAltRequest},
	"ping":              {"not vetted", bdSplitAltRequest},
	"companion":         {"not vetted", bdSplitAltRequest},
	"getToken":          {"not vetted", bdSplitAltRequest},
	"login":             {"not vetted", bdSplitAltRequest},
	"logout":            {"not vetted", bdSplitAltRequest},
	"profile":           {"not vetted", bdSplitAltRequest},
	"whoami":            {"not vetted", bdSplitAltRequest},
	"codex-hook":        {"a hidden editor hook", bdSplitAltRequest},
	"cursor-hook":       {"a hidden editor hook", bdSplitAltRequest},
	"db-proxy-child":    {"a hidden database proxy process", bdSplitAltRequest},
	"send-metrics":      {"a hidden metrics sender", bdSplitAltRequest},
	"migrate-issues":    {"a hidden spelling of migrate issues, which is not vetted", bdSplitAltBulk},
	"__gw-credential":   {"a hidden enterprise credential helper", bdSplitAltRequest},
	"observer":          {"enterprise observer administration is not vetted", bdSplitAltRequest},
}

// bdSplitRedirectFlags are the flags, on any command, that point bd at a
// store, workspace, repo or server other than the one gc resolved and judged.
// They are refused on a split city wherever they appear.
// TestGcBdSplitRefusesEveryRedirectFlag holds every path/dir/repo/db/server/
// remote-shaped flag of the inventory to a decision: here, or in
// bdSplitReviewedPathFlags.
var bdSplitRedirectFlags = map[string]string{
	"--directory":      "it replaces BEADS_DIR, so bd opens a different workspace",
	"--db":             "it opens a different database",
	"--database":       "it opens a different database",
	"--profile":        "a named storage profile can name a different store",
	"--server-url":     "it runs against another bd serve",
	"--global":         "it uses the shared global database",
	"--repo":           "it targets a different repository",
	"--remote":         "it names a remote, outside the store gc judged",
	"--search-path":    "it reads formulas from paths gc did not resolve",
	"--backend":        "it selects a different storage backend",
	"--server":         "it selects server mode",
	"--server-host":    "it names a server",
	"--server-port":    "it names a server",
	"--server-socket":  "it names a server",
	"--server-user":    "it names a server",
	"--server-tls":     "it names a server",
	"--shared-server":  "it selects a shared server",
	"--team-server":    "it selects a team server",
	"--pg-url":         "it names a database",
	"--mysql-url":      "it names a database",
	"--mysql-database": "it names a database",
	"--sqlite-path":    "it names a database",
	"--from-jsonl":     "it imports rows unjudged",
	"--discard-remote": "it replaces remote history",
	"--destroy-token":  "it authorizes destroying a store",
}

// bdSplitReviewedPathFlags are path-shaped flags that do NOT redirect the
// store, and why. TestGcBdSplitRefusesEveryRedirectFlag requires every
// path/dir/repo/db/server/remote-shaped flag name in the inventory to be in
// one of the two maps (or a proxied-server init flag, all of which init's
// vetted set excludes).
var bdSplitReviewedPathFlags = map[string]string{
	"--body-file":        "reads the description text from a file",
	"--description-file": "reads the description text from a file",
	"--design-file":      "reads the design text from a file",
	"--reason-file":      "reads the close reason from a file",
	"--file":             "an input file: refused by the judge on create and dep add, judged on batch, comment text elsewhere",
	"--from-file":        "reads the ids to delete from a file",
	"--input":            "the import source, judged",
	"--output":           "where export writes its JSONL",
	"--mem-profile":      "where bd writes a heap profile",
	"--cpu-profile":      "turns on CPU profiling",
	"--sandbox":          "disables auto-push",
	"--export":           "prime output mode",
	"--direction":        "dep traversal direction",
	"--show-all-paths":   "dep tree output",
	"--root-only":        "mol wisp: create only the root",
	"--no-auto":          "close: skip auto-close",
	"--event-target":     "an event bead's target field",
	"--prefix":           "an id or label prefix (label remove, cook, init)",
	"--agents-file":      "init agent setup (not vetted for init)",
	"--agents-profile":   "init agent setup (not vetted for init)",
	"--proxied-server":   "init server mode (not vetted for init)",
}

// bdSplitConfigSetKeys are the config keys `gc bd config set` may write on a
// split city. Every other key is refused: storage-class.*, routing.*, dolt.*,
// backup.*, sync.*, federation.* and the like redirect writes or change what
// create does.
var bdSplitConfigSetKeys = map[string]string{
	"types.custom": "the custom bead types the store accepts (troubleshooting.md)",
	"types.infra":  "which types list hides without --include-infra (troubleshooting.md)",
}

// bdSplitRefusalFor returns the decision for a path that is not allowlisted:
// its own, else its verb's.
func bdSplitRefusalFor(path string) (bdSplitRefusal, bool) {
	if r, ok := bdSplitRefusals[path]; ok {
		return r, true
	}
	verb, _, _ := strings.Cut(path, " ")
	r, ok := bdSplitRefusals[verb]
	return r, ok
}

// bdSplitVetted reports whether long is a vetted flag of cmd.
func bdSplitVetted(cmd bdSplitCommand, local map[string]bool, long string) (bool, string) {
	if why, refused := bdSplitRedirectFlags[long]; refused {
		return false, why
	}
	if !local[long] {
		return true, "" // a global flag, not a redirect
	}
	if cmd.only != nil && !slices.Contains(cmd.only, long) {
		return false, "not vetted for this command"
	}
	if slices.Contains(cmd.exclude, long) {
		return false, "not vetted for this command"
	}
	return true, ""
}

// bdSplitArgs are an allowlisted command's parsed flags and positionals.
type bdSplitArgs struct {
	flags       map[string][]string
	positionals []string
}

// Values returns the values given to the long flag (a slice flag's elements,
// as pflag split them).
func (a bdSplitArgs) Values(long string) []string { return a.flags[long] }

// Last returns the long flag's value and whether it was given.
func (a bdSplitArgs) Last(long string) (string, bool) {
	v := a.flags[long]
	if len(v) == 0 {
		return "", false
	}
	return v[len(v)-1], true
}

// Bool reports a bool flag's value.
func (a bdSplitArgs) Bool(long string) bool {
	v, ok := a.Last(long)
	return ok && v == "true"
}

// bdSplitResolution is a gc bd argv resolved against the allowlist.
type bdSplitResolution struct {
	path    string
	cmd     bdSplitCommand
	args    bdSplitArgs
	refusal string
}

// resolveBdSplitCommand resolves bdArgs exactly as the bd build inv describes
// would, and checks the result against the allowlist.
func resolveBdSplitCommand(inv *bdflags.Inventory, bdArgs []string, scopeRoot string) bdSplitResolution {
	refuse := func(format string, a ...any) bdSplitResolution {
		return bdSplitResolution{refusal: fmt.Sprintf(format, a...)}
	}
	res, err := inv.Resolve(bdArgs)
	if err != nil {
		return refuse("gc bd: refused on this split city: bd would reject this argv (%v), and gc forwards only vetted commands and flags here. Nothing was written.", err)
	}
	path := res.Path
	label := path
	if label == "" {
		label = "(no command)"
	}
	if res.DisableFlagParsing {
		r, _ := bdSplitRefusalFor(path)
		return refuse("gc bd %s: refused on this split city: bd does not parse this command's flags, so gc cannot judge them; %s. Instead: %s. Nothing was written.", label, r.why, r.alt)
	}
	tokens := strings.Fields(path)
	// The words that name the command, as pflag reads the argv (flag values
	// consumed, interspersed positionals kept in order). cobra's lookup and
	// pflag must agree: the leading pflag positionals must spell bd's command
	// path word for word (a name or an alias of each command on it). They
	// disagree when a flag value spells a command name, or a short-flag run's
	// last flag takes the next token.
	specs := inv.FlagSpecs(path)
	for _, tok := range bdArgs {
		if tok == "--" {
			break
		}
		if bdflags.ClusterTakesNextToken(specs, tok) {
			return refuse("gc bd %s: refused on this split city: the short-flag run %q ends in a flag that takes the next argument, which bd's command lookup and its flag parser read differently. Spell the value flag on its own (-X value). Nothing was written.", label, tok)
		}
	}
	parsed, err := bdflags.ParseArgs(specs, bdArgs)
	if err != nil {
		return refuse("gc bd %s: refused on this split city: %v. Nothing was written.", label, err)
	}
	if len(parsed.Positionals) < len(tokens) {
		return refuse("gc bd %s: refused on this split city: bd's command lookup and its flag parser disagree about which arguments name the command. Nothing was written.", label)
	}
	for i, word := range parsed.Positionals[:len(tokens)] {
		cmdPath := strings.Join(tokens[:i+1], " ")
		if word == tokens[i] {
			continue
		}
		if !slices.Contains(inv.Command(cmdPath).Aliases, word) {
			return refuse("gc bd %s: refused on this split city: bd's command lookup and its flag parser disagree about which arguments name the command (%q is read as the command by one and as an argument by the other). Nothing was written.", label, word)
		}
		if res.Help {
			continue
		}
		alias := strings.TrimSpace(strings.Join(tokens[:i], " ") + " " + word)
		if mapped, ok := bdSplitAllowedAliases[alias]; ok && mapped == cmdPath {
			continue
		}
		why, known := bdSplitRefusedAliases[alias]
		if !known {
			why = "an alias gc has not vetted"
		}
		return refuse("gc bd %s: refused on this split city: %q is %s. gc forwards only explicitly vetted aliases here. Nothing was written.", alias, word, why)
	}
	if res.Help {
		// bd prints the command's help and runs nothing.
		return bdSplitResolution{path: path, args: bdSplitArgs{flags: res.Flags, positionals: res.Positionals}}
	}
	cmd, allowed := bdSplitAllowlist[path]
	if path == "" {
		if len(res.Positionals) > 0 {
			return refuse("gc bd %s: refused on this split city: %q is not a bd command. Nothing was written.", res.Positionals[0], res.Positionals[0])
		}
		cmd, allowed = bdSplitCommand{why: "bd's root prints its help or version"}, true
	}
	if !allowed {
		r, decided := bdSplitRefusalFor(path)
		if !decided {
			r = bdSplitRefusal{"it has no decision", bdSplitAltRequest}
		}
		return refuse("gc bd %s: refused on this split city: %s. Instead: %s. Nothing was written.", path, r.why, strings.ReplaceAll(r.alt, "the scope", scopeRoot))
	}
	local := map[string]bool{}
	for _, f := range inv.Command(path).Local {
		local["--"+f.Name] = true
	}
	for _, long := range slices.Sorted(maps.Keys(res.Flags)) {
		if ok, why := bdSplitVetted(cmd, local, long); !ok {
			return refuse("gc bd %s: refused on this split city: %s is not allowed here (%s). gc forwards only vetted flags for %s (%s). Nothing was written.", label, long, why, label, bdSplitAltRequest)
		}
	}
	return bdSplitResolution{path: path, cmd: cmd, args: bdSplitArgs{flags: res.Flags, positionals: res.Positionals}}
}

// bdReadResult is what a pre-read of a bead found.
type bdReadResult int

const (
	// bdReadFound: exactly the bead the id names was read.
	bdReadFound bdReadResult = iota
	// bdReadNotFound: the store answered that no bead has this id.
	bdReadNotFound
	// bdReadCollision: bd resolved the id (a short or partial one) to a
	// DIFFERENT bead. bd's write would land on that bead, so it is a
	// refusal, never a not-found.
	bdReadCollision
	// bdReadFailed: the store could not answer.
	bdReadFailed
)

// bdClassGuardDeps are the guard's views of the world, injected so the guard
// itself runs nothing.
type bdClassGuardDeps struct {
	// stdin is what bd would read; the guard reads it for import - and batch.
	stdin io.Reader
	// inventory returns the pinned inventory the bd gc is about to run
	// matches (bd_inventory_trust.go), or an error.
	inventory func() (*bdflags.Inventory, error)
	// read returns the bead id resolves to in the work store, collision-aware.
	read func(id string) (beads.Bead, bdReadResult)
	// endpoints reads edge endpoints across the city's WORK stores (the
	// city's and every rig's, by id prefix; a cross-rig dependency is a
	// shipped order and stays allowed), batched per store. nil falls back to
	// read, one id at a time, in this scope's store.
	endpoints func(ids []string) map[string]bdReadResult
	// held reports which ids a class binding holds (a migrated relic keeps a
	// work-shaped id), over one topology built for the whole call.
	held func(ids []string) (map[string]bool, error)
	// bindingDependents returns, per id, the binding beads that depend on it
	// (a reverse lookup in the class binding).
	bindingDependents func(ids []string) (map[string][]string, error)
	// templates lists the work store's template-labeled beads (bd's legacy
	// proto lookup for a mol-* name).
	templates func() ([]beads.Bead, error)
	// bdRead runs a read-only bd command exactly as gc bd would run the
	// command being judged (the scope's bd binary, root and environment) and
	// returns its stdout. The delete-shaped judges ask bd itself which beads
	// it would remove: a molecule's members (`list --parent`) and the wisps a
	// wisp gc selects (its own --dry-run).
	bdRead func(args ...string) ([]byte, error)
}

// bdClassGuardInput is everything a judge may consult.
type bdClassGuardInput struct {
	path   string
	args   bdSplitArgs
	bdArgs []string // the argv to forward, gc's own flag removed
	// gcFlag reports the verb's gc-only flag (import --skip-infra, delete
	// --gc-allow-dangling-edges).
	gcFlag    bool
	holds     []coordclass.Class
	scopeRoot string
	bdClassGuardDeps
}

func (in bdClassGuardInput) refuse(format string, a ...any) bdClassGuard {
	return bdClassGuard{refusal: fmt.Sprintf(format, a...)}
}

func (in bdClassGuardInput) pass() bdClassGuard { return bdClassGuard{args: in.bdArgs} }

// bdClassFlipGuard is gc bd's split-city gate: the bd build check, the
// allowlist, then the allowlisted command's judge. It runs after the by-id
// door, which has already served or refused every binding resident. A
// single-store city passes everything through unchanged (gc's own
// --skip-infra on import is dropped).
func bdClassFlipGuard(cfg *config.City, scopeRoot string, bdArgs []string, deps bdClassGuardDeps) bdClassGuard {
	holds := workStoreClassHolds(cfg)
	args, gcFlag := stripGCOwnFlags(bdArgs)
	if holds == nil {
		return bdClassGuard{args: args}
	}
	if deps.inventory == nil {
		return bdClassGuard{kind: bdRefusalUninventoried, refusal: "gc bd: refused on this split city: gc could not determine which bd command tree it would run. Run `gc doctor` to check this city's bd. Nothing was written."}
	}
	inv, err := deps.inventory()
	if err != nil || inv == nil {
		return bdClassGuard{kind: bdRefusalUninventoried, refusal: fmt.Sprintf("gc bd: refused on this split city: %v. gc forwards gc bd only to a bd whose command tree matches a pinned, vetted inventory, because a different tree can have commands and flags gc has not vetted. Run `gc doctor` to see the differences, install a pinned bd build, or have the gc maintainers inventory this build (internal/bdflags/inventorygen). Nothing was written.", err)}
	}
	res := resolveBdSplitCommand(inv, args, scopeRoot)
	if res.refusal != "" {
		return bdClassGuard{kind: bdRefusalAllowlist, path: res.path, refusal: res.refusal}
	}
	if res.cmd.judge == nil {
		return bdClassGuard{args: args}
	}
	g := res.cmd.judge(bdClassGuardInput{
		path: res.path, args: res.args, bdArgs: args, gcFlag: gcFlag, holds: holds, scopeRoot: scopeRoot, bdClassGuardDeps: deps,
	})
	if g.refusal != "" {
		g.kind, g.path = bdRefusalJudge, res.path
	}
	return g
}

// bdSplitAllowlistPaths returns the allowlisted paths, sorted (for the docs
// test and the coverage report).
func bdSplitAllowlistPaths() []string {
	paths := slices.Collect(maps.Keys(bdSplitAllowlist))
	sort.Strings(paths)
	return paths
}
