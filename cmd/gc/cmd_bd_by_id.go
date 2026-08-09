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
// # Three deliberate properties
//
//   - The routed operations take ONLY the closed contract
//     (storebinding.GraphStore). They cannot reach a raw bead store through
//     their parameter, so claim/release CAS lives in the store the contract is
//     bound to and is never re-implemented as a read-then-write in the CLI.
//   - A miss is not flattened into a fall-through when the id can only live in
//     the class store. A reserved-prefix id (gcg-…) is minted by that store and
//     nowhere else, so its absence is genuine absence and is reported in bd's
//     own shape. Falling through would print a work-store answer for a bead the
//     work store never held.
//   - A resolution failure surfaces. Reading "the binding could not be opened"
//     as "the bead is not there" is the root-loss shape this whole lane exists
//     to prevent. A city this build must not serve resolves to the funnel's
//     refusing store, so every routed read on it returns the boot refusal that
//     names the remedy — never absence, and never the work store's answer.
//
// Served here: show, update --claim, release-if-current, and dep list. `gc bd
// heartbeat` is not — it is rewritten to a metadata update before this hook
// runs — and neither is the general query surface, which bd_relocated_classes.go
// refuses on its own terms.
//
// An operation that is NOT served but whose subject the class store owns does
// not fall through either: bd would open the work store, which cannot see the
// bead, and the command would hang or answer about the wrong workspace. It is
// refused instead, before the subprocess. Refusal is the floor, not the goal —
// each verb worth serving is served, and the rest at least fail in a way an
// operator can act on.
//
// Ownership is read from an id in an ID POSITION and never from an id-shaped
// value. A `gc bd list --metadata-field workflow_id=gcg-…` probe is a work
// question that quotes a class id: the work store answers for its own rows and
// must keep doing so. A `--parent gcg-…` names a class bead, and letting the
// work store answer that returns a silent empty result.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

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
	Direction string
	DepType   string
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
		claim := false
		id, jsonOut, ok := parseBdByIDPositional(bdArgs[1:], map[string]*bool{"--claim": &claim})
		if !ok || !claim {
			return bdByIDOp{}, false
		}
		return bdByIDOp{Verb: bdByIDClaim, ID: id, JSON: jsonOut}, true
	case "release-if-current":
		id, assignee, ok, err := parseBdReleaseIfCurrentArgs(bdArgs)
		if err != nil || !ok {
			return bdByIDOp{}, false
		}
		return bdByIDOp{Verb: bdByIDRelease, ID: id, Assignee: assignee}, true
	case "dep":
		if len(bdArgs) < 2 || bdArgs[1] != "list" {
			return bdByIDOp{}, false
		}
		return parseBdDepListArgs(bdArgs[2:])
	}
	return bdByIDOp{}, false
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
		case arg == "--direction", arg == "--type":
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

// bdByIDResolution is what the class-ownership walk concluded about one id: the
// front door of the class that would own it, whether the id can only live there
// (a reserved-prefix id), and the row itself when it is resident.
type bdByIDResolution struct {
	Graph    storebinding.GraphStore
	Bead     beads.Bead
	Found    bool
	Reserved bool
}

// Owned reports whether the class store answers for this id — either because
// the id carries the class's reserved prefix, or because the row is resident.
func (r bdByIDResolution) Owned() bool { return r.Reserved || r.Found }

// bdByIDClassDoor is the opened class front door serving this city.
//
// It owns no handle to release. The store belongs to the one-shot funnel, which
// opens each city's binding at most once per process and closes it where the
// process ends — so a command that touches this surface and a class resolver
// reads one database rather than two, and a routed read cannot close a handle
// another call site is still using.
type bdByIDClassDoor struct {
	Graph   storebinding.GraphStore
	Binding string
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
func openBdByIDClassFrontDoor(cityPath string) (bdByIDClassDoor, bool, error) {
	routes := cliStorageRoutes(cityPath)
	store, relocated := graphClassBinding(routes)
	if !relocated {
		return bdByIDClassDoor{}, false, nil
	}
	graph, err := storebinding.NewBeadsGraphStore(store)
	if err != nil {
		return bdByIDClassDoor{}, false, fmt.Errorf("projecting the class front door of binding %q: %w", routes.binding, err)
	}
	return bdByIDClassDoor{Graph: graph, Binding: routes.binding}, true, nil
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
	resolution := bdByIDResolution{Graph: d.Graph, Reserved: bdIDIsClassReserved(id)}
	bead, err := d.Graph.Get(id)
	switch {
	case err == nil:
		resolution.Bead = bead
		resolution.Found = true
	case errors.Is(err, beads.ErrNotFound):
	case isStandingStorageRefusal(err) && !resolution.Reserved:
	default:
		return bdByIDResolution{}, fmt.Errorf("reading %q from the %s class binding: %w", id, d.Binding, err)
	}
	return resolution, nil
}

// bdIDIsClassReserved reports whether id carries a reserved class id prefix.
// Those prefixes are minted only by the relocated class stores, so such an id
// existing anywhere else is not a thing bd can answer for.
func bdIDIsClassReserved(id string) bool {
	for _, prefix := range config.ReservedClassPrefixes() {
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
func maybeRouteBdByID(cityPath string, bdArgs []string, stdout, stderr io.Writer) (int, bool) {
	op, served := parseBdByIDOp(bdArgs)
	refusable, refusableOK, ambiguous := bdMutationWriteIDs(bdArgs)
	mutationRefusable := refusableOK && !ambiguous && len(refusable) > 0
	listNamed, listNames := bdListNamesClassOwnedBead(bdArgs)
	if !served && !mutationRefusable && !listNames {
		// Nothing here could concern a class-owned id, so the binding is not
		// even opened. An ambiguous mutation scan is left to the fail-closed
		// guard doBd already applies to it.
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
		if listNames {
			// A reserved prefix is proof of ownership on its own, so no
			// residence probe is needed to know the work store cannot answer.
			return refuseClassOwnedTarget(door, "list", listNamed, stderr)
		}
		return refuseClassOwnedPassthrough(door, bdArgs[0], refusable, stderr)
	}

	resolution, err := door.resolve(op.ID)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1, true
	}
	if !resolution.Owned() {
		// A work-store id the class store has never seen: bd is still its
		// truth, and the passthrough answers it byte-identically.
		return 0, false
	}
	if !resolution.Found {
		// Reserved-prefix id with no row: it has nowhere else to live.
		printBdByIDNotFound(stderr, op.ID)
		return 1, true
	}
	switch op.Verb {
	case bdByIDShow:
		return printBdByIDBead(resolution.Bead, op.JSON, stdout, stderr), true
	case bdByIDClaim:
		return doBdByIDClaim(resolution.Graph, op.ID, bdByIDClaimActor(), op.JSON, stdout, stderr), true
	case bdByIDRelease:
		return doBdByIDReleaseIfCurrent(resolution.Graph, op.ID, op.Assignee, stdout, stderr), true
	case bdByIDDepList:
		return doBdByIDDepList(resolution.Graph, op, stdout, stderr), true
	}
	return 0, false
}

// refuseClassOwnedPassthrough stops a write mutation whose subject the class
// store owns but which this surface does not serve.
//
// The alternative is what a bare passthrough does: hand it to bd, which opens
// the work store, cannot see the bead, and either blocks on a work backend that
// has nothing to say or mutates whatever its substring resolver found there
// instead. A refusal with the bead named is recoverable; both of those are not.
//
// handled=false — the passthrough, unchanged — is the answer whenever no
// subject is class-owned, which is every work mutation on these cities.
func refuseClassOwnedPassthrough(door bdByIDClassDoor, verb string, ids []string, stderr io.Writer) (int, bool) {
	for _, id := range ids {
		resolution, err := door.resolve(id)
		if err != nil {
			fmt.Fprintf(stderr, "gc bd: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1, true
		}
		if !resolution.Owned() {
			continue
		}
		return refuseClassOwnedTarget(door, verb, id, stderr)
	}
	return 0, false
}

// refuseClassOwnedTarget is the single refusal message, so every unsupported
// class-owned operation reports the same way.
func refuseClassOwnedTarget(door bdByIDClassDoor, verb, id string, stderr io.Writer) (int, bool) {
	fmt.Fprintf(stderr, "gc bd: %s is owned by the %s class binding, and `gc bd %s` is not served in process; refusing rather than running it against the work store, which does not hold the bead\n", id, door.Binding, verb) //nolint:errcheck // best-effort stderr
	return 1, true
}

// bdListNamesClassOwnedBead reports whether a `gc bd list` NAMES a class-owned
// bead as its target, as opposed to merely mentioning one inside a filter
// value.
//
// A `--metadata-field workflow_id=gcg-…` probe is a work question that happens
// to quote a class id: the work store answers for its own rows and must keep
// doing so, and refusing it exec-fails the consumer that asks it. A
// `--parent gcg-…` or a positional class id is a class-targeted read, and
// letting the work store answer it produces the silent empty result.
//
// Ownership is decided by reserved prefix alone, with no residence probe: this
// runs on every `gc bd list`, and a prefix is proof enough for the only
// decision being made — whether a work store could possibly be the right
// answerer.
func bdListNamesClassOwnedBead(bdArgs []string) (string, bool) {
	if len(bdArgs) == 0 || bdArgs[0] != "list" {
		return "", false
	}
	// The filters whose value is content to match, never a bead being
	// addressed. Every other token in an id position addresses one.
	valueOnly := map[string]bool{"--metadata-field": true, "--label": true}
	args := bdArgs[1:]
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if valueOnly[arg] {
			i++
			continue
		}
		if eq := strings.IndexByte(arg, '='); eq > 0 && strings.HasPrefix(arg, "--") && valueOnly[arg[:eq]] {
			continue
		}
		if bdIDIsClassReserved(arg) {
			return arg, true
		}
	}
	return "", false
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
func doBdByIDClaim(graph storebinding.GraphStore, id, assignee string, jsonOut bool, stdout, stderr io.Writer) int {
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
	return printBdByIDBead(claimed, jsonOut, stdout, stderr)
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

// printBdByIDBead renders a bead in bd's show shape: --json emits a JSON array
// of issues, which is what bd emits and what the pack parsers already read; the
// text form is the id/status/title line.
func printBdByIDBead(b beads.Bead, jsonOut bool, stdout, stderr io.Writer) int {
	if jsonOut {
		out, err := json.MarshalIndent([]beads.Bead{b}, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "gc bd: rendering %q: %v\n", b.ID, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		fmt.Fprintln(stdout, string(out)) //nolint:errcheck // best-effort stdout
		return 0
	}
	fmt.Fprintf(stdout, "%s\t%s\t%s\n", b.ID, b.Status, b.Title) //nolint:errcheck // best-effort stdout
	return 0
}

// printBdByIDNotFound renders genuine absence in bd's own shape so existing
// parsers see the same signal the passthrough would have produced, and adds the
// one thing that differs: class stores resolve ids exactly, so a truncated id
// pasted from a log reads as absent here where bd would have substring-matched
// it.
func printBdByIDNotFound(stderr io.Writer, id string) {
	fmt.Fprintf(stderr, "Error fetching %s: no issue found matching %q\n", id, id)                                                       //nolint:errcheck // best-effort stderr
	fmt.Fprintf(stderr, "gc bd: %s is a class-store id; class stores resolve ids exactly (no substring match) — pass the full id\n", id) //nolint:errcheck // best-effort stderr
}
