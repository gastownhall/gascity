package main

// The split-city conformance table for the bead and session commands.
//
// A split city serves its coordination classes from a binding the bd
// passthrough cannot see, so a gc command that addresses a bead the binding
// holds has exactly two acceptable answers: serve it from the binding, or
// refuse in words that name what to run instead. Answering from the work
// store — bd's not-found, or a confident empty — is the defect every row here
// guards against. The table runs each command form against both id shapes a
// binding holds: an id the binding minted (a reserved class prefix) and a
// relic, a work-shaped id `gc storage migrate` preserved
// (beads.ForeignIDCreator). Adding a verb means adding a row.
//
// Rows that name no alternative carry a gap saying why; those are the forms
// the class store contract cannot carry yet, listed so they stay visible.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// splitCityIDShape names which of the two ids a binding holds a row runs on.
type splitCityIDShape string

const (
	// splitCityMinted is an id the binding minted, with a reserved prefix.
	splitCityMinted splitCityIDShape = "minted"
	// splitCityRelic is a work-shaped id the migration preserved.
	splitCityRelic splitCityIDShape = "relic"
	// splitCityNoID is a row whose command addresses no bead.
	splitCityNoID splitCityIDShape = "no-id"
)

// splitCityRelicStepID and splitCityRelicSessionID are the relics' ids, under
// the fixture city's work prefix.
const (
	splitCityRelicStepID    = "demo-wisp-abc"
	splitCityRelicSessionID = "demo-wisp-sess1"
	// splitCityWorkID is a work-shaped id the binding does not hold, so the
	// work store is its truth.
	splitCityWorkID = "demo-notinthebinding"
)

// splitCityEdgeType is the type of the edge each step waits on the other step
// by.
const splitCityEdgeType = "blocks"

// splitCitySessionCityTOML is appended to the fixture's city.toml: the agent
// whose template the seeded session beads run, which wake requires.
const splitCitySessionCityTOML = `
[[agent]]
name = "riley"
provider = "codex"
start_command = "echo"
`

// splitCitySessionIdentity is the configured named-session identity every
// seeded session bead carries, so reset and kill reach the circuit clear.
const splitCitySessionIdentity = "riley"

// splitCityRelicGap is the reason a relic row is forwarded to bd: the door's
// known hole for an argv its scanners cannot reduce to a subject.
const splitCityRelicGap = "ga-zvetw: the argv reaches no residence probe, so bd answers from the work store, which does not hold the bead"

// splitCityConformanceFixture is one split city with beads in its class
// binding: a graph step and a session bead in each id shape.
type splitCityConformanceFixture struct {
	cityDir string
	// capture is the bd stub's invocation log (bdSQLRefusalCityDir).
	capture string
	// stepID, otherStepID and sessionID were minted by the binding.
	stepID, otherStepID, sessionID string
}

// newSplitCityConformanceFixture builds the city, seeds the binding and takes
// the census a booting process takes, after the relics exist.
func newSplitCityConformanceFixture(t *testing.T) splitCityConformanceFixture {
	t.Helper()
	capture, cityDir := bdSQLRefusalCityDir(t, bdSQLRefusalSplitStorage+splitCitySessionCityTOML)
	resetCLIStorageRoutes(t)
	captureCLIStorageStderr(t)

	binding := soleClassBindingStore(t, cityDir)
	root := mustCreateClassBead(t, binding, beads.Bead{Title: "a workflow root", Type: "task", Metadata: map[string]string{beadmeta.KindMetadataKey: "workflow"}})
	step := func(title string) beads.Bead {
		return beads.Bead{Title: title, Type: "task", Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey:  root.ID,
			beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeNoOp,
		}}
	}
	fx := splitCityConformanceFixture{cityDir: cityDir, capture: capture}
	fx.stepID = mustCreateClassBead(t, binding, step("a graph step")).ID
	fx.otherStepID = mustCreateClassBead(t, binding, step("another graph step")).ID
	relic := step("a migrated step")
	relic.ID = splitCityRelicStepID
	if _, err := migrationSeed(binding, relic); err != nil {
		t.Fatalf("seeding the relic step: %v", err)
	}
	// Both steps wait on the other step, so the dependency reads have an
	// edge to answer with rather than an empty list.
	for _, id := range []string{fx.stepID, splitCityRelicStepID} {
		if err := binding.DepAdd(id, fx.otherStepID, splitCityEdgeType); err != nil {
			t.Fatalf("seeding the edge %s -> %s: %v", id, fx.otherStepID, err)
		}
	}
	fx.sessionID = seedSplitCitySessionBead(t, binding, "", "s-riley-minted").ID
	seedSplitCitySessionBead(t, binding, splitCityRelicSessionID, "s-riley-relic")
	recensusAfterSeedingARelic(t, cityDir)
	return fx
}

// bindingStore resolves the class binding as this process serves it now. A
// runtime registration retires the funnel's memo and closes its handles, so a
// row resolves the store when it reads rather than holding one.
func (fx splitCityConformanceFixture) bindingStore(t *testing.T) beads.Store {
	t.Helper()
	return soleClassBindingStore(t, fx.cityDir)
}

// stepFor returns the graph step of shape.
func (fx splitCityConformanceFixture) stepFor(shape splitCityIDShape) string {
	if shape == splitCityRelic {
		return splitCityRelicStepID
	}
	return fx.stepID
}

// sessionFor returns the session bead of shape.
func (fx splitCityConformanceFixture) sessionFor(shape splitCityIDShape) string {
	if shape == splitCityRelic {
		return splitCityRelicSessionID
	}
	return fx.sessionID
}

// bindingRow reads id from the class binding.
func (fx splitCityConformanceFixture) bindingRow(t *testing.T, id string) beads.Bead {
	t.Helper()
	got, err := fx.bindingStore(t).Get(id)
	if err != nil {
		t.Fatalf("binding Get(%s): %v", id, err)
	}
	return got
}

// bdCalls returns the argv lines the work-store bd stub received.
func (fx splitCityConformanceFixture) bdCalls(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(fx.capture)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading the bd capture: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// requireBdNeverTargeted fails when the work-store bd was invoked with id as an
// argument: a bead the binding holds is never read or written through the
// ledger that cannot hold it. A filter that merely names it, such as session
// close's `list --assignee=<id>` sweep of the WORK store, is not flagged.
func (fx splitCityConformanceFixture) requireBdNeverTargeted(t *testing.T, id string) {
	t.Helper()
	for _, line := range fx.bdCalls(t) {
		if slices.Contains(strings.Fields(line), id) {
			t.Errorf("the work-store bd was invoked on %s: %q", id, line)
		}
	}
}

// seedSplitCitySessionBead plants an awake named session bead with an open
// circuit breaker in store. An empty id lets the binding mint one; a non-empty
// id is seeded the way `gc storage migrate` preserves a relic, through
// ForeignIDCreator, and the caller recensuses afterwards.
func seedSplitCitySessionBead(t *testing.T, store beads.Store, id, sessionName string) beads.Bead {
	t.Helper()
	bead := beads.Bead{
		ID:     id,
		Title:  "riley",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"template":                     "riley",
			"session_name":                 sessionName,
			"state":                        string(session.StateAwake),
			namedSessionMetadataKey:        "true",
			namedSessionIdentityMetadata:   splitCitySessionIdentity,
			sessionCircuitStateMetadata:    circuitOpen.String(),
			sessionCircuitRestartsMetadata: `["2026-04-10T12:00:00Z"]`,
		},
	}
	if id == "" {
		return mustCreateClassBead(t, store, bead)
	}
	created, err := migrationSeed(store, bead)
	if err != nil {
		t.Fatalf("seeding relic session bead %s: %v", id, err)
	}
	if bdIDIsClassReserved(created.ID) {
		t.Fatalf("relic id %q carries a reserved class prefix", created.ID)
	}
	return created
}

// splitCityOutcome is what a row's command must do.
type splitCityOutcome string

const (
	// splitCityServed answers from the binding: exit 0, bd never runs on it.
	splitCityServed splitCityOutcome = "served"
	// splitCityRefused refuses before anything is written, naming the
	// alternative: exit 1, bd never runs on it.
	splitCityRefused splitCityOutcome = "refused"
	// splitCityPassthrough is bd's to answer: gc forwards the argv.
	splitCityPassthrough splitCityOutcome = "passthrough"
)

// splitCityBeadRow is one `gc bd` command form run against a split city.
type splitCityBeadRow struct {
	// form is the command form as the table documents it; <id> is the bead.
	form string
	// argv builds the gc bd argv for id (empty for a no-id row).
	argv func(fx splitCityConformanceFixture, id string) []string
	// shapes are the id shapes the row runs on; nil means minted and relic.
	shapes []splitCityIDShape
	// arrange prepares the binding row before the command runs.
	arrange func(t *testing.T, fx splitCityConformanceFixture, id string)
	want    splitCityOutcome
	// refusal is a substring of a refused row's documented refusal, and
	// instead a substring of the alternative it names; instead is empty only
	// with gap set, which says why no alternative exists yet. A passthrough
	// row that is a known hole rather than bd's to answer carries the gap too.
	refusal, instead, gap string
	// runs is how many runnable `gc bd` commands a refused row's refusal
	// names (one with no <placeholder> left in it). Each is run after the
	// refusal and must succeed, so an alternative is a command that works.
	runs int
	// check asserts what a served row did, given its stdout and stderr.
	check func(t *testing.T, fx splitCityConformanceFixture, id, stdout, stderr string)
}

// splitCityArgv returns an argv builder that substitutes the row's id for
// every "<id>" token of form.
func splitCityArgv(form ...string) func(splitCityConformanceFixture, string) []string {
	return func(_ splitCityConformanceFixture, id string) []string {
		argv := make([]string, 0, len(form))
		for _, token := range form {
			argv = append(argv, strings.ReplaceAll(token, "<id>", id))
		}
		return argv
	}
}

// closedWith asserts a served close left id closed with reason.
func closedWith(reason string) func(*testing.T, splitCityConformanceFixture, string, string, string) {
	return func(t *testing.T, fx splitCityConformanceFixture, id, _, _ string) {
		t.Helper()
		got := fx.bindingRow(t, id)
		if got.Status != "closed" || got.CloseReason != reason {
			t.Errorf("%s is %q with close reason %q, want closed with %q", id, got.Status, got.CloseReason, reason)
		}
	}
}

// splitCityBeadRows is the `gc bd` half of the table.
func splitCityBeadRows(t *testing.T) []splitCityBeadRow {
	reasonFile := filepath.Join(t.TempDir(), "reason.md")
	if err := os.WriteFile(reasonFile, []byte("the reason from a file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	minted := []splitCityIDShape{splitCityMinted}
	relic := []splitCityIDShape{splitCityRelic}
	noID := []splitCityIDShape{splitCityNoID}
	refusedFlag := func(flag string) string { return "(" + flag + " has no representation in the class store contract)" }
	return []splitCityBeadRow{
		{
			form: "show <id>", argv: splitCityArgv("show", "<id>"), want: splitCityServed,
			check: func(t *testing.T, _ splitCityConformanceFixture, id, stdout, stderr string) {
				if !strings.Contains(stdout, "id:          "+id) || !strings.Contains(stderr, "served in process") {
					t.Errorf("show rendered stdout %q stderr %q, want the record served in process", stdout, stderr)
				}
			},
		},
		{
			form: "show <id> --json", argv: splitCityArgv("show", "<id>", "--json"), want: splitCityServed,
			check: requireShownJSON,
		},
		{
			form: "show <id> --json --include-comments", argv: splitCityArgv("show", "<id>", "--json", "--include-comments"), want: splitCityServed,
			check: func(t *testing.T, fx splitCityConformanceFixture, id, stdout, stderr string) {
				requireShownJSON(t, fx, id, stdout, stderr)
				if !strings.Contains(stderr, "carries no comments, so --include-comments adds none") {
					t.Errorf("stderr %q does not say why no comments are included", stderr)
				}
			},
		},
		{
			// The minted id meets the by-ID door's refusal; the relic's argv
			// cannot be scanned for subjects (--comments is not a bd flag), so
			// it meets the split allowlist's. Both name the same command.
			form: "show <id> --comments", argv: splitCityArgv("show", "<id>", "--comments"), want: splitCityRefused,
			refusal: "--comments", instead: "ask for comments with `gc bd show <id> --json --include-comments`", runs: 1,
		},
		{
			form: "show <id> --long", argv: splitCityArgv("show", "<id>", "--long"), want: splitCityRefused,
			refusal: "`gc bd show` with --long is not served in process", instead: "the served spelling is `gc bd show <id> [--json] [--include-comments]`", runs: 1,
		},
		{
			form: "show <id> <other-id>", argv: func(fx splitCityConformanceFixture, id string) []string { return []string{"show", id, fx.otherStepID} },
			want: splitCityRefused, refusal: "with more than one id is not served in process", instead: "read each id on its own: `gc bd show <id>`", runs: 2,
		},
		{form: "dep list <id> --json", argv: splitCityArgv("dep", "list", "<id>", "--json"), want: splitCityServed, check: requireTheSeededEdge},
		{form: "dep tree <id> --json", argv: splitCityArgv("dep", "tree", "<id>", "--json"), want: splitCityServed, check: requireTheSeededTree},
		{
			form: "update <id> --set-metadata k=v --status in_progress", argv: splitCityArgv("update", "<id>", "--set-metadata", "conformance.key=value", "--status", "in_progress"),
			want: splitCityServed,
			check: func(t *testing.T, fx splitCityConformanceFixture, id, _, _ string) {
				got := fx.bindingRow(t, id)
				if got.Metadata["conformance.key"] != "value" || got.Status != "in_progress" {
					t.Errorf("%s has metadata %v and status %q after the update", id, got.Metadata, got.Status)
				}
			},
		},
		{
			form: "update <id> --add-label L", argv: splitCityArgv("update", "<id>", "--add-label", "conformance"), want: splitCityServed,
			check: func(t *testing.T, fx splitCityConformanceFixture, id, _, _ string) {
				if got := fx.bindingRow(t, id); !slices.Contains(got.Labels, "conformance") {
					t.Errorf("%s labels %v after --add-label", id, got.Labels)
				}
			},
		},
		{
			form: "update <id> --claim --json", argv: splitCityArgv("update", "<id>", "--claim", "--json"), want: splitCityServed,
			arrange: func(t *testing.T, _ splitCityConformanceFixture, _ string) { t.Setenv("BEADS_ACTOR", "riley") },
			check: func(t *testing.T, fx splitCityConformanceFixture, id, _, _ string) {
				if got := fx.bindingRow(t, id); got.Assignee != "riley" || got.Status != "in_progress" {
					t.Errorf("%s is %q held by %q after the claim", id, got.Status, got.Assignee)
				}
			},
		},
		{
			form: "update <id> --append-notes N", argv: splitCityArgv("update", "<id>", "--append-notes", "a progress note"), want: splitCityRefused,
			refusal: refusedFlag("--append-notes"), instead: "keep the text as metadata instead: `gc bd update <id> --set-metadata <key>=<text>`",
		},
		{
			form: "update <id> --set-metadata gc.outcome=pass --status closed --notes N", argv: splitCityArgv("update", "<id>", "--set-metadata", "gc.outcome=pass", "--status", "closed", "--notes", "a note"),
			want: splitCityRefused, refusal: refusedFlag("--notes"), runs: 1,
			instead: "run it without --notes (a bead in the class store has no notes field): `gc bd update <id> --set-metadata gc.outcome=pass --status closed`",
		},
		{form: "close <id>", argv: splitCityArgv("close", "<id>"), want: splitCityServed, check: closedWith("")},
		{form: "close <id> --reason R", argv: splitCityArgv("close", "<id>", "--reason", "done"), want: splitCityServed, check: closedWith("done")},
		{form: "close <id> -r R --json", argv: splitCityArgv("close", "<id>", "-r", "done", "--json"), want: splitCityServed, check: closedWith("done")},
		{form: "close <id> -m R", argv: splitCityArgv("close", "<id>", "-m", "done"), want: splitCityServed, check: closedWith("done")},
		{form: "close <id> --message R", argv: splitCityArgv("close", "<id>", "--message", "done"), want: splitCityServed, check: closedWith("done")},
		{form: "close <id> --resolution R", argv: splitCityArgv("close", "<id>", "--resolution", "done"), want: splitCityServed, check: closedWith("done")},
		{form: "close <id> --comment R", argv: splitCityArgv("close", "<id>", "--comment", "done"), want: splitCityServed, check: closedWith("done")},
		{form: "close <id> --reason-file F", argv: splitCityArgv("close", "<id>", "--reason-file", reasonFile), want: splitCityServed, check: closedWith("the reason from a file")},
		{
			form: "close <id> <other-id> --reason R (batch)", argv: func(fx splitCityConformanceFixture, id string) []string {
				return []string{"close", id, fx.otherStepID, "--reason", "drained"}
			},
			want: splitCityServed,
			check: func(t *testing.T, fx splitCityConformanceFixture, id, stdout, stderr string) {
				closedWith("drained")(t, fx, id, stdout, stderr)
				closedWith("drained")(t, fx, fx.otherStepID, stdout, stderr)
			},
		},
		{
			form: "close <id> <work-id> --reason R (mixed batch)", argv: splitCityArgv("close", "<id>", splitCityWorkID, "--reason", "drained"),
			want: splitCityRefused, refusal: "one close never spans two stores, so nothing was closed", runs: 2,
			instead: "Close them in two commands: `gc bd close <id> --reason drained` (served in process) and `gc bd close " + splitCityWorkID + " --reason drained`",
		},
		{
			form: "close <id> --reason A --reason B", argv: splitCityArgv("close", "<id>", "--reason", "one", "--reason", "two"),
			want: splitCityRefused, refusal: "got 2 close reasons for 1 issue IDs", instead: "provide exactly one shared reason or one reason per issue",
		},
		{
			form: "close <id> --reason-file -", argv: splitCityArgv("close", "<id>", "--reason-file", "-"), want: splitCityRefused,
			refusal: refusedFlag("--reason-file -"), instead: "pass the reason in a file or inline: `gc bd close <id> --reason-file <path>` or `gc bd close <id> --reason <text>`",
		},
		{
			form: "close <id> <work-id> --reason-file - (split)", argv: splitCityArgv("close", "<id>", splitCityWorkID, "--reason-file", "-"), want: splitCityRefused,
			refusal: refusedFlag("--reason-file -"), runs: 1,
			instead: "`gc bd close <id> --reason <text>`, and the rest as typed, which bd answers from the work store: `gc bd close " + splitCityWorkID + " --reason-file -`",
		},
		{
			form: "close <id> <work-id> --actor A (split)", argv: splitCityArgv("close", "<id>", splitCityWorkID, "--actor", "someone"), want: splitCityRefused,
			refusal: refusedFlag("--actor"), runs: 2,
			instead: "run the ids the infra class binding holds without --actor (the class store records no close actor): `gc bd close <id>`, and the rest as typed, which bd answers from the work store: `gc bd close " + splitCityWorkID + " --actor someone`",
		},
		{form: "close <id> -r= (pflag reads =)", argv: splitCityArgv("close", "<id>", "-r="), want: splitCityServed, check: closedWith("=")},
		{
			form: "close <id> --actor A", argv: splitCityArgv("close", "<id>", "--actor", "someone"), want: splitCityRefused,
			refusal: refusedFlag("--actor"), instead: "run it without --actor (the class store records no close actor): `gc bd close <id>`", runs: 1,
		},
		{
			form: "close <id> --session S", argv: splitCityArgv("close", "<id>", "--session", "s-1"), want: splitCityRefused,
			refusal: refusedFlag("--session"), instead: "run it without --session (the class store records no closing session): `gc bd close <id>`", runs: 1,
		},
		{
			form: "close <id> --force", argv: splitCityArgv("close", "<id>", "--force"), want: splitCityRefused,
			refusal: refusedFlag("--force"), instead: "run it without --force (the class store's close runs none of the pinned-issue or gate checks it waives): `gc bd close <id>`", runs: 1,
		},
		{
			form: "close <id> --claim-next", argv: splitCityArgv("close", "<id>", "--claim-next"), want: splitCityRefused,
			refusal: refusedFlag("--claim-next"), instead: "`gc bd close <id>`, then `gc hook --claim`", runs: 1,
		},
		{
			form: "close <id> --continue", argv: splitCityArgv("close", "<id>", "--continue"), want: splitCityRefused,
			refusal: refusedFlag("--continue"), instead: "`gc bd close <id>`, then `gc hook --claim`", runs: 1,
		},
		{
			form: "close <id> --continue --no-auto", argv: splitCityArgv("close", "<id>", "--continue", "--no-auto"), want: splitCityRefused,
			refusal: refusedFlag("--continue"), instead: "`gc bd close <id>`, then `gc ready`", runs: 1,
		},
		{
			form: "close <id> --suggest-next", argv: splitCityArgv("close", "<id>", "--suggest-next"), want: splitCityRefused,
			refusal: refusedFlag("--suggest-next"), instead: "`gc bd close <id>`, then `gc ready`", runs: 1,
		},
		{
			form: "reopen <id>", argv: splitCityArgv("reopen", "<id>"), want: splitCityServed,
			arrange: func(t *testing.T, fx splitCityConformanceFixture, id string) {
				if err := fx.bindingStore(t).Close(id); err != nil {
					t.Fatalf("pre-closing %s: %v", id, err)
				}
			},
			check: func(t *testing.T, fx splitCityConformanceFixture, id, _, _ string) {
				if got := fx.bindingRow(t, id); got.Status == "closed" {
					t.Errorf("%s is still closed after the reopen", id)
				}
			},
		},
		{
			form: "release-if-current <id> <assignee>", argv: splitCityArgv("release-if-current", "<id>", "someone-else"), want: splitCityServed,
			check: func(t *testing.T, _ splitCityConformanceFixture, _, stdout, _ string) {
				if strings.TrimSpace(stdout) != "skipped" {
					t.Errorf("release-if-current printed %q, want skipped for a bead someone else does not hold", stdout)
				}
			},
		},
		{
			form: "heartbeat <id>", argv: splitCityArgv("heartbeat", "<id>"), want: splitCityRefused,
			refusal: "`gc bd heartbeat` is not served in process", instead: "a claim the class store holds carries no lease, so there is nothing to refresh", runs: 1,
		},
		{
			form: "label add <id> L", argv: splitCityArgv("label", "add", "<id>", "conformance"), shapes: minted, want: splitCityRefused,
			refusal: "`gc bd label add` is not served in process", instead: "the served update takes the same labels: `gc bd update <id> --add-label conformance`", runs: 1,
		},
		{
			form: "comments <id>", argv: splitCityArgv("comments", "<id>"), shapes: minted, want: splitCityRefused,
			refusal: "`gc bd comments` is not served in process", instead: "the class store contract carries no comments, so the bead has none to list", runs: 1,
		},
		{
			form: "comment <id> T", argv: splitCityArgv("comment", "<id>", "a comment"), shapes: minted, want: splitCityRefused,
			refusal: "`gc bd comment` is not served in process", instead: "keep the text as metadata instead: `gc bd update <id> --set-metadata <key>=<text>`",
		},
		{
			form: "children <id>", argv: splitCityArgv("children", "<id>"), shapes: minted, want: splitCityRefused,
			refusal: "`gc bd children` is not served in process", gap: "GraphStore.Children is on the contract but the door does not serve `children` yet",
		},
		{
			form: "list --parent <id>", argv: splitCityArgv("list", "--parent", "<id>"), shapes: minted, want: splitCityRefused,
			refusal: "`gc bd list` is not served in process", gap: "a parent-scoped list is a projection the door does not serve; it is `children` in another spelling",
		},
		// The relic shape of the five rows above is the door's known hole
		// (ga-zvetw): a work-shaped id in an argv that is neither a write
		// mutation the door's scanner reads nor a by-ID read reaches no residence
		// probe, so the command goes to bd and the work store answers for a bead
		// it does not hold. These rows pin that, so closing the hole flips them.
		{form: "label add <relic> L (ga-zvetw)", argv: splitCityArgv("label", "add", "<id>", "conformance"), shapes: relic, want: splitCityPassthrough, gap: splitCityRelicGap},
		{form: "comments <relic> (ga-zvetw)", argv: splitCityArgv("comments", "<id>"), shapes: relic, want: splitCityPassthrough, gap: splitCityRelicGap},
		{form: "comment <relic> T (ga-zvetw)", argv: splitCityArgv("comment", "<id>", "a comment"), shapes: relic, want: splitCityPassthrough, gap: splitCityRelicGap},
		{form: "children <relic> (ga-zvetw)", argv: splitCityArgv("children", "<id>"), shapes: relic, want: splitCityPassthrough, gap: splitCityRelicGap},
		{form: "list --parent <relic> (ga-zvetw)", argv: splitCityArgv("list", "--parent", "<id>"), shapes: relic, want: splitCityPassthrough, gap: splitCityRelicGap},
		{
			form: "dep add <id> <other-id>", argv: func(fx splitCityConformanceFixture, id string) []string {
				return []string{"dep", "add", id, fx.otherStepID}
			},
			want: splitCityRefused, refusal: "`gc bd dep add` is not served in process", gap: "edge writes between binding residents are not served; molecules get their edges from the graph apply",
		},
		{
			form: "delete <id>", argv: splitCityArgv("delete", "<id>"), want: splitCityRefused,
			refusal: "`gc bd delete` is not served in process", gap: "a delete of a class resident is not served; the class store's own sweeps retire its rows",
		},
		{
			form: "list --metadata-field gc.root_bead_id=<id>", argv: splitCityArgv("list", "--metadata-field", "gc.root_bead_id=<id>"), shapes: minted,
			want: splitCityRefused, refusal: "bd cannot read a relocated coordination class", instead: "`gc bd dep tree <root-id> --json` answers in process",
		},
		{
			form: "ready", argv: splitCityArgv("ready"), shapes: noID, want: splitCityRefused,
			refusal: "bd ready", instead: "Use `gc ready`",
		},
		{
			form: "linear status", argv: splitCityArgv("linear", "status"), shapes: noID, want: splitCityPassthrough,
		},
		{
			form: "linear sync", argv: splitCityArgv("linear", "sync"), shapes: noID, want: splitCityRefused,
			refusal: "an external-tracker sync writes the tracker's labels and types onto work-store beads unjudged", instead: "request an addition to the allowlist",
		},
	}
}

// requireShownJSON asserts stdout is bd's show --json shape for id.
func requireShownJSON(t *testing.T, _ splitCityConformanceFixture, id, stdout, _ string) {
	t.Helper()
	var shown []beads.Bead
	if err := json.Unmarshal([]byte(stdout), &shown); err != nil || len(shown) != 1 || shown[0].ID != id {
		t.Errorf("show --json rendered %q (%v), want a one-element array for %s", stdout, err, id)
	}
}

// requireTheSeededEdge asserts `dep list --json` rendered the edge the
// fixture seeded: the other step, related by splitCityEdgeType.
func requireTheSeededEdge(t *testing.T, fx splitCityConformanceFixture, id, stdout, _ string) {
	t.Helper()
	var rows []bdByIDDepRow
	if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
		t.Fatalf("dep list rendered %q, want a JSON array: %v", stdout, err)
	}
	for _, row := range rows {
		if row.ID == fx.otherStepID && row.DepType == splitCityEdgeType && !row.External {
			return
		}
	}
	t.Errorf("dep list of %s rendered %q, want the seeded %s edge to %s, read from the binding", id, stdout, splitCityEdgeType, fx.otherStepID)
}

// requireTheSeededTree asserts `dep tree --json` rendered the subject at the
// root and the other step one level below it.
func requireTheSeededTree(t *testing.T, fx splitCityConformanceFixture, id, stdout, _ string) {
	t.Helper()
	var rows []bdByIDTreeRow
	if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
		t.Fatalf("dep tree rendered %q, want a JSON array: %v", stdout, err)
	}
	walked := map[string]bdByIDTreeRow{}
	for _, row := range rows {
		walked[row.ID] = row
	}
	if root, ok := walked[id]; !ok || root.Depth != 0 {
		t.Errorf("dep tree of %s rendered %q, want the subject at depth 0", id, stdout)
	}
	if child, ok := walked[fx.otherStepID]; !ok || child.Depth != 1 || child.External {
		t.Errorf("dep tree of %s rendered %q, want %s at depth 1, read from the binding", id, stdout, fx.otherStepID)
	}
}

// splitCityAlternativeSpan is a backticked `gc bd` command in a refusal.
var splitCityAlternativeSpan = regexp.MustCompile("`(gc bd [^`]+)`")

// splitCityOptionalGroup is a usage spelling's optional group, `[--json]`.
var splitCityOptionalGroup = regexp.MustCompile(`\s*\[[^\]]*\]`)

// splitCityRunnableAlternatives returns the gc bd argv of every command a
// refusal names that can run as it stands: a backticked `gc bd` span with its
// optional groups dropped, left with no <placeholder>, and naming one of ids.
// A span naming no id is the refused verb quoted (`gc bd close` is not served),
// not a command to run.
func splitCityRunnableAlternatives(t *testing.T, stderr string, ids []string) [][]string {
	t.Helper()
	var out [][]string
	for _, match := range splitCityAlternativeSpan.FindAllStringSubmatch(stderr, -1) {
		command := splitCityOptionalGroup.ReplaceAllString(match[1], "")
		if strings.ContainsAny(command, "<>") {
			continue
		}
		argv := shellquote.Split(command)[2:]
		if !slices.ContainsFunc(argv, func(arg string) bool { return slices.Contains(ids, arg) }) {
			continue
		}
		out = append(out, argv)
	}
	return out
}

// TestSplitCityBeadCommandConformance runs every `gc bd` row of the table
// through doBd — the whole CLI path, the split allowlist and the read guards
// included — against a fresh split city.
func TestSplitCityBeadCommandConformance(t *testing.T) {
	for _, row := range splitCityBeadRows(t) {
		if row.want == splitCityRefused && row.instead == "" && row.gap == "" {
			t.Errorf("%s: a refused row must name the alternative or say why there is none", row.form)
		}
		shapes := row.shapes
		if shapes == nil {
			shapes = []splitCityIDShape{splitCityMinted, splitCityRelic}
		}
		for _, shape := range shapes {
			t.Run(row.form+"/"+string(shape), func(t *testing.T) {
				fx := newSplitCityConformanceFixture(t)
				id := ""
				if shape != splitCityNoID {
					id = fx.stepFor(shape)
				}
				if row.arrange != nil {
					row.arrange(t, fx, id)
				}
				before := beads.Bead{}
				if id != "" {
					before = fx.bindingRow(t, id)
				}
				argv := row.argv(fx, id)
				var stdout, stderr bytes.Buffer
				code := doBd(argv, &stdout, &stderr)
				switch row.want {
				case splitCityServed:
					if code != 0 {
						t.Fatalf("gc bd %v exited %d: %s", argv, code, stderr.String())
					}
					fx.requireBdNeverTargeted(t, id)
					if row.check != nil {
						row.check(t, fx, id, stdout.String(), stderr.String())
					}
				case splitCityRefused:
					if code != 1 {
						t.Fatalf("gc bd %v exited %d, want the documented refusal: stdout %q stderr %q", argv, code, stdout.String(), stderr.String())
					}
					for _, want := range []string{row.refusal, strings.ReplaceAll(row.instead, "<id>", id)} {
						if !strings.Contains(stderr.String(), want) {
							t.Errorf("gc bd %v refused without %q:\n%s", argv, want, stderr.String())
						}
					}
					if id != "" {
						fx.requireBdNeverTargeted(t, id)
						if after := fx.bindingRow(t, id); after.Status != before.Status {
							t.Errorf("the refused gc bd %v moved %s from %q to %q", argv, id, before.Status, after.Status)
						}
					}
					alternatives := splitCityRunnableAlternatives(t, stderr.String(), []string{id, fx.otherStepID, splitCityWorkID})
					if len(alternatives) != row.runs {
						t.Errorf("gc bd %v named %d runnable alternative(s) %q, want %d:\n%s", argv, len(alternatives), alternatives, row.runs, stderr.String())
					}
					for _, alternative := range alternatives {
						var altOut, altErr bytes.Buffer
						if code := doBd(alternative, &altOut, &altErr); code != 0 {
							t.Errorf("the alternative `gc bd %s` exited %d: %s", strings.Join(alternative, " "), code, altErr.String())
						}
						// The work half of a split is bd's to answer.
						if slices.Contains(alternative, splitCityWorkID) && !slices.Contains(fx.bdCalls(t), strings.Join(alternative, " ")) {
							t.Errorf("the work half `gc bd %s` was not forwarded to bd; bd saw %q", strings.Join(alternative, " "), fx.bdCalls(t))
						}
					}
					if id != "" {
						// The alternatives for a bead the binding holds are served
						// in process too.
						fx.requireBdNeverTargeted(t, id)
					}
				case splitCityPassthrough:
					if code != 0 {
						t.Fatalf("gc bd %v exited %d: %s", argv, code, stderr.String())
					}
					if calls := fx.bdCalls(t); !slices.Contains(calls, strings.Join(argv, " ")) {
						t.Errorf("gc bd %v was not forwarded to bd; bd saw %q", argv, calls)
					}
				}
			})
		}
	}
}

// splitCitySessionRow is one `gc session` verb run against a split city.
type splitCitySessionRow struct {
	verb string
	// run executes the verb against id and returns its exit code, stdout and
	// stderr.
	run func(t *testing.T, fx splitCityConformanceFixture, id string) (code int, stdout, stderr string)
	// check asserts the effect on the binding row after a successful run.
	check func(t *testing.T, fx splitCityConformanceFixture, id string)
}

// useSplitCityFakeSessionProvider routes newSessionProvider to one fake
// runtime (the newKillPokeSession pattern) and returns it.
func useSplitCityFakeSessionProvider(t *testing.T) *runtime.Fake {
	t.Helper()
	t.Setenv("GC_SESSION", "fake")
	fake := runtime.NewFake()
	old := buildSessionProviderByName
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		return fake, nil
	}
	t.Cleanup(func() { buildSessionProviderByName = old })
	return fake
}

// startSplitCityRuntime starts the fake runtime the bead id names, so a kill
// or close reaches a live handle.
func startSplitCityRuntime(t *testing.T, fx splitCityConformanceFixture, fake *runtime.Fake, id string) {
	t.Helper()
	name := fx.bindingRow(t, id).Metadata["session_name"]
	if err := fake.Start(context.Background(), name, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("fake Start(%s): %v", name, err)
	}
	if err := fake.SetMeta(name, "GC_SESSION_ID", id); err != nil {
		t.Fatalf("fake SetMeta(%s): %v", name, err)
	}
}

// registerSplitCityController stands up the in-process controller a reset
// needs. The socket comes from startSessionResetTestController, which
// registers a single-store runtime over the city's work store and so retires
// the funnel's memo; the split routes this city boots with are then opened
// afresh and registered over it, as a booting runtime registers its own. The
// circuit reset answers from whichever runtime is registered when it runs.
func registerSplitCityController(t *testing.T, fx splitCityConformanceFixture) {
	t.Helper()
	startSessionResetTestController(t, fx.cityDir)
	routes, unsettled := resolveCLIStorageRoutes(fx.cityDir)
	if routes == nil || unsettled {
		t.Fatalf("the split city resolved routes %+v (unsettled %t), want its class binding", routes, unsettled)
	}
	work := beads.NewMemStore()
	registerResidencyRoutes(fx.cityDir, routes, func() beads.Store { return work })
	t.Cleanup(func() {
		unregisterResidencyRoutes(fx.cityDir, routes)
		if err := routes.close(); err != nil {
			t.Errorf("closing the registered split routes: %v", err)
		}
	})
}

// splitCitySessionRows is the `gc session` half of the table: each verb reads
// and writes the session bead in the class binding. Every verb runs through
// its CLI entry point with the runtime faked; reset reaches a real controller
// socket in process.
func splitCitySessionRows() []splitCitySessionRow {
	pinRow := func(verb string, pinned bool, want string) splitCitySessionRow {
		return splitCitySessionRow{
			verb: verb,
			run: func(t *testing.T, fx splitCityConformanceFixture, id string) (int, string, string) {
				if !pinned {
					if err := fx.bindingStore(t).SetMetadata(id, "pin_awake", "true"); err != nil {
						t.Fatalf("arranging pin_awake on %s: %v", id, err)
					}
				}
				var stdout, stderr bytes.Buffer
				code := cmdSessionSetPin([]string{id}, pinned, &stdout, &stderr)
				return code, stdout.String(), stderr.String()
			},
			check: func(t *testing.T, fx splitCityConformanceFixture, id string) {
				if got := fx.bindingRow(t, id).Metadata["pin_awake"]; got != want {
					t.Errorf("binding pin_awake = %q, want %q", got, want)
				}
			},
		}
	}
	return []splitCitySessionRow{
		{
			verb: "gc session reset",
			run: func(t *testing.T, fx splitCityConformanceFixture, id string) (int, string, string) {
				useSplitCityFakeSessionProvider(t)
				registerSplitCityController(t, fx)
				var stdout, stderr bytes.Buffer
				code := cmdSessionReset([]string{id}, "", &stdout, &stderr, false)
				return code, stdout.String(), stderr.String()
			},
			check: func(t *testing.T, fx splitCityConformanceFixture, id string) {
				got := fx.bindingRow(t, id)
				if s := got.Metadata[sessionCircuitStateMetadata]; s != "" {
					t.Errorf("binding circuit state = %q, want cleared", s)
				}
				if got.Metadata[sessionCircuitResetGenerationMetadata] == "" {
					t.Error("binding reset generation is empty, want the explicit reset recorded")
				}
			},
		},
		{
			verb: "gc session kill",
			run: func(t *testing.T, fx splitCityConformanceFixture, id string) (int, string, string) {
				fake := useSplitCityFakeSessionProvider(t)
				startSplitCityRuntime(t, fx, fake, id)
				old := sessionKillPokeController
				sessionKillPokeController = func(string, reconcilekey.Key) error { return nil }
				t.Cleanup(func() { sessionKillPokeController = old })
				var stdout, stderr bytes.Buffer
				code := cmdSessionKillWithForce([]string{id}, "", &stdout, &stderr, false, false)
				return code, stdout.String(), stderr.String()
			},
			check: func(t *testing.T, fx splitCityConformanceFixture, id string) {
				if got := fx.bindingRow(t, id).Metadata["state"]; got != string(session.StateAsleep) {
					t.Errorf("binding state = %q, want %q", got, session.StateAsleep)
				}
			},
		},
		{
			verb: "gc session close",
			run: func(t *testing.T, fx splitCityConformanceFixture, id string) (int, string, string) {
				fake := useSplitCityFakeSessionProvider(t)
				startSplitCityRuntime(t, fx, fake, id)
				var stdout, stderr bytes.Buffer
				code := cmdSessionClose([]string{id}, "", &stdout, &stderr, false)
				return code, stdout.String(), stderr.String()
			},
			check: func(t *testing.T, fx splitCityConformanceFixture, id string) {
				if got := fx.bindingRow(t, id).Status; got != "closed" {
					t.Errorf("binding status = %q, want closed", got)
				}
			},
		},
		{
			// The managed-reconciler path: suspend records the hold and leaves
			// the stop to the controller. The direct worker-handle stop is not
			// exercised here.
			verb: "gc session suspend",
			run: func(t *testing.T, _ splitCityConformanceFixture, id string) (int, string, string) {
				useSplitCityFakeSessionProvider(t)
				oldManaged, oldPoke, oldEnqueue := sessionSuspendManagedReconciler, sessionSuspendPokeController, sessionSuspendEnqueueController
				sessionSuspendManagedReconciler = func(string) bool { return true }
				sessionSuspendPokeController = func(string) error { return nil }
				sessionSuspendEnqueueController = func(string, reconcilekey.Key) error { return nil }
				t.Cleanup(func() {
					sessionSuspendManagedReconciler, sessionSuspendPokeController, sessionSuspendEnqueueController = oldManaged, oldPoke, oldEnqueue
				})
				var stdout, stderr bytes.Buffer
				code := cmdSessionSuspend([]string{id}, "", &stdout, &stderr, false)
				return code, stdout.String(), stderr.String()
			},
			check: func(t *testing.T, fx splitCityConformanceFixture, id string) {
				if got := fx.bindingRow(t, id).Metadata["state"]; got != string(session.StateSuspended) {
					t.Errorf("binding state = %q, want %q", got, session.StateSuspended)
				}
			},
		},
		{
			verb: "gc session wake",
			run: func(t *testing.T, fx splitCityConformanceFixture, id string) (int, string, string) {
				if err := fx.bindingStore(t).SetMetadata(id, "state", string(session.StateSuspended)); err != nil {
					t.Fatalf("arranging a suspended %s: %v", id, err)
				}
				useSplitCityFakeSessionProvider(t)
				var stdout, stderr bytes.Buffer
				code := cmdSessionWake([]string{id}, "", &stdout, &stderr, false)
				return code, stdout.String(), stderr.String()
			},
			check: func(t *testing.T, fx splitCityConformanceFixture, id string) {
				got := fx.bindingRow(t, id)
				if s := got.Metadata["state"]; s == string(session.StateSuspended) {
					t.Errorf("binding state is still %q after the wake", s)
				}
				if got.Metadata["wake_request"] == "" {
					t.Error("binding wake_request is empty, want the explicit wake recorded")
				}
			},
		},
		pinRow("gc session pin", true, "true"),
		pinRow("gc session unpin", false, ""),
	}
}

// TestSplitCitySessionCommandConformance runs every `gc session` row against
// a session bead the binding holds, in both id shapes.
func TestSplitCitySessionCommandConformance(t *testing.T) {
	for _, row := range splitCitySessionRows() {
		for _, shape := range []splitCityIDShape{splitCityMinted, splitCityRelic} {
			t.Run(row.verb+"/"+string(shape), func(t *testing.T) {
				fx := newSplitCityConformanceFixture(t)
				id := fx.sessionFor(shape)
				code, stdout, stderr := row.run(t, fx, id)
				if code != 0 {
					t.Fatalf("%s %s exited %d: stdout %q stderr %q", row.verb, id, code, stdout, stderr)
				}
				row.check(t, fx, id)
				fx.requireBdNeverTargeted(t, id)
			})
		}
	}
}
