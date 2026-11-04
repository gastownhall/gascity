package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// brnCheck is gc bd's check for guarded rig beta, over rows keyed by id.
func brnCheck(t *testing.T, actor string, rows ...beads.Bead) *bdRigNamespaceCheck {
	t.Helper()
	ns, _ := rnsCityBNamespace(t)
	byID := map[string]beads.Bead{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	return &bdRigNamespaceCheck{
		ns:    ns,
		actor: actor,
		get: func(id string) (beads.Bead, error) {
			if b, ok := byID[id]; ok {
				return b, nil
			}
			return beads.Bead{}, beads.ErrNotFound
		},
	}
}

var (
	brnOwned   = beads.Bead{ID: "bt-1", Status: "open", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "beta/crew.worker"}}
	brnForeign = beads.Bead{ID: "bt-2", Status: "in_progress", Assignee: "alpha/crew.worker"}
	brnPlain   = beads.Bead{ID: "bt-3", Status: "open"}
)

func TestBdRigNamespaceRefusesWritesToForeignRows(t *testing.T) {
	c := brnCheck(t, "beta/crew.worker", brnOwned, brnForeign, brnPlain)
	for _, args := range [][]string{
		{"update", "bt-2", "--status", "open"},
		{"close", "bt-2"},
		{"reopen", "bt-2"},
		{"delete", "bt-2"},
		{"heartbeat", "bt-2"},
		{"release-if-current", "bt-2", "alpha/crew.worker"},
		{"close", "bt-1", "bt-2"},
		{"--actor", "beta/crew.worker", "close", "bt-2"},
	} {
		_, msg, refused := c.check(args)
		if !refused {
			t.Errorf("%v: not refused", args)
			continue
		}
		if !strings.Contains(msg, `on bt-2: rig "beta" (federation = "rig") does not own it (owner "alpha/crew.worker")`) {
			t.Errorf("%v: message = %q", args, msg)
		}
	}
	for _, args := range [][]string{
		{"close", "bt-1"},
		{"update", "bt-3", "--status", "in_progress"},
		{"show", "bt-2"},
		{"list"},
	} {
		if got, msg, refused := c.check(args); refused || strings.Join(got, " ") != strings.Join(args, " ") {
			t.Errorf("%v: refused=%v msg=%q got=%v, want passthrough", args, refused, msg, got)
		}
	}
}

func TestBdRigNamespaceFailsClosedOnUnreadableRow(t *testing.T) {
	c := brnCheck(t, "beta/crew.worker")
	if _, msg, refused := c.check([]string{"close", "bt-404"}); !refused || !strings.Contains(msg, "could not read it to check its owner") {
		t.Fatalf("refused=%v msg=%q", refused, msg)
	}
	if _, _, refused := c.check([]string{"update", "bt-1", "--mystery-flag", "x"}); !refused {
		t.Fatal("an argv the scanner cannot read must be refused")
	}
}

func TestBdRigNamespaceClaimNeedsAdmittedActor(t *testing.T) {
	c := brnCheck(t, "mayor", brnOwned)
	if _, msg, refused := c.check([]string{"update", "bt-1", "--claim"}); !refused || !strings.Contains(msg, `actor "mayor" is outside it`) {
		t.Fatalf("env actor: refused=%v msg=%q", refused, msg)
	}
	if _, _, refused := c.check([]string{"--actor", "beta/crew.worker", "update", "bt-1", "--claim"}); refused {
		t.Fatal("an admitted --actor must claim")
	}
	if _, _, refused := c.check([]string{"update", "bt-1", "--claim", "--actor=" + poolIDB}); refused {
		t.Fatal("the city's own pool session must claim")
	}
	if _, _, refused := c.check([]string{"update", "bt-1", "--claim", "--actor", poolIDA}); !refused {
		t.Fatal("another city's pool session must not claim")
	}
	if _, _, refused := c.check([]string{"update", "bt-1", "--assignee", shortIDB}); !refused {
		t.Fatal("a short-id assignee must be refused")
	}
	for _, args := range [][]string{{"ready", "--claim"}, {"close", "bt-1", "--claim-next"}, {"delete", "bt-1", "--cascade"}, {"delete", "--from-file", "ids.txt"}} {
		if _, _, refused := c.check(args); !refused {
			t.Errorf("%v: unvettable write not refused", args)
		}
	}
}

func TestBdRigNamespaceRoutes(t *testing.T) {
	c := brnCheck(t, "beta/crew.worker", brnOwned, brnPlain)
	cases := []struct {
		args    []string
		refused bool
	}{
		{[]string{"update", "bt-1", "--set-metadata", "gc.routed_to=beta/crew.reviewer"}, false},
		// A bare route is never written.
		{[]string{"update", "bt-1", "--set-metadata", "gc.routed_to=crew.reviewer"}, true},
		// Empty segments are not rig-qualified.
		{[]string{"update", "bt-1", "--set-metadata", "gc.routed_to=beta/"}, true},
		{[]string{"update", "bt-1", "--set-metadata", "gc.routed_to=/crew"}, true},
		// An Owned row may be handed off outside the namespace...
		{[]string{"update", "bt-1", "--set-metadata", "gc.routed_to=alpha/crew.reviewer"}, false},
		// ...an Unattributed one may not.
		{[]string{"update", "bt-3", "--set-metadata", "gc.routed_to=alpha/crew.reviewer"}, true},
		{[]string{"update", "bt-3", `--metadata={"gc.execution_routed_to":"crew.worker"}`}, true},
	}
	for _, tc := range cases {
		if _, msg, refused := c.check(tc.args); refused != tc.refused {
			t.Errorf("%v: refused=%v (%q), want %v", tc.args, refused, msg, tc.refused)
		}
	}
}

func TestBdRigNamespaceCreationStamp(t *testing.T) {
	c := brnCheck(t, "beta/crew.worker", brnOwned)
	// Setting or unsetting the stamp is refused, whatever its spelling.
	for _, args := range [][]string{
		{"create", "--title", "x", "--metadata", `{"gc.owner_namespace":"beta/"}`},
		{"new", "--title", "x", "--metadata", `{" gc.owner_namespace ":"beta/"}`},
		{"update", "bt-1", "--set-metadata", "gc.owner_namespace=beta/"},
		{"update", "bt-1", "--unset-metadata", "gc.owner_namespace"},
		{"update", "bt-1", "--unset-metadata=gc.owner_namespace"},
	} {
		if _, msg, refused := c.check(args); !refused || !strings.Contains(msg, "gc.owner_namespace is written only by the rig namespace guard") {
			t.Errorf("%v: refused=%v msg=%q", args, refused, msg)
		}
	}

	// A create is stamped: as a new --metadata right after the verb...
	got, msg, refused := c.check([]string{"--actor", "beta/crew.worker", "create", "--title", "x", "--", "--not-a-flag"})
	if refused {
		t.Fatalf("create refused: %q", msg)
	}
	want := []string{"--actor", "beta/crew.worker", "create", "--metadata", `{"gc.owner_namespace":"beta/"}`, "--title", "x", "--", "--not-a-flag"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("stamped argv = %q, want %q", got, want)
	}
	// ...or merged into the one it has.
	got, _, refused = c.check([]string{"create", "--title", "x", "--metadata", `{"k":"v"}`})
	if refused {
		t.Fatal("create with metadata refused")
	}
	var md map[string]string
	if err := json.Unmarshal([]byte(got[4]), &md); err != nil || md["k"] != "v" || md[beadmeta.OwnerNamespaceMetadataKey] != "beta/" {
		t.Fatalf("merged metadata = %q (%v)", got[4], err)
	}
	got, _, _ = c.check([]string{"create", "--metadata={}", "--title", "x"})
	if got[1] != `--metadata={"gc.owner_namespace":"beta/"}` {
		t.Fatalf("inline merge = %q", got)
	}

	// Batch creates cannot be stamped; bare routes and outside assignees are
	// refused at create.
	for _, args := range [][]string{
		{"create", "--file", "plan.md"},
		{"create", "--graph", "g.json"},
		{"create", "--title", "x", "--assignee", "mayor"},
		{"create", "--title", "x", "--metadata", `{"gc.routed_to":"crew.worker"}`},
	} {
		if _, _, refused := c.check(args); !refused {
			t.Errorf("%v: not refused", args)
		}
	}

	// A deny-all namespace cannot stamp, so it refuses every create.
	deny := &bdRigNamespaceCheck{ns: denyAllRigNamespace(nil), get: c.get}
	if _, msg, refused := deny.check([]string{"create", "--title", "x"}); !refused || !strings.Contains(msg, "cannot stamp") {
		t.Fatalf("deny-all create: refused=%v msg=%q", refused, msg)
	}
}

func TestNewBdRigNamespaceCheckOnlyForGuardedScope(t *testing.T) {
	cfg := hrgCity(t)
	// An unguarded scope of a city with a guarded rig gets the per-id check
	// only: no namespace, and a write of its own beads passes.
	for _, scope := range []string{hrgDir(cfg, "alpha"), "/cities/b"} {
		c := newBdRigNamespaceCheck(cfg, "/cities/b", scope, nil)
		if c == nil || c.ns != nil {
			t.Fatalf("scope %s: check = %+v, want the per-id check with no namespace", scope, c)
		}
		if _, msg, refused := c.check([]string{"update", "al-1", "--claim"}); refused {
			t.Fatalf("scope %s: a write of an unguarded rig's bead was refused: %s", scope, msg)
		}
	}
	if c := newBdRigNamespaceCheck(&config.City{Rigs: []config.Rig{{Name: "alpha", Prefix: "al"}}}, "/cities/b", "/cities/b", nil); c != nil {
		t.Fatal("a city with no guarded rig got a check")
	}
	c := newBdRigNamespaceCheck(cfg, "/cities/b", hrgDir(cfg, "beta"), nil)
	if c == nil || c.ns.rigName() != "beta" {
		t.Fatalf("guarded scope check = %+v", c)
	}
	var nilCheck *bdRigNamespaceCheck
	if got, _, refused := nilCheck.check([]string{"close", "x"}); refused || len(got) != 2 {
		t.Fatal("a nil check must pass everything through")
	}
}

func TestBdEffectiveActorFromEnv(t *testing.T) {
	if got := bdEffectiveActorFromEnv(func(string) string { return "" }); got != "" {
		t.Fatalf("default actor = %q", got)
	}
	if got := bdEffectiveActorFromEnv(func(string) string { return " beta/x " }); got != "beta/x" {
		t.Fatalf("env actor = %q", got)
	}
}

// TestGcBdRefusesAGuardedRigsBeadOutsideItsScope: gc bd checks the scope it
// resolved, and bd may write another store than that scope (an explicit
// --rig or --city, -C, ids from two stores). A write naming a guarded rig's
// bead outside that rig's scope is refused before bd runs, in every one of
// those shapes; a read and a write of an unguarded rig's bead still run.
func TestGcBdRefusesAGuardedRigsBeadOutsideItsScope(t *testing.T) {
	capture, cityDir := bdSQLRefusalCityDir(t, "")
	var toml strings.Builder
	dirs := map[string]string{}
	for _, r := range []struct{ name, prefix, extra string }{
		{"alpha", "al", ""},
		{"beta", "bt", "federation = \"rig\"\n"},
	} {
		dir := filepath.Join(cityDir, r.name)
		dirs[r.name] = dir
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".beads", "config.yaml"), []byte("issue_prefix: "+r.prefix+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		toml.WriteString("\n[[rigs]]\nname = \"" + r.name + "\"\npath = \"" + dir + "\"\nprefix = \"" + r.prefix + "\"\n" + r.extra)
	}
	f, err := os.OpenFile(filepath.Join(cityDir, "city.toml"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(toml.String()); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	run := func(args ...string) (int, string, string) {
		t.Helper()
		_ = os.Remove(capture)
		var stdout, stderr bytes.Buffer
		code := doBd(args, &stdout, &stderr)
		data, _ := os.ReadFile(capture)
		return code, string(data), stderr.String()
	}
	for _, args := range [][]string{
		{"--rig", "alpha", "update", "bt-1", "--claim"},
		{"close", "bt-1", "--rig", "alpha"},
		{"--city", cityDir, "update", "bt-1", "--status", "closed"},
		{"release-if-current", "bt-1", "x", "--rig", "alpha"},
		{"-C", dirs["alpha"], "close", "bt-1"},
		{"close", "al-1", "bt-1"},
		{"--rig", "alpha", "label", "add", "bt-1", "urgent"},
	} {
		code, ran, stderr := run(args...)
		if code == 0 || strings.Contains(ran, "update ") || strings.Contains(ran, "close ") || strings.Contains(ran, "release") || strings.Contains(ran, "label ") {
			t.Errorf("gc bd %q: code=%d, bd ran %q; want a refusal before any write", args, code, ran)
			continue
		}
		if !strings.Contains(stderr, "bt-1") || !strings.Contains(stderr, "refusing") {
			t.Errorf("gc bd %q: stderr %q does not refuse bt-1", args, stderr)
		}
	}
	// Reads of a guarded rig's bead and writes of an unguarded rig's bead
	// from an unguarded scope still reach bd.
	for _, args := range [][]string{
		{"--rig", "alpha", "show", "bt-1"},
		{"--rig", "alpha", "update", "al-1", "--status", "in_progress"},
		// A label is no bead id, even one that starts like a guarded rig's.
		{"--rig", "alpha", "label", "add", "al-1", "bt-urgent"},
		{"--rig", "alpha", "label", "remove", "al-1", "bt-urgent"},
		{"--rig", "alpha", "tag", "al-1", "bt-urgent"},
	} {
		if code, ran, stderr := run(args...); code != 0 || strings.TrimSpace(ran) == "" {
			t.Errorf("gc bd %q: code=%d, bd ran %q, stderr %q; want it run", args, code, ran, stderr)
		}
	}
}

// TestBdMetadataJSONWritesAreInKeyOrder: a --metadata object is read in key
// order, so with two guarded keys holding non-string values the refusal
// names the same one on every run, and the writes come back sorted.
func TestBdMetadataJSONWritesAreInKeyOrder(t *testing.T) {
	const twoBad = `{"gc.routed_to": 1, "zz": "x", "gc.owner_namespace": 2, "aa": "y"}`
	want := "gc bd: refusing non-string gc.owner_namespace before write\n"
	for i := 0; i < 50; i++ {
		_, msg, refused := bdMetadataJSONWrites(twoBad)
		if !refused || msg != want {
			t.Fatalf("run %d: refusal = %q (refused %v), want %q", i, msg, refused, want)
		}
	}
	writes, msg, refused := bdMetadataJSONWrites(`{"zz": "1", "mm": "2", "aa": "3", "skip": 4}`)
	if refused {
		t.Fatalf("refused a plain object: %s", msg)
	}
	var keys []string
	for _, w := range writes {
		keys = append(keys, w.key)
	}
	if got := strings.Join(keys, ","); got != "aa,mm,zz" {
		t.Fatalf("write keys = %s, want aa,mm,zz", got)
	}
}
