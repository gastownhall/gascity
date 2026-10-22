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
// federation = "rig" the passthrough checks the write itself before bd runs:
//
//   - a by-id write (update, close, reopen, delete, heartbeat,
//     release-if-current) is refused when any row it names is Foreign, or
//     cannot be read to tell;
//   - a claim (update --claim) needs an admitted actor, and an assignee it
//     sets must be admitted;
//   - a routing value it sets must be rig-qualified, and admitted unless the
//     row is already this city's (a hand-off of its own work);
//   - the creation stamp (gc.owner_namespace) is never set or unset through
//     gc bd, and a create is stamped with the rig's namespace (refused when
//     it cannot be: a batch create, or an ambiguous guarded scope);
//   - writes it cannot vet are refused: `ready --claim`, `close
//     --claim-next`, `delete --cascade` and `delete --from-file`.
//
// Operators keep raw bd. Other bd write verbs (dep, label, comment, mol, ...)
// are outside this check.
type bdRigNamespaceCheck struct {
	ns *rigNamespace
	// get reads a row for classification through the scope's bd front door.
	get func(id string) (beads.Bead, error)
	// actor is the claim actor when the argv names none (--actor).
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
	ns, guarded := rigNamespaceFor(cfg, cityPath, scopeRoot, sessions)
	if !guarded {
		return nil
	}
	return &bdRigNamespaceCheck{ns: ns, get: get, actor: bdEffectiveActorFromEnv(os.Getenv)}
}

// bdEffectiveActorFromEnv is the BEADS_ACTOR the bd child runs as when the
// argv names no actor (bdCommandEnv defaults it to "controller").
func bdEffectiveActorFromEnv(getenv func(string) string) string {
	if v := strings.TrimSpace(getenv("BEADS_ACTOR")); v != "" {
		return v
	}
	return "controller"
}

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
	if verb == "create" || verb == "update" {
		writes, msg, refused := bdMetadataWrites(verb, rest)
		if refused {
			return nil, msg, true
		}
		for _, w := range writes {
			if strings.TrimSpace(w.key) == rigNamespaceOwnerStampKey {
				return nil, fmt.Sprintf("gc bd: refusing %s: %s is written only by the rig namespace guard (rig %q, federation = \"rig\")\n", verb, rigNamespaceOwnerStampKey, rigNamespaceLabel(c.ns)), true
			}
		}
		if verb == "create" {
			return c.checkCreate(bdArgs, rest, writes)
		}
		return c.checkByID(verb, bdArgs, rest, writes)
	}
	switch verb {
	case "close", "reopen", "delete", "heartbeat":
		return c.checkByID(verb, bdArgs, rest, nil)
	case "release-if-current":
		if len(rest) == 0 {
			return bdArgs, "", false // doBd reports the usage error
		}
		return c.checkRows(verb, bdArgs, rest[:1])
	case "ready":
		if bdArgsHaveFlag(rest, "--claim") {
			return nil, fmt.Sprintf("gc bd: refusing ready --claim: rig %q (federation = \"rig\") lets gc claim only rows it can check first; claim a named bead instead\n", rigNamespaceLabel(c.ns)), true
		}
	}
	return bdArgs, "", false
}

func (c *bdRigNamespaceCheck) checkCreate(bdArgs, rest []string, writes []bdMetadataWrite) ([]string, string, bool) {
	stamp, ok := c.ns.ownerStamp()
	if !ok {
		return nil, fmt.Sprintf("gc bd: refusing create: rig %q (federation = \"rig\") cannot stamp the rows it creates\n", rigNamespaceLabel(c.ns)), true
	}
	for _, flag := range []string{"--file", "-f", "--graph"} {
		if bdArgsHaveFlag(rest, flag) {
			return nil, fmt.Sprintf("gc bd: refusing create %s: rig %q (federation = \"rig\") stamps every row gc creates, and a batch create cannot be stamped\n", flag, rigNamespaceLabel(c.ns)), true
		}
	}
	for _, a := range bdFlagValues(rest, "--assignee", "-a") {
		if strings.TrimSpace(a) != "" && !rigNamespacePinAdmitted(c.ns, a) {
			return nil, c.actorRefusal("create", "a new bead", a), true
		}
	}
	for _, w := range writes {
		if !w.unset && isRigNamespaceRouteKey(w.key) && strings.TrimSpace(w.value) != "" && !rigNamespaceRouteAdmitted(c.ns, w.value) {
			return nil, c.routeRefusal("create", "a new bead", w), true
		}
	}
	out, err := bdArgsWithOwnerStamp(bdArgs, stamp)
	if err != nil {
		return nil, fmt.Sprintf("gc bd: refusing create: %v\n", err), true
	}
	return out, "", false
}

func (c *bdRigNamespaceCheck) checkByID(verb string, bdArgs, rest []string, writes []bdMetadataWrite) ([]string, string, bool) {
	switch {
	case verb == "close" && bdArgsHaveFlag(rest, "--claim-next"):
		return nil, fmt.Sprintf("gc bd: refusing close --claim-next: rig %q (federation = \"rig\") lets gc claim only rows it can check first\n", rigNamespaceLabel(c.ns)), true
	case verb == "delete" && (bdArgsHaveFlag(rest, "--cascade") || bdArgsHaveFlag(rest, "--from-file")):
		return nil, fmt.Sprintf("gc bd: refusing delete --cascade/--from-file: rig %q (federation = \"rig\") deletes only rows it can check first\n", rigNamespaceLabel(c.ns)), true
	}
	ids, ambiguous := bdScanPositionalIDs(rest, bdSubcmdValueFlags(verb), bdSubcmdBoolFlags(verb), nil)
	if ambiguous {
		return nil, fmt.Sprintf("gc bd: refusing %s: cannot tell which beads it writes, so rig %q (federation = \"rig\") cannot check them\n", verb, rigNamespaceLabel(c.ns)), true
	}
	args, msg, refused := c.checkRows(verb, bdArgs, ids)
	if refused || verb != "update" {
		return args, msg, refused
	}
	if bdArgsHaveFlag(rest, "--claim") {
		actor := c.actor
		if vs := bdFlagValues(bdArgs, "--actor"); len(vs) > 0 {
			actor = vs[len(vs)-1]
		}
		if !rigNamespacePinAdmitted(c.ns, actor) {
			return nil, c.actorRefusal(verb, strings.Join(ids, ", "), actor), true
		}
	}
	for _, a := range bdFlagValues(rest, "--assignee", "-a") {
		if strings.TrimSpace(a) != "" && !rigNamespacePinAdmitted(c.ns, a) {
			return nil, c.actorRefusal(verb, strings.Join(ids, ", "), a), true
		}
	}
	for _, w := range writes {
		if w.unset || !isRigNamespaceRouteKey(w.key) || strings.TrimSpace(w.value) == "" {
			continue
		}
		if !rigRouteQualified(w.value) {
			return nil, c.routeRefusal(verb, strings.Join(ids, ", "), w), true
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

// checkRows refuses the write when any row it names is Foreign or cannot be
// read to tell.
func (c *bdRigNamespaceCheck) checkRows(verb string, bdArgs, ids []string) ([]string, string, bool) {
	for _, id := range ids {
		row, err := c.get(id)
		if err != nil {
			return nil, fmt.Sprintf("gc bd: refusing %s on %s: rig %q (federation = \"rig\") could not read it to check its owner: %v\n", verb, id, rigNamespaceLabel(c.ns), err), true
		}
		cls := c.ns.classifyRow(row)
		if cls.Class != rowForeign {
			continue
		}
		if cls.Err != nil {
			return nil, fmt.Sprintf("gc bd: refusing %s on %s: rig %q (federation = \"rig\") does not own it (owner %q could not be checked: %v)\n", verb, id, rigNamespaceLabel(c.ns), cls.Value, cls.Err), true
		}
		return nil, fmt.Sprintf("gc bd: refusing %s on %s: rig %q (federation = \"rig\") does not own it (owner %q)\n", verb, id, rigNamespaceLabel(c.ns), cls.Value), true
	}
	return bdArgs, "", false
}

func (c *bdRigNamespaceCheck) actorRefusal(verb, subject, actor string) string {
	return fmt.Sprintf("gc bd: refusing %s on %s: rig %q (federation = \"rig\") serves only %s and this city's pool sessions; actor %q is outside it%s\n", verb, subject, rigNamespaceLabel(c.ns), rigNamespaceShown(c.ns), strings.TrimSpace(actor), rigNamespaceActorHint(c.ns, actor))
}

func (c *bdRigNamespaceCheck) routeRefusal(verb, subject string, w bdMetadataWrite) string {
	return fmt.Sprintf("gc bd: refusing %s on %s: rig %q (federation = \"rig\") takes only routes to its own agents (%s); %s=%q is not one\n", verb, subject, rigNamespaceLabel(c.ns), rigNamespaceShown(c.ns), strings.TrimSpace(w.key), w.value)
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
// (a missing or malformed value, @file input, a non-string value) is
// refused, since the write could then carry anything.
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
					if strings.TrimSpace(k) == rigNamespaceOwnerStampKey || isRigNamespaceRouteKey(k) {
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

// bdArgsHaveFlag reports whether a bool flag appears before "--".
func bdArgsHaveFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == flag || strings.HasPrefix(a, flag+"=") {
			return true
		}
	}
	return false
}

// bdFlagValues returns the values given to any of names before "--", in
// order, in the separate and the inline spelling.
func bdFlagValues(args []string, names ...string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		for _, n := range names {
			if a == n && i+1 < len(args) {
				out = append(out, args[i+1])
			} else if v, ok := strings.CutPrefix(a, n+"="); ok {
				out = append(out, v)
			}
		}
	}
	return out
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
