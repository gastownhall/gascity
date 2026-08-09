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
	err := RelocatedClassRefusal("ready projection", []RelocatedClass{graphRelocated()})
	if err == nil {
		t.Fatal("RelocatedClassRefusal returned nil for a matched class")
	}
	if !errors.Is(err, ErrBdSQLClassRelocated) {
		t.Fatalf("refusal does not match ErrBdSQLClassRelocated: %v", err)
	}
	for _, want := range []string{
		"ready projection",  // which read stopped
		"graph-class beads", // which class
		`"gcg-"`,            // which id namespace
		`the "infra" storage binding (provider sqlite-beads, .gc/store)`, // which store
		"bd has no SQLite backend",                                       // why bd did not error
		"empty result",                                                   // what would have been returned
		"gc bd show <id>",                                                // the federated verb
		"gc bd dep tree <id>",                                            // the federated verb
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal is missing %q:\n%v", want, err)
		}
	}
}

func TestRelocatedClassRefusalIsNilWhenNothingMatched(t *testing.T) {
	if err := RelocatedClassRefusal("bd sql", nil); err != nil {
		t.Fatalf("RelocatedClassRefusal(nil) = %v, want nil", err)
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
		"work only":             {"select id from issues where status <> 'closed'", false},
		"embedded in a word":    {"select * from issues where id = 'mygcg-abc'", false},
		"hyphenated tail":       {"select * from issues where id = 'x-gcg-abc'", false},
		"prefix without hyphen": {"select * from issues where id = 'gcgabc'", false},
		"empty":                 {"", false},
	} {
		t.Run(name, func(t *testing.T) {
			got := len(RelocatedClassesInSQL(relocated, tc.sql)) > 0
			if got != tc.want {
				t.Fatalf("RelocatedClassesInSQL(%q) matched = %v, want %v", tc.sql, got, tc.want)
			}
		})
	}
}

// TestRelocatedClassesInSQLIsInertWithoutRelocation is the single-store
// compatibility proof for the text scanner: the exact query that a split city
// refuses is not even examined when nothing is relocated.
func TestRelocatedClassesInSQLIsInertWithoutRelocation(t *testing.T) {
	if got := RelocatedClassesInSQL(nil, "select * from issues where id = 'gcg-abc'"); len(got) != 0 {
		t.Fatalf("RelocatedClassesInSQL with no relocated classes matched %v, want none", got)
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

func TestReadyProjectionRefusesWhenAskedAboutARelocatedClass(t *testing.T) {
	runner := readyProjectionRunner(`[]`)
	s := NewBdStore("/city", runner.run, WithBdStoreRelocatedClasses(graphRelocated()))

	_, err := s.enrichReadyProjectionForCache([]Bead{
		{ID: "bd-1", Type: "task", Status: "open"},
		{ID: "gcg-root", Type: "task", Status: "open"},
	})
	if !errors.Is(err, ErrBdSQLClassRelocated) {
		t.Fatalf("enrichReadyProjectionForCache error = %v, want ErrBdSQLClassRelocated", err)
	}
	if !strings.Contains(err.Error(), "ready projection") {
		t.Errorf("refusal does not name the refused operation: %v", err)
	}
	for _, call := range runner.calls {
		if len(call) > 1 && call[1] == "sql" {
			t.Fatalf("ready projection ran %v against bd despite the refusal", call)
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
