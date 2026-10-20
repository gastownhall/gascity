//go:build integration

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// blockedRepairFixture is a real embedded bd store whose is_blocked column
// carries the damage beads migration 0059 does on upgrade (beads#7037): two
// beads that only relate to a blocked bead (relates-to, discovered-from) are
// flagged blocked although nothing blocks them.
type blockedRepairFixture struct {
	dir    string
	run    beads.CommandRunner
	hidden []string // over-blocked ids `bd ready` should list
}

// requireBlockedRepairTools returns the bd under test: GC_TEST_BD_BIN when set
// (to pin a specific release), else the installed bd. This package's TestMain
// puts a testscript `bd` shim first on PATH, so a plain PATH lookup finds the
// fake; findPreferredBinary skips it. dolt stands in for the faulty migration,
// so it is required too.
func requireBlockedRepairTools(t *testing.T) string {
	t.Helper()
	bd, err := findPreferredBinary("bd", strings.TrimSpace(os.Getenv("GC_TEST_BD_BIN")))
	if err != nil {
		t.Skip("bd not found (set GC_TEST_BD_BIN or install bd)")
	}
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt not found")
	}
	return bd
}

func newBlockedRepairFixture(t *testing.T, bdPath string, overBlock bool) blockedRepairFixture {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	dir := filepath.Join(root, "ws")
	for _, d := range []string{home, dir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeTestGitIdentity(home); err != nil {
		t.Fatal(err)
	}
	if err := writeTestDoltIdentity(home); err != nil {
		t.Fatal(err)
	}
	run := beads.ExecCommandRunnerWithEnvWithoutAmbientBeads(map[string]string{
		"HOME":               home,
		"XDG_CONFIG_HOME":    filepath.Join(home, ".config"),
		"GIT_CONFIG_GLOBAL":  filepath.Join(home, ".gitconfig"),
		"BD_BIN":             bdPath,
		"BEADS_DIR":          filepath.Join(dir, ".beads"),
		"BD_NON_INTERACTIVE": "1",
		"BD_BACKUP_ENABLED":  "false",
		"BD_EXPORT_AUTO":     "false",
		"BEADS_ACTOR":        "gc-test",
	})
	must := func(dir string, name string, args ...string) string {
		t.Helper()
		out, err := run(dir, name, args...)
		if err != nil {
			t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	create := func(title string) string {
		t.Helper()
		return parseCreatedBeadID(t, must(dir, "bd", "create", "--json", title, "-t", "task"))
	}
	must(dir, "git", "init", "-q", ".")
	must(dir, "bd", "init", "-p", "rb", "--quiet", "--skip-hooks")
	blocker := create("blocker")
	blocked := create("blocked")
	related := create("relates to blocked")
	found := create("discovered from blocked")
	must(dir, "bd", "dep", "add", blocked, blocker)
	must(dir, "bd", "dep", "add", related, blocked, "--type", "relates-to")
	must(dir, "bd", "dep", "add", found, blocked, "--type", "discovered-from")
	f := blockedRepairFixture{dir: dir, run: run}
	if !overBlock {
		return f
	}

	// Migration 0059's dropped d.type predicate, applied by hand: flag the two
	// non-blocking dependents blocked and commit, as the migration does.
	dbDirs, err := filepath.Glob(filepath.Join(dir, ".beads", "embeddeddolt", "*", ".dolt"))
	if err != nil || len(dbDirs) != 1 {
		t.Fatalf("embedded dolt database: %v %v", dbDirs, err)
	}
	db := filepath.Dir(dbDirs[0])
	must(db, "dolt", "sql", "-q", "UPDATE issues SET is_blocked = 1 WHERE id IN ('"+related+"', '"+found+"')")
	must(db, "dolt", "add", "-A")
	must(db, "dolt", "commit", "-m", "simulate beads#7037 over-blocking")
	f.hidden = []string{related, found}
	return f
}

func (f blockedRepairFixture) ready(t *testing.T) []string {
	t.Helper()
	out, err := f.run(f.dir, "bd", "ready", "--json")
	if err != nil {
		t.Fatalf("bd ready: %v\n%s", err, out)
	}
	var rows []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(out[bytes.IndexByte(out, '['):], &rows); err != nil {
		t.Fatalf("parse bd ready: %v\n%s", err, out)
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	slices.Sort(ids)
	return ids
}

func (f blockedRepairFixture) repair(t *testing.T) (*events.Fake, string) {
	t.Helper()
	rec := events.NewFake()
	var stderr bytes.Buffer
	runBlockedRepair([]blockedRepairScope{{id: "city", root: f.dir}}, blockedRepairDeps{
		openStore:    func(scope blockedRepairScope) *beads.BdStore { return beads.NewBdStore(scope.root, f.run) },
		openRecorder: func() (events.Recorder, func()) { return rec, func() {} },
	}, &stderr, "gc start")
	return rec, stderr.String()
}

// TestBlockedRepairOnRealBdRestoresOverBlockedWork runs the upgrade repair
// against the bd under test on a store carrying beads#7037's damage: the hidden
// work reappears in `bd ready`, the marker records the bd version, and the next
// start does nothing.
func TestBlockedRepairOnRealBdRestoresOverBlockedWork(t *testing.T) {
	f := newBlockedRepairFixture(t, requireBlockedRepairTools(t), true)
	before := f.ready(t)
	for _, id := range f.hidden {
		if slices.Contains(before, id) {
			t.Fatalf("fixture is not over-blocked: %s is ready before the repair (%v)", id, before)
		}
	}

	rec, out := f.repair(t)
	if !strings.Contains(out, "2 rows corrected") {
		t.Fatalf("stderr = %q, want 2 rows corrected", out)
	}
	if len(rec.Events) != 1 || rec.Events[0].Type != events.BeadsBlockedRecomputed {
		t.Fatalf("events = %+v, want one %s", rec.Events, events.BeadsBlockedRecomputed)
	}
	var payload events.BlockedRecomputedPayload
	if err := json.Unmarshal(rec.Events[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.RowsCorrected != 2 || payload.Scope != "city" || payload.BDVersion == "" {
		t.Fatalf("payload = %+v", payload)
	}
	after := f.ready(t)
	for _, id := range f.hidden {
		if !slices.Contains(after, id) {
			t.Fatalf("%s still hidden from bd ready after the repair: %v", id, after)
		}
	}
	marker, err := beads.NewBdStore(f.dir, f.run).ConfigGet(blockedRepairMarkerKey)
	if err != nil || marker != payload.BDVersion {
		t.Fatalf("marker = %q (err %v), want %q", marker, err, payload.BDVersion)
	}

	rec, out = f.repair(t)
	if len(rec.Events) != 0 || out != "" {
		t.Fatalf("second start under the same bd was not a no-op: events=%+v stderr=%q", rec.Events, out)
	}
}

// TestBlockedRepairOnRealBdIsHarmlessOnACleanStore shows the repair changes
// nothing on a store migration 0059 did not damage.
func TestBlockedRepairOnRealBdIsHarmlessOnACleanStore(t *testing.T) {
	f := newBlockedRepairFixture(t, requireBlockedRepairTools(t), false)
	before := f.ready(t)
	rec, out := f.repair(t)
	if !strings.Contains(out, "0 rows corrected") || len(rec.Events) != 1 {
		t.Fatalf("stderr=%q events=%+v, want one 0-row repair", out, rec.Events)
	}
	if after := f.ready(t); !slices.Equal(before, after) {
		t.Fatalf("bd ready changed on a clean store: before %v after %v", before, after)
	}
}
