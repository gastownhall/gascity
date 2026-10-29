//go:build integration

package main

// The split-city allowlist's one formula-verb admission, against a real bd.
// `gc bd mol wisp <formula> --root-only` is forwarded on a split city because
// the gastown patrols depend on it, and the in-process judge rests on one fact
// about bd: with --root-only bd mints exactly one bead, the molecule root, and
// the formula's step types, labels and metadata do not reach it. Only bd itself
// can show that, so this test seeds a temporary workspace (embedded Dolt), pours
// a formula whose steps are infrastructure-shaped, and classifies what bd wrote.
// It also shows the shape the allowlist refuses: without --root-only the same
// formula mints the infrastructure-class steps.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bazeltest"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// realBdWorkspace seeds a temporary bd workspace run by the pinned bd:
// embedded Dolt, or what initArgs add to bd init (--proxied-server). It
// returns the workspace, a bd runner, and the same runner failing the test on
// an error.
func realBdWorkspace(t *testing.T, initArgs ...string) (string, func(args ...string) ([]byte, error), func(args ...string) []byte) {
	t.Helper()
	bd := bazeltest.DataPath(t, "GC_TEST_PINNED_BD_BIN")
	if bd == "" {
		found, err := findPreferredBinary("bd", strings.TrimSpace(os.Getenv("GC_TEST_BD_BIN")))
		if err != nil {
			t.Skip("bd not found (set GC_TEST_BD_BIN or install bd)")
		}
		bd = found
	}
	configureTestDoltIdentityEnv(t)
	ws := t.TempDir()
	run := beads.ExecCommandRunnerWithEnvWithoutAmbientBeads(map[string]string{
		"BD_BIN":             bd,
		"BEADS_DIR":          filepath.Join(ws, ".beads"),
		"BD_NON_INTERACTIVE": "1",
		"BD_BACKUP_ENABLED":  "false",
		"BD_EXPORT_AUTO":     "false",
		"BEADS_ACTOR":        "gc-test",
	})
	read := func(args ...string) ([]byte, error) { return run(ws, "bd", args...) }
	must := func(args ...string) []byte {
		t.Helper()
		out, err := read(args...)
		if err != nil {
			t.Fatalf("bd %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	if out, err := run(ws, "git", "init", "-q", "."); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	must(append([]string{"init", "-p", "tt", "--quiet", "--skip-agents", "--skip-hooks"}, initArgs...)...)
	if err := os.MkdirAll(filepath.Join(ws, ".beads", "formulas"), 0o755); err != nil {
		t.Fatal(err)
	}
	return ws, read, must
}

func TestGcBdMolWispRootOnlyMintsOneWorkBead(t *testing.T) {
	ws, _, must := realBdWorkspace(t)
	formulas := filepath.Join(ws, ".beads", "formulas")
	formula := `formula = "mol-strand"
version = 1

[vars.label]
default = "triage"

[[steps]]
id = "session"
title = "a label-only infra step"
labels = ["gc:session"]

[[steps]]
id = "graph"
title = "a metadata-only infra step"
[steps.metadata]
"gc.root_bead_id" = "gcg-1"
`
	if err := os.WriteFile(filepath.Join(formulas, "mol-strand.formula.toml"), []byte(formula), 0o644); err != nil {
		t.Fatal(err)
	}

	argv := []string{"mol", "wisp", "mol-strand", "--root-only", "--var", "label=gc:session", "--json"}
	notFound := splitTestDeps(nil, func(string) (beads.Bead, bdReadResult) { return beads.Bead{}, bdReadNotFound })
	if g := bdClassFlipGuard(splitGuardTestCity(), ws, argv, notFound); g.refusal != "" {
		t.Fatalf("the allowlist refused the patrol shape: %s", g.refusal)
	}
	var created struct {
		NewEpicID string            `json:"new_epic_id"`
		IDMapping map[string]string `json:"id_mapping"`
	}
	if err := json.Unmarshal(must(argv...), &created); err != nil || created.NewEpicID == "" {
		t.Fatalf("decoding the wisp result: %v (%+v)", err, created)
	}
	if len(created.IDMapping) > 1 {
		t.Fatalf("a root-only wisp minted %d beads: %v", len(created.IDMapping), created.IDMapping)
	}
	root := realBdShow(t, must, created.NewEpicID)
	if class := coordclass.Classify(root); class != coordclass.ClassWork {
		t.Fatalf("the root-only wisp root %s is %s class (type %q, labels %v, metadata %v)", root.ID, class, root.Type, root.Labels, root.Metadata)
	}

	// Without --root-only, bd mints the infra-shaped steps: why it is refused.
	full := []string{"mol", "wisp", "mol-strand", "--json"}
	if g := bdClassFlipGuard(splitGuardTestCity(), ws, full, notFound); g.refusal == "" {
		t.Fatal("the allowlist forwarded a full wisp")
	}
	if err := json.Unmarshal(must(full...), &created); err != nil {
		t.Fatal(err)
	}
	infra := 0
	for _, id := range created.IDMapping {
		if coordclass.Classify(realBdShow(t, must, id)) != coordclass.ClassWork {
			infra++
		}
	}
	if infra != 2 {
		t.Fatalf("a full wisp minted %d infrastructure-class beads, want the 2 infra steps (mapping %v)", infra, created.IDMapping)
	}
}

// realBdShow reads one bead back from a real bd.
func realBdShow(t *testing.T, must func(args ...string) []byte, id string) beads.Bead {
	t.Helper()
	out := must("show", id, "--json")
	var rows []struct {
		ID       string           `json:"id"`
		Type     string           `json:"issue_type"`
		Labels   []string         `json:"labels"`
		Metadata *beads.StringMap `json:"metadata"`
	}
	if err := json.Unmarshal(out, &rows); err != nil || len(rows) != 1 {
		var row struct {
			ID       string           `json:"id"`
			Type     string           `json:"issue_type"`
			Labels   []string         `json:"labels"`
			Metadata *beads.StringMap `json:"metadata"`
		}
		if err2 := json.Unmarshal(out, &row); err2 != nil {
			t.Fatalf("decoding bd show %s: %v / %v\n%s", id, err, err2, out)
		}
		rows = append(rows[:0], row)
	}
	b := beads.Bead{ID: rows[0].ID, Type: rows[0].Type, Labels: rows[0].Labels}
	if rows[0].Metadata != nil {
		b.Metadata = map[string]string(*rows[0].Metadata)
	}
	return b
}

// realBdPlainFormula writes mol-plain, a formula of two plain steps, into a
// real bd workspace.
func realBdPlainFormula(t *testing.T, ws string) {
	t.Helper()
	formula := "formula = \"mol-plain\"\nversion = 1\n\n[[steps]]\nid = \"a\"\ntitle = \"step a\"\n\n[[steps]]\nid = \"b\"\ntitle = \"step b\"\n"
	if err := os.WriteFile(filepath.Join(ws, ".beads", "formulas", "mol-plain.formula.toml"), []byte(formula), 0o644); err != nil {
		t.Fatal(err)
	}
}

// realBdPour pours (verb pour) or wisps (verb wisp) mol-plain and returns the
// molecule's root.
func realBdPour(t *testing.T, must func(args ...string) []byte, verb string) string {
	t.Helper()
	var created struct {
		NewEpicID string `json:"new_epic_id"`
	}
	if err := json.Unmarshal(must("mol", verb, "mol-plain", "--json"), &created); err != nil || created.NewEpicID == "" {
		t.Fatalf("decoding the %s result: %v (%+v)", verb, err, created)
	}
	return created.NewEpicID
}

// realBdAllIDs lists every bead of a real bd workspace, in every status, type
// and plane, sorted.
func realBdAllIDs(t *testing.T, must func(args ...string) []byte) []string {
	t.Helper()
	var rows []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(must("list", "--all", "--include-infra", "--include-gates", "--include-templates", "--include-ephemeral", "--limit", "0", "--json"), &rows); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	sort.Strings(ids)
	return ids
}

// realBdCreate creates a plain bead with a real bd and returns its id.
func realBdCreate(t *testing.T, must func(args ...string) []byte, args ...string) string {
	t.Helper()
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(must(append([]string{"create", "--json"}, args...)...), &created); err != nil || created.ID == "" {
		t.Fatalf("decoding the created bead: %v (%+v)", err, created)
	}
	return created.ID
}

// realBdGone returns the ids of before that after no longer holds.
func realBdGone(before, after []string) []string {
	var gone []string
	for _, id := range before {
		if !slices.Contains(after, id) {
			gone = append(gone, id)
		}
	}
	return gone
}

// checkForwardedWispDelete runs, against a real bd, the delete gc forwards
// for a judged `mol wisp gc` (bdWispGCDeleteArgs), naming only the root of
// wisp, whose steps depend on it, and checks that bd deleted exactly that
// root: delete --force orphans the beads that depend on it rather than
// deleting them.
func checkForwardedWispDelete(t *testing.T, in bdClassGuardInput, must func(args ...string) []byte, wisp string) {
	t.Helper()
	members, err := in.bdBurnMembers([]string{wisp})
	if err != nil || len(members) != 3 {
		t.Fatalf("listing the wisp's steps: %v, %v", members, err)
	}
	before := realBdAllIDs(t, must)
	must(in.bdWispGCDeleteArgs([]string{wisp})...)
	if gone := realBdGone(before, realBdAllIDs(t, must)); !slices.Equal(gone, []string{wisp}) {
		t.Fatalf("bd %v deleted %v; want exactly the wisp it names, %s, and its steps orphaned", in.bdWispGCDeleteArgs([]string{wisp}), gone, wisp)
	}
}

// TestGcBdBurnAndWispGCJudgesReadRealBd pins the reads the delete-shaped
// judges make to a real bd. The member listing covers every bead `bd mol
// burn` deletes, including a hierarchical child with no parent edge, which
// bd's burn deletes and its `mol show` does not list; the wisp gc dry run
// names the wisps the same gc selects and deletes none of them, and the delete
// gc forwards in place of the gc deletes exactly the wisps it names.
func TestGcBdBurnAndWispGCJudgesReadRealBd(t *testing.T) {
	ws, read, must := realBdWorkspace(t)
	realBdPlainFormula(t, ws)
	all := func() []string { return realBdAllIDs(t, must) }
	root := realBdPour(t, must, "pour")
	orphan := root + ".9"
	must("create", "--id", orphan, "--title", "a hierarchical child with no parent edge", "--json")
	wisp := realBdPour(t, must, "wisp")

	// The whole judge on the patrols' shape, with the id read through bd
	// show as gc bd reads it: a wisp resolves exactly, and nothing in the
	// binding depends on it or its steps.
	store := beads.NewBdStore(ws, func(_, _ string, args ...string) ([]byte, error) { return read(args...) })
	deps := splitTestDeps(nil, func(id string) (beads.Bead, bdReadResult) {
		b, err := store.Get(id)
		if err != nil {
			t.Logf("reading %s: %v", id, err)
			return beads.Bead{}, bdReadFailed
		}
		return b, bdReadFound
	})
	deps.bdRead = read
	var asked []string
	deps.bindingDependents = func(ids []string) (map[string][]string, error) {
		asked = append(asked, ids...)
		return map[string][]string{}, nil
	}
	if g := bdClassFlipGuard(splitGuardTestCity(), ws, []string{"mol", "burn", wisp, "--force"}, deps); g.refusal != "" {
		t.Fatalf("the patrols' wisp burn was refused: %s", g.refusal)
	}
	if len(asked) != 3 || !slices.Contains(asked, wisp) {
		t.Fatalf("the binding was asked about %v; want the wisp %s and its 2 steps", asked, wisp)
	}

	in := bdClassGuardInput{path: "mol burn", bdClassGuardDeps: bdClassGuardDeps{bdRead: read}}
	members, err := in.bdBurnMembers([]string{root})
	if err != nil {
		t.Fatalf("listing the molecule's members: %v", err)
	}
	// On this (direct) route bd's burn orphans a bead that depends on the
	// molecule rather than deleting it; the judge checks it anyway.
	convoy := realBdCreate(t, must, "--title", "tracks a step", "--deps", "tracks:"+members[1])
	if cascade, err := in.bdBurnDependents(members); err != nil || !slices.Equal(cascade, []string{convoy}) {
		t.Fatalf("the dependents read found %v (%v); want %s", cascade, err, convoy)
	}
	before := all()
	must("mol", "burn", root, "--force")
	after := all()
	deleted := realBdGone(before, after)
	if !slices.Contains(deleted, orphan) || len(deleted) != 4 || slices.Contains(deleted, convoy) {
		t.Fatalf("bd mol burn deleted %v; want the root, its 2 steps and the edge-less %s, and %s orphaned", deleted, orphan, convoy)
	}
	for _, id := range deleted {
		if !slices.Contains(members, id) {
			t.Errorf("bd mol burn deleted %s, which the member listing %v did not name", id, members)
		}
	}

	in.bdArgs = []string{"mol", "wisp", "gc", "--age", "1ms", "--all"}
	wisps, err := in.bdWispGCCandidates()
	if err != nil {
		t.Fatalf("the wisp gc dry run: %v", err)
	}
	if len(wisps) != 3 || !slices.Contains(wisps, wisp) {
		t.Fatalf("the dry run selected %v; want the wisp %s and its 2 steps", wisps, wisp)
	}
	if now := all(); !slices.Equal(now, after) {
		t.Fatalf("the dry run deleted beads: %v -> %v", after, now)
	}
	checkForwardedWispDelete(t, in, must, wisp)
}

// TestGcBdBurnJudgeCoversAProxiedServerCascade pins the burn judge to bd's
// proxied-server route, the default transport for a new rig scope. There bd
// burns a persistent molecule with Cascade, which also deletes every bead
// that depends on one of its beads, transitively and through any edge (a
// convoy tracking a step, and whatever depends on the convoy). The beads the
// judge checks (the member listing and bdBurnDependents) must cover
// everything bd deletes, and a class-binding bead waiting on the outermost
// one refuses the burn. The delete gc forwards in place of a judged wisp gc
// deletes exactly the wisps it names on this route too.
func TestGcBdBurnJudgeCoversAProxiedServerCascade(t *testing.T) {
	ws, read, must := realBdWorkspace(t, "--proxied-server")
	t.Cleanup(func() {
		if out, err := read("dolt", "stop"); err != nil {
			t.Logf("stopping the workspace's proxy: %v\n%s", err, out)
		}
	})
	realBdPlainFormula(t, ws)
	root := realBdPour(t, must, "pour")
	in := bdClassGuardInput{path: "mol burn", bdClassGuardDeps: bdClassGuardDeps{bdRead: read}}
	members, err := in.bdBurnMembers([]string{root})
	if err != nil || len(members) != 3 {
		t.Fatalf("listing the molecule's members: %v, %v", members, err)
	}
	convoy := realBdCreate(t, must, "--title", "tracks a step", "--deps", "tracks:"+members[1])
	further := realBdCreate(t, must, "--title", "waits on the convoy", "--deps", convoy)
	unrelated := realBdCreate(t, must, "--title", "depends on nothing")

	cascade, err := in.bdBurnDependents(members)
	if err != nil {
		t.Fatalf("listing the beads that depend on the molecule: %v", err)
	}
	if !slices.Contains(cascade, convoy) || !slices.Contains(cascade, further) || slices.Contains(cascade, unrelated) {
		t.Fatalf("the dependents read found %v; want %s and %s, not %s", cascade, convoy, further, unrelated)
	}

	store := beads.NewBdStore(ws, func(_, _ string, args ...string) ([]byte, error) { return read(args...) })
	deps := splitTestDeps(nil, func(id string) (beads.Bead, bdReadResult) {
		b, err := store.Get(id)
		if err != nil {
			t.Logf("reading %s: %v", id, err)
			return beads.Bead{}, bdReadFailed
		}
		return b, bdReadFound
	})
	deps.bdRead = read
	deps.bindingDependents = func(ids []string) (map[string][]string, error) {
		if slices.Contains(ids, further) {
			return map[string][]string{further: {"gcg-gate"}}, nil
		}
		return map[string][]string{}, nil
	}
	if g := bdClassFlipGuard(splitGuardTestCity(), ws, []string{"mol", "burn", root, "--force"}, deps); !strings.Contains(g.refusal, "gcg-gate") || !strings.Contains(g.refusal, further) {
		t.Fatalf("a binding gate waiting on %s, which the burn cascades to: refusal %q", further, g.refusal)
	}

	before := realBdAllIDs(t, must)
	must("mol", "burn", root, "--force")
	deleted := realBdGone(before, realBdAllIDs(t, must))
	if !slices.Contains(deleted, convoy) || !slices.Contains(deleted, further) {
		t.Fatalf("bd's proxied-server burn deleted %v, sparing what depends on the molecule; this test's premise no longer holds", deleted)
	}
	checked := append(slices.Clone(members), cascade...)
	for _, id := range deleted {
		if !slices.Contains(checked, id) {
			t.Errorf("bd mol burn deleted %s, which the judge did not check (%v)", id, checked)
		}
	}
	if slices.Contains(deleted, unrelated) {
		t.Errorf("bd mol burn deleted %s, which depends on nothing", unrelated)
	}

	checkForwardedWispDelete(t, in, must, realBdPour(t, must, "wisp"))
}
