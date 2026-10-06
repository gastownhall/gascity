package beads

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// graphRelocated is the split every one of these tests is written against: a
// city serving graph-class beads from a SQLite binding while work stays on bd.
func graphRelocated() RelocatedClass {
	return RelocatedClass{
		Class:    "graph",
		IDPrefix: "gcg",
		Location: `the "infra" storage binding (provider sqlite-beads, .gc/store)`,
	}
}

// TestRelocatedClassRefusalNamesEverythingAnOperatorNeeds pins the message
// content, because the whole point of the refusal is that someone hitting it at
// 2am does not have to read source to know what happened or what to run.
func TestRelocatedClassRefusalNamesEverythingAnOperatorNeeds(t *testing.T) {
	err := RelocatedClassRefusal("release-if-current gcg-abc123", []RelocatedClass{graphRelocated()})
	if err == nil {
		t.Fatal("RelocatedClassRefusal returned nil for a matched class")
	}
	if !errors.Is(err, ErrBdSQLClassRelocated) {
		t.Fatalf("refusal does not match ErrBdSQLClassRelocated: %v", err)
	}
	for _, want := range []string{
		"release-if-current gcg-abc123", // which read stopped
		"graph-class beads",             // which class
		`"gcg-"`,                        // which id namespace
		`the "infra" storage binding (provider sqlite-beads, .gc/store)`, // which store
		"holds no row under their reserved id prefixes",                  // why bd did not error, without naming a backend
		"cannot match here",                 // stated as a property of the prefix, not of this query's result
		"gc beads show <id>",                // the verb that is actually class-routed
		"GET /v0/city/{cityName}/bead/{id}", // and the route it uses
		"gc ready --metadata-field",         // the SET-returning escape, for a refused projection
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal is missing %q:\n%v", want, err)
		}
	}
}

// TestRelocatedClassRefusalDoesNotRecommendABlindVerb pins the correction that
// mattered most: a verb this message names as the way out must not answer from
// the same ledger the message just said cannot hold the rows. Recommending one
// sent the operator to a command carrying the exact bug this refusal warns
// about.
//
// The set of blind verbs shrank once. `gc bd show` and `gc bd dep list` acquired
// in-process class routing (cmd/gc/cmd_bd_by_id.go) and are now the read this
// message names first. `gc bd dep tree` did not acquire routing, but it is no
// longer blind either: on a relocated id that surface refuses it. The pin moved
// with the fact rather than being deleted, because a message that offered a
// verb which cannot answer would be the original defect returning.
func TestRelocatedClassRefusalDoesNotRecommendABlindVerb(t *testing.T) {
	msg := RelocatedClassRefusal("bd sql", []RelocatedClass{graphRelocated()}).Error()
	if strings.Contains(msg, "Use the federated `gc bd") {
		t.Errorf("refusal still recommends a raw bd passthrough as federated:\n%s", msg)
	}
	if !strings.Contains(msg, "`gc bd dep tree <id>` is not served in process") {
		t.Errorf("refusal does not say dep tree is unavailable on a relocated id:\n%s", msg)
	}
	if !strings.Contains(msg, "`gc bd show <id>`") {
		t.Errorf("refusal does not name the by-ID read that IS class-routed:\n%s", msg)
	}
}

func TestRelocatedClassRefusalIsNilWhenNothingMatched(t *testing.T) {
	if err := RelocatedClassRefusal("bd sql", nil); err != nil {
		t.Fatalf("RelocatedClassRefusal(nil) = %v, want nil", err)
	}
}

// TestRelocatedClassRefusalStatesTheLimitsOfTheSetEscape is the honesty pin on
// the one line of this message that is steering rather than diagnosis.
//
// Naming `gc ready --metadata-field` and stopping there sends an operator to a
// command that answers a NARROWER question: with no --status it returns only
// claimable work, so a molecule whose steps are in flight, blocked or closed
// comes back `[]` — the refused bug, one command over, from the command this
// message recommended. --status takes exactly one of open/in_progress/blocked/
// closed (cmd/gc/cmd_ready.go readyKnownStatuses): no --all, no comma list, and
// `deferred` is not selectable at all, so `bd list --all`'s question has no
// federated spelling yet.
//
// A refusal that oversells its escape is the same failure as the silent empty
// it exists to remove: a confident answer to a question that was not asked. So
// the message states the limit, and this test fails if the statement is dropped.
func TestRelocatedClassRefusalStatesTheLimitsOfTheSetEscape(t *testing.T) {
	msg := RelocatedClassRefusal("bd list", []RelocatedClass{graphRelocated()}).Error()
	for _, want := range []string{
		"with no --status it returns only claimable work",
		"exactly one of open, in_progress, blocked, closed",
		"deferred is not selectable",
		"no federated equivalent of `bd list --all`",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the steering does not state %q, so it oversells `gc ready` as the way out:\n%s", want, msg)
		}
	}
}

// TestRelocatedClassRefusalLeavesTheOverrideToTheCLI pins where the escape
// hatch is named, and why it is not named here.
//
// This string is also what BdStore's id-scoped guard returns, and that path
// honors no environment variable. Naming GC_BD_ALLOW_RELOCATED_CLASS_READ in
// the shared text would advertise an escape that does not work on half the
// paths that print it — a false statement of the same kind this guard exists to
// stop. cmd/gc appends it at the one seam where it is true.
func TestRelocatedClassRefusalLeavesTheOverrideToTheCLI(t *testing.T) {
	msg := RelocatedClassRefusal("release-if-current gcg-abc123", []RelocatedClass{graphRelocated()}).Error()
	if strings.Contains(msg, "GC_BD_ALLOW_RELOCATED_CLASS_READ") {
		t.Errorf("the shared refusal names a `gc bd` override that does not apply to the store guard:\n%s", msg)
	}
}

// nudgesRelocated is the split with a subsystem inside it: the nudges binding
// mints "gcn" and also HOLDS the queue's "gcnq" records, which the queue itself
// mints from a content hash rather than from the store's sequence.
func nudgesRelocated() RelocatedClass {
	return RelocatedClass{
		Class:        "nudges",
		IDPrefix:     "gcn",
		HeldPrefixes: []string{"gcnq"},
		Location:     `the "infra" storage binding (provider sqlite-beads, .gc/store)`,
	}
}

// TestRelocatedClassGuardsANamespaceItHoldsWithoutMinting is the gap this
// field closed.
//
// A held namespace is exactly as invisible to bd as a minted one: no row of the
// work ledger carries "gcnq-" either, so a read scoped to one runs successfully
// against the ledger, matches nothing, and returns the same confident empty
// answer. Keying the guard on the MINT prefix alone made the queue's own
// records the one class-owned namespace a raw `bd sql` could ask about without
// being stopped.
func TestRelocatedClassGuardsANamespaceItHoldsWithoutMinting(t *testing.T) {
	nudges := []RelocatedClass{nudgesRelocated()}

	if !nudgesRelocated().matchesID("gcnq-abc-q") {
		t.Error("a queue record id is not recognized as class-owned; the id-scoped store guard lets it through")
	}
	if len(RelocatedClassesInSQL(nudges, "select * from issues where id = 'gcnq-abc-q'")) == 0 {
		t.Error("bd SQL naming a queue record id is not refused; bd would answer it emptily against the work ledger")
	}
	if len(RelocatedClassesInQueryExpr(nudges, "id=gcnq-*")) == 0 {
		t.Error("a bd query over the queue namespace is not refused")
	}
	if len(RelocatedClassesInSelector(nudges, "gc.root_bead_id=gcnq-abc-q")) == 0 {
		t.Error("a selector predicate over the queue namespace is not refused")
	}

	// One match, not two. "gcn" is a prefix of "gcnq" as a STRING, and a class
	// reported twice would print its topology twice in the refusal.
	if got := RelocatedClassesInSQL(nudges, "select * from issues where id in ('gcn-1','gcnq-abc-q')"); len(got) != 1 {
		t.Errorf("a statement naming both namespaces matched %d classes, want the class once", len(got))
	}

	// The control: a class that holds nothing extra must NOT claim the queue
	// namespace. "gcnq-abc" starts with "gcn" as a string, and the boundary rule
	// — not the held list — is what keeps that from being a match, so a guard
	// that "passes" by over-matching is not passing.
	mintOnly := []RelocatedClass{{Class: "nudges", IDPrefix: "gcn", Location: "somewhere"}}
	if mintOnly[0].matchesID("gcnq-abc-q") {
		t.Error("a mint-only class claimed the queue namespace by string prefix; the id boundary rule is gone")
	}
	if len(RelocatedClassesInSQL(mintOnly, "select * from issues where id = 'gcnq-abc-q'")) != 0 {
		t.Error("a mint-only class matched a queue id in SQL; the match is not anchored at an id boundary")
	}
}

// TestRelocatedClassRefusalNamesAHeldNamespace pins the operator-facing half.
// Someone refused while holding a "gcnq-" id and shown a message that names
// only "gcn-" reads the guard as having misfired, and reaches for the override.
func TestRelocatedClassRefusalNamesAHeldNamespace(t *testing.T) {
	msg := RelocatedClassRefusal("bd sql", []RelocatedClass{nudgesRelocated()}).Error()
	for _, want := range []string{`"gcn-"`, `"gcnq-"`} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not name %s:\n%s", want, msg)
		}
	}
}

func TestRelocatedClassesInSQLMatchesOnlyAtIDBoundaries(t *testing.T) {
	relocated := []RelocatedClass{graphRelocated()}
	for name, tc := range map[string]struct {
		sql  string
		want bool
	}{
		"literal id":            {"select * from issues where id = 'gcg-abc123'", true},
		"like pattern":          {"select id from issues where id like 'gcg-%'", true},
		"in list":               {"select id from issues where id in ('bd-1','gcg-2')", true},
		"uppercase":             {"SELECT * FROM issues WHERE id = 'GCG-ABC'", true},
		"start of statement":    {"gcg-abc", true},
		"double quoted literal": {`select * from issues where id = "gcg-abc"`, true},
		"work only":             {"select id from issues where status <> 'closed'", false},
		"embedded in a word":    {"select * from issues where id = 'mygcg-abc'", false},
		"hyphenated tail":       {"select * from issues where id = 'x-gcg-abc'", false},
		"prefix without hyphen": {"select * from issues where id = 'gcgabc'", false},
		"empty":                 {"", false},

		// A LIKE pattern that omits the hyphen still names the namespace:
		// 'gcg%' and 'gcg_%' both match every gcg- id in the ledger, so a
		// matcher keyed on the literal "gcg-" would wave them through.
		"like without the hyphen":   {"select id from issues where id like 'gcg%'", true},
		"like single-char wildcard": {"select id from issues where id like 'gcg_%'", true},

		// Everything below is a query ABOUT the work ledger that merely mentions
		// a relocated id. bd answers these correctly and non-emptily — the work
		// ledger really does carry gcg- strings in text and JSON columns (e.g.
		// ensureDrainUnitConvoy stamps gc.drain_control_id = <graph control id>
		// on a convoy coordclass deliberately keeps work-class) — so refusing
		// them is a false positive, and the refusal's own wording asserts the
		// opposite of what would have happened.
		"like contains on a text column": {"select id,title from issues where metadata like '%gcg-abc%'", false},
		"like contains on the namespace": {"select id,title,metadata from issues where metadata like '%gcg-%'", false},
		"block comment mention":          {"/* related: gcg-9 */ select id from issues where status='open'", false},
		"trailing line comment":          {"select 1 -- gcg-x", false},
		"path-shaped token":              {"--out /tmp/gcg-dump.csv", false},
		"bare mention mid-statement":     {"select id from issues where notes like '%see gcg-1%'", false},
	} {
		t.Run(name, func(t *testing.T) {
			got := len(RelocatedClassesInSQL(relocated, tc.sql)) > 0
			if got != tc.want {
				t.Fatalf("RelocatedClassesInSQL(%q) matched = %v, want %v", tc.sql, got, tc.want)
			}
		})
	}
}

// TestRelocatedClassesInQueryExprMatchesTheValueSide covers bd's OTHER ad-hoc
// verb. `bd query "id=gcg-*"` names the relocated namespace without quoting it,
// pushes it down to an id filter against the same ledger, and prints [] with
// exit 0 on no match — so the sql guard alone left the incident one word away.
func TestRelocatedClassesInQueryExprMatchesTheValueSide(t *testing.T) {
	relocated := []RelocatedClass{graphRelocated()}
	for name, tc := range map[string]struct {
		expr string
		want bool
	}{
		"id equality":           {"id=gcg-abc123", true},
		"id wildcard":           {"id=gcg-*", true},
		"id wildcard no hyphen": {"id=gcg*", true},
		"parent":                {"parent=gcg-root", true},
		"spaces around the op":  {"id = gcg-1", true},
		"compound":              {"status=open AND id=gcg-1", true},
		"negated":               {"NOT id=gcg-1", true},
		"grouped":               {"(id=gcg-1 OR id=gcg-2)", true},
		"quoted value":          {"id='gcg-1'", true},
		"work only":             {"status=open AND priority>1", false},
		"work id":               {"id=bd-42", false},
		"mentioned inside text": {`title="fix gcg-1 regression"`, false},
		"embedded in a word":    {"id=mygcg-1", false},
		"prefix without hyphen": {"id=gcgabc", false},
		"empty":                 {"", false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := len(RelocatedClassesInQueryExpr(relocated, tc.expr)) > 0; got != tc.want {
				t.Fatalf("RelocatedClassesInQueryExpr(%q) matched = %v, want %v", tc.expr, got, tc.want)
			}
		})
	}
}

// TestRelocatedClassesInSelectorMatchesOnlyThePredicateValue covers the third
// dialect, and the one that produced the measurement this guard family was
// opened for: `gc bd list --metadata-field gc.root_bead_id=<gcg root>` answered
// `[]` with exit 0 on a converged split city. `list` is a PROJECTION — bd runs
// it successfully against the work ledger and matches nothing, because no gcg-
// row is there to match — so the empty array is a confident wrong answer about
// a class the ledger does not serve.
//
// The negatives matter MORE than the positives here, and they are the reason
// this dialect does not reuse atQueryValueStart. `bd list` accepts no
// positional arguments, so every token this scan sees is some flag's VALUE; an
// offset-0 anchor would therefore make every free-text selector id-shaped and
// refuse `--title-contains gcg-abc123`, a LIKE-contains over this ledger's own
// title column that bd answers correctly and often non-emptily. So the whole
// negative half of this table is a bare or embedded id in a value with no `=`
// in front of it, in every spelling an operator produces — including the ones
// with punctuation before the id, which a prose-only negative would miss.
func TestRelocatedClassesInSelectorMatchesOnlyThePredicateValue(t *testing.T) {
	relocated := []RelocatedClass{graphRelocated()}
	for name, tc := range map[string]struct {
		selector string
		want     bool
	}{
		"metadata field on the root id": {"gc.root_bead_id=gcg-abc123", true},
		"metadata field on a step ref":  {"gc.step_ref=gcg-abc123.mol.work", true},
		"quoted value":                  {`gc.root_bead_id="gcg-abc123"`, true},
		"single-quoted value":           {"gc.root_bead_id='gcg-abc123'", true},
		"spaced predicate":              {"gc.root_bead_id= gcg-abc123", true},
		"uppercase":                     {"gc.root_bead_id=GCG-ABC123", true},

		"metadata field on a work root":  {"gc.root_bead_id=demo-abc123", false},
		"metadata key with no value":     {"gc.root_bead_id", false},
		"prefix continuation":            {"gc.root_bead_id=gcgx-1", false},
		"prefix without the hyphen":      {"gc.root_bead_id=gcgabc", false},
		"a status the work store serves": {"open", false},
		"empty":                          {"", false},

		// Every one of these is a whole free-text selector value — the shape a
		// `--title-contains`/`--label`/`--assignee` search arrives in. All of
		// them refused before this anchor existed.
		"bare id as a whole selector value": {"gcg-abc123", false},
		"bare prefix":                       {"gcg", false},
		"label glob":                        {"gcg-*", false},
		"title search mentioning an id":     {"fix gcg-1 regression", false},
		"prose with a paren before the id":  {"fix (gcg-1) regression", false},
		"prose with a comma before the id":  {"regressions: gcg-1, gcg-2", false},
		"prose with a quote before the id":  {"root is 'gcg-1' here", false},
		"prose with a colon before the id":  {"root: gcg-1", false},
		"prose with an angle before the id": {"<gcg-1> is stuck", false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := len(RelocatedClassesInSelector(relocated, tc.selector)) > 0; got != tc.want {
				t.Fatalf("RelocatedClassesInSelector(%q) matched = %v, want %v", tc.selector, got, tc.want)
			}
		})
	}
}

// TestRelocatedClassesInSelectorDivergesFromTheQueryDialect states the split
// between the two dialects as a fact rather than leaving it implied by two
// tables that happen to disagree.
//
// `bd query` takes ONE expression, so a bare term at offset 0 is a value
// position and `bd query gcg-1` is an id-scoped read. A selector flag has
// already consumed its own token, so the same text arriving here is one flag's
// search string. Same characters, different question — and reusing the query
// anchor for selectors is exactly the bug this pins.
func TestRelocatedClassesInSelectorDivergesFromTheQueryDialect(t *testing.T) {
	relocated := []RelocatedClass{graphRelocated()}
	const bare = "gcg-abc123"
	if len(RelocatedClassesInQueryExpr(relocated, bare)) == 0 {
		t.Errorf("RelocatedClassesInQueryExpr(%q) did not match; a bare term IS a value in the query DSL", bare)
	}
	if got := RelocatedClassesInSelector(relocated, bare); len(got) != 0 {
		t.Errorf("RelocatedClassesInSelector(%q) matched %v; a whole-token selector value is a search string, and refusing it breaks `--title-contains`", bare, got)
	}
}

// TestRelocatedClassesInSQLIsInertWithoutRelocation is the single-store
// compatibility proof for the text scanner: the exact query that a split city
// refuses is not even examined when nothing is relocated.
func TestRelocatedClassesInSQLIsInertWithoutRelocation(t *testing.T) {
	if got := RelocatedClassesInSQL(nil, "select * from issues where id = 'gcg-abc'"); len(got) != 0 {
		t.Fatalf("RelocatedClassesInSQL with no relocated classes matched %v, want none", got)
	}
	if got := RelocatedClassesInQueryExpr(nil, "id=gcg-abc"); len(got) != 0 {
		t.Fatalf("RelocatedClassesInQueryExpr with no relocated classes matched %v, want none", got)
	}
	if got := RelocatedClassesInSelector(nil, "gc.root_bead_id=gcg-abc"); len(got) != 0 {
		t.Fatalf("RelocatedClassesInSelector with no relocated classes matched %v, want none", got)
	}
}

// recordingRunner captures every command a store issues so a test can assert
// both what was sent and that nothing was sent at all.
type recordingRunner struct {
	calls [][]string
	reply func(args []string) ([]byte, error)
}

func (r *recordingRunner) run(_, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	if r.reply != nil {
		return r.reply(args)
	}
	return []byte(`{"rows_affected":1,"schema_version":1}`), nil
}

func TestReleaseIfCurrentRefusesRelocatedClassIDWithoutTouchingBd(t *testing.T) {
	runner := &recordingRunner{}
	s := NewBdStore("/city", runner.run, WithBdStoreRelocatedClasses(graphRelocated()))

	released, err := s.ReleaseIfCurrent("gcg-abc123", "worker-1")
	if !errors.Is(err, ErrBdSQLClassRelocated) {
		t.Fatalf("ReleaseIfCurrent error = %v, want ErrBdSQLClassRelocated", err)
	}
	if released {
		t.Fatal("ReleaseIfCurrent released = true on a refusal")
	}
	if !strings.Contains(err.Error(), "release-if-current gcg-abc123") {
		t.Errorf("refusal does not name the refused operation: %v", err)
	}
	// The point of the guard is that bd is never asked a question whose empty
	// answer would be believed.
	if len(runner.calls) != 0 {
		t.Fatalf("ReleaseIfCurrent ran %v against bd despite the refusal", runner.calls)
	}
}

// releaseMutationCalls drops the collision-preflight read ReleaseIfCurrent runs
// before its conditional-release verb (bdstore_conditional_release.go), leaving
// only the calls that would write.
func releaseMutationCalls(calls [][]string) [][]string {
	var mutations [][]string
	for _, call := range calls {
		if len(call) == 4 && call[1] == "show" {
			continue
		}
		mutations = append(mutations, call)
	}
	return mutations
}

// TestReleaseIfCurrentIsByteIdenticalWithoutRelocation is the mutation proof:
// the same store, the same call, with the relocated class removed, must produce
// exactly the argv the unguarded code produced. That argv is bd's native
// conditional-release verb preceded by the collision-preflight read — pinned
// here in full, because a guard that perturbed a single-store city at all would
// show up as a diff in this sequence. The verb itself is owned by
// TestReleaseIfCurrentPrefersTheNativeVerb; if it changes, both move together.
func TestReleaseIfCurrentIsByteIdenticalWithoutRelocation(t *testing.T) {
	runner := &recordingRunner{}
	s := NewBdStore("/city", runner.run)

	released, err := s.ReleaseIfCurrent("gcg-abc123", "worker-1")
	if err != nil {
		t.Fatalf("ReleaseIfCurrent: %v", err)
	}
	if !released {
		t.Fatal("ReleaseIfCurrent released = false, want true")
	}
	want := [][]string{
		{"bd", "show", "--json", "gcg-abc123"},
		{"bd", "update", "gcg-abc123", "--if-assignee", "worker-1", "--if-status", "in_progress", "--status", "open", "--assignee", ""},
	}
	if len(runner.calls) != len(want) {
		t.Fatalf("calls = %v, want exactly %v", runner.calls, want)
	}
	for i := range want {
		if !equalStrings(runner.calls[i], want[i]) {
			t.Fatalf("call %d = %v, want %v", i, runner.calls[i], want[i])
		}
	}
}

// TestReleaseIfCurrentPassesWorkIDsThroughOnASplitCity proves the guard is
// scoped to the relocated namespace and does not tax the work ledger it still
// serves.
func TestReleaseIfCurrentPassesWorkIDsThroughOnASplitCity(t *testing.T) {
	runner := &recordingRunner{}
	s := NewBdStore("/city", runner.run, WithBdStoreRelocatedClasses(graphRelocated()))

	if _, err := s.ReleaseIfCurrent("bd-42", "worker-1"); err != nil {
		t.Fatalf("ReleaseIfCurrent on a work id: %v", err)
	}
	mutations := releaseMutationCalls(runner.calls)
	if len(mutations) != 1 {
		t.Fatalf("calls = %v, want one conditional-release write", runner.calls)
	}
	if mutations[0][2] != "bd-42" {
		t.Fatalf("write did not target the work bead: %v", mutations[0])
	}
}

// TestReleaseIfCurrentDoesNotMatchAPrefixContinuation guards the boundary rule
// on the id path: "gcgx-1" is a work id in a rig whose prefix merely starts
// with the reserved letters, and refusing it would be a false alarm.
func TestReleaseIfCurrentDoesNotMatchAPrefixContinuation(t *testing.T) {
	runner := &recordingRunner{}
	s := NewBdStore("/city", runner.run, WithBdStoreRelocatedClasses(graphRelocated()))

	if _, err := s.ReleaseIfCurrent("gcgx-1", "worker-1"); err != nil {
		t.Fatalf("ReleaseIfCurrent on gcgx-1: %v", err)
	}
	mutations := releaseMutationCalls(runner.calls)
	if len(mutations) != 1 || mutations[0][2] != "gcgx-1" {
		t.Fatalf("calls = %v, want the write to pass through targeting gcgx-1", runner.calls)
	}
}

func readyProjectionRunner(reply string) *recordingRunner {
	r := &recordingRunner{}
	r.reply = func(args []string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case joined == "version":
			return []byte("bd version 1.1.0\n"), nil
		case len(args) > 0 && args[0] == "sql":
			return []byte(reply), nil
		}
		return nil, fmt.Errorf("unexpected command: %s", joined)
	}
	return r
}

// TestReadyProjectionPartitionsARelocatedIDOutOfTheBatch pins that one stray
// relocated id costs the batch nothing but itself. The guard used to be
// whole-batch: CachingStore prime/reconcile hand EVERY active bead to this
// enrichment, and a single refusal returned the whole slice unenriched, so
// every row in the city lost is_blocked on every cycle forever (the call sites
// only recordProblem and continue — there is no backoff and no escalation, and
// the refusal is a pure function of config, so it never heals).
func TestReadyProjectionPartitionsARelocatedIDOutOfTheBatch(t *testing.T) {
	runner := readyProjectionRunner(`[{"id":"bd-1","is_blocked":false},{"id":"bd-2","is_blocked":true}]`)
	s := NewBdStore("/city", runner.run, WithBdStoreRelocatedClasses(graphRelocated()))

	out, err := s.enrichReadyProjectionForCache([]Bead{
		{ID: "bd-1", Type: "task", Status: "open"},
		{ID: "bd-2", Type: "task", Status: "open"},
		{ID: "gcg-stray", Type: "task", Status: "open"},
	})
	if err != nil {
		t.Fatalf("enrichReadyProjectionForCache: %v", err)
	}
	byID := make(map[string]Bead, len(out))
	for _, bead := range out {
		byID[bead.ID] = bead
	}
	for id, want := range map[string]bool{"bd-1": false, "bd-2": true} {
		got := byID[id].IsBlocked
		if got == nil {
			t.Fatalf("bead %s lost its is_blocked because a relocated id shared the batch", id)
		}
		if *got != want {
			t.Errorf("bead %s is_blocked = %v, want %v", id, *got, want)
		}
	}
	// The relocated row keeps bd's nil fallback, which is the documented benign
	// state (preserveCachedReadyProjectionLocked) and exactly what its absence
	// from the answer produced before the guard existed.
	if byID["gcg-stray"].IsBlocked != nil {
		t.Errorf("relocated bead was enriched from the wrong ledger: %v", *byID["gcg-stray"].IsBlocked)
	}
}

// TestReadyProjectionSkipsBdEntirelyWhenEveryIDIsRelocated is the other half of
// the partition: with nothing left to ask about, the projection asks nothing.
func TestReadyProjectionSkipsBdEntirelyWhenEveryIDIsRelocated(t *testing.T) {
	runner := readyProjectionRunner(`[{"id":"gcg-root","is_blocked":true}]`)
	s := NewBdStore("/city", runner.run, WithBdStoreRelocatedClasses(graphRelocated()))

	out, err := s.enrichReadyProjectionForCache([]Bead{{ID: "gcg-root", Type: "task", Status: "open"}})
	if err != nil {
		t.Fatalf("enrichReadyProjectionForCache: %v", err)
	}
	if len(out) != 1 || out[0].IsBlocked != nil {
		t.Fatalf("relocated bead was enriched from the work ledger: %+v", out)
	}
	for _, call := range runner.calls {
		if len(call) > 1 && call[1] == "sql" {
			t.Fatalf("ready projection ran %v against bd for a relocated-only batch", call)
		}
	}
}

// TestReadyProjectionIsByteIdenticalWithoutRelocation is the mutation proof for
// the hot path: the identical call, with the relocated class removed, must
// issue the same bd sql and enrich the same rows.
func TestReadyProjectionIsByteIdenticalWithoutRelocation(t *testing.T) {
	runner := readyProjectionRunner(`[{"id":"bd-1","is_blocked":false},{"id":"gcg-root","is_blocked":true}]`)
	s := NewBdStore("/city", runner.run)

	out, err := s.enrichReadyProjectionForCache([]Bead{
		{ID: "bd-1", Type: "task", Status: "open"},
		{ID: "gcg-root", Type: "task", Status: "open"},
	})
	if err != nil {
		t.Fatalf("enrichReadyProjectionForCache: %v", err)
	}
	wantSQL := []string{"bd", "sql", readyProjectionSQL(), "--json"}
	sawSQL := false
	for _, call := range runner.calls {
		if len(call) > 1 && call[1] == "sql" {
			sawSQL = true
			if !equalStrings(call, wantSQL) {
				t.Fatalf("bd sql call = %v, want %v", call, wantSQL)
			}
		}
	}
	if !sawSQL {
		t.Fatalf("ready projection issued no bd sql; calls = %v", runner.calls)
	}
	for _, bead := range out {
		if bead.IsBlocked == nil {
			t.Fatalf("bead %s was not enriched", bead.ID)
		}
	}
}

// TestReadyProjectionPassesWorkOnlySetsThroughOnASplitCity proves the reconcile
// cycle of a split city keeps running: the work ledger's own rows are still
// enriched, because they are not the ones that moved.
func TestReadyProjectionPassesWorkOnlySetsThroughOnASplitCity(t *testing.T) {
	runner := readyProjectionRunner(`[{"id":"bd-1","is_blocked":true}]`)
	s := NewBdStore("/city", runner.run, WithBdStoreRelocatedClasses(graphRelocated()))

	out, err := s.enrichReadyProjectionForCache([]Bead{{ID: "bd-1", Type: "task", Status: "open"}})
	if err != nil {
		t.Fatalf("enrichReadyProjectionForCache: %v", err)
	}
	if len(out) != 1 || out[0].IsBlocked == nil || !*out[0].IsBlocked {
		t.Fatalf("work bead was not enriched: %+v", out)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
