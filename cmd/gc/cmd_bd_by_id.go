package main

// The in-process by-ID surface for `gc bd`.
//
// `gc bd` resolves a WORK scope and hands the command to the `bd` subprocess.
// On a city whose coordination classes are served from a non-work [storage]
// binding, an infrastructure bead lives in that binding and not in the work
// workspace bd is pointed at, so a by-ID read of one is answered by a
// subprocess that cannot see it: bd opens the work backend instead, which on a
// managed-Dolt city means a connect attempt that can block for the whole
// command, and otherwise means an empty answer indistinguishable from a real
// one.
//
// So an id whose owning class is the relocated binding is answered HERE,
// through that class's closed front door, and the subprocess is never spawned.
//
// # Where the store comes from, and why not from the migration
//
// The store is resolved through the one-shot storage funnel
// (cli_storage_routes.go), which takes the same verdict the controller takes at
// boot and opens the binding through the planned provider's own EngineOpener.
//
// The obvious alternative — resolveInfraBindingTarget, the migration's own
// destination resolver — is the defect this file was written to avoid, and it
// is worth naming because it looks like the right function. That resolver
// answers "is this a binding THIS BUILD can migrate onto", which is true only
// of the built-in SQLite engine: it returns not-configured for a beads
// workspace, for a fork's own engine, and for every provider added after it.
// A by-ID surface gated on that answer routes correctly for one provider and
// falls through to the subprocess for all the others — which is the wrong-answer
// lane this surface exists to close, reappearing on exactly the cities that
// moved furthest from the default. The question a READ has is not "can I
// migrate this", it is "where is this class served from", and the funnel
// answers that for every provider because the answer comes from the provider.
//
// # This IS the shared candidate list, with the work leg served by bd
//
// storeref.ClassCandidates is the tree's by-id resolver, and its answer for a
// relocated city is [class, work] — the class store first, because it is the
// reserved namespace's AUTHORITY, then the work store, because authority is not
// exclusive possession. The class store is not the namespace's only MINTER
// either: a rig configured with a prefix inside one mints work beads there, and
// `gc storage migrate` preserved ids in the other direction. Leading is a
// statement about probe ORDER, never about who could have created the id. This
// surface probes exactly that list in exactly that order.
// What differs is only how the work leg is READ: here it is the `bd`
// subprocess, which is why the leg appears as a fall-through rather than as a
// beads.Store. The in-process form of the same list, for the one-shot commands
// that hold two ordinary stores, is by_id_store_route.go.
//
// The class leg is probed for EVERY id, not only for ids inside the class
// namespace, and that is the one place this surface is deliberately STRONGER
// than the shared resolver rather than merely equal to it. ClassCandidates
// gates on the namespace before it builds a list (IDInNamespace), and `gc
// storage migrate` preserves ids — so a bead the migration relocated keeps its
// HQ/rig-era prefix and the resolver returns nil for exactly the ids that
// moved. Only a residence probe reaches those, and without it their reads are
// answered from the work store's retained pre-migration copy, frozen at
// migration time. See resolve.
//
// # Three deliberate properties
//
//   - The routed operations take ONLY the closed contract
//     (storebinding.GraphStore). They cannot reach a raw bead store through
//     their parameter, so claim/release CAS lives in the store the contract is
//     bound to and is never re-implemented as a read-then-write in the CLI.
//   - A binding MISS is a fall-through, for every id. The binding is the
//     authority for its reserved namespaces, not their only lawful holder: a
//     rig may be configured with a prefix inside one (config.ValidateRigs
//     allows it and ReservedPrefixWarnings only advises), and `gc storage
//     migrate` preserved ids in the other direction. So "the binding does not
//     hold it" is a statement about the binding, and the ledger the passthrough
//     is pointed at answers next. Answering absence here instead stranded every
//     read and every write addressed at such a rig's own beads.
//   - A resolution failure surfaces. Reading "the binding could not be opened"
//     as "the bead is not there" is the root-loss shape this whole lane exists
//     to prevent. On a city this build must not serve, an id only the class
//     binding could own gets the boot refusal that names the remedy — never
//     absence, and never the work store's answer. A WORK id on that same city
//     keeps its existing path, because the refusal is a fact about the city's
//     storage and says nothing about a bead the work ledger still serves. See
//     resolve.
//
// Served here: show, update (fields and metadata, including --claim), close,
// reopen, release-if-current, dep list, and dep tree. `gc bd heartbeat` is not — it
// forwards to bd's native owner-only lease-refresh verb (commit 80aad8c), a
// literal spelling parseBdByIDOp does not recognize, and it is no longer
// rewritten to a metadata update the way it once was. On a city that relocates
// the owning class a heartbeat of a bead the binding HOLDS is not a
// fall-through: the ownership gate below (bdArgsAddressedClassIDs) puts the id
// to the residence walk, and a hit is refused by refuseClassOwnedTarget, which
// names the routing cause rather than letting the work store answer with bd's
// not-found. A miss falls through with every other binding miss, so a rig whose
// prefix shadows the namespace keeps bd's own heartbeat against its own ledger.
// The general query surface is likewise unserved, and
// bd_relocated_classes.go refuses it on its own terms.
//
// close and reopen are here for the same reason update is, one lifecycle step
// further on. A class store MINTS ids from its binding workspace's own prefix,
// so a synthetic it creates with no id — an input convoy, a patrol root, a
// wisp — carries a WORK prefix while residing only in the binding. Reads found
// those (the class leg is probed for every id); the close that would retire one
// was not a by-ID verb at all, so it never opened this door, fell through to a
// subprocess pointed at the prefix store, and died there with bd's not-found.
// Every such open bead was a permanent ready-frontier polluter with no
// supported drain path.
//
// A close this door serves does NOT reach the ADR-0009 work-record close gate,
// which doBd runs later. The coverage boundary and why it is sound are recorded
// where the gate defines itself, in work_record_gate.go's header.
//
// # Ownership is decided before servability
//
// An operation that is NOT served but whose subject the class binding HOLDS
// does not fall through either: bd would open the work store, which cannot see
// the bead, and the command would hang or answer about the wrong workspace. It
// is refused instead, before the subprocess.
//
// Ownership is one question with one answer: RESIDENCE. A reserved prefix is
// what decides which ids get ASKED — bdArgsAddressedClassIDs reads them from the
// argv, so every reserved id is put to the binding even in an invocation the
// mutation scanner cannot reduce to subjects — and residence is what decides
// the answer. Reading the prefix as the answer instead was the defect this
// door carried: the binding is its namespaces' authority, not their only lawful
// holder, so every id a shadow-prefixed rig minted was refused on a proof that
// did not hold. The other half of the population runs the same walk from the
// other end: a MUTATION whose positional ids are unambiguous is probed even
// though its ids carry a work prefix, because `gc storage migrate` preserved
// ids and its relics reside in the binding. Without that a work-prefixed class
// resident got no protection at all — its `--notes` update or its `delete` fell
// through to a ledger that cannot hold it and died with bd's not-found, which
// reads as "the bead is gone" rather than "you addressed the wrong store".
//
// Ownership and servability are answered by different code on purpose, and
// answering them with one function was a real defect rather than a
// hypothetical. The served parsers reject every flag they do not implement, so
// "can this be served" said no to `show --long`, `show --id`, `dep list -t` and
// the rest of bd's own manifest — and while a rejection meant fall-through,
// those were precisely the invocations sent to the ledger that cannot answer.
// Which ids to ask about is now decided by bdArgsAddressedClassIDs, which reads
// only the argv and knows nothing about what this surface implements.
//
// The id asked about is read from an ID POSITION and never from an id-shaped
// value. A `gc bd list --metadata-field workflow_id=gcg-…` probe quotes a class
// id rather than addressing one, so this surface declines it — OWNERSHIP is not
// what is wrong with it. A `--parent gcg-…` DOES address one, so it is put to
// the binding — and the binding's answer, not the prefix, is what happens next.
// A parent the binding holds is refused, because letting the work store answer
// that returns a silent empty result. A parent it cleanly misses falls through,
// and bd runs the create against the work ledger, which is the right ledger for
// the population this door re-routed: a shadow-prefix rig referencing its own
// bead. An id no ledger holds is bd's own dep validation to reject, not this
// door's to guess about.
//
// Declining is not forwarding. That same selector is refused one pre-flight
// earlier, by bd_relocated_classes.go, and for a different reason: `list` is a
// PROJECTION over a class this ledger cannot see, so its `[]` is a confident
// wrong answer whatever the id's position. The two doors ask different
// questions — "who owns this bead" and "can this ledger answer this verb at
// all" — and only the first one is answered here.
//
// # What entering costs, and who pays it
//
// Resolving the funnel loads the city config, resolves a storage plan, opens a
// binding and — on a born-split city — re-proves its invariant with a full work
// store census. That is once per process, memoized with every other one-shot
// command's routing, but it is not free, so it is paid only by an invocation
// that could concern a class-owned bead: a served by-ID form, an argv that
// addresses a reserved-prefix id, or a MUTATION with unambiguous positional
// ids. An ordinary work READ or SELECTOR never enters; a mutation addressing
// ids enters exactly once per process.
//
// The mutation arm is the widening, and the id shape is why it cannot be
// narrower. A class store mints from its binding workspace's own prefix and
// `gc storage migrate` preserves ids, so "gc-123" says nothing about which
// store holds the row — only a probe does, and skipping it is what sent every
// unserved mutation of a class resident to the ledger that cannot hold it.
// Reads and selectors stay outside because they address no subject whose
// residence could decide anything: a selector QUOTES ids, and a read that this
// surface serves is already inside. An ambiguous scan stays outside too, for
// the plainest reason — it yields no ids to probe.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/bdflags"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/storebinding"
)

// bdByIDVerb names a recognized by-ID gc bd invocation.
type bdByIDVerb string

const (
	// bdByIDShow is `gc bd show <id> [--json]`.
	bdByIDShow bdByIDVerb = "show"
	// bdByIDClaim is `gc bd update <id> --claim [--json]`, the acquire half of
	// the conditional-assignment pair.
	bdByIDClaim bdByIDVerb = "claim"
	// bdByIDRelease is `gc bd release-if-current <id> <assignee>`, the release
	// half. doBd already ran this in process, but only ever against the work
	// scope.
	bdByIDRelease bdByIDVerb = "release-if-current"
	// bdByIDDepList is `gc bd dep list <id> [--direction=up|down] [--type=T]
	// [--json]`. This is the dependency read the cascade-nudge order makes on
	// every closed blocker, and on a split city it is the operation whose
	// subprocess never returned.
	bdByIDDepList bdByIDVerb = "dep-list"
	// bdByIDDepTree is `gc bd dep tree <id> [--direction=up|down] [--reverse]
	// [--max-depth=N] [--json]`, the recursive form of the same read.
	//
	// It is the one federated read that resolves a whole relocated molecule, and
	// every status summary asks for exactly that: pr_review.py's
	// city_dep_subtree walks a root plus its descendants in one call, because
	// the selector projections (`list`, `ready`, `sql`) cannot see the class at
	// all. While this was unserved, the ownership gate refused it on every
	// class-owned root — correctly, since bd would have answered from the ledger
	// that does not hold the bead — and the pr-review label poller failed on
	// every tick for every labeled PR (ga-pxppl).
	//
	// Serving it asks nothing new of the store: the walk is DepList to a level
	// and Get for each related bead, which is what dep list already does once.
	bdByIDDepTree bdByIDVerb = "dep-tree"
	// bdByIDUpdate is `gc bd update <id>` carrying field and metadata writes.
	//
	// It is here because it is the step-completion write the core pack makes on
	// every worked bead — graph-worker.md renders
	// `gc bd update <id> --set-metadata gc.outcome=pass --status closed`, and
	// mol-do-work.toml closes the formula step the same way. On a split city
	// that bead is class-owned, so the passthrough wrote the outcome into the
	// work ledger and left the step open in the binding forever: a molecule
	// that stalls with no error anywhere.
	bdByIDUpdate bdByIDVerb = "update"
	// bdByIDClose is `gc bd close <id> [--json]`, and bdByIDReopen its undo.
	//
	// They are the drain verbs for a class resident: the row a class store
	// minted under a work-shaped id has no other way to be retired, because
	// every prefix-routed lane resolves it against a ledger that never held
	// it. Only the bare form is served — see parseBdByIDCloseArgs.
	bdByIDClose  bdByIDVerb = "close"
	bdByIDReopen bdByIDVerb = "reopen"
)

// bdByIDDepDirectionUp asks for the beads that depend ON the subject; the
// default direction asks for the beads the subject depends on. These are bd's
// own two --direction values and the only ones the routed arm accepts.
const (
	bdByIDDepDirectionUp   = "up"
	bdByIDDepDirectionDown = "down"
)

// bdByIDOp is a parsed by-ID invocation: exactly one bead id and the operation
// to apply to it.
type bdByIDOp struct {
	Verb     bdByIDVerb
	ID       string
	JSON     bool
	Assignee string
	// Direction and DepType carry `dep list`'s two selectors. Direction is
	// always populated for that verb; an empty DepType means every edge type.
	// Direction is populated for `dep tree` too, which takes the same axis.
	Direction string
	DepType   string
	// MaxDepth is `dep tree`'s safety limit, always populated for that verb.
	MaxDepth int
	// Update carries the field and metadata writes of the update verb, already
	// translated into the object model's own shape.
	Update beads.UpdateOpts
}

// parseBdByIDOp recognizes the by-ID invocations this surface serves. Anything
// else — a different verb, several ids, or a flag the in-process arm does not
// implement — is not recognized, and the caller leaves it on its existing path
// rather than serving a partial imitation of bd.
func parseBdByIDOp(bdArgs []string) (bdByIDOp, bool) {
	if len(bdArgs) == 0 {
		return bdByIDOp{}, false
	}
	switch bdArgs[0] {
	case "show":
		id, jsonOut, ok := parseBdByIDPositional(bdArgs[1:], nil)
		if !ok {
			return bdByIDOp{}, false
		}
		return bdByIDOp{Verb: bdByIDShow, ID: id, JSON: jsonOut}, true
	case "update":
		op, _, ok := parseBdByIDUpdateArgs(bdArgs[1:])
		return op, ok
	case "close":
		op, _, ok := parseBdByIDCloseArgs(bdByIDClose, bdArgs[1:])
		return op, ok
	case "reopen":
		op, _, ok := parseBdByIDCloseArgs(bdByIDReopen, bdArgs[1:])
		return op, ok
	case "release-if-current":
		id, assignee, ok, err := parseBdReleaseIfCurrentArgs(bdArgs)
		if err != nil || !ok {
			return bdByIDOp{}, false
		}
		return bdByIDOp{Verb: bdByIDRelease, ID: id, Assignee: assignee}, true
	case "dep":
		if len(bdArgs) < 2 {
			return bdByIDOp{}, false
		}
		switch bdArgs[1] {
		case "list":
			return parseBdDepListArgs(bdArgs[2:])
		case "tree":
			return parseBdDepTreeArgs(bdArgs[2:])
		}
		return bdByIDOp{}, false
	}
	return bdByIDOp{}, false
}

// parseBdByIDUpdateArgs parses the tail of `gc bd update`.
//
// rejected names the first flag that stopped this from being served, so the
// refusal an unserved class-owned update produces can say WHICH flag it was
// rather than leaving an operator to bisect their own command line.
//
// The served set is exactly the flags that map onto beads.UpdateOpts, which is
// the whole of what the object model can represent. Anything else is rejected
// rather than dropped: a flag this arm silently ignored would change the
// meaning of a command it then reported as executed, and on the step-completion
// write that means an outcome an operator believes was recorded and was not.
//
// `--notes` is the consequential rejection and it is not an oversight — see
// bdByIDUpdateUnrepresentable.
func parseBdByIDUpdateArgs(args []string) (op bdByIDOp, rejected string, ok bool) {
	op = bdByIDOp{Verb: bdByIDUpdate}
	claim := false
	metadata := map[string]string{}
	set := func(field **string, value string) { v := value; *field = &v }

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			if op.ID != "" || arg == "" {
				return bdByIDOp{}, arg, false
			}
			op.ID = arg
			continue
		}
		name, inline, hasInline := strings.Cut(arg, "=")
		// The inline form already carries the value; the separated form takes
		// the next token. An `=` on a flag that takes no value is not a
		// spelling bd accepts, and guessing at it would change the command.
		value := inline
		takesValue := bdByIDUpdateValueFlags[name]
		switch {
		case takesValue && !hasInline:
			if i+1 >= len(args) {
				return bdByIDOp{}, name, false
			}
			i++
			value = args[i]
		case !takesValue && hasInline:
			return bdByIDOp{}, name, false
		}
		switch name {
		case "--json":
			op.JSON = true
		case "--claim":
			claim = true
		case "--status", "-s":
			set(&op.Update.Status, value)
		case "--title":
			set(&op.Update.Title, value)
		case "--description", "-d":
			set(&op.Update.Description, value)
		case "--assignee", "-a":
			set(&op.Update.Assignee, value)
		case "--type", "-t":
			set(&op.Update.Type, value)
		case "--parent":
			set(&op.Update.ParentID, value)
		case "--priority", "-p":
			priority, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return bdByIDOp{}, name, false
			}
			op.Update.Priority = &priority
		case "--add-label":
			op.Update.Labels = append(op.Update.Labels, value)
		case "--remove-label":
			op.Update.RemoveLabels = append(op.Update.RemoveLabels, value)
		case "--set-metadata":
			key, metaValue, split := strings.Cut(value, "=")
			if !split || strings.TrimSpace(key) == "" {
				return bdByIDOp{}, name, false
			}
			metadata[key] = metaValue
		default:
			return bdByIDOp{}, name, false
		}
	}
	if op.ID == "" {
		return bdByIDOp{}, "", false
	}
	if claim {
		// The claim is a compare-and-swap the store owns; bundling other field
		// writes into it here would either apply them outside that swap or
		// re-implement it. bd's own `--claim` is likewise its own operation.
		if bdByIDUpdateWritesFields(op, metadata) {
			return bdByIDOp{}, "--claim", false
		}
		return bdByIDOp{Verb: bdByIDClaim, ID: op.ID, JSON: op.JSON}, "", true
	}
	if len(metadata) > 0 {
		op.Update.Metadata = metadata
	}
	if !bdByIDUpdateWritesFields(op, metadata) {
		// `gc bd update <id>` with nothing to write is bd's to answer.
		return bdByIDOp{}, "", false
	}
	return op, "", true
}

// bdByIDUpdateValueFlags are the update flags this arm serves that consume the
// next argument as their value. It is deliberately a local set rather than
// bdflags.ValueFlags("update"): that manifest describes what BD accepts, and
// this describes what the object model can represent. Reading the wider one
// here would make every flag gc cannot write look served.
var bdByIDUpdateValueFlags = map[string]bool{
	"--status": true, "-s": true, "--title": true, "--description": true,
	"-d": true, "--assignee": true, "-a": true, "--type": true, "-t": true,
	"--parent": true, "--priority": true, "-p": true, "--add-label": true,
	"--remove-label": true, "--set-metadata": true,
}

// bdByIDUpdateUnrepresentable explains the rejection an operator is most likely
// to hit, because the core pack writes it.
//
// beads.Bead has no notes field and beads.UpdateOpts has no notes write: notes
// are outside gc's object model entirely, and `gc bd update --notes` works on an
// unrelocated city only because gc hands the whole command to bd. There is
// therefore no faithful in-process spelling of it for a class-owned bead, and
// the honest answer is to refuse and say so — a partial write reported as a
// success is the failure mode this whole surface exists to remove.
const bdByIDUpdateUnrepresentable = "--notes"

// bdByIDUpdateWritesFields reports whether an update carries anything to write.
func bdByIDUpdateWritesFields(op bdByIDOp, metadata map[string]string) bool {
	u := op.Update
	return u.Status != nil || u.Title != nil || u.Description != nil ||
		u.Assignee != nil || u.Type != nil || u.ParentID != nil ||
		u.Priority != nil || len(u.Labels) > 0 || len(u.RemoveLabels) > 0 ||
		len(metadata) > 0
}

// parseBdByIDCloseArgs parses the tail of `gc bd close` and `gc bd reopen`.
//
// rejected names the first flag that stopped this from being served, the same
// way parseBdByIDUpdateArgs does, so a refusal can say WHICH spelling kept the
// command off this surface.
//
// The served form is one bare id plus --json, and that is the whole of what the
// closed contract can express: GraphStore.Close and GraphStore.Reopen take an
// id and return an error. bd's -r/--reason/--reason-file/--session carry text
// the contract has nowhere to put, and --claim-next/--continue/--force/
// --no-auto/--suggest-next name workflow this arm does not run. Serving any of
// them by ignoring it would report a command as executed after silently
// changing what it meant — the partial write this whole surface exists to
// remove.
//
// Several ids stay unrecognized for a different reason: a batch spans stores,
// so it would need per-id routing with partial-failure semantics bd's exit
// contract does not express. The refusal path keeps such a batch honest when
// one of its ids is class-owned; it is never half-applied here.
func parseBdByIDCloseArgs(verb bdByIDVerb, args []string) (op bdByIDOp, rejected string, ok bool) {
	op = bdByIDOp{Verb: verb}
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			if op.ID != "" || arg == "" {
				// A second positional is a batch, not a flag: there is no
				// spelling to name, and naming the id would read as a claim
				// about that id rather than about the shape.
				return bdByIDOp{}, "", false
			}
			op.ID = arg
			continue
		}
		if arg == "--json" {
			op.JSON = true
			continue
		}
		name, _, _ := strings.Cut(arg, "=")
		return bdByIDOp{}, name, false
	}
	if op.ID == "" {
		return bdByIDOp{}, "", false
	}
	return op, "", true
}

// bdByIDUnservedFlag names the flag that kept an otherwise-routable invocation
// off this surface, or "" when the shape itself was never one this serves.
func bdByIDUnservedFlag(bdArgs []string) string {
	if len(bdArgs) == 0 {
		return ""
	}
	var rejected string
	var ok bool
	switch bdArgs[0] {
	case "update":
		_, rejected, ok = parseBdByIDUpdateArgs(bdArgs[1:])
	case "close":
		_, rejected, ok = parseBdByIDCloseArgs(bdByIDClose, bdArgs[1:])
	case "reopen":
		_, rejected, ok = parseBdByIDCloseArgs(bdByIDReopen, bdArgs[1:])
	default:
		return ""
	}
	if ok {
		return ""
	}
	return rejected
}

// parseBdDepListArgs parses the tail of `gc bd dep list`.
//
// It knows every flag it accepts and whether that flag consumes the next
// argument, so a value can never be mistaken for the subject id. That is the
// whole point: `--type gcg-blocks` names an edge type, not a bead, and a
// scanner that took the first non-flag token would route the command on it.
// An unrecognized flag makes the invocation unrecognized rather than
// best-guessed — the routed arm would otherwise silently drop a selector and
// answer a different question than the one asked.
func parseBdDepListArgs(args []string) (bdByIDOp, bool) {
	op := bdByIDOp{Verb: bdByIDDepList, Direction: bdByIDDepDirectionDown}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--json":
			op.JSON = true
		case strings.HasPrefix(arg, "--direction="):
			op.Direction = strings.TrimPrefix(arg, "--direction=")
		case strings.HasPrefix(arg, "--type="):
			op.DepType = strings.TrimPrefix(arg, "--type=")
		case strings.HasPrefix(arg, "-t="):
			op.DepType = strings.TrimPrefix(arg, "-t=")
		// -t is bd's own short form of --type (bdflags "dep list"). Omitting it
		// meant the short spelling parsed as unrecognized, and while an
		// unrecognized invocation fell through, `gc bd dep list <gcg-id> -t
		// blocks` asked the work ledger a question only the binding can answer.
		case arg == "--direction", arg == "--type", arg == "-t":
			if i+1 >= len(args) {
				return bdByIDOp{}, false
			}
			i++
			if arg == "--direction" {
				op.Direction = args[i]
			} else {
				op.DepType = args[i]
			}
		case strings.HasPrefix(arg, "-"):
			return bdByIDOp{}, false
		default:
			if op.ID != "" || arg == "" {
				return bdByIDOp{}, false
			}
			op.ID = arg
		}
	}
	if op.ID == "" {
		return bdByIDOp{}, false
	}
	if op.Direction != bdByIDDepDirectionUp && op.Direction != bdByIDDepDirectionDown {
		return bdByIDOp{}, false
	}
	return op, true
}

// bdByIDDepTreeDefaultMaxDepth is bd's own default tree depth (dep.go registers
// --max-depth with it), carried here so an unflagged walk cuts in the same place
// on both arms.
const bdByIDDepTreeDefaultMaxDepth = 50

// parseBdDepTreeArgs parses the tail of `gc bd dep tree`.
//
// It serves the flags whose meaning this walk implements — --direction, its
// deprecated --reverse spelling, --max-depth (with bd's -d short form) and
// --json — and rejects the rest rather than accepting them as no-ops.
// --show-all-paths, --status and --format all change what bd prints, so
// answering them with the plain walk would report a different question as
// executed; unrecognized here means the ownership gate refuses them loudly on a
// class-owned id, which is the honest answer until they are implemented.
//
// --direction=both is rejected for the same reason and not because it is hard:
// bd merges two walks for it, and this arm walks one.
func parseBdDepTreeArgs(args []string) (bdByIDOp, bool) {
	op := bdByIDOp{Verb: bdByIDDepTree, MaxDepth: bdByIDDepTreeDefaultMaxDepth}
	reverse := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, inline, hasInline := strings.Cut(arg, "=")
		switch name {
		case "--json":
			if hasInline {
				return bdByIDOp{}, false
			}
			op.JSON = true
			continue
		case "--reverse":
			if hasInline {
				return bdByIDOp{}, false
			}
			reverse = true
			continue
		case "--direction", "--max-depth", "-d":
			if !hasInline {
				if i+1 >= len(args) {
					return bdByIDOp{}, false
				}
				i++
				inline = args[i]
			}
			if name == "--direction" {
				op.Direction = inline
				continue
			}
			depth, err := strconv.Atoi(strings.TrimSpace(inline))
			if err != nil || depth < 1 {
				return bdByIDOp{}, false
			}
			op.MaxDepth = depth
			continue
		}
		if strings.HasPrefix(arg, "-") {
			return bdByIDOp{}, false
		}
		if op.ID != "" || arg == "" {
			return bdByIDOp{}, false
		}
		op.ID = arg
	}
	if op.ID == "" {
		return bdByIDOp{}, false
	}
	// bd's own precedence: an explicit --direction wins, and bare --reverse is
	// the deprecated spelling of --direction=up.
	if op.Direction == "" {
		op.Direction = bdByIDDepDirectionDown
		if reverse {
			op.Direction = bdByIDDepDirectionUp
		}
	}
	if op.Direction != bdByIDDepDirectionUp && op.Direction != bdByIDDepDirectionDown {
		return bdByIDOp{}, false
	}
	return op, true
}

// parseBdByIDPositional extracts exactly one positional bead id from args,
// accepting --json plus the boolean flags named in extra and rejecting
// everything else. Rejecting unknown flags is what keeps this arm honest: a
// flag it silently ignored would change the meaning of a command it then
// claimed to have executed.
func parseBdByIDPositional(args []string, extra map[string]*bool) (id string, jsonOut, ok bool) {
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			if id != "" || arg == "" {
				return "", false, false
			}
			id = arg
			continue
		}
		if arg == "--json" {
			jsonOut = true
			continue
		}
		seen, known := extra[arg]
		if !known {
			return "", false, false
		}
		*seen = true
	}
	if id == "" {
		return "", false, false
	}
	return id, jsonOut, true
}

// bdByIDResolution is what the residency walk concluded about one id: the front
// door of the class that would own it, and the row itself when the binding
// holds it.
//
// There is no "the id's prefix says it is ours" bit. RESIDENCE is the whole
// answer — a reserved prefix names the authority for a namespace, not its only
// lawful holder — and a second bit that could disagree with Found is exactly
// how this door came to report a rig's own bead as absent.
type bdByIDResolution struct {
	Graph storebinding.GraphStore
	Bead  beads.Bead
	Found bool
}

// bdByIDClassDoor is the opened class front door serving this city.
//
// It owns no handle to release. The store belongs to the one-shot funnel, which
// opens each city's binding at most once per process and closes it where the
// process ends — so a command that touches this surface and a class resolver
// reads one database rather than two, and a routed read cannot close a handle
// another call site is still using.
type bdByIDClassDoor struct {
	Graph storebinding.GraphStore
	// Store is the same class binding Graph wraps, kept in its beads.Store shape
	// so the work-record close gate — which is store-driven, not graph-driven —
	// can evaluate the resolved class bead this door is about to write. It is a
	// second view of the one handle the funnel owns, not a second handle: nothing
	// here is released.
	Store   beads.Store
	Binding string
}

// bindingName names the binding in operator-facing text.
//
// A refused city whose [storage.classes] describe an arrangement this build
// cannot read a class map out of has no binding NAME to report — the funnel
// routes those at a refusing store under the empty name — and "owned by the
// class binding" with a hole in it reads as a formatting bug rather than as a
// fact. The literal keeps the sentence true when the name is unknown.
func (d bdByIDClassDoor) bindingName() string {
	if strings.TrimSpace(d.Binding) == "" {
		return "the relocated class binding"
	}
	return fmt.Sprintf("the %s class binding", d.Binding)
}

// openBdByIDClassFrontDoor opens the class front door serving this city, for
// whatever provider its coordination classes are served from.
//
// routed=false means the city relocates no class, so every id is still the work
// store's and the caller's existing path is correct and byte-identical. An error
// means the city IS relocated and the front door could not be projected; the
// caller surfaces it instead of guessing.
//
// The gate is the funnel's verdict, which is the boot gate's: a city configured
// for a binding it has not converged on, or one this build must not serve,
// resolves to a store whose every operation returns that refusal. So an
// unconverged city neither serves stale answers from the work store here nor
// goes quiet — it reports the same sentence `gc start` would have printed.
//
// # One door for every reserved prefix
//
// bdIDIsClassReserved matches ANY reserved class prefix and every match is
// answered from this ONE store, which is sound only because the shape the
// runtime serves is storageSplitWhole: storageSplitShapeOf admits a split only
// when all five infrastructure classes name the same binding, and refuses a
// per-class fan-out outright (storageSupportedTopologyStatement). If that ever
// stops holding, this door has to resolve per class — the graph store would
// otherwise be asked for a sessions-class id it does not hold, and would
// truthfully answer that it is absent.
//
// cliSoleClassBinding is how that condition is CHECKED rather than assumed: it
// reports the city's relocated bindings grouped by store and refuses to name a
// sole one when there is more than one. Asking the graph class specifically
// (graphClassBinding) could not tell the two shapes apart.
func openBdByIDClassFrontDoor(cityPath string) (bdByIDClassDoor, bool, error) {
	binding, relocated, err := cliSoleClassBinding(cityPath)
	if err != nil {
		return bdByIDClassDoor{}, false, fmt.Errorf("resolving the class front door: %w", err)
	}
	if !relocated {
		return bdByIDClassDoor{}, false, nil
	}
	graph, err := storebinding.NewBeadsGraphStore(binding.Store)
	if err != nil {
		return bdByIDClassDoor{}, false, fmt.Errorf("projecting the class front door of binding %q: %w", binding.Name, err)
	}
	return bdByIDClassDoor{Graph: graph, Store: binding.Store, Binding: binding.Name}, true, nil
}

// resolve asks the open front door whether it owns id.
//
// A read failure is an error, never absence. Reading "the binding could not
// answer" as "the bead is not there" is the root-loss shape this whole lane
// exists to prevent, and it is the one classification a caller cannot recover
// from once it has been flattened.
//
// The residence probe is why an id with no reserved prefix is asked about at
// all: `gc storage migrate` copies the work store's infrastructure slice with
// its ids PRESERVED, so a converged city holds work-shaped ids in its binding
// and deciding ownership by prefix alone would send those reads back to the
// ledger they were moved off.
//
// The one error that is not a fault is the funnel's standing refusal. It says
// this city's storage configuration cannot be served, which is a statement
// about the city and about no particular bead — and a refused city still serves
// work from its work ledger. So for a work id it establishes nothing and the
// existing path stands; for an id only the class binding could own it is the
// answer, and falls through to the fault arm.
func (d bdByIDClassDoor) resolve(id string) (bdByIDResolution, error) {
	resolution := bdByIDResolution{Graph: d.Graph}
	reserved := bdIDIsClassReserved(id)
	bead, err := d.Graph.Get(id)
	switch {
	case err == nil:
		resolution.Bead = bead
		resolution.Found = true
	case errors.Is(err, beads.ErrNotFound):
	case isStandingStorageRefusal(err) && !reserved:
	default:
		return bdByIDResolution{}, fmt.Errorf("reading %q from %s: %w", id, d.bindingName(), err)
	}
	return resolution, nil
}

// firstResident returns the first id the class binding actually holds, or ""
// when it holds none of them.
//
// It is bounded by construction: the ids come from one mutation argv, which is
// a handful of tokens, and the probe stops at the first hit. Residence — not
// the reserved-prefix rule — is the only thing that can decide ownership for
// these, so a failure to READ is a failure to decide and surfaces as one
// (resolve's classification, unchanged).
func (d bdByIDClassDoor) firstResident(ids []string) (string, error) {
	for _, id := range ids {
		resolution, err := d.resolve(id)
		if err != nil {
			return "", err
		}
		if resolution.Found {
			return id, nil
		}
	}
	return "", nil
}

// bdByIDMutationSubjects returns the bead ids a write-mutation argv ADDRESSES,
// or nil when the argv is not a mutation or cannot be scanned into ids.
//
// It reads bdMutationWriteIDs, the scanner doBd's exact-ID collision guard
// already trusts for the same argv, so the two agree on what a mutation
// addresses rather than each deciding it. An ambiguous scan yields nil: an
// unrecognized flag may or may not consume the next token, and a guess here
// would probe residence for a value rather than a subject.
func bdByIDMutationSubjects(bdArgs []string) []string {
	ids, ok, ambiguous := bdMutationWriteIDs(bdArgs)
	if !ok || ambiguous {
		return nil
	}
	return ids
}

// bdIDIsClassReserved reports whether id carries a reserved class id prefix.
// Those namespaces belong to the relocated class stores — whether the store's
// own sequence minted the id or a subsystem inside it did — so such an id
// existing anywhere else is not a thing bd can answer for.
func bdIDIsClassReserved(id string) bool {
	for _, prefix := range config.AllReservedClassPrefixes() {
		if prefix != "" && strings.HasPrefix(id, prefix+"-") {
			return true
		}
	}
	return false
}

// maybeRouteBdByID serves a by-ID gc bd invocation from the owning class front
// door. handled=false means the caller proceeds on its existing path unchanged
// — the city relocates no class, the invocation is not one of the routed by-ID
// forms, or the id is a work-store id the class store does not answer for.
//
// rigName is the EXPLICIT --rig the operator wrote (or `gc --rig X bd …`, which
// extractBdScopeFlags folds into the same value). It is never the auto-detected
// scope: GC_RIG, -C and cwd resolve inside resolveBdScopeTarget and never reach
// here, which is deliberate — a rig agent runs with GC_RIG set on every
// invocation, so treating that as a deliberate scope would refuse the
// step-completion write the core pack renders on every worked bead. See
// refuseRigScopedClassOwnedTarget.
func maybeRouteBdByID(cityPath, rigName string, bdArgs []string, stdout, stderr io.Writer) (int, bool) {
	op, served := parseBdByIDOp(bdArgs)
	classIDs := bdArgsAddressedClassIDs(bdArgs)
	mutationIDs := bdByIDMutationSubjects(bdArgs)
	if !served && len(classIDs) == 0 && len(mutationIDs) == 0 {
		// Nothing here can concern a class-owned bead, so the binding is not
		// opened and the funnel is not entered.
		//
		// This is also the cost gate. Entering the funnel resolves a plan and
		// opens a database, and the born-split arm re-proves its invariant with
		// a full work-store census; a read or selector that addresses no
		// subject must not pay that, and neither must a mutation whose argv
		// cannot be scanned into ids at all — there would be nothing to probe.
		return 0, false
	}
	door, routed, err := openBdByIDClassFrontDoor(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1, true
	}
	if !routed {
		return 0, false
	}
	if !served {
		return refuseUnservedClassMutation(door, bdArgs, classIDs, mutationIDs, stderr)
	}
	return serveBdByIDResolved(door, op, bdArgs, rigName, cityPath, stdout, stderr)
}

// refuseUnservedClassMutation answers a by-ID invocation that addresses a bead
// the class binding HOLDS in a spelling this surface does not serve. Returning
// (0, false) means none of the subjects PROBED is resident there — bd is still
// their truth and the caller's passthrough answers byte-identically, including
// doBd's own exact-ID collision guard, which this arm must not displace.
//
// The candidates are EVERY reserved-prefix id the argv addresses
// (bdArgsAddressedClassIDs) plus whatever bdMutationWriteIDs could reduce to
// subjects. Probing every addressed id, not just the first, is what makes a
// multi-subject `dep add`/`dep remove` order-independent: an argv whose first
// reserved id is a clean miss and whose LATER id is class-resident is refused on
// the later id rather than falling through on the first miss. The one spelling
// still outside this net is the all-work-prefix argv, which carries no reserved
// id and is no write-mutation verb, so it never opens this door to be probed at
// all; ga-zvetw tracks that remaining end.
//
// Ownership is decided by RESIDENCE for every subject, including one carrying a
// reserved prefix. The prefix decides which ids are asked about — an argv naming
// a reserved id is asking about that id whether or not the mutation scanner
// could reduce the rest to subjects — but it does not decide the answer. The
// binding is the namespace's authority, not its only lawful holder, so refusing
// on the prefix alone strands every write to a rig configured with a prefix
// inside the namespace.
func refuseUnservedClassMutation(door bdByIDClassDoor, bdArgs, classIDs, mutationIDs []string, stderr io.Writer) (int, bool) {
	candidates := make([]string, 0, len(classIDs)+len(mutationIDs))
	candidates = append(candidates, classIDs...)
	candidates = append(candidates, mutationIDs...)
	resident, err := door.firstResident(candidates)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1, true
	}
	if resident == "" {
		return 0, false
	}
	// The invocation addresses a bead the class binding holds, in a spelling
	// this surface does not serve. Forwarding it would run the command against
	// the one ledger that cannot hold the bead.
	return refuseClassOwnedTarget(door, bdByIDRefusedVerb(bdArgs), resident, bdByIDUnservedFlag(bdArgs), stderr)
}

// serveBdByIDResolved answers a served by-ID op from the class front door. It
// resolves the id against the class binding, refuses the cases this surface
// must not serve (a work-store id it does not own, a reserved-prefix id with no
// row, an explicit --rig work scope), runs the ADR-0009 work-record close gate,
// and dispatches the verb against the class graph. repoFallback is the git repo
// the close gate falls back to when a closing bead carries no gc.work_dir.
func serveBdByIDResolved(door bdByIDClassDoor, op bdByIDOp, bdArgs []string, rigName, repoFallback string, stdout, stderr io.Writer) (int, bool) {
	resolution, err := door.resolve(op.ID)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1, true
	}
	if !resolution.Found {
		// The binding does not hold this id: bd is still its truth, and the
		// passthrough answers it byte-identically. That is the same answer
		// whatever the id's prefix, because a reserved prefix makes the binding
		// the namespace's AUTHORITY and not its only lawful holder — a rig
		// configured with a prefix inside the namespace mints work beads there
		// (config.ValidateRigs allows it; ReservedPrefixWarnings only advises),
		// and the migration preserved ids in the other direction.
		//
		// It runs BEFORE the --rig refusal below, and the order is the
		// diagnosis. The refusal's whole claim is that the binding owns this
		// bead and the named rig's work store does not hold it — a sentence
		// that is false when the binding never answered for it. Blaming the
		// flag there sends the operator to fix the one thing that was not
		// wrong, and refuses the one scope that could still resolve the id.
		//
		// A miss is not a shrug: only an ANSWER of "absent" reaches here. A
		// binding that could not answer — unopenable, refusing, or faulting —
		// left through the error arm above, because the plan carries the
		// authority leg as PolicyFatal and resolve classifies every non-absence
		// as a read failure.
		return 0, false
	}
	if rig := strings.TrimSpace(rigName); rig != "" {
		// The invocation pins a WORK rig scope and names a bead the class
		// binding owns. Both answers available here are wrong: serving it
		// ignores a flag the operator reached for to be MORE specific, and
		// honoring it sends the read to a ledger that does not hold the bead.
		// So neither is taken. See refuseRigScopedClassOwnedTarget.
		return refuseRigScopedClassOwnedTarget(door, op.ID, rig, stderr)
	}
	if gateBdByIDClassClose(door, op, bdArgs, resolution, repoFallback, stderr) {
		return 1, true
	}
	switch op.Verb {
	case bdByIDShow:
		return printBdByIDBead(resolution.Bead, op.JSON, door.bindingName(), stdout, stderr), true
	case bdByIDClaim:
		return doBdByIDClaim(resolution.Graph, op.ID, bdByIDClaimActor(), op.JSON, door.bindingName(), stdout, stderr), true
	case bdByIDRelease:
		return doBdByIDReleaseIfCurrent(resolution.Graph, op.ID, op.Assignee, stdout, stderr), true
	case bdByIDDepList:
		return doBdByIDDepList(resolution.Graph, op, stdout, stderr), true
	case bdByIDDepTree:
		return doBdByIDDepTree(resolution.Graph, resolution.Bead, op, stdout, stderr), true
	case bdByIDUpdate:
		return doBdByIDUpdate(resolution.Graph, op, door.bindingName(), stdout, stderr), true
	case bdByIDClose:
		return doBdByIDClose(resolution.Graph, op, door.bindingName(), stdout, stderr), true
	case bdByIDReopen:
		return doBdByIDReopen(resolution.Graph, op, door.bindingName(), stdout, stderr), true
	}
	// Unreachable while the verb set and this switch agree, and a refusal
	// rather than a fall-through because the two disagreeing is exactly the
	// case that must not reach bd: the id is already known to be class-owned,
	// so the passthrough would run a verb this build recognized against the one
	// ledger that cannot hold the bead.
	return refuseClassOwnedTarget(door, string(op.Verb), op.ID, "", stderr)
}

// gateBdByIDClassClose runs the ADR-0009 work-record close gate against the
// resolved class bead before the door serves a close or a closing update. The
// class door writes the class copy, so — unlike the fall-through path in
// cmd_bd.go — the prefix-store gate never saw these closes; this is where the
// contract is enforced for them. It is a no-op for every non-closing op:
// evaluateWorkRecordCloseGate consults workRecordCloseTargets, which reports
// not-a-close for show/claim/release/dep/reopen and for updates that do not set
// status closed. The resolved bead is handed in as the preFetched value so the
// gate reuses it rather than re-reading the class store, and door.Store answers
// any other id the argv might name. Returns true only when the close must be
// blocked (enforcement on and the work record invalid).
func gateBdByIDClassClose(door bdByIDClassDoor, op bdByIDOp, bdArgs []string, resolution bdByIDResolution, repoFallback string, stderr io.Writer) bool {
	if op.Verb != bdByIDClose && op.Verb != bdByIDUpdate {
		return false
	}
	preFetched := map[string]beads.Bead{op.ID: resolution.Bead}
	return evaluateWorkRecordCloseGate(bdArgs, door.Store, preFetched, repoFallback, workRecordEnforceEnabled(), stderr)
}

// refuseRigScopedClassOwnedTarget refuses a by-ID invocation that pins a rig
// work scope with an explicit --rig while naming a bead the relocated class
// binding owns.
//
// # The rule, and why it is a refusal rather than a narrowing
//
// --rig names a WORK scope: a rig's own bd workspace, one leg of the city's
// work ledger. A relocated coordination class is not partitioned by rig — the
// served split shape is storageSplitWhole, one binding for all five
// infrastructure classes and every scope in the city — so there is nothing for
// --rig to narrow WITHIN. The flag and the subject describe two different
// ledgers, and the command has to say so.
//
// Both silent answers are worse:
//
//   - Serving it anyway ignores the flag, which is what this surface did before
//     the rule: `gc bd show <class-id> --rig <rig>` and the same command with no
//     --rig produced identical output. An operator who wrote --rig to be more
//     specific got an answer from a store they did not name, with no
//     diagnostic — and a script pinning --rig to keep two rigs apart was reading
//     whichever ledger the class routing chose.
//   - Honoring it routes the read at the rig's work store, which does not hold
//     the bead. That is the pre-#5132 behavior and the wrong-answer lane this
//     whole surface exists to close: an empty result indistinguishable from a
//     real one.
//
// # What it does NOT cover
//
// Only the EXPLICIT flag. GC_RIG, -C/--directory and cwd detection are
// auto-detected scope, not a deliberate one, and the controller sets GC_RIG on
// every rig agent — refusing those would break the step-completion write the
// core pack renders on every worked bead (`gc bd update <id> --set-metadata
// gc.outcome=pass --status closed`, run from a rig worktree against a
// class-owned step). resolveBdScopeTarget already keeps them in a lower
// priority band than the flag for the same reason.
//
// --city is also untouched, and for the opposite reason: it selects WHICH CITY
// answers, and the class binding this door opened is that city's. A --city
// scope and class routing agree; a --rig scope and class routing cannot.
func refuseRigScopedClassOwnedTarget(door bdByIDClassDoor, id, rigName string, stderr io.Writer) (int, bool) {
	fmt.Fprintf(stderr, "gc bd: %s is owned by %s, but --rig %s pins that rig's work store, which does not hold it; a relocated class is not partitioned by rig, so drop --rig (auto-detected scope — GC_RIG, -C, cwd — is not affected)\n", id, door.bindingName(), rigName) //nolint:errcheck // best-effort stderr
	return 1, true
}

// refuseClassOwnedTarget is the single refusal message, so every unsupported
// class-owned operation reports the same way.
//
// flag, when known, names the spelling that kept the command off this surface.
// Without it an operator hitting a rejected `--notes` sees only that `gc bd
// update` "is not served", which is false in general and unactionable in
// particular.
func refuseClassOwnedTarget(door bdByIDClassDoor, verb, id, flag string, stderr io.Writer) (int, bool) {
	because := ""
	if flag != "" {
		because = fmt.Sprintf(" (%s has no representation in the class store contract)", flag)
	}
	fmt.Fprintf(stderr, "gc bd: %s is owned by %s, and `gc bd %s` is not served in process%s; refusing rather than running it against the work store, which does not hold the bead\n", id, door.bindingName(), verb, because) //nolint:errcheck // best-effort stderr
	return 1, true
}

// bdByIDRefusedVerb names the subcommand a refusal is about, as an operator
// typed it.
//
// It reports the bd subcommand rather than argv[0], so a root flag before the
// verb (`gc bd --json close gcg-1`) does not make the refusal name a flag. When
// the manifest does not carry the subcommand it reconstructs the spelling from
// the leading non-flag tokens instead of truncating to the first: `dep tree` is
// not in bdflags (only dep add/list/remove are), and reporting it as "dep" tells
// an operator to look for a command they did not run.
func bdByIDRefusedVerb(bdArgs []string) string {
	if sub, _, resolved := bdByIDSubcommand(bdArgs); resolved {
		return sub
	}
	globals := bdflags.GlobalValueFlags()
	bools := bdflags.GlobalBoolFlags()
	// Two is bd's deepest verb nesting ("mol pour", "dep tree"); a third token
	// is an argument, and on these paths it is usually the bead being refused.
	words := make([]string, 0, 2)
	for i := 0; i < len(bdArgs) && len(words) < 2; i++ {
		arg := bdArgs[i]
		if !strings.HasPrefix(arg, "-") {
			if bdIDIsClassReserved(arg) {
				break
			}
			words = append(words, arg)
			continue
		}
		if strings.IndexByte(arg, '=') >= 0 || bools[arg] {
			continue
		}
		if globals[arg] {
			i++
			continue
		}
		break
	}
	if len(words) == 0 {
		return "(no subcommand)"
	}
	return strings.Join(words, " ")
}

// bdArgsAddressedClassIDs returns EVERY reserved-prefix id an invocation
// ADDRESSES, in argv order, for any bd subcommand, and is what keeps an unserved
// spelling from reaching the work ledger. Returning all addressed ids — not just
// the first — is what makes a multi-subject verb's protection independent of the
// order the subjects were typed (see refuseUnservedClassMutation).
//
// It exists because the served parsers are deliberately strict: they reject
// every flag they do not implement, and a rejection used to mean the command
// fell through to the subprocess. bd's own manifest names the spellings that
// took that exit — `show --long/--short/--children/--refs/--thread/
// --include-comments/--include-dependents/--as-of/--id`, `dep list -t/--type/
// --direction` — so the very flags an operator reaches for when the terse
// answer is not enough were the ones routed at the ledger that cannot answer.
// Ownership is therefore decided HERE, independently of whether this surface
// can serve the command, and an unserved spelling on a class-owned bead is
// refused rather than forwarded.
//
// # ID position
//
// An id is ADDRESSED when it is a positional token, or the value of a flag that
// takes a bead id. It is merely QUOTED when it is the value of any other flag,
// and a quoted id decides nothing ABOUT OWNERSHIP: `gc bd list --metadata-field
// workflow_id=gcg-…` selects work rows by a field they carry, so no bead of the
// relocated class is being addressed and this walk returns false for it.
//
// False here does not mean forwarded. The selector dialect guard in
// bd_relocated_classes.go runs first and refuses that same argv on servability
// — a projection whose predicate names a namespace this ledger holds no row
// under cannot answer it, and `[]` is a confident wrong answer. What this walk
// must not do is claim OWNERSHIP from a quoted id, because that decides which
// STORE serves the read, and a quoted id says nothing about that.
//
// The value/positional split comes from bdflags — the tree's own manifest of
// what each subcommand's flags consume — rather than from a local list, so a
// flag bd grows does not silently become an id position here. The id-valued
// subset is local because bdflags records arity, not meaning.
//
// # Failing closed
//
// Two things can stop the walk from deciding: an unresolvable subcommand and an
// unknown flag, and both mean the same thing — from here on, nothing can be
// told from a value. Both widen the scan rather than ending it: every remaining
// token is treated as addressed. That is the same choice bdRelocatedClassVerb
// makes and it is bounded the same way, because only a token that actually
// carries a reserved class prefix can refuse anything.
func bdArgsAddressedClassIDs(bdArgs []string) []string {
	sub, args, resolved := bdByIDSubcommand(bdArgs)
	valueFlags := bdflags.GlobalValueFlags()
	boolFlags := bdflags.GlobalBoolFlags()
	if resolved {
		for flag := range bdflags.ValueFlags(sub) {
			valueFlags[flag] = true
		}
		for flag := range bdflags.BoolFlags(sub) {
			boolFlags[flag] = true
		}
	}
	// An unresolved subcommand cannot say what any flag consumes, so nothing is
	// skipped and every token is judged.
	undecidable := !resolved
	positionalOnly := false

	var ids []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case positionalOnly:
			ids = append(ids, bdReservedClassIDs(arg)...)
		case arg == "--":
			positionalOnly = true
		case strings.HasPrefix(arg, "-") && arg != "-":
			var flagIDs []string
			flagIDs, i, undecidable = bdArgsFlagAddressedIDs(args, i, undecidable, valueFlags, boolFlags)
			ids = append(ids, flagIDs...)
		default:
			ids = append(ids, bdReservedClassIDs(arg)...)
		}
	}
	return ids
}

// bdArgsFlagAddressedIDs judges a single flag token args[i] — already known to
// start with "-" and not be a bare "-" — for the reserved-prefix class ids its
// value addresses. It returns those ids, the index to resume the argv scan from
// (advanced past a value token the flag consumed), and whether argv alignment is
// now undecidable.
//
// A bd flag either inlines its value (`--deps=a,b`), consumes the next token
// (`--deps a,b`), or takes none (a bool). An unknown flag might do any of these,
// so once one is seen every following offset is undecidable and no later token is
// skipped; the caller threads that state in and back out.
func bdArgsFlagAddressedIDs(args []string, i int, undecidable bool, valueFlags, boolFlags map[string]bool) ([]string, int, bool) {
	name, inline, hasInline := strings.Cut(args[i], "=")
	var ids []string
	switch {
	case hasInline:
		if bdIDValuedFlags[name] || (undecidable && !boolFlags[name]) {
			ids = bdReservedClassIDs(inline)
		}
	case boolFlags[name]:
		// A bool flag takes no value; there is nothing to judge.
	case !valueFlags[name]:
		// Unknown flag: it may or may not consume the next token, so from here
		// nothing is skipped.
		undecidable = true
	case i+1 < len(args):
		// A value flag consumes the next token. Judge it when the flag names a
		// bead, or when an earlier unknown flag may already have shifted the
		// alignment so the offset cannot be trusted.
		i++
		if bdIDValuedFlags[name] || undecidable {
			ids = bdReservedClassIDs(args[i])
		}
	}
	return ids, i, undecidable
}

// bdArgsNameClassOwnedBead reports the FIRST reserved-prefix id an invocation
// addresses, and whether it addresses one at all. It is the singular projection
// of bdArgsAddressedClassIDs, kept for callers that only need the leading id or
// the boolean — the dep-tree ownership conformance test reads the boolean alone.
func bdArgsNameClassOwnedBead(bdArgs []string) (string, bool) {
	if ids := bdArgsAddressedClassIDs(bdArgs); len(ids) > 0 {
		return ids[0], true
	}
	return "", false
}

// bdByIDSubcommand locates the bd subcommand in an argv and returns the
// arguments that follow it.
//
// bd accepts its root flags BEFORE the subcommand (`bd --json show <id>`) and
// `gc bd` forwards argv verbatim, so indexing bdArgs[0] reads a flag as the
// verb. Compound subcommands ("dep list", "mol pour") are matched two tokens
// first, the way bdflags itself keys them.
func bdByIDSubcommand(bdArgs []string) (sub string, args []string, resolved bool) {
	globals := bdflags.GlobalValueFlags()
	bools := bdflags.GlobalBoolFlags()
	for i := 0; i < len(bdArgs); i++ {
		arg := bdArgs[i]
		if !strings.HasPrefix(arg, "-") {
			if i+1 < len(bdArgs) {
				if two := arg + " " + bdArgs[i+1]; bdflags.Known(two) {
					return two, bdArgs[i+2:], true
				}
			}
			if bdflags.Known(arg) {
				return arg, bdArgs[i+1:], true
			}
			// A subcommand this build's manifest does not carry. Everything
			// after it is judged rather than skipped.
			return "", bdArgs[i+1:], false
		}
		if strings.IndexByte(arg, '=') >= 0 || bools[arg] {
			continue
		}
		if globals[arg] {
			i++
			continue
		}
		return "", bdArgs[i+1:], false
	}
	return "", nil, false
}

// bdIDValuedFlags are the flags whose value NAMES a bead rather than describing
// one. bdflags records what a flag consumes; this records what it means, which
// is the distinction between addressing a bead and quoting one.
//
// Over-inclusion is cheap and under-inclusion is not: a flag listed here whose
// value is not an id (`-p 2` is a priority on most subcommands and a parent on
// some) costs nothing, because only a value carrying a reserved class prefix
// decides anything, and no priority ever does.
var bdIDValuedFlags = map[string]bool{
	"--id": true, "--parent": true, "-p": true, "--depends-on": true,
	"--blocked-by": true, "--await-id": true, "--deps": true,
	"--waits-for": true, "--waits-for-gate": true, "--mol": true,
	"--for": true, "--attach": true,
}

// bdReservedClassIDs returns EVERY reserved-prefix id in a token, splitting the
// comma lists bd accepts for `--deps`/`--blocked-by`/`--depends-on` and friends.
// A single value can address more than one bead, and each reserved id is a
// distinct class-ownership question, so returning only the first would let a
// later class-resident id fall through on an earlier clean miss — the exact
// order dependence the by-id door exists to remove, reproduced inside one token.
func bdReservedClassIDs(token string) []string {
	var ids []string
	for _, part := range strings.Split(token, ",") {
		part = strings.TrimSpace(part)
		if bdIDIsClassReserved(part) {
			ids = append(ids, part)
		}
	}
	return ids
}

// bdByIDClaimActor returns the identity a routed claim acquires the bead for.
// It is BEADS_ACTOR — the same variable gc puts in the subprocess environment
// and bd itself claims under — so a routed claim and a passthrough claim record
// the same owner.
func bdByIDClaimActor() string { return strings.TrimSpace(os.Getenv("BEADS_ACTOR")) }

// doBdByIDClaim acquires a class-store bead for assignee through the closed
// graph contract.
//
// The CAS is the store's, not this function's: the contract's Claim is the
// acquire dual of ReleaseIfCurrent and already carries the deployed semantics —
// a same-owner reclaim is a true no-op, a bead held by someone else or in a
// terminal state is a conflict rather than an error, and concurrent claims
// serialize so exactly one wins. Re-deriving any of that here as a
// read-then-write would lose the single-winner guarantee, which is why the CLI
// projection only calls it.
//
// A conflict is reported to the operator as a non-zero exit, matching what the
// bd passthrough does with a lost claim race; the conflict-is-not-an-error
// property is about the store contract, not about the shell.
func doBdByIDClaim(graph storebinding.GraphStore, id, assignee string, jsonOut bool, binding string, stdout, stderr io.Writer) int {
	if assignee == "" {
		fmt.Fprintf(stderr, "gc bd: claiming %s requires BEADS_ACTOR to name the claimant\n", id) //nolint:errcheck // best-effort stderr
		return 1
	}
	claimed, ok, err := graph.Claim(id, assignee)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd: claiming %s: %v\n", id, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !ok {
		reason := "it is held by another claimant"
		if current, getErr := graph.Get(id); getErr == nil {
			if held := strings.TrimSpace(current.Assignee); held != "" {
				reason = fmt.Sprintf("it is held by %q", held)
			} else {
				reason = fmt.Sprintf("its status is %q", current.Status)
			}
		}
		fmt.Fprintf(stderr, "gc bd: %s was not claimed for %q: %s\n", id, assignee, reason) //nolint:errcheck // best-effort stderr
		return 1
	}
	return printBdByIDBead(claimed, jsonOut, binding, stdout, stderr)
}

// doBdByIDReleaseIfCurrent applies the conditional release through the closed
// graph contract, preserving doBdReleaseIfCurrent's released/skipped output so
// the orphan-recovery scripts that read it keep working when the bead lives in
// a class binding.
func doBdByIDReleaseIfCurrent(graph storebinding.GraphStore, id, expectedAssignee string, stdout, stderr io.Writer) int {
	released, err := graph.ReleaseIfCurrent(id, expectedAssignee)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd release-if-current: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if released {
		fmt.Fprintln(stdout, "released") //nolint:errcheck // best-effort stdout
		return 0
	}
	fmt.Fprintln(stdout, "skipped") //nolint:errcheck // best-effort stdout
	return 0
}

// bdByIDDepRow is one row of `bd dep list --json`: the related issue, plus the
// edge type that related it. bd emits exactly this — an issue object with an
// added dependency_type — and the pack scripts read it with jq field by field,
// so the embedded bead must stay inlined rather than nested.
type bdByIDDepRow struct {
	beads.Bead
	DepType string `json:"dependency_type"`
	// External marks an edge whose other end is not resident in this class
	// store — a declared reference to a work bead. Its typed kind is kept and
	// it is not reported as dangling, but no fields are invented for it: this
	// arm answers from one class store and does not read across the boundary.
	External bool `json:"external,omitempty"`
}

// doBdByIDDepList answers the dependency read from the owning class store.
//
// The edges come from the closed contract's DepList, and each related bead is
// then read through the same front door. An edge pointing out of this class is
// reported as external rather than resolved: crossing to the work store here
// would be a federated cross-store dependency read, and dropping the edge would
// misreport a declared reference as absent.
func doBdByIDDepList(graph storebinding.GraphStore, op bdByIDOp, stdout, stderr io.Writer) int {
	deps, err := graph.DepList(op.ID, op.Direction)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd dep list: listing %s dependencies of %s: %v\n", op.Direction, op.ID, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	rows := make([]bdByIDDepRow, 0, len(deps))
	for _, dep := range deps {
		if op.DepType != "" && dep.Type != op.DepType {
			continue
		}
		related := dep.DependsOnID
		if op.Direction == bdByIDDepDirectionUp {
			related = dep.IssueID
		}
		bead, err := graph.Get(related)
		switch {
		case err == nil:
			rows = append(rows, bdByIDDepRow{Bead: bead, DepType: dep.Type})
		case errors.Is(err, beads.ErrNotFound):
			rows = append(rows, bdByIDDepRow{Bead: beads.Bead{ID: related}, DepType: dep.Type, External: true})
		default:
			fmt.Fprintf(stderr, "gc bd dep list: reading %s: %v\n", related, err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}
	if op.JSON {
		out, err := json.MarshalIndent(rows, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "gc bd dep list: rendering %s: %v\n", op.ID, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		fmt.Fprintln(stdout, string(out)) //nolint:errcheck // best-effort stdout
		return 0
	}
	for _, row := range rows {
		title := row.Title
		if row.External {
			title = "(not resident in this class binding)"
		}
		fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", row.ID, row.DepType, row.Status, title) //nolint:errcheck // best-effort stdout
	}
	return 0
}

// bdByIDTreeRow is one node of `bd dep tree --json`: the bead, plus where the
// walk found it. bd emits a FLAT array in pre-order rather than a nested tree —
// TreeNode has no children field — and the consumers read it that way, so the
// shape is kept rather than improved on.
//
// TreeParentID is the edge the walk arrived by and is a different fact from the
// bead's own `parent`, which the embedded record still carries under that name.
type bdByIDTreeRow struct {
	beads.Bead
	Depth          int    `json:"depth"`
	TreeParentID   string `json:"parent_id"`
	EdgeFromParent string `json:"edge_from_parent,omitempty"`
	// Truncated marks a node whose children were cut by --max-depth. bd's own
	// walker declares the field and never sets it, which reports a silently
	// shortened subtree as complete; this arm answers it, because a summary that
	// reads a cut tree as the whole molecule is the wrong-answer class this
	// surface exists to close.
	Truncated bool `json:"truncated"`
	// External marks an edge whose other end is not resident in this class
	// store, exactly as dep list reports it. Such a node is named but not
	// walked: crossing to the work store here would be a federated cross-store
	// read, and dropping the edge would misreport a declared reference.
	External bool `json:"external,omitempty"`
}

// doBdByIDDepTree answers the recursive dependency read from the owning class
// store, walking the same contract dep list reads once.
//
// The walk reproduces bd's own semantics deliberately, because the two arms
// answer the same command and a consumer cannot tell which one served it:
// pre-order with the root at depth 0, one visit per bead however many paths
// reach it, `relates-to` edges excluded as loose knowledge-graph links rather
// than structure, and --max-depth cutting at depth >= max.
func doBdByIDDepTree(graph storebinding.GraphStore, root beads.Bead, op bdByIDOp, stdout, stderr io.Writer) int {
	visited := map[string]bool{}
	rows := []bdByIDTreeRow{}

	// Recursion depth is bounded by op.MaxDepth, which the parser floors at 1.
	var walk func(bead beads.Bead, depth int, parentID, edge string) error
	walk = func(bead beads.Bead, depth int, parentID, edge string) error {
		row := bdByIDTreeRow{Bead: bead, Depth: depth, TreeParentID: parentID, EdgeFromParent: edge}
		visited[bead.ID] = true
		deps, err := graph.DepList(bead.ID, op.Direction)
		if err != nil {
			return fmt.Errorf("listing %s dependencies of %s: %w", op.Direction, bead.ID, err)
		}
		children := make([]beads.Dep, 0, len(deps))
		for _, dep := range deps {
			if dep.Type == bdByIDDepTreeLooseEdge {
				continue
			}
			children = append(children, dep)
		}
		row.Truncated = len(children) > 0 && depth+1 >= op.MaxDepth
		rows = append(rows, row)
		if row.Truncated {
			return nil
		}
		for _, dep := range children {
			related := dep.DependsOnID
			if op.Direction == bdByIDDepDirectionUp {
				related = dep.IssueID
			}
			if visited[related] {
				continue
			}
			child, err := graph.Get(related)
			switch {
			case err == nil:
				if err := walk(child, depth+1, bead.ID, dep.Type); err != nil {
					return err
				}
			case errors.Is(err, beads.ErrNotFound):
				visited[related] = true
				rows = append(rows, bdByIDTreeRow{
					Bead:           beads.Bead{ID: related},
					Depth:          depth + 1,
					TreeParentID:   bead.ID,
					EdgeFromParent: dep.Type,
					External:       true,
				})
			default:
				return fmt.Errorf("reading %s: %w", related, err)
			}
		}
		return nil
	}

	if err := walk(root, 0, "", ""); err != nil {
		fmt.Fprintf(stderr, "gc bd dep tree: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if op.JSON {
		out, err := json.MarshalIndent(rows, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "gc bd dep tree: rendering %s: %v\n", op.ID, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		fmt.Fprintln(stdout, string(out)) //nolint:errcheck // best-effort stdout
		return 0
	}
	fmt.Fprintf(stderr, "gc bd dep tree: served in process from the class binding\n") //nolint:errcheck // best-effort stderr
	for _, row := range rows {
		title := row.Title
		switch {
		case row.External:
			title = "(not resident in this class binding)"
		case row.Truncated:
			title += " (children cut by --max-depth)"
		}
		fmt.Fprintf(stdout, "%s%s\t%s\t%s\n", strings.Repeat("  ", row.Depth), row.ID, row.Status, title) //nolint:errcheck // best-effort stdout
	}
	return 0
}

// bdByIDDepTreeLooseEdge is the one edge kind bd's tree walker skips: a
// knowledge-graph link rather than structure, so following it would pull an
// unrelated bead into a molecule's subtree.
const bdByIDDepTreeLooseEdge = "relates-to"

// printBdByIDBead renders a routed bead.
//
// --json emits a JSON array of issues, which is what bd emits and what the pack
// parsers already read, and it carries the whole record because beads.Bead
// marshals whole.
//
// The text form is the one that had to change. It used to be an id/status/title
// line, and that is the wrong-answer harm class this surface exists to close,
// one layer in: graph-worker.md tells an agent to `gc bd show <id>` and then
// "execute exactly that bead's description", so a well-formed record with the
// description silently absent is an instruction to do nothing, reported as a
// successful read. It renders the full record instead — description, metadata,
// labels, dependencies, assignee, priority, parent — and says on stderr where
// the record came from, so a human who expected bd's own layout can see why it
// differs rather than assume the bead is thin.
func printBdByIDBead(b beads.Bead, jsonOut bool, binding string, stdout, stderr io.Writer) int {
	if jsonOut {
		out, err := json.MarshalIndent([]beads.Bead{b}, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "gc bd: rendering %q: %v\n", b.ID, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		fmt.Fprintln(stdout, string(out)) //nolint:errcheck // best-effort stdout
		return 0
	}
	fmt.Fprintf(stderr, "gc bd: served in process from %s\n", binding) //nolint:errcheck // best-effort stderr

	line := func(label, value string) {
		if strings.TrimSpace(value) == "" {
			return
		}
		fmt.Fprintf(stdout, "%-12s %s\n", label+":", value) //nolint:errcheck // best-effort stdout
	}
	line("id", b.ID)
	line("title", b.Title)
	line("status", b.Status)
	line("type", b.Type)
	if b.Priority != nil {
		line("priority", strconv.Itoa(*b.Priority))
	}
	line("assignee", b.Assignee)
	line("from", b.From)
	line("parent", b.ParentID)
	line("ref", b.Ref)
	line("created", b.CreatedAt.Format(time.RFC3339))
	if !b.UpdatedAt.IsZero() {
		line("updated", b.UpdatedAt.Format(time.RFC3339))
	}
	line("labels", strings.Join(b.Labels, ", "))
	line("needs", strings.Join(b.Needs, ", "))
	if len(b.Metadata) > 0 {
		fmt.Fprintln(stdout, "metadata:") //nolint:errcheck // best-effort stdout
		keys := make([]string, 0, len(b.Metadata))
		for key := range b.Metadata {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			fmt.Fprintf(stdout, "  %s=%s\n", key, b.Metadata[key]) //nolint:errcheck // best-effort stdout
		}
	}
	if len(b.Dependencies) > 0 {
		fmt.Fprintln(stdout, "dependencies:") //nolint:errcheck // best-effort stdout
		for _, dep := range b.Dependencies {
			fmt.Fprintf(stdout, "  %s\t%s\n", dep.DependsOnID, dep.Type) //nolint:errcheck // best-effort stdout
		}
	}
	// Last, and never elided: an agent reads this to know what to do, so it is
	// the field whose silent absence costs the most.
	fmt.Fprintln(stdout, "description:") //nolint:errcheck // best-effort stdout
	if strings.TrimSpace(b.Description) == "" {
		fmt.Fprintln(stdout, "  (none)") //nolint:errcheck // best-effort stdout
		return 0
	}
	for _, descLine := range strings.Split(b.Description, "\n") {
		fmt.Fprintf(stdout, "  %s\n", descLine) //nolint:errcheck // best-effort stdout
	}
	return 0
}

// doBdByIDUpdate applies a field and metadata write through the closed graph
// contract, then re-reads and renders what the store now holds.
//
// It is one Update call carrying every change, rather than a status write and a
// metadata write in sequence: the step-completion pattern sets the outcome and
// closes in the same command, and splitting it would let a crash between the
// two leave a bead closed with no outcome — the state the reader of that
// metadata cannot tell from a bead that failed to record one.
//
// The re-read is deliberate. bd prints the row after a mutation and callers
// have learned to trust that output; rendering the UpdateOpts back would report
// what was asked for rather than what the store now holds.
func doBdByIDUpdate(graph storebinding.GraphStore, op bdByIDOp, binding string, stdout, stderr io.Writer) int {
	if err := graph.Update(op.ID, op.Update); err != nil {
		fmt.Fprintf(stderr, "gc bd update: %s: %v\n", op.ID, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	updated, err := graph.Get(op.ID)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd update: %s was written and could not be re-read: %v\n", op.ID, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return printBdByIDBead(updated, op.JSON, binding, stdout, stderr)
}

// doBdByIDClose retires a class-store bead through the closed graph contract.
//
// The already-closed answer is the STORE's: the contract's Close carries the
// canonical no-op semantics, and a "is it closed yet" check here would be a
// second implementation of a rule the store already has, free to drift from it.
func doBdByIDClose(graph storebinding.GraphStore, op bdByIDOp, binding string, stdout, stderr io.Writer) int {
	return doBdByIDLifecycleWrite(graph, op, "close", graph.Close, binding, stdout, stderr)
}

// doBdByIDReopen is doBdByIDClose's undo, and exists for the same reason: a
// drain that can close a class resident but not reopen one is a one-way door.
func doBdByIDReopen(graph storebinding.GraphStore, op bdByIDOp, binding string, stdout, stderr io.Writer) int {
	return doBdByIDLifecycleWrite(graph, op, "reopen", graph.Reopen, binding, stdout, stderr)
}

// doBdByIDLifecycleWrite applies a status transition through the closed graph
// contract, then re-reads and renders what the store now holds.
//
// The re-read is doBdByIDUpdate's rule, for doBdByIDUpdate's reason: bd prints
// the row after a mutation and callers have learned to trust that output, so
// rendering the request back would report what was asked for rather than what
// the store now holds. On these two verbs that difference is the whole signal —
// a binding whose read leg and write leg resolve against different tiers is
// visible here as an error, and invisible to a caller that only echoed the
// verb.
func doBdByIDLifecycleWrite(graph storebinding.GraphStore, op bdByIDOp, verb string, write func(string) error, binding string, stdout, stderr io.Writer) int {
	if err := write(op.ID); err != nil {
		fmt.Fprintf(stderr, "gc bd %s: %s: %v\n", verb, op.ID, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	written, err := graph.Get(op.ID)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd %s: %s was written and could not be re-read: %v\n", verb, op.ID, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return printBdByIDBead(written, op.JSON, binding, stdout, stderr)
}
