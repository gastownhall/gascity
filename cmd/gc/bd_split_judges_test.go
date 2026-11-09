//go:build !windows

package main

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/bdflags"
	"github.com/gastownhall/gascity/internal/beads"
)

// TestGcBdSplitMolBurnJudgesWhatItDeletes: `mol burn` deletes the molecule an
// id names at once, so like delete every id must name exactly one work-store
// bead, and the burn is refused when a class-binding bead depends on any bead
// it could delete: the root, and every bead bd lists below it, recursively.
func TestGcBdSplitMolBurnJudgesWhatItDeletes(t *testing.T) {
	read := func(id string) (beads.Bead, bdReadResult) {
		switch id {
		case "demo-1":
			return beads.Bead{ID: id, Type: "molecule"}, bdReadFound
		case "demo-3":
			return beads.Bead{ID: id, Type: "molecule", Ephemeral: true}, bdReadFound
		case "vrt":
			return beads.Bead{}, bdReadNotFound
		case "dem":
			return beads.Bead{}, bdReadCollision
		}
		return beads.Bead{}, bdReadFailed
	}
	// demo-1 has a step, which has a child of its own; demo-3 is a
	// root-only patrol wisp.
	children := map[string][]string{"demo-1": {"demo-1.1"}, "demo-1.1": {"demo-1.1.1"}}
	var asked []string
	deps := splitTestDeps(nil, read)
	deps.bdRead = fakeBdRead(children, nil, nil)
	deps.bindingDependents = func(ids []string) (map[string][]string, error) {
		asked = append(asked, ids...)
		return map[string][]string{"demo-1.1.1": {"gcg-gate1"}}, nil
	}
	judge := func(argv ...string) bdClassGuard {
		return bdClassFlipGuard(splitGuardTestCity(), t.TempDir(), argv, deps)
	}

	g := judge("mol", "burn", "demo-1", "--force")
	for _, want := range []string{"gcg-gate1", "demo-1.1.1", "which the burn would delete", "Nothing was written"} {
		if !strings.Contains(g.refusal, want) {
			t.Errorf("burning a molecule a binding bead depends on: refusal %q does not carry %q", g.refusal, want)
		}
	}
	for _, id := range []string{"demo-1", "demo-1.1", "demo-1.1.1"} {
		if !slices.Contains(asked, id) {
			t.Errorf("the binding was not asked about %s, which the burn deletes (asked %v)", id, asked)
		}
	}
	if g := judge("mol", "burn", "demo-3", "demo-1", "--force"); !strings.Contains(g.refusal, "gcg-gate1") {
		t.Errorf("a batch burn including that molecule: refusal %q", g.refusal)
	}
	if g := judge("mol", "burn", "demo-3", "--force"); g.refusal != "" {
		t.Errorf("burning a root-only wisp nothing depends on was refused: %q", g.refusal)
	}
	if g := judge("mol", "burn", "demo-1", "--dry-run"); g.refusal != "" {
		t.Errorf("a dry run deletes nothing but was refused: %q", g.refusal)
	}
	for argv, want := range map[string]string{
		"mol burn vrt --force":        `no bead "vrt"`,
		"mol burn dem --force":        "a short or partial id",
		"mol burn demo-x --force":     "could not read",
		"mol burn demo-3 vrt --force": `no bead "vrt"`,
	} {
		if g := judge(strings.Fields(argv)...); !strings.Contains(g.refusal, want) {
			t.Errorf("gc bd %s: refusal = %q, want %q", argv, g.refusal, want)
		}
	}

	// A read that fails, or an answer gc cannot read, is a refusal.
	deps.bdRead = func(...string) ([]byte, error) { return nil, errors.New("bd: database locked") }
	if g := judge("mol", "burn", "demo-3", "--force"); !strings.Contains(g.refusal, "database locked") {
		t.Errorf("a failed member listing: refusal %q", g.refusal)
	}
	deps.bdRead = func(...string) ([]byte, error) { return []byte("Issue 'demo-3' has no children"), nil }
	if g := judge("mol", "burn", "demo-3", "--force"); !strings.Contains(g.refusal, "could not list the beads") {
		t.Errorf("an unreadable member listing: refusal %q", g.refusal)
	}
	deps.bdRead = fakeBdRead(children, nil, nil)
	deps.bindingDependents = func([]string) (map[string][]string, error) { return nil, errors.New("binding unavailable") }
	if g := judge("mol", "burn", "demo-3", "--force"); !strings.Contains(g.refusal, "binding unavailable") {
		t.Errorf("a failed binding lookup: refusal %q", g.refusal)
	}
}

// TestGcBdSplitMolBurnJudgesWhatAPersistentBurnCascadesTo: on a proxied-server
// store bd burns a persistent molecule with Cascade, which also deletes every
// bead that depends on one of its beads, transitively and through any edge.
// So that burn is refused when a class-binding bead depends on one of those
// beads, however far out it is. A wisp's burn deletes no dependent, so a
// binding bead waiting on a wisp step's dependent does not refuse it.
func TestGcBdSplitMolBurnJudgesWhatAPersistentBurnCascadesTo(t *testing.T) {
	read := func(id string) (beads.Bead, bdReadResult) {
		switch id {
		case "demo-1":
			return beads.Bead{ID: id, Type: "molecule"}, bdReadFound
		case "demo-w":
			return beads.Bead{ID: id, Type: "molecule", Ephemeral: true}, bdReadFound
		}
		return beads.Bead{}, bdReadFailed
	}
	// demo-1 has a step, a convoy (demo-9) tracks the step, and a work bead
	// (demo-10) depends on the convoy, which depends on it in turn (a
	// cycle). A work bead (demo-11) depends on the wisp demo-w's step.
	children := map[string][]string{"demo-1": {"demo-1.1"}, "demo-w": {"demo-w.1"}}
	dependents := map[string][]string{
		"demo-1":   {"demo-1.1"},
		"demo-1.1": {"demo-9"},
		"demo-9":   {"demo-10"},
		"demo-10":  {"demo-9"},
		"demo-w.1": {"demo-11"},
	}
	binding := map[string][]string{}
	var asked []string
	deps := splitTestDeps(nil, read)
	deps.bdRead = fakeBdRead(children, dependents, nil)
	deps.bindingDependents = func(ids []string) (map[string][]string, error) {
		asked = append(asked, ids...)
		return binding, nil
	}
	judge := func(argv ...string) bdClassGuard {
		asked = nil
		return bdClassFlipGuard(splitGuardTestCity(), t.TempDir(), argv, deps)
	}

	if g := judge("mol", "burn", "demo-1", "--force"); g.refusal != "" {
		t.Fatalf("a burn nothing in the binding waits on was refused: %q", g.refusal)
	}
	for _, id := range []string{"demo-1", "demo-1.1", "demo-9", "demo-10"} {
		if !slices.Contains(asked, id) {
			t.Errorf("the binding was not asked about %s, which a proxied-server burn deletes (asked %v)", id, asked)
		}
	}
	binding = map[string][]string{"demo-10": {"gcg-gate2"}}
	g := judge("mol", "burn", "demo-1", "--force")
	for _, want := range []string{"gcg-gate2", "demo-10", "depends on the molecule", "--force", "Nothing was written"} {
		if !strings.Contains(g.refusal, want) {
			t.Errorf("a binding gate waiting on a bead two edges out of the molecule: refusal %q does not carry %q", g.refusal, want)
		}
	}
	if g := judge("mol", "burn", "demo-w", "demo-1", "--force"); !strings.Contains(g.refusal, "gcg-gate2") {
		t.Errorf("a batch burn including that molecule: refusal %q", g.refusal)
	}

	binding = map[string][]string{"demo-11": {"gcg-gate3"}}
	if g := judge("mol", "burn", "demo-w", "--force"); g.refusal != "" {
		t.Errorf("a wisp burn, which deletes no dependent, was refused over one: %q", g.refusal)
	}
	if slices.Contains(asked, "demo-11") {
		t.Errorf("the binding was asked about demo-11, which no wisp burn deletes (asked %v)", asked)
	}

	// A dependents read that fails, or an answer gc cannot read, is a refusal.
	for name, answer := range map[string]struct {
		out []byte
		err error
	}{
		"a failed read":     {nil, errors.New("bd: dep list failed")},
		"an unreadable one": {[]byte("No issues depend on demo-1\n"), nil},
	} {
		deps.bdRead = func(args ...string) ([]byte, error) {
			if args[0] == "dep" {
				return answer.out, answer.err
			}
			return fakeBdRead(children, dependents, nil)(args...)
		}
		if g := judge("mol", "burn", "demo-1", "--force"); !strings.Contains(g.refusal, "could not list the beads that depend on the molecule") {
			t.Errorf("%s: refusal %q", name, g.refusal)
		}
	}
}

// TestBdDependentsArgsListEveryDependent: the dependents read is bd's dep
// list read upward, unfiltered by edge type, for every id at once; every
// pinned build resolves it to dep list.
func TestBdDependentsArgsListEveryDependent(t *testing.T) {
	args := bdDependentsArgs([]string{"demo-1", "demo-1.1"})
	if want := []string{"dep", "list", "--direction=up", "--json", "demo-1", "demo-1.1"}; !slices.Equal(args, want) {
		t.Errorf("bd %v, want bd %v", args, want)
	}
	for _, inv := range bdflags.Inventories() {
		res, err := inv.Resolve(args)
		if err != nil || res.Path != "dep list" || !slices.Equal(res.Positionals, []string{"demo-1", "demo-1.1"}) {
			t.Errorf("%s: the dependents read does not resolve to bd dep list of both ids: %+v, %v", inv.Name, res, err)
		}
		if _, typed := res.Flags["--type"]; typed {
			t.Errorf("%s: the dependents read filters by edge type", inv.Name)
		}
	}
}

// TestBdBurnChildrenArgsListEveryChild: the member listing asks bd for every
// child of every status, type and plane, unpaged; bd list hides closed rows,
// infra types, gates, templates and the wisp plane and stops at 50 by default.
func TestBdBurnChildrenArgsListEveryChild(t *testing.T) {
	got := strings.Join(bdBurnChildrenArgs("demo-1"), " ")
	for _, want := range []string{"list ", "--parent=demo-1", "--all", "--include-infra", "--include-gates", "--include-templates", "--include-ephemeral", "--limit 0", "--json"} {
		if !strings.Contains(got, want) {
			t.Errorf("bd %s does not carry %s", got, want)
		}
	}
	for _, inv := range bdflags.Inventories() {
		res, err := inv.Resolve(bdBurnChildrenArgs("demo-1"))
		if err != nil || res.Path != "list" {
			t.Errorf("%s: the member listing does not resolve to bd list: %+v, %v", inv.Name, res, err)
		}
	}
}

// TestGcBdSplitMolWispGCJudgesBdsDryRun: `mol wisp gc` deletes the wisps bd
// selects at once. gc asks bd for a dry run of the same argv and refuses when
// a class-binding bead depends on one of them, and otherwise forwards bd's
// delete of the wisps it checked; --closed --force and an --age under an hour
// are refused before bd is asked.
func TestGcBdSplitMolWispGCJudgesBdsDryRun(t *testing.T) {
	var reads [][]string
	deps := splitTestDeps(nil, func(string) (beads.Bead, bdReadResult) { return beads.Bead{}, bdReadNotFound })
	wisps := []string{"demo-w1", "demo-w2"}
	deps.bdRead = func(args ...string) ([]byte, error) {
		reads = append(reads, args)
		return fakeBdRead(nil, nil, wisps)(args...)
	}
	dependents := map[string][]string{"demo-w2": {"gcg-drain"}}
	var asked []string
	deps.bindingDependents = func(ids []string) (map[string][]string, error) {
		asked = append(asked, ids...)
		return dependents, nil
	}
	judge := func(argv ...string) bdClassGuard {
		reads = nil
		return bdClassFlipGuard(splitGuardTestCity(), t.TempDir(), argv, deps)
	}

	g := judge("mol", "wisp", "gc", "--age", "24h", "--exclude-type", "agent,rig")
	for _, want := range []string{"gcg-drain", "demo-w2", "which this gc would delete", bdDeleteAllowDanglingFlag, "Nothing was written"} {
		if !strings.Contains(g.refusal, want) {
			t.Errorf("a gc that would delete a wisp a binding bead depends on: refusal %q does not carry %q", g.refusal, want)
		}
	}
	want := []string{"mol", "wisp", "gc", "--age", "24h", "--exclude-type", "agent,rig", "--dry-run", "--json"}
	if len(reads) != 1 || !slices.Equal(reads[0], want) {
		t.Errorf("bd was asked %v, want one dry run of the same argv %v", reads, want)
	}
	if !slices.Equal(asked, wisps) {
		t.Errorf("the binding was asked about %v, want every wisp the dry run selected %v", asked, wisps)
	}

	dependents = map[string][]string{}
	for _, argv := range [][]string{
		{"mol", "wisp", "gc", "--age", "24h"},
		{"mol", "wisp", "gc"},
		{"mol", "wisp", "gc", "--age="},
		{"mol", "wisp", "gc", "--all", "--age", "90m"},
	} {
		g := judge(argv...)
		if g.refusal != "" || len(reads) != 2 {
			t.Errorf("gc bd %s: refused=%q after %d read(s); want it forwarded after two dry runs", strings.Join(argv, " "), g.refusal, len(reads))
		}
		if want := []string{"delete", "--force", "demo-w1", "demo-w2"}; !slices.Equal(g.args, want) {
			t.Errorf("gc bd %s forwards bd %v, want bd's delete of the wisps gc checked, %v", strings.Join(argv, " "), g.args, want)
		}
	}
	// Nothing to delete, or nothing deleted: bd is not asked.
	for _, argv := range [][]string{
		{"mol", "wisp", "gc", "--dry-run"},
		{"mol", "wisp", "gc", "--closed"},
		{"mol", "wisp", "gc", "--closed", "--dry-run", "--force"},
	} {
		if g := judge(argv...); g.refusal != "" || len(reads) != 0 {
			t.Errorf("gc bd %s: refused=%q after %d read(s); it deletes nothing", strings.Join(argv, " "), g.refusal, len(reads))
		}
	}
	for argv, want := range map[string]string{
		"mol wisp gc --closed --force":                      "--closed --force",
		"mol wisp gc --closed -f":                           "--closed --force",
		"mol wisp gc --age 30m":                             "under 1h",
		"mol wisp gc --age=1s --all":                        "under 1h",
		"mol wisp gc --age banana":                          "not a duration",
		"mol wisp gc --age 24h -- --closed":                 "terminator",
		"mol wisp gc --age 24h --dry-run=false --closed -f": "--closed --force",
	} {
		if g := judge(strings.Fields(argv)...); !strings.Contains(g.refusal, want) || len(reads) != 0 {
			t.Errorf("gc bd %s: refusal = %q after %d read(s), want %q before bd is asked", argv, g.refusal, len(reads), want)
		}
	}

	// A dry run gc cannot get or read is a refusal.
	deps.bdRead = func(...string) ([]byte, error) { return nil, errors.New("bd: database locked") }
	if g := judge("mol", "wisp", "gc", "--age", "24h"); !strings.Contains(g.refusal, "database locked") {
		t.Errorf("a failed dry run: refusal %q", g.refusal)
	}
	for name, answer := range map[string]string{
		"text, not JSON":          "No abandoned wisps found\n",
		"an answer not a dry run": `{"cleaned_ids":[],"cleaned_count":0}`,
	} {
		deps.bdRead = func(...string) ([]byte, error) { return []byte(answer), nil }
		if g := judge("mol", "wisp", "gc", "--age", "24h"); g.refusal == "" || !strings.Contains(g.refusal, "Nothing was written") {
			t.Errorf("%s: refusal %q", name, g.refusal)
		}
	}
	deps.bdRead = fakeBdRead(nil, nil, wisps)
	deps.bindingDependents = func([]string) (map[string][]string, error) { return nil, errors.New("binding unavailable") }
	if g := judge("mol", "wisp", "gc", "--age", "24h"); !strings.Contains(g.refusal, "binding unavailable") {
		t.Errorf("a failed binding lookup: refusal %q", g.refusal)
	}
}

// TestGcBdSplitMolWispGCDeletesOnlyTheWispsItChecked: gc forwards bd's delete
// of exactly the wisps it checked, never the gc itself, whose own selection
// could reach a wisp gc never checked. A wisp that became a candidate after
// the check (demo-w9, say a new dependent of an abandoned wisp, which bd
// selects whatever its age) is left for the next gc, and a checked wisp that
// bd's second dry run no longer selects (demo-w2, claimed meanwhile) is left
// out. The invocation's global flags reach the delete.
func TestGcBdSplitMolWispGCDeletesOnlyTheWispsItChecked(t *testing.T) {
	selections := [][]string{{"demo-w1", "demo-w2", "demo-w3"}, {"demo-w3", "demo-w9", "demo-w1"}}
	dryRuns := 0
	deps := splitTestDeps(nil, func(string) (beads.Bead, bdReadResult) { return beads.Bead{}, bdReadNotFound })
	deps.bdRead = func(args ...string) ([]byte, error) {
		selected := selections[min(dryRuns, len(selections)-1)]
		dryRuns++
		return fakeBdRead(nil, nil, selected)(args...)
	}
	var asked []string
	deps.bindingDependents = func(ids []string) (map[string][]string, error) {
		asked = append(asked, ids...)
		return map[string][]string{}, nil
	}
	judge := func(argv ...string) bdClassGuard {
		dryRuns, asked = 0, nil
		return bdClassFlipGuard(splitGuardTestCity(), t.TempDir(), argv, deps)
	}

	g := judge("mol", "wisp", "gc", "--age", "24h", "--json", "--actor", "deacon", "-q")
	if g.refusal != "" {
		t.Fatalf("refused: %q", g.refusal)
	}
	if want := []string{"delete", "--force", "--actor=deacon", "--json=true", "--quiet=true", "demo-w3", "demo-w1"}; !slices.Equal(g.args, want) {
		t.Errorf("forwarded bd %v, want bd %v", g.args, want)
	}
	if !slices.Equal(asked, selections[0]) {
		t.Errorf("the binding was asked about %v, want the wisps the first dry run selected %v", asked, selections[0])
	}
	for _, id := range g.args[5:] {
		if !slices.Contains(asked, id) {
			t.Errorf("the forwarded delete names %s, which gc did not check", id)
		}
	}
	if !strings.Contains(g.notice, "2 wisp(s)") {
		t.Errorf("notice %q does not say how many wisps gc deletes", g.notice)
	}
	for _, inv := range bdflags.Inventories() {
		if res, err := inv.Resolve(g.args); err != nil || res.Path != "delete" || !slices.Equal(res.Positionals, []string{"demo-w3", "demo-w1"}) || !slices.Equal(res.Flags["--actor"], []string{"deacon"}) {
			t.Errorf("%s: the forwarded argv does not resolve to bd delete of the checked wisps as deacon: %+v, %v", inv.Name, res, err)
		}
	}

	// No wisp gc checked is still a candidate, or bd selected none: nothing
	// is deleted, and bd only dry-runs.
	dryRun := []string{"mol", "wisp", "gc", "--age", "24h", "--dry-run"}
	for name, sel := range map[string][][]string{
		"checked wisps no longer selected": {{"demo-w1"}, {"demo-w9"}},
		"nothing selected":                 {{}},
	} {
		selections = sel
		g := judge("mol", "wisp", "gc", "--age", "24h")
		if g.refusal != "" || !slices.Equal(g.args, dryRun) || dryRuns != len(sel) {
			t.Errorf("%s: refused=%q, forwarded bd %v after %d dry run(s); want bd %v after %d", name, g.refusal, g.args, dryRuns, dryRun, len(sel))
		}
	}

	// A second dry run gc cannot get is a refusal.
	selections = [][]string{{"demo-w1"}}
	deps.bdRead = func(args ...string) ([]byte, error) {
		if dryRuns++; dryRuns > 1 {
			return nil, errors.New("bd: database locked")
		}
		return fakeBdRead(nil, nil, selections[0])(args...)
	}
	if g := judge("mol", "wisp", "gc", "--age", "24h"); !strings.Contains(g.refusal, "database locked") || !strings.Contains(g.refusal, "again") {
		t.Errorf("a failed second dry run: refusal %q", g.refusal)
	}
}

// TestGcBdSplitSweepsAreHeldToAWeek: `purge` and `prune` delete the closed
// beads bd selects at once, and bd's dry run does not name them, so a run
// that deletes must give an --older-than of at least bdSweepMinAge, read as
// bd reads it, and prune's --ignore-references is refused. A dry run, and a
// run without --force, delete nothing. The judge asks neither bd nor the
// binding anything.
func TestGcBdSplitSweepsAreHeldToAWeek(t *testing.T) {
	deps := splitTestDeps(nil, func(string) (beads.Bead, bdReadResult) { return beads.Bead{}, bdReadFailed })
	deps.bdRead = func(args ...string) ([]byte, error) {
		t.Errorf("the sweep judge asked bd %v", args)
		return nil, errors.New("unexpected read")
	}
	deps.bindingDependents = func(ids []string) (map[string][]string, error) {
		t.Errorf("the sweep judge asked the binding about %v", ids)
		return nil, errors.New("unexpected lookup")
	}
	judge := func(argv string) bdClassGuard {
		return bdClassFlipGuard(splitGuardTestCity(), t.TempDir(), strings.Fields(argv), deps)
	}
	for _, argv := range []string{
		"purge --wisps-plane --older-than 168h --json --force --limit 500",
		"prune --pattern gm-* --older-than 720h --force --json",
		"purge --older-than 7d --force",
		"purge --older-than 7 -f",
		"prune --older-than 1w --force",
		"prune --older-than 168H --force",
		"purge --force --older-than=169h",
		"purge --older-than 1s --older-than 168h --force",
		// These delete nothing.
		"purge --wisps-plane --older-than 1s --json --dry-run",
		"purge --force --dry-run",
		"purge",
		"prune --older-than 1s",
		"prune --pattern * --force=false",
		"prune --older-than 1s --ignore-references --dry-run",
	} {
		if g := judge(argv); g.refusal != "" {
			t.Errorf("gc bd %s was refused: %q", argv, g.refusal)
		}
	}
	for argv, want := range map[string]string{
		"purge --force": "without --older-than",
		"purge --wisps-plane --pattern * --force":             "without --older-than",
		"prune --pattern gm-* --force":                        "without --older-than",
		"purge --older-than= --force":                         "without --older-than",
		"purge --wisps-plane --older-than 1s --force":         "under 168h",
		"prune --older-than 167h --force":                     "under 168h",
		"purge --older-than 6d -f":                            "under 168h",
		"purge --older-than 168h --older-than 1s --force":     "under 168h",
		"purge --dry-run=false --older-than 1h --force":       "under 168h",
		"purge --older-than banana --force":                   "not an age bd reads",
		"purge --older-than 0 --force":                        "not an age bd reads",
		"purge --older-than=-168h --force":                    "not an age bd reads",
		"prune --older-than=-213503 --force":                  "not an age bd reads",
		"prune --older-than 720h --ignore-references --force": "--ignore-references",
	} {
		if g := judge(argv); !strings.Contains(g.refusal, want) || !strings.Contains(g.refusal, "Nothing was written") {
			t.Errorf("gc bd %s: refusal = %q, want one carrying %q", argv, g.refusal, want)
		}
	}
}

// TestBdOlderThanAgeReadsAsBdDoes: the sweep judge compares its floor with the
// age bd selects at, so it reads --older-than as bd's parseOlderThan does and
// refuses what bd 1.3.2 refuses, a day count the Enterprise build would wrap
// around included.
func TestBdOlderThanAgeReadsAsBdDoes(t *testing.T) {
	day := 24 * time.Hour
	for _, c := range []struct {
		value string
		want  time.Duration
	}{
		{"7", 7 * day},
		{"+7", 7 * day},
		{"7d", 7 * day},
		{"7D", 7 * day},
		{"2w", 14 * day},
		{"2W", 14 * day},
		{" 30d ", 30 * day},
		{"168h", 168 * time.Hour},
		{"36H", 36 * time.Hour},
		{"90m", 90 * time.Minute},
		{"1h30m", 90 * time.Minute},
	} {
		if got, err := bdOlderThanAge(c.value); err != nil || got != c.want {
			t.Errorf("bdOlderThanAge(%q) = %v, %v; want %v", c.value, got, err, c.want)
		}
	}
	for _, value := range []string{"", " ", "0", "0d", "-7", "-1w", "0h", "-168h", "-213503", "106752d", "9999999999999w", "1.5d", "7 d", "7x", "1e3", "banana"} {
		if got, err := bdOlderThanAge(value); err == nil {
			t.Errorf("bdOlderThanAge(%q) = %v; bd 1.3.2 refuses it", value, got)
		}
	}
}

// TestBdWispGCMinAgeStaysAboveTheGateSweep pins the wisp gc judge's age floor
// to the gate sweep: a wisp the age test selects was last touched at least
// the floor ago, which must leave the gate sweep (it releases a class-binding
// gate once its work blocker closes) ample time to have run, or gc would
// refuse the gc over a gate about to be released.
func TestBdWispGCMinAgeStaysAboveTheGateSweep(t *testing.T) {
	sweep := readBundledPackFileForTest(t, "core", "orders/gate-sweep.toml")
	m := regexp.MustCompile(`(?m)^interval\s*=\s*"([^"]+)"`).FindStringSubmatch(sweep)
	if m == nil {
		t.Fatal("gate-sweep.toml has no interval")
	}
	interval, err := time.ParseDuration(m[1])
	if err != nil {
		t.Fatal(err)
	}
	if bdWispGCMinAge < 100*interval {
		t.Errorf("the wisp gc age floor %s is not well above the gate-sweep interval %s (want >= 100x)", bdWispGCMinAge, interval)
	}
}

// TestBdWispGCDefaultAgeMeetsTheFloor: the judge accepts `mol wisp gc`
// without --age, whose dry run selects at bd's default age, so every pinned
// build's default must meet the floor.
func TestBdWispGCDefaultAgeMeetsTheFloor(t *testing.T) {
	for _, inv := range bdflags.Inventories() {
		cmd := inv.Command("mol wisp gc")
		if cmd == nil {
			t.Fatalf("%s has no mol wisp gc", inv.Name)
		}
		found := false
		for _, f := range cmd.Local {
			if f.Name != "age" {
				continue
			}
			found = true
			age, err := time.ParseDuration(f.Default)
			if err != nil || age < bdWispGCMinAge {
				t.Errorf("%s: mol wisp gc --age defaults to %q (%v), under the floor %s", inv.Name, f.Default, err, bdWispGCMinAge)
			}
		}
		if !found {
			t.Errorf("%s: mol wisp gc has no --age", inv.Name)
		}
	}
}
