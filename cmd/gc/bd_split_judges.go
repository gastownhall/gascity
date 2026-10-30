package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/bdflags"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// The judges of the split-city allowlist's mutating commands. Each reads the
// argv as bd parsed it (bdSplitArgs: slice flags already split as pflag splits
// them) and decides with the class-must-not-flip guard (coordclass.CheckFlip)
// and the cross-store edge rule (checkEdgeEndpoints).

// normalizeBdLabels trims labels and drops empty ones, as bd's
// utils.NormalizeLabels does after pflag split them.
func normalizeBdLabels(values []string) []string {
	var out []string
	for _, l := range values {
		if l = strings.TrimSpace(l); l != "" && !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	return out
}

// checkIDs judges a write against every addressed bead. variants are the
// readings of the one write a bd build might make (see bdUpdateMutation); the
// write is refused if ANY of them flips the class. A write that touches no
// field Classify reads passes without a read. Otherwise each id must resolve
// to exactly one bead — the one bd would write — and that bead is judged; an
// id that is absent, resolves to a different bead, or cannot be read is
// refused, because the guard cannot know what the write would do.
func (in bdClassGuardInput) checkIDs(ids []string, variants ...coordclass.Mutation) bdClassGuard {
	touches := false
	for _, m := range variants {
		touches = touches || m.TouchesClass()
	}
	if !touches {
		return in.pass()
	}
	if len(ids) == 0 {
		return in.refuse("gc bd %s: refused on this split city: a write of a bead's type, labels or metadata is judged against the bead it lands on; name the bead explicitly (bd's id-less form writes the last touched issue, which gc cannot read). Nothing was written.", in.path)
	}
	for _, id := range ids {
		before, result := in.read(id)
		if refusal := in.readRefusal(id, result); refusal != "" {
			return in.refuse("%s", refusal)
		}
		for _, m := range variants {
			if flip := coordclass.CheckFlip(before, m.Apply(before), in.holds); flip != nil {
				return in.refuse("%s", classFlipRefusalText(fmt.Sprintf("gc bd %s %s", in.path, id), flip))
			}
		}
	}
	return in.pass()
}

// readRefusal is the refusal for a pre-read that did not find exactly the
// bead id names, or "".
func (in bdClassGuardInput) readRefusal(id string, result bdReadResult) string {
	switch result {
	case bdReadFound:
		return ""
	case bdReadCollision:
		return fmt.Sprintf("gc bd %s %s: refused on this split city: bd resolves %q to a different bead (a short or partial id), so the write would land on a bead gc did not judge. Name the bead by its full id. Nothing was written.", in.path, id, id)
	case bdReadNotFound:
		return fmt.Sprintf("gc bd %s %s: refused on this split city: no bead %q in this store, so gc cannot judge what the write would do. Nothing was written.", in.path, id, id)
	default:
		return fmt.Sprintf("gc bd %s %s: refused on this split city: gc could not read the bead to check the write. Retry once the store answers. Nothing was written.", in.path, id)
	}
}

// checkEdgeEndpoints enforces the cross-store edge rule: every endpoint of an
// edge must be a bead of one of this city's work stores — found there, exactly
// — and one no class binding holds (a migrated relic keeps a work-shaped id
// but lives in the binding). bd accepts an edge to an id it cannot resolve,
// and a dep row naming a bead in another store is one no Ready computation
// clears. An "external:<project>:<capability>" endpoint is bd's
// cross-project capability reference, not a bead id of any store, and is
// allowed as such.
func (in bdClassGuardInput) checkEdgeEndpoints(ids ...string) string {
	var check []string
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || strings.HasPrefix(id, "external:") || slices.Contains(check, id) {
			continue
		}
		check = append(check, id)
	}
	if len(check) == 0 {
		return ""
	}
	results := map[string]bdReadResult{}
	if in.endpoints != nil {
		results = in.endpoints(check)
	} else {
		for _, id := range check {
			_, results[id] = in.read(id)
		}
	}
	for _, id := range check {
		result, ok := results[id]
		if !ok {
			result = bdReadFailed
		}
		if result != bdReadFound {
			return fmt.Sprintf("gc bd %s: refused on this split city: an edge endpoint must be a bead of this city's work stores, and %s. A dependency edge across stores is never written. Nothing was written.", in.path, strings.TrimPrefix(in.readRefusal(id, result), fmt.Sprintf("gc bd %s %s: refused on this split city: ", in.path, id)))
		}
	}
	if in.held == nil {
		return fmt.Sprintf("gc bd %s: refused on this split city: gc cannot check whether %s lives in the class binding. Nothing was written.", in.path, strings.Join(check, ", "))
	}
	held, err := in.held(check)
	if err != nil {
		return fmt.Sprintf("gc bd %s: refused on this split city: gc could not check whether %s lives in the class binding (%v). Nothing was written.", in.path, strings.Join(check, ", "), err)
	}
	for _, id := range check {
		if held[id] {
			return fmt.Sprintf("gc bd %s: refused on this split city: %s lives in the class binding, and a dependency edge across stores is never written. Nothing was written.", in.path, id)
		}
	}
	return ""
}

// bdDepsEndpoints returns the ids a create --deps value names: each element is
// "<id>" or "<type>:<id>".
func bdDepsEndpoints(values []string) []string {
	var out []string
	for _, v := range values {
		v = strings.TrimSpace(v)
		if _, id, typed := strings.Cut(v, ":"); typed {
			v = id
		}
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// bdMetadataObject decodes a --metadata value as bd reads it back: an inline
// JSON object, or @file (read relative to the scope root, where bd runs).
func (in bdClassGuardInput) bdMetadataObject(value string) (map[string]string, string) {
	body := []byte(value)
	if path, isFile := strings.CutPrefix(strings.TrimSpace(value), "@"); isFile {
		if !filepath.IsAbs(path) {
			path = filepath.Join(in.scopeRoot, path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Sprintf("--metadata %s cannot be read to judge it (%v)", value, err)
		}
		body = data
	}
	var fields beads.StringMap
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, fmt.Sprintf("--metadata is not a JSON object (%v)", err)
	}
	return fields, ""
}

// judgeBdCreate classifies the bead `create` / `q` would mint and checks the
// edges it would write.
func judgeBdCreate(in bdClassGuardInput) bdClassGuard {
	if len(in.args.Values("--file")) > 0 {
		return in.refuse("gc bd %s --file: refused on this split city: bd's multi-issue markdown is not classified by gc. Create the beads one at a time with flags. Nothing was written.", in.path)
	}
	if id, ok := in.args.Last("--id"); ok && bdIDIsClassReserved(id) { // residency:allow — refuses minting a NEW bead inside a reserved namespace; resolves no bead
		return in.refuse("gc bd %s --id %s: refused on this split city: that id is inside a class binding's reserved namespace, so the bead would answer to a store that never holds it. Nothing was written.", in.path, id)
	}
	var shape beads.Bead
	if t, ok := in.args.Last("--type"); ok {
		shape.Type = strings.TrimSpace(t)
	}
	shape.Labels = normalizeBdLabels(append(slices.Clone(in.args.Values("--labels")), in.args.Values("--label")...))
	if v, ok := in.args.Last("--metadata"); ok {
		fields, problem := in.bdMetadataObject(v)
		if problem != "" {
			return in.refuse("gc bd %s: refused: %s. Nothing was written.", in.path, problem)
		}
		shape.Metadata = map[string]string{}
		maps.Copy(shape.Metadata, fields)
	}
	if _, ok := in.args.Last("--graph"); ok {
		// bd applies every node of the plan, each with its own type, labels
		// and metadata (decoded as raw JSON), plus parent links and edges.
		// No shipped workflow uses `gc bd create --graph`, so it is refused
		// rather than re-derived node by node.
		return in.refuse("gc bd %s --graph: refused on this split city: a graph plan mints one bead per node, each with its own type, labels, metadata and edges, and gc does not re-derive bd's reading of the plan here. Instead: %s. Nothing was written.", in.path, bdSplitAltSling)
	}
	var endpoints []string
	endpoints = append(endpoints, bdDepsEndpoints(in.args.Values("--deps"))...)
	endpoints = append(endpoints, in.args.Values("--waits-for")...)
	endpoints = append(endpoints, in.args.Values("--parent")...)
	if refusal := in.checkEdgeEndpoints(endpoints...); refusal != "" {
		return in.refuse("%s", refusal)
	}
	// bd create --parent copies the parent's labels onto the child unless
	// --no-inherit-labels is set, reading the parent from its own store (it
	// refuses a parent it does not find there). The edge rule above only
	// says the parent is a work bead of SOME work store, so the labels judged
	// are the ones this store's read returns, and a read that does not find
	// exactly the parent here is refused rather than judged as no labels.
	if parent, ok := in.args.Last("--parent"); ok && strings.TrimSpace(parent) != "" && !in.args.Bool("--no-inherit-labels") {
		p, result := in.read(parent)
		if refusal := in.readRefusal(parent, result); refusal != "" {
			return in.refuse("%s", refusal)
		}
		shape.Labels = normalizeBdLabels(append(shape.Labels, p.Labels...))
	}
	if flip := coordclass.CheckFlip(beads.Bead{}, shape, in.holds); flip != nil {
		return in.refuse("%s", classFlipRefusalText("gc bd "+in.path, flip))
	}
	return in.pass()
}

// judgeBdUpdate judges `gc bd update`: its class-field writes against the
// bead, and a reparent as an edge.
func judgeBdUpdate(in bdClassGuardInput) bdClassGuard {
	variants, problem := in.bdUpdateMutation()
	if problem != "" {
		return in.refuse("gc bd update: refused on this split city: %s. Nothing was written.", problem)
	}
	if parent, ok := in.args.Last("--parent"); ok && strings.TrimSpace(parent) != "" {
		if refusal := in.checkEdgeEndpoints(append(slices.Clone(in.args.positionals), parent)...); refusal != "" {
			return in.refuse("%s", refusal)
		}
	}
	return in.checkIDs(in.args.positionals, variants...)
}

// bdUpdateMutation states a parsed `bd update` as the write it makes, in bd's
// order (labels: replace, add, remove; metadata: merge/set, then unset — see
// coordclass.Mutation.Apply). `--set-metadata k=null` is stored differently by
// the bd builds the fleet runs (bd 1.3.2: JSON null, read back absent; 1.3.0
// Enterprise: the string "null"), so it comes back as two variants and the
// write must pass both.
func (in bdClassGuardInput) bdUpdateMutation() ([]coordclass.Mutation, string) {
	var m coordclass.Mutation
	if t, ok := in.args.Last("--type"); ok {
		v := strings.TrimSpace(t)
		m.Type = &v
	}
	m.AddLabels = normalizeBdLabels(in.args.Values("--add-label"))
	m.RemoveLabels = normalizeBdLabels(in.args.Values("--remove-label"))
	if set, ok := in.args.flags["--set-labels"]; ok {
		m.ReplaceLabels = true
		m.SetLabels = normalizeBdLabels(set)
	}
	var nullKeys []string
	if v, ok := in.args.Last("--metadata"); ok {
		fields, problem := in.bdMetadataObject(v)
		if problem != "" {
			return nil, problem
		}
		m.SetMetadata = map[string]string{}
		maps.Copy(m.SetMetadata, fields)
	}
	for _, kv := range in.args.Values("--set-metadata") {
		key, v, ok := strings.Cut(kv, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return nil, fmt.Sprintf("--set-metadata %q is not key=value", kv)
		}
		if m.SetMetadata == nil {
			m.SetMetadata = map[string]string{}
		}
		m.SetMetadata[key] = v
		if v == "null" {
			nullKeys = append(nullKeys, key)
		}
	}
	m.UnsetMetadata = slices.Clone(in.args.Values("--unset-metadata"))
	variants := []coordclass.Mutation{m}
	if len(nullKeys) > 0 {
		absent := m
		absent.SetMetadata = maps.Clone(m.SetMetadata)
		absent.UnsetMetadata = slices.Clone(m.UnsetMetadata)
		for _, k := range nullKeys {
			delete(absent.SetMetadata, k)
			absent.UnsetMetadata = append(absent.UnsetMetadata, k)
		}
		variants = append(variants, absent)
	}
	return variants, ""
}

// judgeBdEdgeVerb applies the cross-store edge rule to the commands whose job
// is an edge: dep add (positionals, --blocked-by, --depends-on; --file
// refused), dep relate, link, duplicate --of, supersede --with, and gate
// create --blocks.
func judgeBdEdgeVerb(in bdClassGuardInput) bdClassGuard {
	if in.path == "dep add" && len(in.args.Values("--file")) > 0 {
		return in.refuse("gc bd dep add --file: refused on this split city: a dependency file's edges are not judged one by one here; add each edge with gc bd dep add <id> <id>. Nothing was written.")
	}
	var endpoints []string
	switch in.path {
	case "gate create":
		endpoints = in.args.Values("--blocks")
	default:
		endpoints = slices.Clone(in.args.positionals)
		for _, f := range []string{"--blocked-by", "--depends-on", "--of", "--with"} {
			endpoints = append(endpoints, in.args.Values(f)...)
		}
	}
	if refusal := in.checkEdgeEndpoints(endpoints...); refusal != "" {
		return in.refuse("%s", refusal)
	}
	return in.pass()
}

// judgeBdConfigSet admits only the keys in bdSplitConfigSetKeys.
func judgeBdConfigSet(in bdClassGuardInput) bdClassGuard {
	if len(in.args.positionals) < 1 {
		return in.refuse("gc bd config set: refused on this split city: expected <key> <value>. Nothing was written.")
	}
	key := in.args.positionals[0]
	if _, ok := bdSplitConfigSetKeys[key]; !ok {
		return in.refuse("gc bd config set %s: refused on this split city: only %s may be set through gc bd here; other keys (storage-class.*, routing.*, dolt.*, backup.*, sync.*, federation.*, ...) can redirect writes or change what create does. Nothing was written.", key, strings.Join(slices.Sorted(maps.Keys(bdSplitConfigSetKeys)), ", "))
	}
	return in.pass()
}

// judgeBdLabelAddRemove judges `label add|remove <id...> <label[,label]>`, which
// split their label argument on plain commas (no CSV quoting).
func judgeBdLabelAddRemove(in bdClassGuardInput) bdClassGuard {
	pos := in.args.positionals
	if len(pos) < 2 {
		return in.refuse("gc bd %s: refused on this split city: expected <id...> <label>. Nothing was written.", in.path)
	}
	labels := bdLabelVerbList(pos[len(pos)-1])
	m := coordclass.Mutation{AddLabels: labels}
	if in.path == "label remove" {
		m = coordclass.Mutation{RemoveLabels: labels}
	}
	return in.checkIDs(pos[:len(pos)-1], m)
}

// judgeBdLabelPropagate refuses a class-marker label pushed to a parent's
// children, whoever they are.
func judgeBdLabelPropagate(in bdClassGuardInput) bdClassGuard {
	pos := in.args.positionals
	if len(pos) != 2 {
		return in.refuse("gc bd label propagate: refused on this split city: expected <parent-id> <label>. Nothing was written.")
	}
	empty := beads.Bead{}
	if flip := coordclass.CheckFlip(empty, coordclass.Mutation{AddLabels: bdLabelVerbList(pos[1])}.Apply(empty), in.holds); flip != nil {
		return in.refuse("%s", classFlipRefusalText("gc bd label propagate "+pos[0], flip))
	}
	return in.pass()
}

// judgeBdTag judges `tag <id> <label>`, which takes its one label trimmed.
func judgeBdTag(in bdClassGuardInput) bdClassGuard {
	pos := in.args.positionals
	if len(pos) != 2 {
		return in.refuse("gc bd tag: refused on this split city: expected <id> <label>. Nothing was written.")
	}
	return in.checkIDs(pos[:1], coordclass.Mutation{AddLabels: []string{strings.TrimSpace(pos[1])}})
}

// judgeBdSetState judges `set-state <id> <dimension>=<value>`, which swaps the
// bead's first <dimension>:* label for <dimension>:<value>.
func judgeBdSetState(in bdClassGuardInput) bdClassGuard {
	pos := in.args.positionals
	if len(pos) != 2 || !strings.Contains(pos[1], "=") {
		return in.refuse("gc bd set-state: refused on this split city: expected <id> <dimension>=<value>. Nothing was written.")
	}
	id := pos[0]
	dimension, value, _ := strings.Cut(pos[1], "=")
	before, result := in.read(id)
	if refusal := in.readRefusal(id, result); refusal != "" {
		return in.refuse("%s", refusal)
	}
	m := coordclass.Mutation{AddLabels: []string{dimension + ":" + value}}
	for _, l := range before.Labels {
		if strings.HasPrefix(l, dimension+":") {
			m.RemoveLabels = []string{l}
			break
		}
	}
	if flip := coordclass.CheckFlip(before, m.Apply(before), in.holds); flip != nil {
		return in.refuse("%s", classFlipRefusalText("gc bd set-state "+id, flip))
	}
	return in.pass()
}

// judgeBdSQL passes a single plain read statement only (sqlReadOnlyProblem).
func judgeBdSQL(in bdClassGuardInput) bdClassGuard {
	if len(in.args.positionals) != 1 {
		return in.refuse("gc bd sql: refused on this split city: expected exactly one query. Nothing was written.")
	}
	if why := sqlReadOnlyProblem(in.args.positionals[0]); why != "" {
		return in.refuse("gc bd sql: refused on this split city: %s. Only a single plain read statement (SELECT, SHOW, DESCRIBE, EXPLAIN SELECT), with no comments and no Dolt procedures, passes through gc bd sql here: a raw write can give a work-store bead an infrastructure type or label, which strands it. Use the gc bd verbs, which gc judges. Nothing was written.", why)
	}
	return in.pass()
}

// judgeBdMolWisp admits the one wisp shape a workflow depends on — the gastown
// patrols' `gc bd mol wisp <formula> --root-only [--var k=v]...` — and judges,
// in process, every bead it can create, before anything is written.
//
// With --root-only bd creates exactly ONE bead, the molecule root. bd resolves
// the name in this order (wisp.go): a formula (the root is then type molecule
// with no labels or metadata: bd cookFormulaToSubgraph); else a legacy proto by
// id, partial ids included; else, for a mol-* name, the first template bead
// whose title contains the name. In the proto cases bd clones that bead's type,
// labels and metadata. The guard cannot know whether bd's formula lookup will
// succeed, so it judges every candidate: the formula root, the bead the name
// resolves to (a collision is a refusal), and every template bead a mol-* name
// could match. Without --root-only bd instantiates every step, children and
// expansion templates included, and that shape is refused toward gc's own
// sling/cook paths. TestGcBdMolWispRootOnlyMintsOneWorkBead (an integration
// test against a real bd) pins the root shape.
func judgeBdMolWisp(in bdClassGuardInput) bdClassGuard {
	if !in.args.Bool("--root-only") {
		return in.refuse("gc bd %s: refused on this split city: without --root-only it instantiates every step of the formula, and bd copies each step's type, labels and metadata onto the beads it mints in the work store. Instead: %s. Nothing was written.", in.path, bdSplitAltSling)
	}
	if len(in.args.positionals) != 1 {
		return in.refuse("gc bd %s: refused on this split city: expected exactly one formula or proto. Nothing was written.", in.path)
	}
	target := in.args.positionals[0]
	candidates := []beads.Bead{{Type: "molecule"}}
	proto, result := in.read(target)
	switch result {
	case bdReadNotFound:
	case bdReadFound:
		candidates = append(candidates, proto)
	default:
		return in.refuse("%s", in.readRefusal(target, result))
	}
	if strings.HasPrefix(target, "mol-") {
		if in.templates == nil {
			return in.refuse("gc bd %s %s: refused on this split city: gc cannot list the template beads a mol-* name could resolve to. Nothing was written.", in.path, target)
		}
		templates, err := in.templates()
		if err != nil {
			return in.refuse("gc bd %s %s: refused on this split city: gc could not list the template beads a mol-* name could resolve to (%v). Nothing was written.", in.path, target, err)
		}
		for _, t := range templates {
			if strings.Contains(t.Title, target) || t.ID == target {
				candidates = append(candidates, t)
			}
		}
	}
	for _, c := range candidates {
		root := beads.Bead{ID: c.ID, Type: c.Type, Labels: c.Labels, Metadata: c.Metadata}
		if flip := coordclass.CheckFlip(beads.Bead{}, root, in.holds); flip != nil {
			return in.refuse("%s", classFlipRefusalText(fmt.Sprintf("gc bd %s %s (the proto %s it can clone)", in.path, target, c.ID), flip))
		}
	}
	return in.pass()
}

// judgeBdCook passes `cook` that only prints; --persist writes a proto into the
// work store and is refused.
func judgeBdCook(in bdClassGuardInput) bdClassGuard {
	if in.args.Bool("--persist") {
		return in.refuse("gc bd cook --persist: refused on this split city: it writes the formula's steps into the work store as a proto, with each step's type, labels and metadata. Instead: %s. Nothing was written.", bdSplitAltSling)
	}
	return in.pass()
}

// judgeBdBatch judges `gc bd batch`, whose script (stdin or --file) can mint a
// bead of any type with `create <type> ...` and write edges with `dep add`.
func judgeBdBatch(in bdClassGuardInput) bdClassGuard {
	if len(in.args.positionals) != 0 {
		return in.refuse("gc bd batch: refused on this split city: batch takes no positional arguments. Nothing was written.")
	}
	var script []byte
	var err error
	out := in.pass()
	if path, ok := in.args.Last("--file"); ok {
		if !filepath.IsAbs(path) {
			path = filepath.Join(in.scopeRoot, path)
		}
		script, err = os.ReadFile(path)
	} else {
		script, err = io.ReadAll(in.stdin)
		out.stdin = bytes.NewReader(script)
	}
	if err != nil {
		return in.refuse("gc bd batch: refused on this split city: gc must read the batch script to judge it, and could not: %v. Nothing was written.", err)
	}
	if problem := batchScriptClassProblem(script, in.holds); problem != "" {
		return in.refuse("gc bd batch: refused on this split city: %s. Nothing was written.", problem)
	}
	scanner := bufio.NewScanner(bytes.NewReader(script))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		tokens := tokenizeBdBatchLine(strings.TrimSpace(scanner.Text()))
		if len(tokens) >= 4 && tokens[0] == "dep" && tokens[1] == "add" {
			if refusal := in.checkEdgeEndpoints(tokens[2], tokens[3]); refusal != "" {
				return in.refuse("%s", refusal)
			}
		}
	}
	return out
}

// judgeBdImport judges `gc bd import`, the documented restore path for a JSONL
// archive. A row whose class this store does not hold — every infrastructure
// row of an archive taken before the split — would be stranded by bd's
// upsert. Those rows are not routed to the binding: it already holds the live
// copies (gc storage migrate preserved ids), and replaying an archive over them
// would roll sessions and running graphs back. The import is refused and names
// --skip-infra (gc's own flag), which imports a copy holding only the rows this
// store holds and counts the rest. A dry run writes nothing, so it is reported.
// A kept row's dependency must name a kept row of the file or a bead of this
// work store no binding holds (the cross-store edge rule).
func judgeBdImport(in bdClassGuardInput) bdClassGuard {
	args, skipInfra := in.bdArgs, in.gcFlag
	sources := append(slices.Clone(in.args.positionals), in.args.Values("--input")...)
	switch len(sources) {
	case 0:
		return in.refuse("gc bd import: refused on this split city: with no file bd imports its configured import path, which gc does not resolve; name the file (or - for stdin). Nothing was written.")
	case 1:
	default:
		return in.refuse("gc bd import: refused on this split city: more than one import source is named. Nothing was written.")
	}
	source := sources[0]
	var data []byte
	var err error
	if source == "-" {
		data, err = io.ReadAll(in.stdin)
	} else {
		path := source
		if !filepath.IsAbs(path) {
			path = filepath.Join(in.scopeRoot, path)
		}
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return in.refuse("gc bd import: refused on this split city: gc must read %s to check which classes it holds, and could not: %v. Nothing was written.", source, err)
	}
	split, problem := splitBdImportRows(data, in.holds)
	if problem != "" {
		return in.refuse("gc bd import: refused on this split city: %s. Nothing was written.", problem)
	}
	dryRun := in.args.Bool("--dry-run")
	if split.skipped > 0 && !skipInfra && !dryRun {
		return in.refuse("gc bd import: refused on this split city: %s holds %d row(s) of a class this store does not hold (%s). bd import would strand every one in the work store, and every later gc command would refuse until they were repaired. They are not routed to the class store either: it already holds the live copies, and replaying an archive over them would roll sessions and running graphs back. Re-run with %s to import only the %d row(s) this store holds; the rest are left out and counted. Nothing was written.",
			source, split.skipped, formatClassCounts(split.byClass), bdImportSkipInfraFlag, len(split.keep))
	}
	if !dryRun {
		if refusal := in.checkImportEdges(split); refusal != "" {
			return in.refuse("%s", refusal)
		}
	}
	out := bdClassGuard{args: args}
	if split.skipped == 0 {
		if source == "-" {
			out.stdin = bytes.NewReader(data)
		}
		return out
	}
	if dryRun && !skipInfra {
		if source == "-" {
			out.stdin = bytes.NewReader(data)
		}
		out.notice = fmt.Sprintf("gc bd import: dry run: %s holds %d row(s) of a class this store does not hold (%s); on this split city the real import is refused unless re-run with %s, which imports the %d other row(s) only\n",
			source, split.skipped, formatClassCounts(split.byClass), bdImportSkipInfraFlag, len(split.keep))
		return out
	}
	filtered := bytes.Join(split.keep, []byte("\n"))
	if len(filtered) > 0 {
		filtered = append(filtered, '\n')
	}
	out.notice = fmt.Sprintf("gc bd import: %s: importing %d row(s); left out %d row(s) of a class this store does not hold (%s), which this split city keeps in its class store\n",
		source, len(split.keep), split.skipped, formatClassCounts(split.byClass))
	if source == "-" {
		out.stdin = bytes.NewReader(filtered)
		return out
	}
	tmp, err := os.CreateTemp("", "gc-bd-import-work-*.jsonl")
	if err == nil {
		_, err = tmp.Write(filtered)
		if closeErr := tmp.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil {
		if tmp != nil {
			_ = os.Remove(tmp.Name())
		}
		return in.refuse("gc bd import: refused: could not write the filtered copy of %s: %v. Nothing was written.", source, err)
	}
	redirected, ok := bdImportRedirect(args, source, tmp.Name())
	if !ok {
		_ = os.Remove(tmp.Name())
		return in.refuse("gc bd import: refused on this split city: gc could not point bd at the filtered copy of %s (spell the source as a plain argument or --input <file>). Nothing was written.", source)
	}
	out.args = redirected
	name := tmp.Name()
	out.cleanup = func() { _ = os.Remove(name) }
	return out
}

// checkImportEdges applies the cross-store edge rule to the kept rows'
// dependencies: an endpoint must be a kept row of the file, or a bead of this
// work store no binding holds.
func (in bdClassGuardInput) checkImportEdges(split bdImportSplit) string {
	inFile := map[string]bool{}
	for _, id := range split.keptIDs {
		inFile[id] = true
	}
	checked := map[string]bool{}
	for _, dep := range split.keptDeps {
		if inFile[dep] || checked[dep] {
			continue
		}
		checked[dep] = true
		if refusal := in.checkEdgeEndpoints(dep); refusal != "" {
			return refusal
		}
	}
	return ""
}

// bdImportRedirect points the one token of argv that names source (the
// positional, `-i x`, `-ix`, `-i=x`, `--input x`, `--input=x`) at path,
// re-reading argv with import's flags so a flag value that happens to equal
// source is never rewritten.
func bdImportRedirect(argv []string, source, path string) ([]string, bool) {
	out := slices.Clone(argv)
	verbSeen := false
	for i := 0; i < len(out); i++ {
		tok := out[i]
		if !strings.HasPrefix(tok, "-") || tok == "-" {
			if !verbSeen {
				verbSeen = tok == "import"
				continue
			}
			if tok == source {
				out[i] = path
				return out, true
			}
			continue
		}
		name, _, inline := strings.Cut(tok, "=")
		switch {
		case name == "--input" || name == "-i":
			switch {
			case inline:
				out[i] = name + "=" + path
			case i+1 < len(out):
				out[i+1] = path
			default:
				return nil, false
			}
			return out, true
		case strings.HasPrefix(tok, "-i") && !strings.HasPrefix(tok, "--"):
			out[i] = "-i" + path
			return out, true
		}
		// Import's other flags are bools or globals; a global value flag
		// consumes the next token.
		if !inline && bdImportGlobalTakesValue(name) {
			i++
		}
	}
	return nil, false
}

// bdImportGlobalTakesValue reports whether a global flag consumes the next
// token.
func bdImportGlobalTakesValue(name string) bool {
	for _, g := range bdflags.GlobalFlagSpecs() {
		if g.Long == name || (g.Short != "" && g.Short == name) {
			return g.TakesValue
		}
	}
	return false
}

// judgeBdDelete judges `gc bd delete`. Every id (positional or from
// --from-file) must name exactly one bead of this work store: bd resolves a
// short or partial id to whatever bead it prefixes, so a delete is checked
// against the bead it would remove, like every other judged write. Deleting a
// work bead that a class-binding bead depends on, or that an open cross-store
// drain gate waits on (gc.cross_store_blocker), is refused: the binding's edge
// or gate would dangle, and a dependency on a missing bead is one no Ready
// computation clears. --gc-allow-dangling-edges (gc's own flag) overrides that
// check, not the id resolution. --cascade is refused: it deletes dependents gc
// has not judged.
func judgeBdDelete(in bdClassGuardInput) bdClassGuard {
	if in.args.Bool("--cascade") {
		return in.refuse("gc bd delete --cascade: refused on this split city: it deletes every dependent bd finds, which gc does not judge one by one. Delete the beads by id instead. Nothing was written.")
	}
	ids := slices.Clone(in.args.positionals)
	for _, f := range in.args.Values("--from-file") {
		path := f
		if !filepath.IsAbs(path) {
			path = filepath.Join(in.scopeRoot, path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return in.refuse("gc bd delete: refused on this split city: gc must read %s to check the beads it deletes, and could not: %v. Nothing was written.", f, err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
				ids = append(ids, line)
			}
		}
	}
	if len(ids) == 0 {
		return in.pass()
	}
	for _, id := range ids {
		if _, result := in.read(id); result != bdReadFound {
			return in.refuse("%s", in.readRefusal(id, result))
		}
	}
	if in.gcFlag {
		return in.pass()
	}
	id, dependents, err := in.bindingDependentOf(ids)
	switch {
	case err != nil:
		return in.refuse("gc bd delete: refused on this split city: gc could not check the class binding for beads that depend on %s (%v). Nothing was written.", strings.Join(ids, ", "), err)
	case id != "":
		return in.refuse("gc bd delete %s: refused on this split city: %s in the class binding depend(s) or wait(s) on it, and deleting it would leave that edge or gate dangling. Close it instead, or re-run with %s if the dangling edge is intended. Nothing was written.", id, strings.Join(dependents, ", "), bdDeleteAllowDanglingFlag)
	}
	return in.pass()
}

// bindingDependentOf returns the first of ids that a class-binding bead
// depends on, or an open cross-store drain gate waits on
// (gc.cross_store_blocker), with those binding beads; "" when there is none.
// Deleting such a work bead leaves the binding's edge or gate dangling, and a
// dependency on a missing bead is one no Ready computation clears.
func (in bdClassGuardInput) bindingDependentOf(ids []string) (string, []string, error) {
	if len(ids) == 0 {
		return "", nil, nil
	}
	if in.bindingDependents == nil {
		return "", nil, errors.New("gc has no reverse lookup into the class binding here")
	}
	deps, err := in.bindingDependents(ids)
	if err != nil {
		return "", nil, err
	}
	for _, id := range ids {
		if d := deps[id]; len(d) > 0 {
			return id, d, nil
		}
	}
	return "", nil, nil
}

// judgeBdMolBurn judges `gc bd mol burn`, which deletes each molecule it
// names at once: bd resolves the id (a short or partial one to whatever bead
// it prefixes) and deletes the root and the members its molecule loader finds
// below it (for a wisp, the ephemeral ones). On a proxied-server store bd
// deletes a persistent molecule with Cascade, which also deletes every bead
// that depends on one of its beads, transitively and through any edge; its
// direct route orphans those beads instead. Like delete, every id must name
// exactly one bead of this work store, and the burn is refused when a
// class-binding bead depends on, or an open cross-store drain gate waits on,
// any bead it could delete: the molecules' beads (bdBurnMembers) and, for a
// persistent molecule, whatever route bd takes, every bead that depends on
// them (bdBurnDependents). --dry-run deletes nothing.
func judgeBdMolBurn(in bdClassGuardInput) bdClassGuard {
	if in.args.Bool("--dry-run") {
		return in.pass()
	}
	var wispRoots, persistentRoots []string
	for _, id := range in.args.positionals {
		root, result := in.read(id)
		if result != bdReadFound {
			return in.refuse("%s", in.readRefusal(id, result))
		}
		if root.Ephemeral {
			wispRoots = append(wispRoots, id)
		} else {
			persistentRoots = append(persistentRoots, id)
		}
	}
	wispMembers, err := in.bdBurnMembers(wispRoots)
	if err != nil {
		return in.refuse("gc bd mol burn: refused on this split city: gc could not list the beads the burn would delete (%v). Nothing was written.", err)
	}
	molecules, err := in.bdBurnMembers(persistentRoots)
	if err != nil {
		return in.refuse("gc bd mol burn: refused on this split city: gc could not list the beads the burn would delete (%v). Nothing was written.", err)
	}
	cascade, err := in.bdBurnDependents(molecules)
	if err != nil {
		return in.refuse("gc bd mol burn: refused on this split city: gc could not list the beads that depend on the molecule, which bd's burn of a persistent molecule can delete with it (%v). Nothing was written.", err)
	}
	members := append(slices.Clone(molecules), wispMembers...)
	id, dependents, err := in.bindingDependentOf(members)
	switch {
	case err != nil:
		return in.refuse("gc bd mol burn: refused on this split city: gc could not check the class binding for beads that depend on %s (%v). Nothing was written.", strings.Join(members, ", "), err)
	case id != "":
		return in.refuse("gc bd mol burn: refused on this split city: %s in the class binding depend(s) or wait(s) on %s, which the burn would delete, leaving that edge or gate dangling. Close the molecule instead, or delete its beads with gc bd delete %s if the dangling edge is intended. Nothing was written.", strings.Join(dependents, ", "), id, bdDeleteAllowDanglingFlag)
	}
	id, dependents, err = in.bindingDependentOf(cascade)
	switch {
	case err != nil:
		return in.refuse("gc bd mol burn: refused on this split city: gc could not check the class binding for beads that depend on %s (%v). Nothing was written.", strings.Join(cascade, ", "), err)
	case id != "":
		return in.refuse("gc bd mol burn: refused on this split city: %s in the class binding depend(s) or wait(s) on %s, which depends on the molecule: bd's burn of a persistent molecule can delete every bead that depends on one of its beads, leaving that edge or gate dangling. Close the molecule instead, or delete only its own beads with gc bd delete <id>... --force, which leaves the beads that depend on them in place. Nothing was written.", strings.Join(dependents, ", "), id)
	}
	return in.pass()
}

// bdBurnChildrenArgs lists, in every status, type and plane and unpaged, the
// beads bd's `list --parent` filter puts under id: its parent-child
// dependents, and any hierarchical id below it (id.N, id.N.M) that has no
// parent-child edge at all.
func bdBurnChildrenArgs(id string) []string {
	return []string{"list", "--parent=" + id, "--all", "--include-infra", "--include-gates", "--include-templates", "--include-ephemeral", "--limit", "0", "--json"}
}

// bdBurnMembers returns roots and every bead bd lists below them
// (bdBurnChildrenArgs), recursively. That covers the molecule bd's burn
// loads and deletes: its molecule loader (loadTemplateSubgraph, bd 1.3.2 and
// 1.3.0 Enterprise) walks parent-child dependents, and adds a hierarchical
// child id.N only when no parent-child edge points it elsewhere. A persistent
// molecule's burn can delete more (bdBurnDependents).
func (in bdClassGuardInput) bdBurnMembers(roots []string) ([]string, error) {
	if len(roots) == 0 {
		return nil, nil
	}
	if in.bdRead == nil {
		return nil, errors.New("gc cannot read the molecule here")
	}
	var members []string
	seen := map[string]bool{}
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			members = append(members, id)
		}
	}
	for _, id := range roots {
		add(id)
	}
	for i := 0; i < len(members); i++ {
		out, err := in.bdRead(bdBurnChildrenArgs(members[i])...)
		if err != nil {
			return nil, err
		}
		var children []struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(bytes.NewReader(out)).Decode(&children); err != nil {
			return nil, fmt.Errorf("reading bd's list of the children of %s: %w", members[i], err)
		}
		for _, c := range children {
			add(c.ID)
		}
	}
	return members, nil
}

// bdDependentsArgs lists the beads that depend on ids, through any edge type,
// in either plane and in every status: bd's dep list read upward, one answer
// for all of ids.
func bdDependentsArgs(ids []string) []string {
	return append([]string{"dep", "list", "--direction=up", "--json"}, ids...)
}

// bdBurnDependents returns the beads outside ids that depend on one of them,
// transitively and through any edge type, asking bd one level at a time
// (bdDependentsArgs). That is what bd's proxied-server burn of a persistent
// molecule deletes beyond the molecule's own beads: it deletes them with
// Cascade, whose expansion (FindAllDependents, bd 1.3.2 and 1.3.0
// Enterprise) follows every dependency row pointing into the set, in both
// planes — the rows dep list reads.
func (in bdClassGuardInput) bdBurnDependents(ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if in.bdRead == nil {
		return nil, errors.New("gc cannot read the molecule's dependents here")
	}
	seen := map[string]bool{}
	for _, id := range ids {
		seen[id] = true
	}
	var dependents []string
	for frontier := ids; len(frontier) > 0; {
		out, err := in.bdRead(bdDependentsArgs(frontier)...)
		if err != nil {
			return nil, err
		}
		var rows []struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(bytes.NewReader(out)).Decode(&rows); err != nil {
			return nil, fmt.Errorf("reading bd's list of the beads that depend on %s: %w", bdTruncateUTF8(strings.Join(frontier, ", "), 200), err)
		}
		var next []string
		for _, r := range rows {
			if r.ID != "" && !seen[r.ID] {
				seen[r.ID] = true
				next = append(next, r.ID)
			}
		}
		dependents = append(dependents, next...)
		frontier = next
	}
	return dependents, nil
}

// bdWispGCMinAge is the shortest --age `gc bd mol wisp gc` forwards on a split
// city, and bd's own default. It does not decide what may be deleted: gc
// deletes only wisps it checked (judgeBdMolWispGC). It keeps that check from
// refusing the gc over wisps closed a moment ago, whose class-binding gates
// the gate sweep has not released yet: a wisp idle this long closed long
// before the sweep last ran (TestBdWispGCMinAgeStaysAboveTheGateSweep).
const bdWispGCMinAge = time.Hour

// judgeBdMolWispGC judges `gc bd mol wisp gc`, which deletes the work store's
// abandoned wisps at once: open ones idle past --age, closed ones too with
// --all, and the unprotected wisps that depend on them. gc asks bd which wisps
// those are — the same argv with --dry-run --json, which bd answers from the
// same selection and deletes nothing — and refuses, as delete does, when a
// class-binding bead depends on, or an open cross-store drain gate waits on,
// any of them. It then forwards bd's delete of exactly the wisps it checked
// (bdWispGCDeleteArgs), not the gc: bd's gc would select again, and a wisp
// that became a candidate after the check, such as a new dependent of an
// abandoned wisp (bd adds those whatever their age), would be deleted
// unchecked. A checked wisp that a second dry run no longer selects (one
// claimed meanwhile, say) is left out, so bd's protection of live wisps holds
// as of after the check. --closed --force, which deletes every closed wisp
// whatever its age, is refused, and so is an --age under bdWispGCMinAge. A dry
// run, and --closed without --force (bd only counts), delete nothing.
func judgeBdMolWispGC(in bdClassGuardInput) bdClassGuard {
	if in.args.Bool("--dry-run") {
		return in.pass()
	}
	if in.args.Bool("--closed") {
		if !in.args.Bool("--force") {
			return in.pass()
		}
		return in.refuse("gc bd mol wisp gc --closed --force: refused on this split city: it deletes every closed wisp whatever its age, including one closed a moment ago whose class-binding gate the gate sweep has not released yet. Use gc bd mol wisp gc --all --age %gh (or longer), which gc checks wisp by wisp. Nothing was written.", bdWispGCMinAge.Hours())
	}
	// Without --age (or with an empty one) bd selects at its default, an hour
	// in every pinned build (TestBdWispGCDefaultAgeMeetsTheFloor).
	if v, _ := in.args.Last("--age"); v != "" {
		age, err := time.ParseDuration(v)
		if err != nil {
			return in.refuse("gc bd mol wisp gc: refused on this split city: --age %q is not a duration (%v). Nothing was written.", v, err)
		}
		if age < bdWispGCMinAge {
			return in.refuse("gc bd mol wisp gc --age %s: refused on this split city: an age under %gh reaches wisps closed so recently that the gate sweep may not yet have released the class-binding gates waiting on them, and gc refuses the whole gc while one of those gates is open. Use --age %gh or longer. Nothing was written.", v, bdWispGCMinAge.Hours(), bdWispGCMinAge.Hours())
		}
	}
	wisps, err := in.bdWispGCCandidates()
	if err != nil {
		return in.refuse("gc bd mol wisp gc: refused on this split city: gc could not ask bd which wisps it would delete (%v). Nothing was written.", err)
	}
	id, dependents, err := in.bindingDependentOf(wisps)
	switch {
	case err != nil:
		return in.refuse("gc bd mol wisp gc: refused on this split city: gc could not check the class binding for beads that depend on the %d wisp(s) it would delete (%v). Nothing was written.", len(wisps), err)
	case id != "":
		return in.refuse("gc bd mol wisp gc: refused on this split city: %s in the class binding depend(s) or wait(s) on wisp %s, which this gc would delete, leaving that edge or gate dangling. Close that wisp instead, which lets the gate sweep release what waits on it, then run the gc again; or delete it with gc bd delete %s %s if the dangling edge is intended. Nothing was written.", strings.Join(dependents, ", "), id, id, bdDeleteAllowDanglingFlag)
	}
	var again []string
	if len(wisps) > 0 {
		if again, err = in.bdWispGCCandidates(); err != nil {
			return in.refuse("gc bd mol wisp gc: refused on this split city: gc could not ask bd again which of the wisps it checked bd still selects (%v). Nothing was written.", err)
		}
	}
	checked := make(map[string]bool, len(wisps))
	for _, w := range wisps {
		checked[w] = true
	}
	var del []string
	for _, w := range again {
		if checked[w] {
			del = append(del, w)
			checked[w] = false
		}
	}
	if len(del) == 0 {
		return bdClassGuard{
			args:   append(slices.Clone(in.bdArgs), "--dry-run"),
			notice: "gc bd mol wisp gc: no wisp gc checked is still a candidate, so nothing is deleted; bd's dry run follows\n",
		}
	}
	return bdClassGuard{
		args:   in.bdWispGCDeleteArgs(del),
		notice: fmt.Sprintf("gc bd mol wisp gc: deleting the %d wisp(s) bd's dry run selected and gc checked against the class binding (bd delete --force); a wisp that became a candidate after the check is left for the next gc\n", len(del)),
	}
}

// bdWispGCDeleteArgs is the argv gc forwards in place of a judged `mol wisp
// gc`: bd's delete of exactly ids, with the global flags this invocation gave
// (--json, --actor and the rest, restated as --flag=value). --force makes it
// the delete the gc itself runs on bd's direct route (deleteBatch with force
// and no cascade, bd 1.3.2 and 1.3.0 Enterprise): it deletes without asking,
// removes the wisps' edges, and orphans what depends on them.
func (in bdClassGuardInput) bdWispGCDeleteArgs(ids []string) []string {
	args := []string{"delete", "--force"}
	for _, g := range bdflags.GlobalFlagSpecs() {
		for _, v := range in.args.Values(g.Long) {
			args = append(args, g.Long+"="+v)
		}
	}
	return append(args, ids...)
}

// bdWispGCCandidates asks bd which wisps this `mol wisp gc` argv would
// delete: the same argv with --dry-run --json, which bd answers from the same
// selection (findAbandonedWisps, bd 1.3.2 and 1.3.0 Enterprise) and deletes
// nothing. It does not build that argv past a -- terminator, where the added
// flags would be arguments and the run real.
func (in bdClassGuardInput) bdWispGCCandidates() ([]string, error) {
	if slices.Contains(in.bdArgs, "--") {
		return nil, errors.New("gc does not ask bd for a dry run of an argv with a -- terminator; drop the --")
	}
	if in.bdRead == nil {
		return nil, errors.New("gc cannot ask bd for a dry run here")
	}
	out, err := in.bdRead(append(slices.Clone(in.bdArgs), "--dry-run", "--json")...)
	if err != nil {
		return nil, err
	}
	var plan struct {
		CleanedIDs []string `json:"cleaned_ids"`
		DryRun     bool     `json:"dry_run"`
	}
	if err := json.NewDecoder(bytes.NewReader(out)).Decode(&plan); err != nil {
		return nil, fmt.Errorf("reading bd's dry run: %w", err)
	}
	if !plan.DryRun {
		return nil, fmt.Errorf("bd's answer does not say it was a dry run: %q", bdTruncateUTF8(string(out), 200))
	}
	return plan.CleanedIDs, nil
}

// bdSweepMinAge is the shortest --older-than `gc bd purge` and `gc bd prune`
// forward on a split city. Unlike delete, mol burn and mol wisp gc, they
// cannot be checked bead by bead against the class binding: bd's dry run
// counts the beads it would delete without naming them. Age is what keeps them
// clear of a work bead an open cross-store drain gate still waits on: the gate
// sweep releases such a gate once its work blocker closes, and a bead closed a
// week ago closed long before the sweep last ran. The core reaper's default
// ages meet it (TestReaperPurgeAgeStaysAboveTheGateSweep).
const bdSweepMinAge = 7 * 24 * time.Hour

// judgeBdSweep judges `gc bd purge` and `gc bd prune`, which delete the closed
// beads bd selects at once: purge the closed wisps (with --wisps-plane, every
// closed row of the wisps table), prune the closed durable beads. bd keeps
// back a bead that a live bead of the work store depends on (purge) or that an
// open one cites (prune), but it cannot see the class binding, and gc cannot
// ask the binding about beads bd does not name, so the deletion is held to age
// (bdSweepMinAge): --older-than must be given and, read as bd reads it, be at
// least that. prune's --ignore-references, which also deletes closed beads
// open beads still cite, is refused. A dry run, and a run without --force (bd
// only counts), delete nothing.
func judgeBdSweep(in bdClassGuardInput) bdClassGuard {
	if in.args.Bool("--dry-run") || !in.args.Bool("--force") {
		return in.pass()
	}
	if in.args.Bool("--ignore-references") {
		return in.refuse("gc bd %s --ignore-references: refused on this split city: it also deletes closed beads that open beads still cite, and gc does not check the beads bd selects one by one. Delete such a bead with gc bd delete <id> --force, which gc checks against the class binding. Nothing was written.", in.path)
	}
	value, _ := in.args.Last("--older-than")
	if value == "" {
		return in.refuse("gc bd %s: refused on this split city: without --older-than it deletes closed beads whatever their age, including one closed a moment ago that an open cross-store drain gate in the class binding still waits on, and gc cannot check the beads bd selects one by one. Add --older-than %gh (or longer). Nothing was written.", in.path, bdSweepMinAge.Hours())
	}
	age, err := bdOlderThanAge(value)
	if err != nil {
		return in.refuse("gc bd %s: refused on this split city: --older-than %q is not an age bd reads (%v). Nothing was written.", in.path, value, err)
	}
	if age < bdSweepMinAge {
		return in.refuse("gc bd %s --older-than %s: refused on this split city: an age under %gh reaches beads closed so recently that an open cross-store drain gate in the class binding may still wait on one, and gc cannot check the beads bd selects one by one. Use --older-than %gh or longer. Nothing was written.", in.path, value, bdSweepMinAge.Hours(), bdSweepMinAge.Hours())
	}
	return in.pass()
}

// bdOlderThanAge reads a purge or prune --older-than value as bd does
// (parseOlderThan, bd 1.3.2 and 1.3.0 Enterprise): a bare number is days, Nd
// and Nw are days and weeks, and anything else is a Go duration, the unit
// letters in either case. A day count that is not positive is an error, as in
// bd 1.3.2; the Enterprise build does not check it, and there a negative count
// large enough wraps around to a short age.
func bdOlderThanAge(value string) (time.Duration, error) {
	s := strings.TrimSpace(value)
	if s == "" {
		return 0, errors.New("empty duration")
	}
	maxDays := math.MaxInt64 / int64(24*time.Hour)
	days := func(n int64) (time.Duration, error) {
		switch {
		case n <= 0:
			return 0, errors.New("duration must be positive")
		case n > maxDays:
			return 0, fmt.Errorf("duration out of range (at most %d days)", maxDays)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		return days(int64(n))
	}
	switch unit := s[len(s)-1]; unit {
	case 'd', 'D', 'w', 'W':
		n, err := strconv.Atoi(s[:len(s)-1])
		if err != nil {
			return 0, fmt.Errorf("invalid number %q", s[:len(s)-1])
		}
		count := int64(n)
		if unit == 'w' || unit == 'W' {
			if count > maxDays/7 || count < -maxDays/7 {
				return 0, fmt.Errorf("duration out of range (at most %d days)", maxDays)
			}
			count *= 7
		}
		return days(count)
	}
	age, err := time.ParseDuration(strings.ToLower(s))
	if err != nil {
		return 0, err
	}
	if age <= 0 {
		return 0, errors.New("duration must be positive")
	}
	return age, nil
}

// judgeBdInit refuses an issue prefix inside a class binding's reserved
// namespace: the work store would mint ids that answer to the binding.
func judgeBdInit(in bdClassGuardInput) bdClassGuard {
	if prefix, ok := in.args.Last("--prefix"); ok && bdIDIsClassReserved(strings.TrimSuffix(strings.TrimSpace(prefix), "-")+"-x") { // residency:allow — judges a new store's id prefix; resolves no bead
		return in.refuse("gc bd init --prefix %s: refused on this split city: that prefix is a class binding's reserved namespace. Pick a work prefix. Nothing was written.", prefix)
	}
	return in.pass()
}
